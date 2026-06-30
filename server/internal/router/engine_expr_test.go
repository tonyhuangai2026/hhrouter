package router

import (
	"context"
	"testing"

	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/router/probe"
)

// TestExprRouting_ProbeSignal verifies a custom expression routes on the probe's
// w signal: w==1 picks the "write" channel, w==0 falls through to the default.
func TestExprRouting_ProbeSignal(t *testing.T) {
	db := newEngineTestDB(t)
	chWrite := mustCreateChannel(t, db, model.Channel{Name: "write", Models: jsonB(t, []string{"gpt-4o"})})
	chDefault := mustCreateChannel(t, db, model.Channel{Name: "default", Models: jsonB(t, []string{"gpt-4o"})})

	// Priority 1: expr rule for write turns. Priority 2: catch-all default.
	mustCreateRule(t, db, model.RoutingRule{
		Name: "writes-to-write", Enabled: true, Priority: 1,
		Expr:             "w == 1",
		TargetChannelIDs: jsonB(t, []uint{chWrite.ID}),
	})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "catch-all", Enabled: true, Priority: 2,
		TargetChannelIDs: jsonB(t, []uint{chDefault.ID}),
	})

	// Probe says w=1 → write channel.
	engW := NewEngine(db).WithProbe(probe.NewFixedProbe(1, 100))
	selW, err := engW.SelectChannelCtx(context.Background(), RouteInput{Group: "default", Model: "gpt-4o", Prompt: "p"})
	if err != nil {
		t.Fatalf("select (w=1): %v", err)
	}
	if selW.Rule == nil || selW.Rule.Name != "writes-to-write" {
		t.Fatalf("w=1 matched rule = %v, want writes-to-write", ruleName(selW.Rule))
	}

	// Probe says w=0 → the expr rule is skipped, catch-all wins.
	engN := NewEngine(db).WithProbe(probe.NewFixedProbe(0, 100))
	selN, err := engN.SelectChannelCtx(context.Background(), RouteInput{Group: "default", Model: "gpt-4o", Prompt: "p"})
	if err != nil {
		t.Fatalf("select (w=0): %v", err)
	}
	if selN.Rule == nil || selN.Rule.Name != "catch-all" {
		t.Fatalf("w=0 matched rule = %v, want catch-all", ruleName(selN.Rule))
	}
}

// TestExprRouting_NoProbeWhenUnreferenced verifies the probe is NOT called when
// no enabled rule's expression references w/t (zero added latency path).
func TestExprRouting_NoProbeWhenUnreferenced(t *testing.T) {
	db := newEngineTestDB(t)
	ch := mustCreateChannel(t, db, model.Channel{Name: "c", Models: jsonB(t, []string{"gpt-4o"})})
	// A rule with an expression that uses only `group` (not w/t) plus a token rule.
	mustCreateRule(t, db, model.RoutingRule{
		Name: "vip-only", Enabled: true, Priority: 1,
		Expr:             `group == "vip"`,
		TargetChannelIDs: jsonB(t, []uint{ch.ID}),
	})

	spy := &countingProbe{}
	eng := NewEngine(db).WithProbe(spy)
	// group=vip matches the expr; probe must NOT be consulted (no w/t reference).
	if _, err := eng.SelectChannelCtx(context.Background(), RouteInput{Group: "vip", Model: "gpt-4o", Prompt: "p"}); err != nil {
		t.Fatalf("select: %v", err)
	}
	if spy.calls != 0 {
		t.Errorf("probe called %d times, want 0 (no rule references w/t)", spy.calls)
	}
}

// TestExprRouting_NumericThreshold checks a t-threshold expression.
func TestExprRouting_NumericThreshold(t *testing.T) {
	db := newEngineTestDB(t)
	chBig := mustCreateChannel(t, db, model.Channel{Name: "big", Models: jsonB(t, []string{"gpt-4o"})})
	chSmall := mustCreateChannel(t, db, model.Channel{Name: "small", Models: jsonB(t, []string{"gpt-4o"})})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "long-output", Enabled: true, Priority: 1,
		Expr:             "t > 500",
		TargetChannelIDs: jsonB(t, []uint{chBig.ID}),
	})
	mustCreateRule(t, db, model.RoutingRule{
		Name: "default", Enabled: true, Priority: 2,
		TargetChannelIDs: jsonB(t, []uint{chSmall.ID}),
	})

	eng := NewEngine(db).WithProbe(probe.NewFixedProbe(0, 800))
	sel, _ := eng.SelectChannelCtx(context.Background(), RouteInput{Group: "g", Model: "gpt-4o", Prompt: "p"})
	if sel.Rule == nil || sel.Rule.Name != "long-output" {
		t.Fatalf("t=800 matched %v, want long-output", ruleName(sel.Rule))
	}
}

type countingProbe struct{ calls int }

func (c *countingProbe) Name() string { return "counting" }
func (c *countingProbe) Predict(_ context.Context, _ string) (probe.Prediction, error) {
	c.calls++
	return probe.Prediction{}, nil
}

func ruleName(r *model.RoutingRule) string {
	if r == nil {
		return "<nil>"
	}
	return r.Name
}
