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

// --- tool calls ----------------------------------------------------------------

func post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s: status %d body %s", path, rec.Code, rec.Body.String())
	}
	return rec
}

// events returns the data payloads of an event stream, in order, with each
// event's name ("" when it has none).
func events(t *testing.T, rec *httptest.ResponseRecorder) (names, data []string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q, want an event stream: %s", ct, rec.Body.String())
	}
	for _, ev := range strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n") {
		name := ""
		for _, line := range strings.Split(ev, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				names, data = append(names, name), append(data, v)
			} else {
				t.Fatalf("line %q in event %q", line, ev)
			}
		}
	}
	return names, data
}

func wantEcho(t *testing.T, args string) {
	t.Helper()
	var got map[string]string
	if err := json.Unmarshal([]byte(args), &got); err != nil || len(got) != 1 || got["text"] != "hi" {
		t.Fatalf(`arguments %q, want {"text":"hi"}`, args)
	}
}

const (
	chatAsk      = `"messages":[{"role":"system","content":"s"},{"role":"user","content":"call a tool"}]`
	messagesAsk  = `"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"call a tool"}]}]`
	responsesAsk = `"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"call a tool"}]}]`
)

func TestToolCall_ChatCompletions(t *testing.T) {
	// Buffered: a chat completion, not a stream.
	rec := post(t, "/v1/chat/completions", `{"model":"m",`+chatAsk+`}`)
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		RawUsage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content-type %q body %s", rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if out.Object != "chat.completion" || out.Model != "m" || len(out.Choices) != 1 || out.Choices[0].FinishReason != "tool_calls" ||
		out.Choices[0].Message.Content != nil || len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("body %s", rec.Body.String())
	}
	tc := out.Choices[0].Message.ToolCalls[0]
	if tc.ID == "" || tc.Type != "function" || tc.Function.Name != "echo" {
		t.Fatalf("tool call %+v", tc)
	}
	wantEcho(t, tc.Function.Arguments)
	if out.RawUsage["prompt_tokens"] != 9 || out.RawUsage["completion_tokens"] != 6 || out.RawUsage["total_tokens"] != 15 {
		t.Fatalf("usage %v", out.RawUsage)
	}

	// Streamed: the arguments come in pieces, none of them valid JSON alone.
	_, data := events(t, post(t, "/v1/chat/completions", `{"model":"m","stream":true,`+chatAsk+`}`))
	if n := len(data); n < 5 || data[n-1] != "[DONE]" {
		t.Fatalf("stream %q", data)
	}
	args, name, id, finish, pieces, usage := "", "", "", "", 0, 0
	for _, d := range data[:len(data)-1] {
		var ch struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]int `json:"usage"`
		}
		if err := json.Unmarshal([]byte(d), &ch); err != nil {
			t.Fatalf("chunk %q: %v", d, err)
		}
		if ch.Usage != nil {
			usage = ch.Usage["prompt_tokens"]*100 + ch.Usage["completion_tokens"]
		}
		for _, c := range ch.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.Index != 0 {
					t.Fatalf("tool call index %d", tc.Index)
				}
				id, name = id+tc.ID, name+tc.Function.Name
				if tc.Function.Arguments != "" {
					pieces++
					args += tc.Function.Arguments
					if json.Valid([]byte(tc.Function.Arguments)) {
						t.Fatalf("an argument piece is complete JSON on its own: %q", tc.Function.Arguments)
					}
				}
			}
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
		}
	}
	if id == "" || name != "echo" || finish != "tool_calls" || pieces < 2 || usage != 906 {
		t.Fatalf("id %q name %q finish %q pieces %d usage %d: %q", id, name, finish, pieces, usage, data)
	}
	wantEcho(t, args)

	// The turn after the call (a tool result is the last message), and any
	// other request, is answered as before.
	after := `{"model":"m","messages":[{"role":"user","content":"call a tool"},{"role":"assistant","content":null,"tool_calls":[]},{"role":"tool","tool_call_id":"call_mock_1","content":"hi"}]}`
	for _, body := range []string{after, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, `not json`} {
		if out := post(t, "/v1/chat/completions", body).Body.String(); strings.Contains(out, "tool_calls") || !strings.Contains(out, "mockoai") {
			t.Fatalf("request %s was answered %q", body, out)
		}
	}
}

