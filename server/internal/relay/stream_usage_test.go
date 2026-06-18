package relay

import (
	"testing"

	"github.com/agent-router/server/internal/adapter"
)

// TestMergeStreamUsage_AnthropicSplit reproduces the bug where Anthropic
// streaming reported input_tokens=0: Anthropic splits usage across two SSE
// events — message_start carries input_tokens (completion=0) and message_delta
// carries output_tokens (prompt=0). A wholesale replace dropped the prompt
// count; mergeStreamUsage must keep both.
func TestMergeStreamUsage_AnthropicSplit(t *testing.T) {
	var u *adapter.Usage
	// message_start: prompt tokens only.
	u = mergeStreamUsage(u, adapter.Usage{PromptTokens: 25, HasUpstream: true})
	if u == nil || u.PromptTokens != 25 {
		t.Fatalf("after message_start: %+v", u)
	}
	// message_delta: completion tokens only (prompt=0 in this event).
	u = mergeStreamUsage(u, adapter.Usage{CompletionTokens: 42, HasUpstream: true})
	if u.PromptTokens != 25 {
		t.Errorf("prompt tokens dropped: got %d, want 25", u.PromptTokens)
	}
	if u.CompletionTokens != 42 {
		t.Errorf("completion = %d, want 42", u.CompletionTokens)
	}
	if u.TotalTokens != 67 {
		t.Errorf("total = %d, want 67 (recomputed from parts)", u.TotalTokens)
	}
	if !u.HasUpstream {
		t.Error("HasUpstream should be true")
	}
}

// TestMergeStreamUsage_SingleEvent confirms OpenAI/Bedrock (both fields in one
// event) are unaffected — the merge is a no-op equivalent to a replace.
func TestMergeStreamUsage_SingleEvent(t *testing.T) {
	u := mergeStreamUsage(nil, adapter.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, HasUpstream: true})
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 {
		t.Fatalf("single-event usage = %+v", u)
	}
}

// TestMergeStreamUsage_ExplicitTotalPreserved confirms an upstream-provided
// total is not clobbered by the parts recompute.
func TestMergeStreamUsage_ExplicitTotalPreserved(t *testing.T) {
	u := mergeStreamUsage(nil, adapter.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 99, HasUpstream: true})
	if u.TotalTokens != 99 {
		t.Errorf("explicit total = %d, want 99 preserved", u.TotalTokens)
	}
}
