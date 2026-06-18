package adapter

import "testing"

func TestUsageFromOpenAI(t *testing.T) {
	// nil → zero, not upstream
	if u := usageFromOpenAI(nil); u.HasUpstream {
		t.Error("nil usage should not be upstream")
	}
	// present → upstream, total preserved
	u := usageFromOpenAI(&openAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	if !u.HasUpstream || u.TotalTokens != 15 {
		t.Errorf("usage = %+v", u)
	}
	// total omitted → derived from parts
	u = usageFromOpenAI(&openAIUsage{PromptTokens: 10, CompletionTokens: 5})
	if u.TotalTokens != 15 {
		t.Errorf("derived total = %d, want 15", u.TotalTokens)
	}
}

func TestUsageFromBedrock(t *testing.T) {
	if u := usageFromBedrock(nil); u.HasUpstream {
		t.Error("nil usage should not be upstream")
	}
	u := usageFromBedrock(&bedrockUsage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10})
	if !u.HasUpstream || u.PromptTokens != 7 || u.CompletionTokens != 3 || u.TotalTokens != 10 {
		t.Errorf("usage = %+v", u)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 1},     // ceil(1/4)
		{"abcd", 1},  // ceil(4/4)
		{"abcde", 2}, // ceil(5/4)
		{"12345678", 2},
	}
	for _, c := range cases {
		if got := estimateTokens(c.text); got != c.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestEstimateUsageFromText(t *testing.T) {
	u := estimateUsageFromText("abcdefgh", "wxyz") // 8 chars → 2, 4 chars → 1
	if u.HasUpstream {
		t.Error("estimate should not be marked upstream")
	}
	if !u.Estimated {
		t.Error("estimate should be marked Estimated")
	}
	if u.PromptTokens != 2 || u.CompletionTokens != 1 || u.TotalTokens != 3 {
		t.Errorf("usage = %+v", u)
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	uni := UnifiedRequest{
		System: "abcd", // 1
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("abcdefgh")}},  // 2
			{Role: RoleAssistant, Content: []ContentBlock{TextBlock("wxyz")}}, // 1
		},
	}
	if got := EstimatePromptTokens(uni); got != 4 {
		t.Errorf("EstimatePromptTokens = %d, want 4", got)
	}
}
