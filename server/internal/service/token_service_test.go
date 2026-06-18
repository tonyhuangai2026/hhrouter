package service

import (
	"fmt"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

func newTokenTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:tokentest_%d?mode=memory&cache=shared", uniq())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

func TestGenerateKeyAndHash(t *testing.T) {
	k1, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !strings.HasPrefix(k1, "sk-") {
		t.Fatalf("key %q does not start with sk-", k1)
	}
	k2, _ := GenerateKey()
	if k1 == k2 {
		t.Fatal("two generated keys are identical")
	}
	if HashKey(k1) == HashKey(k2) {
		t.Fatal("hashes of distinct keys collide")
	}
	if HashKey(k1) != HashKey(k1) {
		t.Fatal("hash not deterministic")
	}
}

func TestCreateToken_ReturnsPlaintextOnceMasksAfter(t *testing.T) {
	gdb := newTokenTestDB(t)
	svc := NewTokenService(gdb)

	name := "my-key"
	res, err := svc.Create(7, TokenInput{Name: &name})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(res.PlainKey, "sk-") {
		t.Fatalf("plain key %q not sk- prefixed", res.PlainKey)
	}
	// The embedded model must not leak the plaintext key.
	if res.Token.Key != nil {
		t.Fatalf("embedded Key should be nil, got %v", res.Token.Key)
	}
	if res.KeyMasked == "" || strings.Contains(res.KeyMasked, res.PlainKey) {
		t.Fatalf("mask %q must be present and not contain plaintext", res.KeyMasked)
	}

	// Stored row keeps only the hash, never the plaintext.
	var stored model.Token
	gdb.First(&stored, res.Token.ID)
	if stored.Key != nil {
		t.Fatalf("stored Key column should be NULL, got %v", stored.Key)
	}
	if stored.KeyHash != HashKey(res.PlainKey) {
		t.Fatalf("stored key_hash mismatch")
	}

	// Subsequent reads only ever show the mask, never plaintext.
	got, err := svc.Get(7, res.Token.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Token.Key != nil {
		t.Fatalf("Get leaked plaintext key")
	}
	if got.KeyMasked == "" {
		t.Fatalf("Get should return a mask")
	}
}

func TestTokenOwnershipGating(t *testing.T) {
	gdb := newTokenTestDB(t)
	svc := NewTokenService(gdb)

	name := "owner-key"
	res, err := svc.Create(1, TokenInput{Name: &name})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := res.Token.ID

	// Another user cannot read, update or delete it.
	if _, err := svc.Get(2, id); err != ErrTokenNotFound {
		t.Fatalf("cross-user Get err = %v, want ErrTokenNotFound", err)
	}
	newName := "hacked"
	if _, err := svc.Update(2, id, TokenInput{Name: &newName}); err != ErrTokenNotFound {
		t.Fatalf("cross-user Update err = %v, want ErrTokenNotFound", err)
	}
	if err := svc.Delete(2, id); err != ErrTokenNotFound {
		t.Fatalf("cross-user Delete err = %v, want ErrTokenNotFound", err)
	}

	// Owner still has access and List is scoped per-user.
	if _, err := svc.Get(1, id); err != nil {
		t.Fatalf("owner Get: %v", err)
	}
	mine, _ := svc.List(1)
	if len(mine) != 1 {
		t.Fatalf("owner List len = %d, want 1", len(mine))
	}
	others, _ := svc.List(2)
	if len(others) != 0 {
		t.Fatalf("other-user List len = %d, want 0", len(others))
	}
}

func TestTokenDefaultsAndUpdate(t *testing.T) {
	gdb := newTokenTestDB(t)
	svc := NewTokenService(gdb)

	name := "defaults"
	res, err := svc.Create(5, TokenInput{Name: &name})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Token.Quota != model.QuotaUnlimited {
		t.Fatalf("default quota = %d, want -1 (unlimited)", res.Token.Quota)
	}
	if res.Token.Status != model.TokenEnabled {
		t.Fatalf("default status = %q, want enabled", res.Token.Status)
	}
	if res.Token.Group != "default" {
		t.Fatalf("default group = %q, want default", res.Token.Group)
	}

	q := int64(1000)
	disabled := model.TokenDisabled
	updated, err := svc.Update(5, res.Token.ID, TokenInput{Quota: &q, Status: &disabled})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Token.Quota != 1000 || updated.Token.Status != model.TokenDisabled {
		t.Fatalf("update not applied: quota=%d status=%q", updated.Token.Quota, updated.Token.Status)
	}
}

// TestTokenOutputFormatRoundTrip covers persisting/clearing the output_format
// override: default nil (follow endpoint), set to bedrock, then cleared via "".
func TestTokenOutputFormatRoundTrip(t *testing.T) {
	gdb := newTokenTestDB(t)
	svc := NewTokenService(gdb)

	name := "fmt"
	res, err := svc.Create(3, TokenInput{Name: &name})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Token.OutputFormat != nil {
		t.Fatalf("default output_format = %v, want nil (follow endpoint)", *res.Token.OutputFormat)
	}

	// Set to bedrock.
	bedrock := "bedrock"
	up, err := svc.Update(3, res.Token.ID, TokenInput{OutputFormat: &bedrock})
	if err != nil {
		t.Fatalf("Update set: %v", err)
	}
	if up.Token.OutputFormat == nil || *up.Token.OutputFormat != "bedrock" {
		t.Fatalf("output_format = %v, want bedrock", up.Token.OutputFormat)
	}
	// Persisted (reload via Get).
	got, _ := svc.Get(3, res.Token.ID)
	if got.Token.OutputFormat == nil || *got.Token.OutputFormat != "bedrock" {
		t.Fatalf("reloaded output_format = %v, want bedrock", got.Token.OutputFormat)
	}

	// Clear via empty string → nil.
	empty := ""
	cleared, err := svc.Update(3, res.Token.ID, TokenInput{OutputFormat: &empty})
	if err != nil {
		t.Fatalf("Update clear: %v", err)
	}
	if cleared.Token.OutputFormat != nil {
		t.Fatalf("after clear output_format = %v, want nil", *cleared.Token.OutputFormat)
	}
}

func TestCreateTokenRequiresName(t *testing.T) {
	gdb := newTokenTestDB(t)
	svc := NewTokenService(gdb)
	if _, err := svc.Create(1, TokenInput{}); err == nil {
		t.Fatal("expected error creating token without name")
	}
	blank := "   "
	if _, err := svc.Create(1, TokenInput{Name: &blank}); err == nil {
		t.Fatal("expected error creating token with blank name")
	}
}

func TestGetByKeyHash(t *testing.T) {
	gdb := newTokenTestDB(t)
	svc := NewTokenService(gdb)
	name := "lookup"
	res, _ := svc.Create(9, TokenInput{Name: &name})

	tok, err := svc.GetByKeyHash(HashKey(res.PlainKey))
	if err != nil {
		t.Fatalf("GetByKeyHash: %v", err)
	}
	if tok.ID != res.Token.ID || tok.UserID != 9 {
		t.Fatalf("GetByKeyHash returned wrong token")
	}
	if _, err := svc.GetByKeyHash("nonexistent"); err != ErrTokenNotFound {
		t.Fatalf("GetByKeyHash(missing) err = %v, want ErrTokenNotFound", err)
	}
}
