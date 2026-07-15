package router

import (
	"strings"
	"testing"
)

func turn(role, text string) struct{ Role, Text string } {
	return struct{ Role, Text string }{Role: role, Text: text}
}

// TestRenderProbePrompt_Basic keeps the framing and trailing open assistant turn.
func TestRenderProbePrompt_Basic(t *testing.T) {
	out := RenderProbePrompt("", []struct{ Role, Text string }{
		turn("user", "hello"),
	})
	if !strings.Contains(out, "<|im_start|>user\nhello<|im_end|>\n") {
		t.Fatalf("missing user turn: %q", out)
	}
	if !strings.HasSuffix(out, "<|im_start|>assistant\n") {
		t.Fatalf("must end with open assistant turn: %q", out)
	}
}

// TestRenderProbePrompt_CapsTotal drops OLDEST turns when over budget, keeping
// the most recent context and always the trailing marker.
func TestRenderProbePrompt_CapsTotal(t *testing.T) {
	big := strings.Repeat("x", 3000)
	turns := make([]struct{ Role, Text string }, 0, 20)
	for i := 0; i < 20; i++ {
		turns = append(turns, turn("user", big))
	}
	// Mark the most recent turn so we can assert it survived.
	turns[len(turns)-1] = turn("user", "MOST_RECENT_"+big)
	out := RenderProbePrompt("", turns)
	if len(out) > maxProbePromptChars {
		t.Fatalf("prompt exceeds cap: %d > %d", len(out), maxProbePromptChars)
	}
	if !strings.Contains(out, "MOST_RECENT_") {
		t.Fatal("most recent turn was dropped — should be retained")
	}
	if !strings.HasSuffix(out, "<|im_start|>assistant\n") {
		t.Fatal("trailing assistant marker lost after truncation")
	}
}

// TestRenderProbePrompt_TruncatesHugeTurn elides the middle of a single oversized
// turn with the training-style marker.
func TestRenderProbePrompt_TruncatesHugeTurn(t *testing.T) {
	huge := strings.Repeat("A", 10000) + "TAILMARK"
	out := RenderProbePrompt("", []struct{ Role, Text string }{turn("user", huge)})
	if !strings.Contains(out, "[truncated]") {
		t.Fatalf("expected truncation marker: %q", out[:200])
	}
	if len(out) > maxProbePromptChars {
		t.Fatalf("still over cap: %d", len(out))
	}
}

// TestTruncateMiddle_KeepsEnds keeps head and tail.
func TestTruncateMiddle_KeepsEnds(t *testing.T) {
	s := "HEAD" + strings.Repeat("m", 5000) + "TAIL"
	got := truncateMiddle(s, 100)
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("head/tail not preserved: %q", got)
	}
	if !strings.Contains(got, "[truncated]") {
		t.Fatalf("marker missing: %q", got)
	}
}

// TestTruncateMiddle_ShortUnchanged leaves short strings intact.
func TestTruncateMiddle_ShortUnchanged(t *testing.T) {
	if got := truncateMiddle("short", 100); got != "short" {
		t.Fatalf("short string altered: %q", got)
	}
}
