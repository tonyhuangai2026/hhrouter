package relay

import (
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/model"
)

func TestErrorBodyOpenAISchema(t *testing.T) {
	body := ErrorBody(FormatOpenAI, "invalid_request_error", "boom").(gin.H)
	errObj, ok := body["error"].(gin.H)
	if !ok {
		t.Fatalf("openai error body missing error object: %#v", body)
	}
	if errObj["message"] != "boom" || errObj["type"] != "invalid_request_error" {
		t.Fatalf("unexpected openai error object: %#v", errObj)
	}
	if _, leaked := body["type"]; leaked {
		t.Fatalf("openai schema must not carry a top-level type: %#v", body)
	}
}

func TestErrorBodyAnthropicSchema(t *testing.T) {
	body := ErrorBody(FormatAnthropic, "authentication_error", "nope").(gin.H)
	if body["type"] != "error" {
		t.Fatalf("anthropic error body must have top-level type=error: %#v", body)
	}
	errObj, ok := body["error"].(gin.H)
	if !ok {
		t.Fatalf("anthropic error body missing error object: %#v", body)
	}
	if errObj["message"] != "nope" || errObj["type"] != "authentication_error" {
		t.Fatalf("unexpected anthropic error object: %#v", errObj)
	}
}

func TestErrTypeQuotaDiffersByFormat(t *testing.T) {
	if got := ErrType(FormatOpenAI, ClassQuota); got != "insufficient_quota" {
		t.Fatalf("openai quota type = %q, want insufficient_quota", got)
	}
	if got := ErrType(FormatAnthropic, ClassQuota); got != "rate_limit_error" {
		t.Fatalf("anthropic quota type = %q, want rate_limit_error", got)
	}
}

func TestUsageTokensPromptFloorAndTotalDerivation(t *testing.T) {
	// Upstream reported no prompt tokens; the pre-flight estimate is used as a
	// floor and total is derived from the parts.
	u := adapter.Usage{PromptTokens: 0, CompletionTokens: 7}
	prompt, completion, total := usageTokens(u, 12)
	if prompt != 12 || completion != 7 || total != 19 {
		t.Fatalf("usageTokens floor/derivation = (%d,%d,%d), want (12,7,19)", prompt, completion, total)
	}

	// Upstream-authoritative numbers are preserved as-is.
	u2 := adapter.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}
	p2, c2, t2 := usageTokens(u2, 100)
	if p2 != 5 || c2 != 3 || t2 != 8 {
		t.Fatalf("usageTokens authoritative = (%d,%d,%d), want (5,3,8)", p2, c2, t2)
	}
}

func TestModelAllowed(t *testing.T) {
	unrestricted := &model.Token{}
	if !modelAllowed(unrestricted, "anything") {
		t.Fatal("empty allowed_models must permit any model")
	}

	restricted := &model.Token{AllowedModels: []byte(`["gpt-4o","claude-3"]`)}
	if !modelAllowed(restricted, "gpt-4o") {
		t.Fatal("listed model must be allowed")
	}
	if modelAllowed(restricted, "gpt-3.5") {
		t.Fatal("unlisted model must be rejected")
	}
}

func TestFinalizeUsageStreamFallback(t *testing.T) {
	// No upstream usage: estimate completion from text, prompt from estimate.
	p, c, total := finalizeUsage(nil, "abcd", 10) // 4 chars -> 1 token
	if p != 10 || c != 1 || total != 11 {
		t.Fatalf("finalizeUsage fallback = (%d,%d,%d), want (10,1,11)", p, c, total)
	}

	// Upstream usage present: prefer it.
	u := &adapter.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11}
	p2, c2, t2 := finalizeUsage(u, "ignored", 999)
	if p2 != 9 || c2 != 2 || t2 != 11 {
		t.Fatalf("finalizeUsage upstream = (%d,%d,%d), want (9,2,11)", p2, c2, t2)
	}
}
