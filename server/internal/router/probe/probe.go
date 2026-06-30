// Package probe wraps the "small-model" routing classifier (the Qwen router
// described in the API reference): given a conversation context it predicts
// {"w":0|1, "t":<int>} — whether the next turn likely contains a write/tool
// call (w) and the predicted output token length (t). The routing engine feeds
// these into custom rule expressions to pick an upstream BEFORE calling the
// (expensive) downstream model.
//
// The real classifier is a SageMaker real-time endpoint reached via
// InvokeEndpoint (SigV4-signed; not plain HTTP). That integration is deferred
// (the prod box deliberately has no IAM role) — for now a deterministic Mock
// implementation drives development and tests. Selection between mock and real
// is config-driven (see RouterProbeMock / RouterProbeEndpoint options) so the
// real client can be dropped in later behind the same Probe interface.
package probe

import (
	"context"
	"strings"
)

// Prediction is the classifier output.
type Prediction struct {
	// W is 1 when the next turn likely contains a write/tool call, else 0.
	W int `json:"w"`
	// T is the predicted output token length for the next turn.
	T int `json:"t"`
}

// Probe predicts routing signals for a conversation context. Implementations
// must be safe for concurrent use.
type Probe interface {
	// Predict returns the {w,t} signals for the given prompt (the conversation
	// context, already rendered to the classifier's expected single-string form).
	// It must respect ctx cancellation/timeout.
	Predict(ctx context.Context, prompt string) (Prediction, error)
	// Name identifies the probe implementation (for logs / diagnostics).
	Name() string
}

// MockProbe is a deterministic, dependency-free Probe for development and tests.
// It derives a stable {w,t} from the prompt content so behaviour is predictable
// without any network call:
//   - w = 1 when the prompt contains any write/tool keyword (else 0).
//   - t = a length proxy: min(prompt_len/4, cap), floored at a small base.
//
// A fixed override (FixedW/FixedT) can pin the output for targeted tests.
type MockProbe struct {
	// Fixed, when non-nil, makes Predict always return *Fixed (ignoring prompt).
	Fixed *Prediction
}

// NewMockProbe constructs a heuristic mock.
func NewMockProbe() *MockProbe { return &MockProbe{} }

// NewFixedProbe constructs a mock that always returns the given prediction.
func NewFixedProbe(w, t int) *MockProbe { return &MockProbe{Fixed: &Prediction{W: w, T: t}} }

// Name implements Probe.
func (m *MockProbe) Name() string { return "mock" }

// writeKeywords are substrings that, when present in the prompt, make the mock
// predict a write/tool turn (w=1). Chosen to make the mock useful for demoing
// "route tool/write turns to a stronger model".
var writeKeywords = []string{
	"tool", "function", "write", "create", "edit", "修改", "写", "工具", "调用",
	"insert", "update", "delete", "patch", "生成", "新增",
}

// Predict implements Probe with a deterministic heuristic.
func (m *MockProbe) Predict(_ context.Context, prompt string) (Prediction, error) {
	if m.Fixed != nil {
		return *m.Fixed, nil
	}
	lower := strings.ToLower(prompt)
	w := 0
	for _, kw := range writeKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			w = 1
			break
		}
	}
	// Length proxy for t: ~4 chars/token, capped so the value stays sane, with a
	// small floor so an empty prompt still yields a non-zero estimate.
	t := len(prompt) / 4
	if t < 16 {
		t = 16
	}
	if t > 4096 {
		t = 4096
	}
	return Prediction{W: w, T: t}, nil
}
