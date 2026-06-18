package router

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/agent-router/server/internal/model"
)

var engineTestSeq int64

func uniqDSN() string {
	n := atomic.AddInt64(&engineTestSeq, 1)
	return fmt.Sprintf("file:enginetest_%d?mode=memory&cache=shared", n)
}

func newEngineTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(uniqDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}, &model.RoutingRule{}, &model.Option{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

func jsonB(t *testing.T, v any) datatypes.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return datatypes.JSON(b)
}

func mustCreateChannel(t *testing.T, db *gorm.DB, ch model.Channel) model.Channel {
	t.Helper()
	if ch.Status == "" {
		ch.Status = model.ChannelEnabled
	}
	if ch.Models == nil {
		ch.Models = datatypes.JSON([]byte("[]"))
	}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch
}

func mustCreateRule(t *testing.T, db *gorm.DB, r model.RoutingRule) model.RoutingRule {
	t.Helper()
	if r.Match == nil {
		r.Match = datatypes.JSON([]byte("{}"))
	}
	if r.TargetChannelIDs == nil {
		r.TargetChannelIDs = datatypes.JSON([]byte("[]"))
	}
	wantEnabled := r.Enabled
	if err := db.Create(&r).Error; err != nil {
		t.Fatalf("create rule: %v", err)
	}
	// The model's `default:true` tag makes GORM coerce a zero-value Enabled=false
	// to true on insert; force the intended value back for disabled rules.
	if !wantEnabled && r.Enabled {
		if err := db.Model(&r).Update("enabled", false).Error; err != nil {
			t.Fatalf("force-disable rule: %v", err)
		}
		r.Enabled = false
	}
	return r
}

// TestPriorityAscMatch verifies that among multiple matching enabled rules the
// lowest-priority (ascending) one wins, and that disabled rules are skipped.
func TestPriorityAscMatch(t *testing.T) {
	db := newEngineTestDB(t)
	chA := mustCreateChannel(t, db, model.Channel{Name: "A", Models: jsonB(t, []string{"gpt-4o"})})
	chB := mustCreateChannel(t, db, model.Channel{Name: "B", Models: jsonB(t, []string{"gpt-4o"})})

	// Rule with higher priority value (10) targets chB; lower priority (1)
	// targets chA. Both match. The priority=1 rule must win.
	mustCreateRule(t, db, model.RoutingRule{
		Name: "low-prio-B", Enabled: true, Priority: 10,
		TargetChannelIDs: jsonB(t, []uint{chB.ID}),
	})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "high-prio-A", Enabled: true, Priority: 1,
		TargetChannelIDs: jsonB(t, []uint{chA.ID}),
	})
	// A disabled rule at the very front must be ignored.
	mustCreateRule(t, db, model.RoutingRule{
		Name: "disabled", Enabled: false, Priority: 0,
		TargetChannelIDs: jsonB(t, []uint{chB.ID}),
	})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "gpt-4o", 100)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	if sel.Rule == nil || sel.Rule.Name != "high-prio-A" {
		t.Fatalf("expected high-prio-A rule, got %+v", sel.Rule)
	}
	cands := sel.Candidates()
	if len(cands) != 1 || cands[0].ID != chA.ID {
		t.Fatalf("expected candidate chA only, got %+v", cands)
	}
}

// TestModelWildcard verifies '*' wildcard matching in a rule's models dimension.
func TestModelWildcard(t *testing.T) {
	db := newEngineTestDB(t)
	ch := mustCreateChannel(t, db, model.Channel{Name: "anthropic", Models: jsonB(t, []string{"claude-3-5-sonnet"})})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "claude-wild", Enabled: true, Priority: 1,
		Match:            jsonB(t, model.MatchSpec{Models: []string{"claude-*"}}),
		TargetChannelIDs: jsonB(t, []uint{ch.ID}),
	})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "claude-3-5-sonnet", 100)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	if sel.Rule == nil || sel.Rule.Name != "claude-wild" {
		t.Fatalf("wildcard rule should match, got %+v", sel.Rule)
	}

	// A model that does not match the wildcard must not select that rule. Add a
	// channel serving it so we still get a fallback (no-rule) selection.
	mustCreateChannel(t, db, model.Channel{Name: "openai", Models: jsonB(t, []string{"gpt-4o"})})
	sel2, err := eng.SelectChannel("default", "gpt-4o", 100)
	if err != nil {
		t.Fatalf("SelectChannel gpt-4o: %v", err)
	}
	if sel2.Rule != nil {
		t.Fatalf("gpt-4o should not match claude-* rule, got rule %+v", sel2.Rule)
	}
}

