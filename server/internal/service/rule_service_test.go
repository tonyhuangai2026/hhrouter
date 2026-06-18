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
