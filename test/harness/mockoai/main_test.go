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

// The Responses mock echoes the model it was asked for and reports usage in
// the Responses spelling (input_tokens / output_tokens).
func TestResponses(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-x","input":"hi","previous_response_id":"resp_0"}`))
	handler().ServeHTTP(rec, req)
	var out struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			In    int `json:"input_tokens"`
			Out   int `json:"output_tokens"`
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body: %s", rec.Body.String())
	}
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || out.ID != "resp_mock" || out.Object != "response" || out.Model != "gpt-x" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if len(out.Output) != 1 || out.Output[0].Type != "message" || out.Output[0].Role != "assistant" ||
		len(out.Output[0].Content) != 1 || out.Output[0].Content[0].Type != "output_text" || out.Output[0].Content[0].Text != "ok" {
		t.Fatalf("output: %s", rec.Body.String())
	}
	if out.Usage.In != 4 || out.Usage.Out != 1 || out.Usage.Total != 5 {
		t.Fatalf("usage: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d", rec.Code)
	}
	// The mock keeps no responses: there is nothing to fetch by id.
	rec = httptest.NewRecorder()
	handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_mock", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET by id: status %d", rec.Code)
	}
}

func TestResponsesSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-x","input":"hi","stream":true}`))
	handler().ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); rec.Code != 200 || !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("status %d content-type %q", rec.Code, ct)
	}
	// Two events, each "event: <type>\ndata: <json>\n\n", in this order.
	events := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n")
	if len(events) != 2 {
		t.Fatalf("events: %q", rec.Body.String())
	}
	data := func(ev, typ string) []byte {
		t.Helper()
		head, payload, ok := strings.Cut(ev, "\n")
		if !ok || head != "event: "+typ || !strings.HasPrefix(payload, "data: ") {
			t.Fatalf("event %q, want type %s", ev, typ)
		}
		return []byte(strings.TrimPrefix(payload, "data: "))
	}
	var delta struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(data(events[0], "response.output_text.delta"), &delta); err != nil || delta.Type != "response.output_text.delta" || delta.Delta != "ok" {
		t.Fatalf("delta event: %q", events[0])
	}
	var done struct {
		Type     string `json:"type"`
		Response struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				In    int `json:"input_tokens"`
				Out   int `json:"output_tokens"`
				Total int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data(events[1], "response.completed"), &done); err != nil {
		t.Fatalf("completed event: %q", events[1])
	}
	if done.Type != "response.completed" || done.Response.ID != "resp_mock" || done.Response.Model != "gpt-x" ||
		done.Response.Usage.In != 4 || done.Response.Usage.Out != 1 || done.Response.Usage.Total != 5 {
		t.Fatalf("completed event: %q", events[1])
	}
}