func TestToolCall_Messages(t *testing.T) {
	rec := post(t, "/v1/messages", `{"model":"c",`+messagesAsk+`}`)
	var out struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Type != "message" || out.StopReason != "tool_use" ||
		len(out.Content) != 1 || out.Content[0].Type != "tool_use" || out.Content[0].ID == "" || out.Content[0].Name != "echo" ||
		out.Usage["input_tokens"] != 9 || out.Usage["output_tokens"] != 6 {
		t.Fatalf("body %s", rec.Body.String())
	}
	wantEcho(t, string(out.Content[0].Input))

	names, data := events(t, post(t, "/v1/messages", `{"model":"c","stream":true,`+messagesAsk+`}`))
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v", names)
	}
	args := ""
	for i, d := range data {
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage map[string]int `json:"usage"`
		}
		if err := json.Unmarshal([]byte(d), &ev); err != nil || ev.Type != names[i] {
			t.Fatalf("event %s: %q", names[i], d)
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type != "tool_use" || ev.ContentBlock.Name != "echo" || ev.ContentBlock.ID == "" {
				t.Fatalf("block start %q", d)
			}
		case "content_block_delta":
			if ev.Delta.Type != "input_json_delta" || json.Valid([]byte(ev.Delta.PartialJSON)) {
				t.Fatalf("delta %q", d)
			}
			args += ev.Delta.PartialJSON
		case "message_delta":
			if ev.Delta.StopReason != "tool_use" || ev.Usage["output_tokens"] != 6 {
				t.Fatalf("message_delta %q", d)
			}
		}
	}
	wantEcho(t, args)

	// A tool result as the last message is answered with text.
	after := `{"model":"c","max_tokens":32,"messages":[{"role":"user","content":"call a tool"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_mock_1","name":"echo","input":{"text":"hi"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_mock_1","content":"hi"}]}]}`
	if out := post(t, "/v1/messages", after).Body.String(); strings.Contains(out, "tool_use") || !strings.Contains(out, `"end_turn"`) {
		t.Fatalf("the turn after the call was answered %q", out)
	}
}

func TestToolCall_Responses(t *testing.T) {
	type item struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type response struct {
		Object string         `json:"object"`
		Status string         `json:"status"`
		Output []item         `json:"output"`
		Usage  map[string]int `json:"usage"`
	}
	check := func(r response, raw string) {
		t.Helper()
		if r.Object != "response" || r.Status != "completed" || len(r.Output) != 1 || r.Output[0].Type != "function_call" ||
			r.Output[0].CallID == "" || r.Output[0].Name != "echo" || r.Usage["input_tokens"] != 9 || r.Usage["output_tokens"] != 6 {
			t.Fatalf("response %s", raw)
		}
		wantEcho(t, r.Output[0].Arguments)
	}
	for _, ask := range []string{responsesAsk, `"input":"call a tool"`} {
		rec := post(t, "/v1/responses", `{"model":"g",`+ask+`}`)
		var out response
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("body %s", rec.Body.String())
		}
		check(out, rec.Body.String())
	}

	names, data := events(t, post(t, "/v1/responses", `{"model":"g","stream":true,`+responsesAsk+`}`))
	if n := len(names); n < 6 || names[0] != "response.created" || names[n-1] != "response.completed" {
		t.Fatalf("events %v", names)
	}
	args := ""
	for i, d := range data {
		var ev struct {
			Type      string   `json:"type"`
			Delta     string   `json:"delta"`
			Arguments string   `json:"arguments"`
			Response  response `json:"response"`
		}
		if err := json.Unmarshal([]byte(d), &ev); err != nil || ev.Type != names[i] {
			t.Fatalf("event %s: %q", names[i], d)
		}
		switch ev.Type {
		case "response.function_call_arguments.delta":
			args += ev.Delta
		case "response.function_call_arguments.done":
			if ev.Arguments != args {
				t.Fatalf("done arguments %q, deltas %q", ev.Arguments, args)
			}
		case "response.completed":
			check(ev.Response, d)
		}
	}
	wantEcho(t, args)

	// The turn after the call is answered with text.
	after := `{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"call a tool"}]},{"type":"function_call","call_id":"call_mock_1","name":"echo","arguments":"{}"},{"type":"function_call_output","call_id":"call_mock_1","output":"hi"}]}`
	if out := post(t, "/v1/responses", after).Body.String(); strings.Contains(out, "function_call") || !strings.Contains(out, `"output_text"`) {
		t.Fatalf("the turn after the call was answered %q", out)
	}
}
