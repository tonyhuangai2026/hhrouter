package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agent-router/server/internal/model"
)

// TestTargetModelEmpty_LegacyBehaviour is the compatibility regression test for
// the target_model feature: with no override (empty, and also whitespace-only,
// which must be treated as empty) candidate filtering and upstream model
// resolution must behave exactly as before the feature existed — i.e. filter on
// the CLIENT's requested model, and resolve the upstream id from that same name.
func TestTargetModelEmpty_LegacyBehaviour(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"spaces":     "   ",
		"tabnewline": "\t\n",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			db := newEngineTestDB(t)
			// Only chServes declares the requested model; chOther serves something
			// else and must stay out of the candidate set.
			chServes := mustCreateChannel(t, db, model.Channel{
				Name:         "serves",
				Models:       jsonB(t, []string{"gpt-4o"}),
				ModelMapping: jsonB(t, map[string]string{"gpt-4o": "gpt-4o-upstream"}),
			})
			mustCreateChannel(t, db, model.Channel{Name: "other", Models: jsonB(t, []string{"haiku"})})

			mustCreateRule(t, db, model.RoutingRule{
				Name: "no-override", Enabled: true, Priority: 1,
				TargetModel: target,
			})

			eng := NewEngine(db)
			sel, err := eng.SelectChannel("default", "gpt-4o", 100)
			if err != nil {
				t.Fatalf("SelectChannel: %v", err)
			}
			if sel.Rule == nil || sel.Rule.Name != "no-override" {
				t.Fatalf("matched rule = %v, want no-override", ruleName(sel.Rule))
			}
			// Candidate filtering unchanged: only the channel serving the requested
			// model, never the one serving a different model.
			cands := sel.Candidates()
			if len(cands) != 1 || cands[0].ID != chServes.ID {
				t.Fatalf("candidates = %+v, want only chServes (id %d)", cands, chServes.ID)
			}
			// Effective model falls back to the requested model, so upstream
			// resolution goes through the same mapping lookup as before.
			if got := sel.EffectiveModel(); got != "gpt-4o" {
				t.Fatalf("EffectiveModel() = %q, want the requested model gpt-4o", got)
			}
			if up := UpstreamModel(&cands[0], sel.EffectiveModel()); up != "gpt-4o-upstream" {
				t.Fatalf("UpstreamModel = %q, want gpt-4o-upstream", up)
			}

			// A model nothing serves is still ErrNoCandidate. The text now names the
			// matched rule (an operator seeing "nothing serves this" needs to know
			// which rule pointed nowhere), so assert the wrapped sentinel rather
			// than exact identity.
			_, err = eng.SelectChannel("default", "nonexistent-model", 10)
			if !errors.Is(err, ErrNoCandidate) {
				t.Fatalf("expected ErrNoCandidate, got %v", err)
			}
		})
	}
}

// TestTargetModelFiltersCandidates verifies the core of the feature: a non-empty
// target_model — not the requested model name — selects the candidate channels.
// The channel only declares haiku, the client asks for opus, and the rule
// overrides to haiku, so the channel must be picked.
func TestTargetModelFiltersCandidates(t *testing.T) {
	db := newEngineTestDB(t)
	chHaiku := mustCreateChannel(t, db, model.Channel{
		Name:   "haiku-only",
		Models: jsonB(t, []string{"claude-haiku-4-5"}),
	})

	mustCreateRule(t, db, model.RoutingRule{
		Name: "cheap-tier", Enabled: true, Priority: 1,
		TargetModel: "claude-haiku-4-5",
	})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "claude-opus-4-8", 100)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	cands := sel.Candidates()
	if len(cands) != 1 || cands[0].ID != chHaiku.ID {
		t.Fatalf("candidates = %+v, want the haiku-only channel (id %d)", cands, chHaiku.ID)
	}
	if got := sel.EffectiveModel(); got != "claude-haiku-4-5" {
		t.Fatalf("EffectiveModel() = %q, want claude-haiku-4-5", got)
	}
}

// TestTargetModelWithChannelMapping verifies the layering order: the rule's
// target_model is an EXTERNAL name, so the channel's model_mapping is applied on
// top of it (override first, mapping second).
func TestTargetModelWithChannelMapping(t *testing.T) {
	db := newEngineTestDB(t)
	mustCreateChannel(t, db, model.Channel{
		Name:         "bedrock",
		Models:       jsonB(t, []string{"claude-opus-4-8"}),
		ModelMapping: jsonB(t, map[string]string{"claude-haiku-4-5": "anthropic.claude-haiku-4-5"}),
	})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "cheap-tier", Enabled: true, Priority: 1,
		TargetModel: "claude-haiku-4-5",
	})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "claude-opus-4-8", 10)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	cands := sel.Candidates()
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v, want 1 (matched via model_mapping key)", cands)
	}
	if up := UpstreamModel(&cands[0], sel.EffectiveModel()); up != "anthropic.claude-haiku-4-5" {
		t.Fatalf("UpstreamModel = %q, want anthropic.claude-haiku-4-5", up)
	}
}

