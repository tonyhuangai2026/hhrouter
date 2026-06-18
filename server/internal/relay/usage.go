package relay

import "github.com/agent-router/server/internal/adapter"

// usageTokens returns the prompt/completion/total token counts from an adapter
// Usage, applying the same normalisation the adapter uses (total defaults to the
// sum of parts). estPrompt is the relay's pre-flight prompt estimate, used as a
// floor for the prompt count when the upstream reported no prompt tokens (some
// streaming upstreams omit prompt usage), so quota consumption and the request
// log never under-count input.
func usageTokens(u adapter.Usage, estPrompt int) (prompt, completion, total int) {
	prompt = u.PromptTokens
	if prompt == 0 && estPrompt > 0 {
		prompt = estPrompt
	}
	completion = u.CompletionTokens
	total = u.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	return prompt, completion, total
}
