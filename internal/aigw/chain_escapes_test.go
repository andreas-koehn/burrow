package aigw_test

// chain_escapes_test.go — streamed tool-call arguments are raw JSON: a key
// right after an escape ("\n", "\t", "\"") follows a backslash and a
// letter there, which the anchored answer patterns refuse. The escapes are
// decoded before the scan, also when one is split across two pieces.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/inspector"
)

// The anchored answer shapes: each needs something other than a letter (or
// base64 character) before its prefix. Fake keys, built at run time.
var anchoredKeys = []struct{ id, key string }{
	{"anthropic_key", "sk-" + "ant-api03-" + "Zq8vR2mXk4LpT7wN9yBc1Dh"},
	{"openai_key", "sk-" + "proj-" + "Zq8vR2mXk4LpT7wN9yBc1Dh"},
	{"openai_key", "sk-" + "Zq8vR2mXk4LpT7wN9yBc1Dh"},
	{"aws_access_key", "AKIA" + "ZQ8VR2MXK4LPT7WN"},
	{"google_api_key", "AIza" + "SyZq8vR2mXk4LpT7wN9yBc1Dh0"},
	{"github_token", "ghp" + "_" + "Zq8vR2mXk4LpT7wN9yBc1Dh"},
	{"github_token", "github_pat" + "_" + "Zq8vR2mXk4LpT7wN9yBc1Dh"},
	{"burrow_automation_token", "bua" + "_" + "ZQ6VR2MXK4LPT7WNYBCDHJ"},
	{"burrow_gateway_key", "bgw" + "_" + "Zq8vR2mXk4LpT7wN9yBc1Dh"},
}

// argumentFrame is one SSE event carrying a piece of a tool call's
// arguments in format.
func argumentFrame(t *testing.T, format, piece string) string {
	t.Helper()
	var v any
	event := ""
	switch format {
	case "chat":
		v = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": piece}}}}}}}
	case "responses":
		event = "event: response.function_call_arguments.delta\n"
		v = map[string]any{"type": "response.function_call_arguments.delta", "sequence_number": 1, "delta": piece}
	case "messages":
		event = "event: content_block_delta\n"
		v = map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": piece}}
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return event + "data: " + string(b) + "\n\n"
}

func streamEnd(format string) string {
	switch format {
	case "responses":
		return "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":9}\n\n"
	case "messages":
		return "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return "data: [DONE]\n\n"
}

func TestChain_StreamedArgumentsKeyAfterAnEscape(t *testing.T) {
	anthropic := aigw.WithKind(context.Background(), aigw.KindAnthropic)
	formats := []struct {
		format, path, req string
		ctx               context.Context
	}{
		{"chat", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, context.Background()},
		{"responses", "/v1/responses", `{"model":"m","input":"hi","stream":true}`, context.Background()},
		{"messages", "/v1/messages", `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, anthropic},
	}
	for _, f := range formats {
		for _, k := range anchoredKeys {
			for _, esc := range []string{`\n`, `\t`, `\"`} {
				// The arguments as the model streams them: JSON whose string
				// holds the key right after an escape.
				args := `{"path":"keys.txt","content":"# keys` + esc + k.key + `\n"}`
				at := strings.Index(args, esc+k.key)
				for _, split := range []struct {
					name   string
					pieces []string
				}{
					{"one piece", []string{args[:at-2], args[at-2:]}},
					// The backslash ends one piece, the escaped letter starts
					// the next.
					{"escape split", []string{args[:at+1], args[at+1:]}},
				} {
					name := f.format + "/" + k.id + "/" + k.key[:4] + "/" + esc + "/" + split.name
					t.Run(name, func(t *testing.T) {
						var body strings.Builder
						for _, p := range split.pieces {
							body.WriteString(argumentFrame(t, f.format, p))
						}
						body.WriteString(streamEnd(f.format))
						obs := &requestObserver{}
						chain := answersChain(inspector.NewManager(), obs, &answerHook{})
						out := runChain(t, chain, &leakUpstream{body: body.String(), stream: true},
							answersSvc("svc-esc", guardrails.AnswerActionBlock), postReq(f.ctx, f.path, f.req))
						if got := out.Body.String(); strings.Contains(got, k.key[len(k.key)-8:]) || !strings.Contains(got, "guardrail_blocked_answer") {
							t.Fatalf("not blocked:\n%s", got)
						}
						if ev := obs.only(t); ev.GuardrailPattern != k.id || ev.GuardrailDirection != "answer" {
							t.Fatalf("event %+v, want %s", ev, k.id)
						}
					})
				}
			}
		}
	}
}