// TestSelectionModelVsEffectiveModel verifies the two accessors keep distinct
// meanings: Model stays the client's requested name (observability relies on it)
// while EffectiveModel reports what actually serves the request.
func TestSelectionModelVsEffectiveModel(t *testing.T) {
	db := newEngineTestDB(t)
	mustCreateChannel(t, db, model.Channel{
		Name:   "multi",
		Models: jsonB(t, []string{"claude-opus-4-8", "claude-haiku-4-5"}),
	})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "override", Enabled: true, Priority: 1,
		TargetModel: "claude-haiku-4-5",
	})

	eng := NewEngine(db)
	sel, err := eng.SelectChannelCtx(context.Background(), RouteInput{
		Group: "default", Model: "claude-opus-4-8", EstTokens: 10,
	})
	if err != nil {
		t.Fatalf("SelectChannelCtx: %v", err)
	}
	if sel.Model != "claude-opus-4-8" {
		t.Fatalf("Selection.Model = %q, want the requested claude-opus-4-8", sel.Model)
	}
	if got := sel.EffectiveModel(); got != "claude-haiku-4-5" {
		t.Fatalf("EffectiveModel() = %q, want claude-haiku-4-5", got)
	}

	// A Selection with no override reports the requested model for both.
	plain := &Selection{Model: "gpt-4o"}
	if plain.EffectiveModel() != "gpt-4o" {
		t.Fatalf("EffectiveModel() on override-less selection = %q, want gpt-4o", plain.EffectiveModel())
	}
}

// TestTargetModelNoCandidateError verifies that when the matched rule's
// target_model cannot be served, the engine fails with an ErrNoCandidate-wrapped
// error naming the rule and the target model — and does NOT backtrack to a
// lower-priority rule that would have matched (silent downgrade is worse than an
// error for quality-tiered routing).
func TestTargetModelNoCandidateError(t *testing.T) {
	db := newEngineTestDB(t)
	// The only channel serves the requested model, but nothing serves the override.
	mustCreateChannel(t, db, model.Channel{Name: "opus-only", Models: jsonB(t, []string{"claude-opus-4-8"})})

	mustCreateRule(t, db, model.RoutingRule{
		Name: "hard-tier", Enabled: true, Priority: 1,
		TargetModel: "claude-haiku-4-5", // unserved
	})
	// A lower-priority catch-all that WOULD produce candidates. It must not be
	// reached: the higher-priority rule's decision is final.
	fallback := mustCreateRule(t, db, model.RoutingRule{
		Name: "fallback", Enabled: true, Priority: 2,
	})

	eng := NewEngine(db)
	sel, err := eng.SelectChannel("default", "claude-opus-4-8", 10)
	if err == nil {
		t.Fatalf("expected an error, got selection with rule %v", ruleName(sel.Rule))
	}
	if sel != nil {
		t.Fatalf("expected nil selection, got rule %v (rule backtracking must not happen; fallback id %d)",
			ruleName(sel.Rule), fallback.ID)
	}
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("errors.Is(err, ErrNoCandidate) = false for %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "hard-tier") {
		t.Fatalf("error text %q must name the matched rule", msg)
	}
	if !strings.Contains(msg, "claude-haiku-4-5") {
		t.Fatalf("error text %q must include the target_model value", msg)
	}
}

// TestTargetModelNoCandidateError_SameNameAsRequest covers the case where a rule
// pins a target_model EQUAL to the model the client asked for. That is legitimate
// tier configuration (an explicit "this tier stays on opus" pin), so when nothing
// can serve it the error must still name the rule and the target model — keying the
// diagnostic off "the names differ" would leave this case undiagnosable.
func TestTargetModelNoCandidateError_SameNameAsRequest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		targetModel string
	}{
		{"exact", "claude-opus-4-8"},
		{"padded", "  claude-opus-4-8  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newEngineTestDB(t)
			mustCreateChannel(t, db, model.Channel{Name: "haiku-only", Models: jsonB(t, []string{"claude-haiku-4-5"})})
			mustCreateRule(t, db, model.RoutingRule{
				Name: "opus-tier", Enabled: true, Priority: 1, TargetModel: tc.targetModel,
			})

			_, err := NewEngine(db).SelectChannel("default", "claude-opus-4-8", 10)
			if !errors.Is(err, ErrNoCandidate) {
				t.Fatalf("errors.Is(err, ErrNoCandidate) = false for %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "opus-tier") {
				t.Fatalf("error text %q must name the matched rule even when target_model equals the requested model", msg)
			}
			if !strings.Contains(msg, "claude-opus-4-8") {
				t.Fatalf("error text %q must include the target_model value", msg)
			}
		})
	}
}

