package service

import (
	"encoding/json"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/agent-router/server/internal/model"
)

func newRuleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file:ruletest_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.RoutingRule{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }
func intptr(i int) *int       { return &i }

func TestRuleCRUD(t *testing.T) {
	svc := NewRuleService(newRuleTestDB(t))

	// Create with a populated match and explicit disabled flag.
	created, err := svc.Create(RuleInput{
		Name:     strptr("vip"),
		Enabled:  boolptr(false),
		Priority: intptr(5),
		Match: &model.MatchSpec{
			Groups:    []string{"vip"},
			Models:    []string{"gpt-4o", "claude-*"},
			MinTokens: 10,
			MaxTokens: 1000,
		},
		TargetChannelIDs: &[]uint{1, 2, 3},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Enabled {
		t.Fatal("explicit enabled:false should persist as disabled")
	}
	if created.Priority != 5 || created.Name != "vip" {
		t.Fatalf("unexpected rule fields: %+v", created)
	}

	// Match JSONB round-trips.
	var spec model.MatchSpec
	if err := json.Unmarshal(created.Match, &spec); err != nil {
		t.Fatalf("decode match: %v", err)
	}
	if len(spec.Models) != 2 || spec.MaxTokens != 1000 || len(spec.Groups) != 1 {
		t.Fatalf("match did not round-trip: %+v", spec)
	}

	// Get.
	got, err := svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "vip" {
		t.Fatalf("Get returned %+v", got)
	}

	// Update: re-enable and rename.
	upd, err := svc.Update(created.ID, RuleInput{
		Name:    strptr("vip-renamed"),
		Enabled: boolptr(true),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !upd.Enabled || upd.Name != "vip-renamed" {
		t.Fatalf("update did not apply: %+v", upd)
	}
	// Priority and match must be unchanged by the partial update.
	if upd.Priority != 5 {
		t.Fatalf("partial update clobbered priority: %d", upd.Priority)
	}

	// List.
	all, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(all))
	}

	// Delete.
	if err := svc.Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(created.ID); err != ErrRuleNotFound {
		t.Fatalf("expected ErrRuleNotFound after delete, got %v", err)
	}
}

// TestRuleTargetModel_CreateAndUpdate: target_model round-trips on create, an
// update can change it, and omitting it (nil) leaves the stored value alone.
func TestRuleTargetModel_CreateAndUpdate(t *testing.T) {
	svc := NewRuleService(newRuleTestDB(t))

	created, err := svc.Create(RuleInput{
		Name:        strptr("hard-tier"),
		TargetModel: strptr("claude-opus-4-8"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.TargetModel != "claude-opus-4-8" {
		t.Fatalf("TargetModel = %q, want %q", created.TargetModel, "claude-opus-4-8")
	}
	// Persisted, not just set on the in-memory struct.
	got, err := svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TargetModel != "claude-opus-4-8" {
		t.Fatalf("persisted TargetModel = %q, want %q", got.TargetModel, "claude-opus-4-8")
	}

	// Explicit change.
	upd, err := svc.Update(created.ID, RuleInput{TargetModel: strptr("claude-haiku-4-5")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.TargetModel != "claude-haiku-4-5" {
		t.Fatalf("after Update: TargetModel = %q, want %q", upd.TargetModel, "claude-haiku-4-5")
	}

	// Omitted (nil) must not clear the existing override.
	upd2, err := svc.Update(created.ID, RuleInput{Name: strptr("hard-tier-renamed")})
	if err != nil {
		t.Fatalf("Update without TargetModel: %v", err)
	}
	if upd2.TargetModel != "claude-haiku-4-5" {
		t.Fatalf("partial update clobbered TargetModel: %q", upd2.TargetModel)
	}

	// Explicit "" clears the override.
	cleared, err := svc.Update(created.ID, RuleInput{TargetModel: strptr("")})
	if err != nil {
		t.Fatalf("Update to empty: %v", err)
	}
	if cleared.TargetModel != "" {
		t.Fatalf("explicit empty did not clear TargetModel: %q", cleared.TargetModel)
	}
}

// TestRuleTargetModel_Trimmed: surrounding whitespace is stripped, and a
// whitespace-only value persists as "" (no override) rather than as spaces.
func TestRuleTargetModel_Trimmed(t *testing.T) {
	svc := NewRuleService(newRuleTestDB(t))

	created, err := svc.Create(RuleInput{
		Name:        strptr("padded"),
		TargetModel: strptr("  claude-sonnet-4-8\t"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.TargetModel != "claude-sonnet-4-8" {
		t.Fatalf("TargetModel = %q, want it trimmed", created.TargetModel)
	}

	blank, err := svc.Update(created.ID, RuleInput{TargetModel: strptr("   ")})
	if err != nil {
		t.Fatalf("Update with blank TargetModel: %v", err)
	}
	if blank.TargetModel != "" {
		t.Fatalf("whitespace-only TargetModel persisted as %q, want empty string", blank.TargetModel)
	}
}

// TestRuleTargetModel_NoChannelServeabilityCheck: saving a model name no channel
// can serve must succeed — that failure is reported at request time as
// router.ErrNoCandidate, not at configuration time (Tech Design §2.4).
func TestRuleTargetModel_NoChannelServeabilityCheck(t *testing.T) {
	svc := NewRuleService(newRuleTestDB(t))

	created, err := svc.Create(RuleInput{
		Name:        strptr("unknown-model"),
		TargetModel: strptr("model-no-channel-serves"),
	})
	if err != nil {
		t.Fatalf("Create with an unserved target model should succeed, got: %v", err)
	}
	if created.TargetModel != "model-no-channel-serves" {
		t.Fatalf("TargetModel = %q, want it stored verbatim", created.TargetModel)
	}
}

func TestRuleCreateValidation(t *testing.T) {
	svc := NewRuleService(newRuleTestDB(t))
	if _, err := svc.Create(RuleInput{Name: strptr("  ")}); err == nil {
		t.Fatal("blank name should be rejected")
	}
	if _, err := svc.Create(RuleInput{}); err == nil {
		t.Fatal("missing name should be rejected")
	}
}

func TestRuleNotFound(t *testing.T) {
	svc := NewRuleService(newRuleTestDB(t))
	if err := svc.Delete(999); err != ErrRuleNotFound {
		t.Fatalf("expected ErrRuleNotFound, got %v", err)
	}
	if _, err := svc.Update(999, RuleInput{Name: strptr("x")}); err != ErrRuleNotFound {
		t.Fatalf("expected ErrRuleNotFound on update, got %v", err)
	}
}
