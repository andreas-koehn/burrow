// test/harness/mockoai/main.go
// test-only — never deploy this shape.
//
// Mock OpenAI-compatible server for Burrow e2e tests.
// Implements POST /v1/chat/completions (SSE), POST /v1/embeddings,
// POST /v1/responses (JSON, or SSE with "stream": true),
// POST /v1/messages and /v1/messages/count_tokens (Anthropic shape).
// Deterministic seeded responses — no real model, no phone-home.
//
// Tool calls: when the last message of a request is the user text
// "call a tool", each of the three inference endpoints answers with one call
// of the tool "echo" with the arguments {"text":"hi"}, in its own shape:
// buffered, or streamed with "stream": true (the arguments split across
// several events). Every other request is answered as before.
// Apache-2.0, stdlib only.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
)

// What a tool-call answer holds, in every shape.
const (
	toolAsk      = "call a tool"
	toolName     = "echo"
	toolArgs     = `{"text":"hi"}`
	toolCallID   = "call_mock_1"
	toolTokensIn = 9
	toolTokensUp = 6
)

// toolArgPieces is toolArgs as a stream delivers it: cut inside a key and
// inside a value, so a reader must join the pieces before parsing.
var toolArgPieces = []string{`{"te`, `xt":"h`, `i"}`}

// request is what the mock reads of a request in any of the three shapes.
type request struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []json.RawMessage `json:"messages"` // Chat Completions, Messages
	Input    json.RawMessage   `json:"input"`    // Responses: a string or a list of items
}

// readRequest decodes a request body; a body that is no JSON object is an
// empty request.
func readRequest(r *http.Request) request {
	var req request
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	_ = json.Unmarshal(body, &req)
	return req
}

// asksForTool reports whether the last message of the request is the user
// text toolAsk. A tool result sent back is a later message, so the turn after
// a tool call is answered with text again.
func (q request) asksForTool() bool {
	var s string
	if json.Unmarshal(q.Input, &s) == nil && len(q.Input) > 0 {
		return s == toolAsk
	}
	items := q.Messages
	if len(items) == 0 {
		_ = json.Unmarshal(q.Input, &items)
	}
	if len(items) == 0 {
		return false
	}
	var last struct {
		Type    string          `json:"type"` // Responses: "message" or absent
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(items[len(items)-1], &last) != nil || last.Role != "user" || (last.Type != "" && last.Type != "message") {
		return false
	}
	if json.Unmarshal(last.Content, &s) == nil {
		return s == toolAsk
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(last.Content, &parts) != nil {
		return false
	}
	text := ""
	for _, p := range parts {
		if p.Type != "text" && p.Type != "input_text" {
			return false // a tool result, an image: not the plain question
		}
		text += p.Text
	}
	return text == toolAsk
}

// sseWriter writes server-sent events, flushing each one.
type sseWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func newSSE(w http.ResponseWriter) sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	return sseWriter{w: w, fl: fl}
}

// send writes one event; event is its name ("" = none), v its data (a string
// is written as it is, anything else as JSON).
func (s sseWriter) send(event string, v any) {
	data, ok := v.(string)
	if !ok {
		b, _ := json.Marshal(v)
		data = string(b)
	}
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	if s.fl != nil {
		s.fl.Flush()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// chatToolCall answers a Chat Completions request with the tool call.
func chatToolCall(w http.ResponseWriter, req request) {
	usage := map[string]int{"prompt_tokens": toolTokensIn, "completion_tokens": toolTokensUp, "total_tokens": toolTokensIn + toolTokensUp}
	if !req.Stream {
		writeJSON(w, map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "model": req.Model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
					"id": toolCallID, "type": "function", "function": map[string]string{"name": toolName, "arguments": toolArgs},
				}}},
			}},
			"usage": usage,
		})
		return
	}
	s := newSSE(w)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	s.send("", chunk(map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
		"index": 0, "id": toolCallID, "type": "function", "function": map[string]string{"name": toolName, "arguments": ""},
	}}}, nil))
	for _, piece := range toolArgPieces {
		s.send("", chunk(map[string]any{"tool_calls": []map[string]any{{"index": 0, "function": map[string]string{"arguments": piece}}}}, nil))
	}
	s.send("", chunk(map[string]any{}, "tool_calls"))
	s.send("", map[string]any{"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{}, "usage": usage})
	s.send("", "[DONE]")
}

// messagesToolCall answers a Messages request with the tool call.
func messagesToolCall(w http.ResponseWriter, req request) {
	const id = "toolu_mock_1"
	if !req.Stream {
		writeJSON(w, map[string]any{
			"id": "msg_mock", "type": "message", "role": "assistant", "model": req.Model,
			"content":     []map[string]any{{"type": "tool_use", "id": id, "name": toolName, "input": json.RawMessage(toolArgs)}},
			"stop_reason": "tool_use", "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": toolTokensIn, "output_tokens": toolTokensUp},
		})
		return
	}
	s := newSSE(w)
	s.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": toolTokensIn, "output_tokens": 1},
	}})
	s.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "tool_use", "id": id, "name": toolName, "input": map[string]any{}}})
	for _, piece := range toolArgPieces {
		s.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": piece}})
	}
	s.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	s.send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": toolTokensUp}})
	s.send("message_stop", map[string]any{"type": "message_stop"})
}

