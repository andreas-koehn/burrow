// test/harness/mockoai/main_test.go
// test-only — never deploy this shape.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

func TestChatCompletionsSSE(t *testing.T) {
	body := strings.NewReader(`{"model":"mock","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type: want text/event-stream, got %q", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "data: ") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("expected SSE chunks + [DONE], got %q", out)
	}
}

func TestEmbeddings(t *testing.T) {
	body := strings.NewReader(`{"model":"mock-embed","input":["a","b"]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Content-Type", "application/json")
	handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"data"`) {
		t.Fatalf("body missing data array: %s", rec.Body.String())
	}
}

func TestAnthropicMessages(t *testing.T) {
	body := strings.NewReader(`{"model":"claude-mock","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"message"`) {
		t.Fatalf("missing type=message: %s", rec.Body.String())
	}
}

// The Messages mock names the model it was asked for, so a test can see which
// model a gateway in front of it forwarded.
func TestAnthropicMessages_EchoesModelAndReportsUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"glm-5.1","max_tokens":32,"messages":[]}`))
	handler().ServeHTTP(rec, req)
	var out struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %s", rec.Body.String())
	}
	if rec.Code != 200 || out.Type != "message" || out.Model != "glm-5.1" || out.Usage.In != 4 || out.Usage.Out != 8 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	// Without a model in the request the mock keeps its own name.
	rec = httptest.NewRecorder()
	handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	if !strings.Contains(rec.Body.String(), `"model":"claude-mock"`) {
		t.Fatalf("default model: %s", rec.Body.String())
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`))
	handler().ServeHTTP(rec, req)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"input_tokens":4}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d", rec.Code)
	}
}
