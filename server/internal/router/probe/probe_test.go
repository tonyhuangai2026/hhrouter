package probe

import (
	"context"
	"testing"
)

func TestMockProbe_Heuristic(t *testing.T) {
	m := NewMockProbe()

	// A write/tool keyword → w=1.
	p, err := m.Predict(context.Background(), "please write a function to sort an array")
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	if p.W != 1 {
		t.Errorf("w = %d, want 1 (write keyword present)", p.W)
	}
	if p.T < 16 {
		t.Errorf("t = %d, want >= 16 floor", p.T)
	}

	// A plain question → w=0.
	p2, _ := m.Predict(context.Background(), "what is the capital of France?")
	if p2.W != 0 {
		t.Errorf("w = %d, want 0 (no write keyword)", p2.W)
	}

	// Chinese write keyword.
	p3, _ := m.Predict(context.Background(), "帮我修改这段代码")
	if p3.W != 1 {
		t.Errorf("w = %d, want 1 (中文写关键词)", p3.W)
	}
}

func TestFixedProbe(t *testing.T) {
	m := NewFixedProbe(1, 999)
	p, _ := m.Predict(context.Background(), "anything at all, ignored")
	if p.W != 1 || p.T != 999 {
		t.Errorf("fixed probe = %+v, want {1,999}", p)
	}
	if m.Name() != "mock" {
		t.Errorf("name = %q", m.Name())
	}
}

func TestMockProbe_Deterministic(t *testing.T) {
	m := NewMockProbe()
	a, _ := m.Predict(context.Background(), "same prompt")
	b, _ := m.Predict(context.Background(), "same prompt")
	if a != b {
		t.Errorf("non-deterministic: %+v vs %+v", a, b)
	}
}