// TestModelMatchHelper directly exercises the wildcard matcher edge cases.
func TestModelMatchHelper(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"*", "anything", true},
		{"gpt-4o", "gpt-4o", true},
		{"gpt-4o", "gpt-4o-mini", false},
		{"claude-*", "claude-3-5-sonnet", true},
		{"claude-*", "gpt-4o", false},
		{"*-mini", "gpt-4o-mini", true},
		{"*-mini", "gpt-4o", false},
		{"gpt-*-turbo", "gpt-4-turbo", true},
		{"gpt-*-turbo", "gpt-4-base", false},
	}
	for _, c := range cases {
		if got := modelMatches(c.pattern, c.model); got != c.want {
			t.Errorf("modelMatches(%q,%q)=%v want %v", c.pattern, c.model, got, c.want)
		}
	}
}

// TestTokenRange verifies the min/max token dimension.
func TestTokenRange(t *testing.T) {
	db := newEngineTestDB(t)
	chSmall := mustCreateChannel(t, db, model.Channel{Name: "small", Models: jsonB(t, []string{"m"})})
	chBig := mustCreateChannel(t, db, model.Channel{Name: "big", Models: jsonB(t, []string{"m"})})

	// Short-context rule [0,1000] -> chSmall; long-context rule [1001, 0(=inf)]
	// -> chBig. Both at distinct priorities; the matching one depends on tokens.
	mustCreateRule(t, db, model.RoutingRule{
		Name: "short", Enabled: true, Priority: 1,
		Match:            jsonB(t, model.MatchSpec{MaxTokens: 1000}),
		TargetChannelIDs: jsonB(t, []uint{chSmall.ID}),
	})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "long", Enabled: true, Priority: 2,
		Match:            jsonB(t, model.MatchSpec{MinTokens: 1001}),
		TargetChannelIDs: jsonB(t, []uint{chBig.ID}),
	})

	eng := NewEngine(db)

	sel, err := eng.SelectChannel("default", "m", 500)
	if err != nil {
		t.Fatalf("select 500: %v", err)
	}
	if sel.Rule.Name != "short" {
		t.Fatalf("500 tokens should hit short rule, got %s", sel.Rule.Name)
	}

	sel, err = eng.SelectChannel("default", "m", 5000)
	if err != nil {
		t.Fatalf("select 5000: %v", err)
	}
	if sel.Rule.Name != "long" {
		t.Fatalf("5000 tokens should hit long rule, got %s", sel.Rule.Name)
	}
}

// TestEmptyDimensionsUnconstrained verifies that empty match dimensions match
// any input (groups/models/min/max all unconstrained).
func TestEmptyDimensionsUnconstrained(t *testing.T) {
	db := newEngineTestDB(t)
	ch := mustCreateChannel(t, db, model.Channel{Name: "any", Models: jsonB(t, []string{"whatever"})})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "catch-all", Enabled: true, Priority: 1,
		Match:            jsonB(t, model.MatchSpec{}), // all empty
		TargetChannelIDs: jsonB(t, []uint{ch.ID}),
	})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("some-group", "whatever", 99999)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	if sel.Rule == nil || sel.Rule.Name != "catch-all" {
		t.Fatalf("empty match should be unconstrained, got %+v", sel.Rule)
	}
}