// TestPinnedTargetsSkipModelFilter is the core of "channel routing takes
// precedence over model routing": once a rule names its target channels, the
// requested model name must NOT narrow that set further. Before this, a rule
// pointing at a channel was silently dropped whenever the channel had not also
// declared whatever model name the client happened to send — which made
// tiering inside one channel impossible to configure.
func TestPinnedTargetsSkipModelFilter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		targetModel string
		wantEff     string
	}{
		// The rule settles the model itself...
		{"with target_model", "tier-haiku", "tier-haiku"},
		// ...and even with no override the pinned channel is still honoured; the
		// model then stays the requested one and is resolved per channel.
		{"without target_model", "", "claude-opus-4-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newEngineTestDB(t)
			// Deliberately does NOT list claude-opus-4-8: the client asks for opus,
			// the channel only offers cheaper tiers.
			pinned := mustCreateChannel(t, db, model.Channel{
				Name: "tiered", Models: jsonB(t, []string{"tier-haiku", "tier-sonnet"}),
			})
			// A channel that DOES serve the requested model, to prove the pin wins
			// rather than the model-name match.
			mustCreateChannel(t, db, model.Channel{
				Name: "opus-only", Models: jsonB(t, []string{"claude-opus-4-8"}),
			})

			mustCreateRule(t, db, model.RoutingRule{
				Name: "pin-tiered", Enabled: true, Priority: 1,
				TargetChannelIDs: jsonB(t, []uint{pinned.ID}),
				TargetModel:      tc.targetModel,
			})

			sel, err := NewEngine(db).SelectChannel("default", "claude-opus-4-8", 10)
			if err != nil {
				t.Fatalf("SelectChannel: %v", err)
			}
			cands := sel.Candidates()
			if len(cands) != 1 || cands[0].ID != pinned.ID {
				t.Fatalf("candidates = %+v, want only the pinned channel (id %d)", cands, pinned.ID)
			}
			if got := sel.EffectiveModel(); got != tc.wantEff {
				t.Fatalf("EffectiveModel() = %q, want %q", got, tc.wantEff)
			}
			// The requested name is preserved for logging/observability either way.
			if sel.Model != "claude-opus-4-8" {
				t.Fatalf("Selection.Model = %q, want the requested claude-opus-4-8", sel.Model)
			}
		})
	}
}

// TestTargetGroupSkipsModelFilter covers the same precedence for a rule that
// pins a channel GROUP rather than explicit ids.
func TestTargetGroupSkipsModelFilter(t *testing.T) {
	db := newEngineTestDB(t)
	inGroup := mustCreateChannel(t, db, model.Channel{
		Name: "grp-a", Group: "cheap", Models: jsonB(t, []string{"tier-haiku"}),
	})
	// Same model, different group: must stay out.
	mustCreateChannel(t, db, model.Channel{
		Name: "grp-b", Group: "other", Models: jsonB(t, []string{"tier-haiku"}),
	})

	mustCreateRule(t, db, model.RoutingRule{
		Name: "pin-group", Enabled: true, Priority: 1,
		TargetGroup: "cheap", TargetModel: "tier-haiku",
	})

	sel, err := NewEngine(db).SelectChannel("default", "claude-opus-4-8", 10)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	cands := sel.Candidates()
	if len(cands) != 1 || cands[0].ID != inGroup.ID {
		t.Fatalf("candidates = %+v, want only the in-group channel (id %d)", cands, inGroup.ID)
	}
}

// TestUnpinnedRuleStillFiltersByModel guards the other half of the contract: a
// rule that names NO targets keeps filtering by model name, and so does the
// no-rule fallback. Without that, a request could be handed to a channel with no
// idea how to serve it.
func TestUnpinnedRuleStillFiltersByModel(t *testing.T) {
	db := newEngineTestDB(t)
	serves := mustCreateChannel(t, db, model.Channel{
		Name: "serves", Models: jsonB(t, []string{"claude-opus-4-8"}),
	})
	mustCreateChannel(t, db, model.Channel{
		Name: "unrelated", Models: jsonB(t, []string{"tier-haiku"}),
	})

	// Rule matches the request but pins nothing.
	mustCreateRule(t, db, model.RoutingRule{Name: "unpinned", Enabled: true, Priority: 1})

	sel, err := NewEngine(db).SelectChannel("default", "claude-opus-4-8", 10)
	if err != nil {
		t.Fatalf("SelectChannel: %v", err)
	}
	if cands := sel.Candidates(); len(cands) != 1 || cands[0].ID != serves.ID {
		t.Fatalf("candidates = %+v, want only the channel serving the model (id %d)", cands, serves.ID)
	}

	// Channel-fallback path (no rule at all) behaves the same way.
	db2 := newEngineTestDB(t)
	s2 := mustCreateChannel(t, db2, model.Channel{Name: "serves", Models: jsonB(t, []string{"claude-opus-4-8"})})
	mustCreateChannel(t, db2, model.Channel{Name: "unrelated", Models: jsonB(t, []string{"tier-haiku"})})
	sel2, err := NewEngine(db2).SelectChannel("default", "claude-opus-4-8", 10)
	if err != nil {
		t.Fatalf("fallback SelectChannel: %v", err)
	}
	if sel2.Rule != nil {
		t.Fatalf("expected the no-rule fallback, matched %v", ruleName(sel2.Rule))
	}
	if cands := sel2.Candidates(); len(cands) != 1 || cands[0].ID != s2.ID {
		t.Fatalf("fallback candidates = %+v, want only id %d", cands, s2.ID)
	}
}
