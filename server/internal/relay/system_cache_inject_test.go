package relay

import (
	"strings"
	"testing"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/model"
)

// mkSystem returns a system prompt of exactly n characters so token-count
// fixtures can be sized precisely against adapter.EstimateSystemTokens
// (ceil(len/4) tokens). "A" repeated is used so TrimSpace inside
// maybeInjectSystemCache does not shrink it.
func mkSystem(n int) string {
	return strings.Repeat("A", n)
}

// The auto system-cache injection threshold is 1024 tokens for non-Haiku models
// and 4096 tokens for Haiku (per the Tech Design §7). These fixtures rely on the
// ~4-chars-per-token estimate:
//   - 5000 chars  -> 1250 tokens  (>=1024, below the 4096 Haiku bar)
//   - 100 chars   -> 25 tokens    (well below 1024)
//   - 8000 chars  -> 2000 tokens  (between 1024 and 4096)
//   - 20000 chars -> 5000 tokens  (>=4096)
func init() {
	// Guardrails: fail fast if the token heuristic ever changes underneath the
	// fixtures so the tests below cannot silently start proving the wrong thing.
	if adapter.EstimateSystemTokens(mkSystem(5000)) < 1024 {
		panic("fixture: 5000-char system must estimate >=1024 tokens")
	}
	if adapter.EstimateSystemTokens(mkSystem(100)) >= 1024 {
		panic("fixture: 100-char system must estimate <1024 tokens")
	}
	if t := adapter.EstimateSystemTokens(mkSystem(8000)); t < 1024 || t >= 4096 {
		panic("fixture: 8000-char system must estimate in [1024,4096)")
	}
	if adapter.EstimateSystemTokens(mkSystem(20000)) < 4096 {
		panic("fixture: 20000-char system must estimate >=4096 tokens")
	}
}

// TestMaybeInjectSystemCache_BedrockLongSystem: switch ON + bedrock + long
// system + nil breakpoint -> injects an ephemeral breakpoint.
func TestMaybeInjectSystemCache_BedrockLongSystem(t *testing.T) {
	uni := &adapter.UnifiedRequest{System: mkSystem(5000)}
	ch := &model.Channel{Type: model.ChannelBedrock, AutoCacheSystem: true}

	maybeInjectSystemCache(uni, ch, "anthropic.claude-sonnet-4")

	if uni.SystemCacheControl == nil {
		t.Fatal("bedrock + auto-cache + long system: SystemCacheControl must be injected, got nil")
	}
	if uni.SystemCacheControl.Type != "ephemeral" {
		t.Fatalf("SystemCacheControl.Type = %q, want ephemeral", uni.SystemCacheControl.Type)
	}
}

// TestMaybeInjectSystemCache_AnthropicLongSystem: switch ON + anthropic + long
// system -> injects.
func TestMaybeInjectSystemCache_AnthropicLongSystem(t *testing.T) {
	uni := &adapter.UnifiedRequest{System: mkSystem(5000)}
	ch := &model.Channel{Type: model.ChannelAnthropic, AutoCacheSystem: true}

	maybeInjectSystemCache(uni, ch, "claude-sonnet-4")

	if uni.SystemCacheControl == nil {
		t.Fatal("anthropic + auto-cache + long system: SystemCacheControl must be injected, got nil")
	}
	if uni.SystemCacheControl.Type != "ephemeral" {
		t.Fatalf("SystemCacheControl.Type = %q, want ephemeral", uni.SystemCacheControl.Type)
	}
}

// TestMaybeInjectSystemCache_SwitchOff: AutoCacheSystem false -> never injects,
// even for a bedrock channel with a long system.
func TestMaybeInjectSystemCache_SwitchOff(t *testing.T) {
	uni := &adapter.UnifiedRequest{System: mkSystem(5000)}
	ch := &model.Channel{Type: model.ChannelBedrock, AutoCacheSystem: false}

	maybeInjectSystemCache(uni, ch, "anthropic.claude-sonnet-4")

	if uni.SystemCacheControl != nil {
		t.Fatalf("switch OFF: SystemCacheControl must stay nil, got %+v", uni.SystemCacheControl)
	}
}

