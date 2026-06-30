package probe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPProbe_ParsesPrediction(t *testing.T) {
	var gotBody chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		// Mirror the API reference: content is a JSON STRING holding {w,t}.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"w\":1,\"t\":79}"}}],"usage":{"prompt_tokens":31,"completion_tokens":13}}`))
	}))
	defer srv.Close()

	p := NewHTTPProbe(srv.URL)
	pred, err := p.Predict(context.Background(), "<|im_start|>user\nhi<|im_end|>\n<|im_start|>assistant\n")
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	if pred.W != 1 || pred.T != 79 {
		t.Errorf("prediction = %+v, want {1,79}", pred)
	}
	// Request must carry the fixed model + single user message + max_tokens 16.
	if gotBody.Model != servedModel {
		t.Errorf("model = %q, want %q", gotBody.Model, servedModel)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Role != "user" {
		t.Errorf("messages = %+v", gotBody.Messages)
	}
	if gotBody.MaxTokens != 16 {
		t.Errorf("max_tokens = %d, want 16", gotBody.MaxTokens)
	}
}

func TestHTTPProbe_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	if _, err := NewHTTPProbe(srv.URL).Predict(context.Background(), "x"); err == nil {
		t.Error("expected error on http 500")
	}
}

func TestHTTPProbe_BadPredictionJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"not json"}}]}`))
	}))
	defer srv.Close()
	if _, err := NewHTTPProbe(srv.URL).Predict(context.Background(), "x"); err == nil {
		t.Error("expected parse error on non-JSON content")
	}
}

func TestHTTPProbe_Name(t *testing.T) {
	if NewHTTPProbe("http://x").Name() != "http" {
		t.Error("name")
	}
	_ = strings.TrimSpace("")
}
