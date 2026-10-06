// test/harness/mockoai/main.go
// test-only — never deploy this shape.
//
// Mock OpenAI-compatible server for Burrow e2e tests.
// Implements POST /v1/chat/completions (SSE), POST /v1/embeddings,
// POST /v1/responses (JSON, or SSE with "stream": true),
// POST /v1/messages and /v1/messages/count_tokens (Anthropic shape).
// Deterministic seeded responses — no real model, no phone-home.
// Apache-2.0, stdlib only.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
)

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
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
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
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "" {
			req.Model = "claude-mock"
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