// TestGroupMatch verifies the groups dimension restricts by token group.
func TestGroupMatch(t *testing.T) {
	db := newEngineTestDB(t)
	chVip := mustCreateChannel(t, db, model.Channel{Name: "vip", Models: jsonB(t, []string{"m"})})
	chDefault := mustCreateChannel(t, db, model.Channel{Name: "default", Models: jsonB(t, []string{"m"})})

	mustCreateRule(t, db, model.RoutingRule{
		Name: "vip-only", Enabled: true, Priority: 1,
		Match:            jsonB(t, model.MatchSpec{Groups: []string{"vip"}}),
		TargetChannelIDs: jsonB(t, []uint{chVip.ID}),
	})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "rest", Enabled: true, Priority: 2,
		TargetChannelIDs: jsonB(t, []uint{chDefault.ID}),
	})

	eng := NewEngine(db)
	sel, _ := eng.SelectChannel("vip", "m", 10)
	if sel.Rule.Name != "vip-only" {
		t.Fatalf("vip group should hit vip-only, got %s", sel.Rule.Name)
	}
	sel, _ = eng.SelectChannel("free", "m", 10)
	if sel.Rule.Name != "rest" {
		t.Fatalf("non-vip group should skip vip-only, got %s", sel.Rule.Name)
	}
}

// TestNoRuleFallback verifies that with no matching rule the engine falls back
// to all enabled channels that can serve the model.
func TestNoRuleFallback(t *testing.T) {
	db := newEngineTestDB(t)
	serves := mustCreateChannel(t, db, model.Channel{Name: "serves", Models: jsonB(t, []string{"gpt-4o"})})
	mustCreateChannel(t, db, model.Channel{Name: "other", Models: jsonB(t, []string{"gpt-3.5"})})
	// A disabled channel that serves the model must NOT be a fallback candidate.
	mustCreateChannel(t, db, model.Channel{Name: "disabled", Models: jsonB(t, []string{"gpt-4o"}), Status: model.ChannelDisabled})

	// No rules at all.
	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "gpt-4o", 10)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	if sel.Rule != nil {
		t.Fatalf("expected no-rule fallback, got rule %+v", sel.Rule)
	}
	cands := sel.Candidates()
	if len(cands) != 1 || cands[0].ID != serves.ID {
		t.Fatalf("fallback should yield only the enabled serving channel, got %+v", cands)
	}

	// A model no enabled channel serves -> ErrNoCandidate.
	if _, err := eng.SelectChannel("default", "nonexistent-model", 10); err != ErrNoCandidate {
		t.Fatalf("expected ErrNoCandidate, got %v", err)
	}
}

// TestModelMappingServes verifies a channel whose model_mapping keys the
// external model is considered a candidate, and UpstreamModel resolves it.
func TestModelMappingServes(t *testing.T) {
	db := newEngineTestDB(t)
	ch := mustCreateChannel(t, db, model.Channel{
		Name:         "mapped",
		Models:       jsonB(t, []string{"internal-id"}),
		ModelMapping: jsonB(t, map[string]string{"gpt-4o": "internal-id"}),
	})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "gpt-4o", 10)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	cands := sel.Candidates()
	if len(cands) != 1 || cands[0].ID != ch.ID {
		t.Fatalf("mapped channel should serve gpt-4o, got %+v", cands)
	}
	if up := UpstreamModel(&cands[0], "gpt-4o"); up != "internal-id" {
		t.Fatalf("UpstreamModel = %q, want internal-id", up)
	}
	if up := UpstreamModel(&cands[0], "unmapped"); up != "unmapped" {
		t.Fatalf("UpstreamModel unmapped = %q, want unmapped", up)
	}
}

// TestPriorityBucketing verifies LoadBalance takes only the highest priority
// bucket as the primary set; lower-priority channels follow for failover.
func TestPriorityBucketing(t *testing.T) {
	db := newEngineTestDB(t)
	hi := mustCreateChannel(t, db, model.Channel{Name: "hi", Models: jsonB(t, []string{"m"}), Priority: 10, Weight: 1})
	lo := mustCreateChannel(t, db, model.Channel{Name: "lo", Models: jsonB(t, []string{"m"}), Priority: 1, Weight: 1})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "m", 10)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	cands := sel.Candidates()
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(cands))
	}
	// Highest priority bucket first.
	if cands[0].ID != hi.ID {
		t.Fatalf("expected hi-priority channel first, got %s", cands[0].Name)
	}
	if cands[1].ID != lo.ID {
		t.Fatalf("expected lo-priority channel second, got %s", cands[1].Name)
	}
}

