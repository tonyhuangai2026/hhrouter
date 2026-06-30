package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// servedModel is the model name the classifier container serves (per the API
// reference: fixed "qwen-router").
const servedModel = "qwen-router"

// HTTPProbe calls the routing classifier through an HTTP(S) proxy that fronts
// the SageMaker endpoint (the proxy performs the SigV4-signed InvokeEndpoint so
// this gateway needs no AWS credentials). The proxy is expected to accept and
// return the OpenAI chat-completions shape the classifier container speaks:
//
//	POST {url}
//	  {"model":"qwen-router","messages":[{"role":"user","content":"<prompt>"}],
//	   "max_tokens":16,"temperature":0}
//	→ {"choices":[{"message":{"content":"{\"w\":0,\"t\":79}"}}], ...}
//
// The prediction is the JSON string in choices[0].message.content.
type HTTPProbe struct {
	url    string
	client *http.Client
}

// NewHTTPProbe constructs an HTTPProbe posting to url. A short timeout keeps a
// slow or stuck classifier from blocking routing (the engine treats a probe
// error as w=0,t=0).
func NewHTTPProbe(url string) *HTTPProbe {
	return &HTTPProbe{
		url:    url,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Name implements Probe.
func (h *HTTPProbe) Name() string { return "http" }

// chatRequest / chatResponse mirror the classifier's OpenAI-compatible shape.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Predict implements Probe by POSTing the prompt to the proxy and parsing the
// {w,t} JSON embedded in the response content.
func (h *HTTPProbe) Predict(ctx context.Context, prompt string) (Prediction, error) {
	body, _ := json.Marshal(chatRequest{
		Model:       servedModel,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		MaxTokens:   16,
		Temperature: 0,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return Prediction{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return Prediction{}, fmt.Errorf("router probe request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Prediction{}, fmt.Errorf("router probe http %d: %s", resp.StatusCode, snippet(raw))
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Prediction{}, fmt.Errorf("router probe decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return Prediction{}, fmt.Errorf("router probe: no choices in response")
	}
	// The prediction is a JSON string in the message content.
	content := strings.TrimSpace(cr.Choices[0].Message.Content)
	var pred Prediction
	if err := json.Unmarshal([]byte(content), &pred); err != nil {
		return Prediction{}, fmt.Errorf("router probe parse prediction %q: %w", snippet([]byte(content)), err)
	}
	return pred, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