// TestMaybeInjectSystemCache_OpenAIChannelSkipped: OpenAI-type channels are
// skipped even with the switch on and a long system (OpenAI upstream auto-caches).
func TestMaybeInjectSystemCache_OpenAIChannelSkipped(t *testing.T) {
	uni := &adapter.UnifiedRequest{System: mkSystem(5000)}
	ch := &model.Channel{Type: model.ChannelOpenAI, AutoCacheSystem: true}

	maybeInjectSystemCache(uni, ch, "gpt-4o")

	if uni.SystemCacheControl != nil {
		t.Fatalf("openai channel: SystemCacheControl must stay nil, got %+v", uni.SystemCacheControl)
	}
}

// TestMaybeInjectSystemCache_ShortSystemSkipped: a system below the 1024-token
// threshold is not cached (Bedrock silently skips too-short prefixes).
func TestMaybeInjectSystemCache_ShortSystemSkipped(t *testing.T) {
	uni := &adapter.UnifiedRequest{System: mkSystem(100)}
	ch := &model.Channel{Type: model.ChannelBedrock, AutoCacheSystem: true}

	maybeInjectSystemCache(uni, ch, "anthropic.claude-sonnet-4")

	if uni.SystemCacheControl != nil {
		t.Fatalf("short system: SystemCacheControl must stay nil, got %+v", uni.SystemCacheControl)
	}
}

// TestMaybeInjectSystemCache_ClientBreakpointNotOverridden: an existing
// SystemCacheControl (client-supplied cache_control) is never replaced or
// stacked — the exact same pointer must survive the call.
func TestMaybeInjectSystemCache_ClientBreakpointNotOverridden(t *testing.T) {
	client := &adapter.CacheControl{Type: "ephemeral"}
	uni := &adapter.UnifiedRequest{System: mkSystem(5000), SystemCacheControl: client}
	ch := &model.Channel{Type: model.ChannelBedrock, AutoCacheSystem: true}

	maybeInjectSystemCache(uni, ch, "anthropic.claude-sonnet-4")

	if uni.SystemCacheControl != client {
		t.Fatalf("client breakpoint must not be overridden: pointer changed to %+v", uni.SystemCacheControl)
	}
}

// TestMaybeInjectSystemCache_HaikuThreshold covers the model-dependent min-length
// bar: Haiku requires >=4096 tokens, other models >=1024.
func TestMaybeInjectSystemCache_HaikuThreshold(t *testing.T) {
	// ~2000-token system: below Haiku's 4096 bar but above the 1024 bar.
	midSystem := mkSystem(8000)

	t.Run("haiku_mid_not_injected", func(t *testing.T) {
		uni := &adapter.UnifiedRequest{System: midSystem}
		ch := &model.Channel{Type: model.ChannelBedrock, AutoCacheSystem: true}

		maybeInjectSystemCache(uni, ch, "anthropic.claude-haiku-4.5")

		if uni.SystemCacheControl != nil {
			t.Fatalf("haiku + ~2000-token system: must NOT inject (needs >=4096), got %+v", uni.SystemCacheControl)
		}
	})

	t.Run("non_haiku_same_system_injected", func(t *testing.T) {
		uni := &adapter.UnifiedRequest{System: midSystem}
		ch := &model.Channel{Type: model.ChannelBedrock, AutoCacheSystem: true}

		maybeInjectSystemCache(uni, ch, "anthropic.claude-sonnet-4")

		if uni.SystemCacheControl == nil {
			t.Fatal("non-haiku + ~2000-token system: must inject (>=1024), got nil")
		}
	})

	t.Run("haiku_large_system_injected", func(t *testing.T) {
		// ~5000-token system clears Haiku's 4096 bar.
		uni := &adapter.UnifiedRequest{System: mkSystem(20000)}
		ch := &model.Channel{Type: model.ChannelBedrock, AutoCacheSystem: true}

		maybeInjectSystemCache(uni, ch, "anthropic.claude-haiku-4.5")

		if uni.SystemCacheControl == nil {
			t.Fatal("haiku + ~5000-token system: must inject (>=4096), got nil")
		}
		if uni.SystemCacheControl.Type != "ephemeral" {
			t.Fatalf("SystemCacheControl.Type = %q, want ephemeral", uni.SystemCacheControl.Type)
		}
	})
}