// TestWeightedRandomDistribution checks that within a priority bucket the
// primary pick frequency tracks the configured weights (statistical).
func TestWeightedRandomDistribution(t *testing.T) {
	db := newEngineTestDB(t)
	// Two channels in the same priority bucket; weights 3:1.
	heavy := mustCreateChannel(t, db, model.Channel{Name: "heavy", Models: jsonB(t, []string{"m"}), Priority: 5, Weight: 3})
	light := mustCreateChannel(t, db, model.Channel{Name: "light", Models: jsonB(t, []string{"m"}), Priority: 5, Weight: 1})

	eng := NewEngine(db)
	const N = 20000
	counts := map[uint]int{}
	for i := 0; i < N; i++ {
		sel, err := eng.SelectChannel("default", "m", 10)
		if err != nil {
			t.Fatalf("SelectChannel: %v", err)
		}
		primary, err := sel.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		counts[primary.ID]++
	}

	// Expect ~75% heavy, ~25% light. Allow a generous tolerance band.
	heavyFrac := float64(counts[heavy.ID]) / float64(N)
	lightFrac := float64(counts[light.ID]) / float64(N)
	if heavyFrac < 0.70 || heavyFrac > 0.80 {
		t.Fatalf("heavy fraction %.3f outside [0.70,0.80] (counts %v)", heavyFrac, counts)
	}
	if lightFrac < 0.20 || lightFrac > 0.30 {
		t.Fatalf("light fraction %.3f outside [0.20,0.30] (counts %v)", lightFrac, counts)
	}
}

// TestFailoverNextCandidate verifies that Next walks the candidate list in
// order for failover and returns ErrFailoverExhausted past the retry budget.
func TestFailoverNextCandidate(t *testing.T) {
	db := newEngineTestDB(t)
	// Three channels at descending priority so the order is deterministic.
	c1 := mustCreateChannel(t, db, model.Channel{Name: "c1", Models: jsonB(t, []string{"m"}), Priority: 30})
	c2 := mustCreateChannel(t, db, model.Channel{Name: "c2", Models: jsonB(t, []string{"m"}), Priority: 20})
	c3 := mustCreateChannel(t, db, model.Channel{Name: "c3", Models: jsonB(t, []string{"m"}), Priority: 10})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "m", 10)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	if sel.MaxRetries() != DefaultMaxRetries {
		t.Fatalf("expected default max retries %d, got %d", DefaultMaxRetries, sel.MaxRetries())
	}

	want := []uint{c1.ID, c2.ID, c3.ID}
	for i, w := range want {
		ch, err := sel.Next()
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		if ch.ID != w {
			t.Fatalf("Next #%d = %s (id %d), want id %d", i, ch.Name, ch.ID, w)
		}
	}
	// Budget is 3 and exactly 3 candidates exist -> next is exhausted.
	if _, err := sel.Next(); err != ErrFailoverExhausted {
		t.Fatalf("expected ErrFailoverExhausted, got %v", err)
	}
}

// TestFailoverRetryCapFromOption verifies the retry cap is read from the
// RouterMaxRetries option and bounds how many candidates Next yields.
func TestFailoverRetryCapFromOption(t *testing.T) {
	db := newEngineTestDB(t)
	for i := 0; i < 5; i++ {
		mustCreateChannel(t, db, model.Channel{
			Name:     fmt.Sprintf("c%d", i),
			Models:   jsonB(t, []string{"m"}),
			Priority: 100 - i, // distinct descending priorities
		})
	}
	// Cap retries at 2.
	if err := db.Create(&model.Option{Key: OptMaxRetries, Value: "2"}).Error; err != nil {
		t.Fatalf("seed option: %v", err)
	}

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "m", 10)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	if sel.MaxRetries() != 2 {
		t.Fatalf("expected max retries 2, got %d", sel.MaxRetries())
	}
	n := 0
	for {
		if _, err := sel.Next(); err != nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Fatalf("expected Next to yield 2 candidates (capped), got %d", n)
	}
}