// responsesToolCall answers a Responses request with the tool call.
func responsesToolCall(w http.ResponseWriter, req request) {
	item := func(status, args string) map[string]any {
		return map[string]any{"type": "function_call", "id": "fc_mock_1", "call_id": toolCallID, "name": toolName, "arguments": args, "status": status}
	}
	response := func(status string, output []map[string]any, usage any) map[string]any {
		return map[string]any{"id": "resp_mock", "object": "response", "status": status, "model": req.Model, "output": output, "usage": usage}
	}
	usage := map[string]int{"input_tokens": toolTokensIn, "output_tokens": toolTokensUp, "total_tokens": toolTokensIn + toolTokensUp}
	done := response("completed", []map[string]any{item("completed", toolArgs)}, usage)
	if !req.Stream {
		writeJSON(w, done)
		return
	}
	s := newSSE(w)
	s.send("response.created", map[string]any{"type": "response.created", "response": response("in_progress", []map[string]any{}, nil)})
	s.send("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item("in_progress", "")})
	for _, piece := range toolArgPieces {
		s.send("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": "fc_mock_1", "output_index": 0, "delta": piece})
	}
	s.send("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": "fc_mock_1", "output_index": 0, "arguments": toolArgs})
	s.send("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item("completed", toolArgs)})
	s.send("response.completed", map[string]any{"type": "response.completed", "response": done})
}

func finishReason(last bool) string {
	if last {
		return `"stop"`
	}
	return "null"
}

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if req := readRequest(r); req.asksForTool() {
			chatToolCall(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, _ := w.(http.Flusher)
		chunks := []string{"Hello", " from", " mockoai", "."}
		for i, c := range chunks {
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":%s}]}\n\n",
				c, finishReason(i == len(chunks)-1))
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Deterministic 4-dim vectors (SHA256 first 4 bytes / 255 each).
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		items := make([]item, len(req.Input))
		for i, s := range req.Input {
			h := sha256.Sum256([]byte(s))
			items[i] = item{Object: "embedding", Index: i,
				Embedding: []float64{float64(h[0]) / 255, float64(h[1]) / 255, float64(h[2]) / 255, float64(h[3]) / 255}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "model": req.Model, "data": items,
		})
	})
	// The Responses API: creating a response only. The mock keeps none, so
	// /v1/responses/<id> is a 404 like any unknown path.
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Echo the model that arrived, so a test sees what a gateway in
		// front of the mock forwarded.
		req := readRequest(r)
		if req.asksForTool() {
			responsesToolCall(w, req)
			return
		}
		const inputTokens = 4
		response := map[string]any{
			"id":     "resp_mock",
			"object": "response",
			"model":  req.Model,
			"output": []map[string]any{{
				"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "output_text", "text": "ok"}},
			}},
			"usage": map[string]int{"input_tokens": inputTokens, "output_tokens": 1, "total_tokens": inputTokens + 1},
		}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, _ := w.(http.Flusher)
		// Usage arrives inside the response object of the closing event.
		for _, ev := range []map[string]any{
			{"type": "response.output_text.delta", "delta": "ok"},
			{"type": "response.completed", "response": response},
		} {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], data)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Echo the model that arrived, so a test sees what a gateway in
		// front of the mock forwarded.
		req := readRequest(r)
		if req.Model == "" {
			req.Model = "claude-mock"
		}
		if req.asksForTool() {
			messagesToolCall(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_mock",
			"type":        "message",
			"role":        "assistant",
			"content":     []map[string]string{{"type": "text", "text": "Hello from mockoai (Anthropic)."}},
			"model":       req.Model,
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 4, "output_tokens": 8},
		})
	})
	mux.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": 4})
	})
	return mux
}

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: handler()}
	log.Printf("[mockoai] listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
