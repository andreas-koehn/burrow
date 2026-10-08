// cmd/server/e2e_translate_matrix_test.go
//
// The translation matrix through the real wiring (see e2e_gateway_test.go for
// what that is): every released pair, buffered and streamed, with a text
// answer and with a tool call, then a full tool round trip per pair.
//
// The stand-in providers speak one format each: "chatonly" is an
// OpenAI-format provider without the Responses API, "msgonly" an
// Anthropic-format one. Both answer with one call of the tool "echo" when the
// request's last message is the user text "call a tool", and with text
// otherwise. The call's arguments are mxArgs, byte for byte. A streamed answer
// is written in several flushed pieces with the tool call's arguments split
// across them; after the first piece the provider waits until the test has
// read the caller's first frame.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	trchat "github.com/ankoehn/burrow/internal/aigw/translate/chat"
	trmessages "github.com/ankoehn/burrow/internal/aigw/translate/messages"
)

const (
	mxAsk        = "call a tool"
	mxToolResult = "tool says hi"
	// The arguments of the tool call as the stand-in providers write them:
	// with spacing and a key order no encoder would produce, so a translator
	// that decodes and re-encodes them is caught. They are never altered.
	mxArgs   = `{"z": 1,  "text" :"hi" }`
	mxSchema = `{"type":"object","properties":{"text":{"type":"string","description":"what to echo"}},"required":["text"],"additionalProperties":false}`
)

// mxTokens is what a stand-in provider reports as usage, per shape of answer.
type mxTokens struct{ in, out int }

// mxArgPieces is mxArgs as a stream delivers it: cut inside a key and inside
// a value.
var mxArgPieces = []string{`{"z": 1,  "te`, `xt" :"h`, `i" }`}

var mxUsage = map[string]mxTokens{
	"chat/buffered/text": {11, 7}, "chat/buffered/tool": {31, 17}, "chat/streamed/text": {21, 4}, "chat/streamed/tool": {41, 19},
	"messages/buffered/text": {13, 9}, "messages/buffered/tool": {33, 15}, "messages/streamed/text": {23, 6}, "messages/streamed/tool": {43, 21},
}

func mxKey(format string, stream, tool bool) string {
	k := format + "/buffered"
	if stream {
		k = format + "/streamed"
	}
	if tool {
		return k + "/tool"
	}
	return k + "/text"
}

// mxGate holds a streaming provider after its first piece until the test lets
// it go on.
type mxGate struct {
	mu       sync.Mutex
	ch       chan struct{}
	held     bool // a provider is waiting, or has waited
	finished bool // the provider wrote its last piece
	timedOut bool // nobody let it go on
}

func (g *mxGate) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ch, g.held, g.finished, g.timedOut = make(chan struct{}), false, false, false
}

func (g *mxGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ch != nil {
		close(g.ch)
		g.ch = nil
	}
}

// wait is called by the provider after its first piece is flushed.
func (g *mxGate) wait() {
	g.mu.Lock()
	ch := g.ch
	g.held = ch != nil
	g.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		g.mu.Lock()
		g.timedOut = true
		g.mu.Unlock()
	}
}

func (g *mxGate) done() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finished = true
}

func (g *mxGate) state() (held, finished, timedOut bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held, g.finished, g.timedOut
}

// mxAsksForTool reports whether the last message of a request, in either
// format, is the user text mxAsk.
func mxAsksForTool(body []byte) bool {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return false
	}
	var s string
	if json.Unmarshal(last.Content, &s) == nil {
		return s == mxAsk
	}
	var parts []struct{ Type, Text string }
	if json.Unmarshal(last.Content, &parts) != nil {
		return false
	}
	text := ""
	for _, p := range parts {
		if p.Type != "text" {
			return false
		}
		text += p.Text
	}
	return text == mxAsk
}

// mxProvider is the behaviour of a stand-in provider of one format.
func mxProvider(format, name string, gate *mxGate) func(w http.ResponseWriter, r *http.Request, body []byte) bool {
	return func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		tool := mxAsksForTool(body)
		use := mxUsage[mxKey(format, req.Stream, tool)]
		text := "hi from " + name
		switch {
		case format == "chat" && strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			if !req.Stream {
				message := map[string]any{"role": "assistant", "content": text}
				finish := "stop"
				if tool {
					finish = "tool_calls"
					message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
						"id": "call_e2e_1", "type": "function", "function": map[string]string{"name": "echo", "arguments": mxArgs},
					}}}
				}
				gwJSON(w, 200, map[string]any{
					"id": "chatcmpl-e2e", "object": "chat.completion", "model": req.Model,
					"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
					"usage":   map[string]int{"prompt_tokens": use.in, "completion_tokens": use.out, "total_tokens": use.in + use.out},
				})
				return true
			}
			chunk := func(delta, finish string) string {
				return `{"id":"chatcmpl-e2e","object":"chat.completion.chunk","model":"` + req.Model + `","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + finish + `}]}`
			}
			args := func(piece string) string {
				b, _ := json.Marshal(piece)
				return chunk(`{"tool_calls":[{"index":0,"function":{"arguments":`+string(b)+`}}]}`, "null")
			}
			var first, rest []string
			if tool {
				first = []string{
					chunk(`{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_e2e_1","type":"function","function":{"name":"echo","arguments":""}}]}`, "null"),
					args(mxArgPieces[0]),
				}
				rest = []string{args(mxArgPieces[1]), args(mxArgPieces[2]), chunk(`{}`, `"tool_calls"`)}
			} else {
				first = []string{chunk(`{"role":"assistant","content":"hi "}`, "null")}
				rest = []string{chunk(`{"content":"from `+name+`"}`, "null"), chunk(`{}`, `"stop"`)}
			}
			rest = append(rest, fmt.Sprintf(`{"id":"chatcmpl-e2e","object":"chat.completion.chunk","model":%q,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
				req.Model, use.in, use.out, use.in+use.out), "[DONE]")
			mxStream(w, gate, first, rest, func(s string) string { return "data: " + s + "\n\n" })
			return true

		case format == "messages" && strings.HasSuffix(r.URL.Path, "/v1/messages"):
			if !req.Stream {
				// Written by hand: an encoder would re-space the input.
				content, stop := fmt.Sprintf(`{"type":"text","text":%q}`, text), "end_turn"
				if tool {
					content, stop = `{"type":"tool_use","id":"toolu_e2e_1","name":"echo","input":`+mxArgs+`}`, "tool_use"
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"msg_e2e","type":"message","role":"assistant","model":%q,"content":[%s],"stop_reason":%q,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d}}`,
					req.Model, content, stop, use.in, use.out)
				return true
			}
			ev := func(name, data string) string { return name + "\n" + data }
			start := ev("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_e2e","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`, req.Model, use.in))
			delta := func(kind, field, piece string) string {
				b, _ := json.Marshal(piece)
				return ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"`+kind+`","`+field+`":`+string(b)+`}}`)
			}
			end := func(stop string) []string {
				return []string{
					ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
					ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, stop, use.out)),
					ev("message_stop", `{"type":"message_stop"}`),
				}
			}
			var first, rest []string
			if tool {
				first = []string{start,
					ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_e2e_1","name":"echo","input":{}}}`),
					delta("input_json_delta", "partial_json", mxArgPieces[0])}
				rest = append([]string{delta("input_json_delta", "partial_json", mxArgPieces[1]), delta("input_json_delta", "partial_json", mxArgPieces[2])}, end("tool_use")...)
			} else {
				first = []string{start,
					ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
					delta("text_delta", "text", "hi ")}
				rest = append([]string{delta("text_delta", "text", "from "+name)}, end("end_turn")...)
			}
			mxStream(w, gate, first, rest, func(s string) string {
				name, data, _ := strings.Cut(s, "\n")
				return "event: " + name + "\ndata: " + data + "\n\n"
			})
			return true
		}
		// Anything else (the Responses API on "chatonly", a count on either)
		// is something this provider does not offer.
		http.NotFound(w, r)
		return true
	}
}

// mxStream writes first, flushed, waits at the gate, then writes rest piece
// by piece.
func mxStream(w http.ResponseWriter, gate *mxGate, first, rest []string, frame func(string) string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	write := func(s string) {
		_, _ = io.WriteString(w, frame(s))
		if fl != nil {
			fl.Flush()
		}
	}
	for _, s := range first {
		write(s)
	}
	gate.wait()
	for _, s := range rest {
		write(s)
		time.Sleep(5 * time.Millisecond)
	}
	gate.done()
}

// mxCaller is one caller format: how it asks, and how its answer is read.
type mxCaller struct {
	name     string // for the test's name
	dialect  string // what the usage row says
	path     string
	header   func(key string) []string
	request  func(model, userText string, stream bool) string
	turnTwo  func(model, callID string, stream bool) string
	buffered func(t *testing.T, body []byte) mxAnswer
	streamed func(t *testing.T, body []byte) mxAnswer
}

// mxAnswer is an answer as its caller understands it.
type mxAnswer struct {
	Text, Stop                 string
	ToolID, ToolName, ToolArgs string
	Calls                      int // tool calls in the answer
	In, Out                    int // usage, where the format carries it
}

// mxFrame is one frame of an event stream.
type mxFrame struct{ Event, Data string }

func mxFrames(body []byte) []mxFrame {
	var out []mxFrame
	for _, block := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n") {
		var f mxFrame
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event:"); ok {
				f.Event = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "data:"); ok {
				f.Data += strings.TrimPrefix(v, " ")
			}
		}
		if f.Event != "" || f.Data != "" {
			out = append(out, f)
		}
	}
	return out
}

var mxAnthropic = mxCaller{
	name: "messages", dialect: "anthropic", path: "/anthropic/v1/messages",
	header: func(key string) []string { return []string{"x-api-key", key, "anthropic-version", "2023-06-01"} },
	request: func(model, text string, stream bool) string {
		return fmt.Sprintf(`{"model":%q,"max_tokens":256,"stream":%t,"tools":[{"name":"echo","description":"Echo a text.","input_schema":%s}],"messages":[{"role":"user","content":%q}]}`,
			model, stream, mxSchema, text)
	},
	turnTwo: func(model, id string, stream bool) string {
		return fmt.Sprintf(`{"model":%q,"max_tokens":256,"stream":%t,"tools":[{"name":"echo","description":"Echo a text.","input_schema":%s}],"messages":[`+
			`{"role":"user","content":%q},`+
			`{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"echo","input":%s}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}]}`, model, stream, mxSchema, mxAsk, id, mxArgs, id, mxToolResult)
	},
	buffered: func(t *testing.T, body []byte) mxAnswer {
		t.Helper()
		var m struct {
			Type, Role string
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type, Text, ID, Name string
				Input                json.RawMessage
			}
			Usage struct {
				In  int `json:"input_tokens"`
				Out int `json:"output_tokens"`
			}
		}
		if err := json.Unmarshal(body, &m); err != nil || m.Type != "message" || m.Role != "assistant" {
			t.Fatalf("not a Messages answer: %s", body)
		}
		a := mxAnswer{Stop: m.StopReason, In: m.Usage.In, Out: m.Usage.Out}
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				a.Text += c.Text
			case "tool_use":
				a.Calls++
				a.ToolID, a.ToolName, a.ToolArgs = c.ID, c.Name, string(c.Input)
			default:
				t.Fatalf("content block of type %q: %s", c.Type, body)
			}
		}
		return a
	},
	streamed: func(t *testing.T, body []byte) mxAnswer {
		t.Helper()
		frames := mxFrames(body)
		var a mxAnswer
		open := map[int]string{} // index -> block type
		var names []string
		for _, f := range frames {
			var ev struct {
				Type         string
				Index        int
				ContentBlock struct{ Type, ID, Name string } `json:"content_block"`
				Delta        struct {
					Type, Text  string
					PartialJSON string `json:"partial_json"`
					StopReason  string `json:"stop_reason"`
				}
				Usage struct {
					Out int `json:"output_tokens"`
				}
			}
			if err := json.Unmarshal([]byte(f.Data), &ev); err != nil || ev.Type != f.Event {
				t.Fatalf("frame %q does not carry its own type: %s", f.Event, f.Data)
			}
			if ev.Type == "ping" {
				continue
			}
			names = append(names, ev.Type)
			switch ev.Type {
			case "content_block_start":
				if _, dup := open[ev.Index]; dup {
					t.Fatalf("block %d started twice", ev.Index)
				}
				open[ev.Index] = ev.ContentBlock.Type
				if ev.ContentBlock.Type == "tool_use" {
					a.Calls++
					a.ToolID, a.ToolName = ev.ContentBlock.ID, ev.ContentBlock.Name
				}
			case "content_block_delta":
				switch ev.Delta.Type {
				case "text_delta":
					a.Text += ev.Delta.Text
				case "input_json_delta":
					if open[ev.Index] != "tool_use" {
						t.Fatalf("arguments for block %d, which is no open tool call", ev.Index)
					}
					a.ToolArgs += ev.Delta.PartialJSON
				}
			case "content_block_stop":
				delete(open, ev.Index)
			case "message_delta":
				a.Stop, a.Out = ev.Delta.StopReason, ev.Usage.Out
			case "error":
				t.Fatalf("the stream carries an error event: %s", f.Data)
			}
		}
		n := len(names)
		if n < 4 || names[0] != "message_start" || names[n-1] != "message_stop" || names[n-2] != "message_delta" || len(open) != 0 {
			t.Fatalf("not a complete Messages stream: %v (open blocks %v)", names, open)
		}
		return a
	},
}

var mxResponses = mxCaller{
	name: "responses", dialect: "openai", path: "/openai/v1/responses",
	header: bearer,
	request: func(model, text string, stream bool) string {
		return fmt.Sprintf(`{"model":%q,"stream":%t,"tools":[{"type":"function","name":"echo","description":"Echo a text.","parameters":%s}],`+
			`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}]}`, model, stream, mxSchema, text)
	},
	turnTwo: func(model, id string, stream bool) string {
		return fmt.Sprintf(`{"model":%q,"stream":%t,"tools":[{"type":"function","name":"echo","description":"Echo a text.","parameters":%s}],"input":[`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]},`+
			`{"type":"function_call","call_id":%q,"name":"echo","arguments":%q},`+
			`{"type":"function_call_output","call_id":%q,"output":%q}]}`, model, stream, mxSchema, mxAsk, id, mxArgs, id, mxToolResult)
	},
	buffered: func(t *testing.T, body []byte) mxAnswer {
		t.Helper()
		return mxResponseObject(t, body)
	},
	streamed: func(t *testing.T, body []byte) mxAnswer {
		t.Helper()
		frames := mxFrames(body)
		var names []string
		var deltas, textDeltas, doneArgs string
		var last json.RawMessage
		for _, f := range frames {
			var ev struct {
				Type, Delta, Arguments string
				Response               json.RawMessage
			}
			if f.Data == "" {
				continue
			}
			if err := json.Unmarshal([]byte(f.Data), &ev); err != nil || (f.Event != "" && ev.Type != f.Event) {
				t.Fatalf("frame %q does not carry its own type: %s", f.Event, f.Data)
			}
			names = append(names, ev.Type)
			switch ev.Type {
			case "response.function_call_arguments.delta":
				deltas += ev.Delta
			case "response.function_call_arguments.done":
				doneArgs = ev.Arguments
			case "response.output_text.delta":
				textDeltas += ev.Delta
			case "response.completed":
				last = ev.Response
			case "response.failed", "error":
				t.Fatalf("the stream failed: %s", f.Data)
			}
		}
		n := len(names)
		if n < 3 || names[0] != "response.created" || names[n-1] != "response.completed" {
			t.Fatalf("not a complete Responses stream: %v", names)
		}
		a := mxResponseObject(t, last)
		if a.Text != textDeltas {
			t.Fatalf("text deltas %q differ from the completed response's text %q", textDeltas, a.Text)
		}
		if a.Calls > 0 && (deltas != a.ToolArgs || doneArgs != a.ToolArgs) {
			t.Fatalf("argument deltas %q, done %q, completed response %q", deltas, doneArgs, a.ToolArgs)
		}
		return a
	},
}

// mxResponseObject reads a Responses "response" object.
func mxResponseObject(t *testing.T, body []byte) mxAnswer {
	t.Helper()
	var m struct {
		Object, Status string
		Output         []struct {
			Type, Name, Arguments string
			CallID                string `json:"call_id"`
			Content               []struct{ Type, Text string }
		}
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		}
	}
	if err := json.Unmarshal(body, &m); err != nil || m.Object != "response" {
		t.Fatalf("not a Responses answer: %s", body)
	}
	a := mxAnswer{Stop: m.Status, In: m.Usage.In, Out: m.Usage.Out}
	for _, item := range m.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type != "output_text" {
					t.Fatalf("message content of type %q: %s", c.Type, body)
				}
				a.Text += c.Text
			}
		case "function_call":
			a.Calls++
			a.ToolID, a.ToolName, a.ToolArgs = item.CallID, item.Name, item.Arguments
		default:
			t.Fatalf("output item of type %q: %s", item.Type, body)
		}
	}
	return a
}

var mxChat = mxCaller{
	name: "chat", dialect: "openai", path: "/openai/v1/chat/completions",
	header: bearer,
	request: func(model, text string, stream bool) string {
		opts := ""
		if stream {
			opts = `"stream_options":{"include_usage":true},`
		}
		return fmt.Sprintf(`{"model":%q,"stream":%t,%s"tools":[{"type":"function","function":{"name":"echo","description":"Echo a text.","parameters":%s}}],`+
			`"messages":[{"role":"user","content":%q}]}`, model, stream, opts, mxSchema, text)
	},
	turnTwo: func(model, id string, stream bool) string {
		opts := ""
		if stream {
			opts = `"stream_options":{"include_usage":true},`
		}
		return fmt.Sprintf(`{"model":%q,"stream":%t,%s"tools":[{"type":"function","function":{"name":"echo","description":"Echo a text.","parameters":%s}}],"messages":[`+
			`{"role":"user","content":%q},`+
			`{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":"echo","arguments":%q}}]},`+
			`{"role":"tool","tool_call_id":%q,"content":%q}]}`, model, stream, opts, mxSchema, mxAsk, id, mxArgs, id, mxToolResult)
	},
	buffered: func(t *testing.T, body []byte) mxAnswer {
		t.Helper()
		var m struct {
			Object  string
			Choices []struct {
				Message struct {
					Role      string
					Content   *string
					ToolCalls []struct {
						ID, Type string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
				FinishReason string `json:"finish_reason"`
			}
			Usage struct {
				In  int `json:"prompt_tokens"`
				Out int `json:"completion_tokens"`
			}
		}
		if err := json.Unmarshal(body, &m); err != nil || m.Object != "chat.completion" || len(m.Choices) != 1 || m.Choices[0].Message.Role != "assistant" {
			t.Fatalf("not a chat completion: %s", body)
		}
		c := m.Choices[0]
		a := mxAnswer{Stop: c.FinishReason, In: m.Usage.In, Out: m.Usage.Out, Calls: len(c.Message.ToolCalls)}
		if c.Message.Content != nil {
			a.Text = *c.Message.Content
		}
		if a.Calls > 0 {
			tc := c.Message.ToolCalls[0]
			if tc.Type != "function" {
				t.Fatalf("tool call of type %q: %s", tc.Type, body)
			}
			a.ToolID, a.ToolName, a.ToolArgs = tc.ID, tc.Function.Name, tc.Function.Arguments
		}
		return a
	},
	streamed: func(t *testing.T, body []byte) mxAnswer {
		t.Helper()
		frames := mxFrames(body)
		if len(frames) < 3 || frames[len(frames)-1].Data != "[DONE]" {
			t.Fatalf("a chat stream must end in [DONE]: %s", body)
		}
		var a mxAnswer
		calls := map[int]bool{}
		for _, f := range frames[:len(frames)-1] {
			var ch struct {
				Object  string
				Error   json.RawMessage
				Choices []struct {
					Delta struct {
						Content   string
						ToolCalls []struct {
							Index    int
							ID       string
							Function struct{ Name, Arguments string }
						} `json:"tool_calls"`
					}
					FinishReason *string `json:"finish_reason"`
				}
				Usage *struct {
					In  int `json:"prompt_tokens"`
					Out int `json:"completion_tokens"`
				}
			}
			if err := json.Unmarshal([]byte(f.Data), &ch); err != nil || ch.Object != "chat.completion.chunk" || ch.Error != nil {
				t.Fatalf("not a chat chunk: %s", f.Data)
			}
			if ch.Usage != nil {
				a.In, a.Out = ch.Usage.In, ch.Usage.Out
			}
			for _, c := range ch.Choices {
				a.Text += c.Delta.Content
				for _, tc := range c.Delta.ToolCalls {
					if tc.Index != 0 {
						t.Fatalf("a second tool call (index %d): %s", tc.Index, f.Data)
					}
					calls[tc.Index] = true
					if tc.ID != "" {
						a.ToolID = tc.ID
					}
					a.ToolName += tc.Function.Name
					a.ToolArgs += tc.Function.Arguments
				}
				if c.FinishReason != nil {
					a.Stop = *c.FinishReason
				}
			}
		}
		a.Calls = len(calls)
		return a
	},
}

// mxSeenTools returns the tools an upstream was sent, as name -> schema, and
// the call/result pair of its history: both read in the upstream's format.
type mxHistory struct {
	tools                    map[string]json.RawMessage
	callID, callName, args   string
	resultID, result         string
	calls, results, messages int
}

func mxReadUpstream(t *testing.T, format string, body []byte) mxHistory {
	t.Helper()
	h := mxHistory{tools: map[string]json.RawMessage{}}
	text := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var parts []struct{ Type, Text string }
		_ = json.Unmarshal(raw, &parts)
		out := ""
		for _, p := range parts {
			out += p.Text
		}
		return out
	}
	if format == "chat" {
		var req struct {
			Tools []struct {
				Type     string
				Function struct {
					Name       string
					Parameters json.RawMessage
				}
			}
			Messages []struct {
				Role       string
				Content    json.RawMessage
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					ID, Type string
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("the upstream was not sent a Chat Completions request: %v\n%s", err, body)
		}
		for _, tl := range req.Tools {
			if tl.Type != "function" {
				t.Fatalf("tool of type %q sent to a Chat Completions target: %s", tl.Type, body)
			}
			h.tools[tl.Function.Name] = tl.Function.Parameters
		}
		h.messages = len(req.Messages)
		for _, m := range req.Messages {
			for _, tc := range m.ToolCalls {
				if m.Role != "assistant" {
					t.Fatalf("tool call in a %s message: %s", m.Role, body)
				}
				h.calls++
				h.callID, h.callName, h.args = tc.ID, tc.Function.Name, tc.Function.Arguments
			}
			if m.Role == "tool" {
				h.results++
				h.resultID, h.result = m.ToolCallID, text(m.Content)
			}
		}
		return h
	}
	var req struct {
		MaxTokens int `json:"max_tokens"`
		Tools     []struct {
			Name        string
			InputSchema json.RawMessage `json:"input_schema"`
		}
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &req); err != nil || req.MaxTokens <= 0 {
		t.Fatalf("the upstream was not sent a Messages request with max_tokens: %v\n%s", err, body)
	}
	for _, tl := range req.Tools {
		h.tools[tl.Name] = tl.InputSchema
	}
	h.messages = len(req.Messages)
	for _, m := range req.Messages {
		var blocks []struct {
			Type, ID, Name string
			Input          json.RawMessage
			ToolUseID      string `json:"tool_use_id"`
			Content        json.RawMessage
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // a plain string
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				if m.Role != "assistant" {
					t.Fatalf("tool_use in a %s message: %s", m.Role, body)
				}
				h.calls++
				h.callID, h.callName, h.args = b.ID, b.Name, string(b.Input)
			case "tool_result":
				if m.Role != "user" {
					t.Fatalf("tool_result in a %s message: %s", m.Role, body)
				}
				h.results++
				h.resultID, h.result = b.ToolUseID, text(b.Content)
			}
		}
	}
	return h
}

// mxEchoArgs reports whether s is valid JSON with the value of mxArgs. Used
// only where a format carries the arguments as a JSON value, not as a string
// (a Messages body); everywhere else the bytes are compared.
func mxEchoArgs(s string) bool {
	var got any
	return json.Unmarshal([]byte(s), &got) == nil && reflect.DeepEqual(got, map[string]any{"z": float64(1), "text": "hi"})
}

func mxSameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func TestE2E_GatewayTranslationMatrix(t *testing.T) {
	e := bootGatewayE2E(t)
	base := e.up.srv.URL
	gate := &mxGate{}
	e.up.set("chatonly", mxProvider("chat", "chatonly", gate))
	e.up.set("msgonly", mxProvider("messages", "msgonly", gate))

	for _, p := range []map[string]any{
		{"slug": "chatonly", "name": "Chat only", "kind": "direct", "api_format": "openai", "supports_responses": false, "base_url": base + "/chatonly/v1", "credential_slot": "ALPHA"},
		{"slug": "msgonly", "name": "Messages only", "kind": "direct", "api_format": "anthropic", "base_url": base + "/msgonly/v1", "credential_slot": "GAMMA", "auth_header": "x-api-key", "auth_format": "{key}"},
	} {
		if code, body := e.admin(t, "POST", "/api/v1/ai/providers", p); code != http.StatusCreated {
			t.Fatalf("create provider %v: %d %s", p["slug"], code, body)
		}
	}
	chatTarget := map[string]string{"dialect": "openai", "provider": "chatonly", "model": "up-chat"}
	msgTarget := map[string]string{"dialect": "anthropic", "provider": "msgonly", "model": "up-msg"}
	for _, m := range []map[string]any{
		{"name": "t-chat", "translate": true, "targets": []map[string]string{chatTarget}},
		{"name": "t-msg", "translate": true, "targets": []map[string]string{msgTarget}},
		{"name": "t-off", "translate": false, "targets": []map[string]string{chatTarget}},
		{"name": "t-both", "translate": true, "targets": []map[string]string{chatTarget, msgTarget}},
	} {
		if code, body := e.admin(t, "POST", "/api/v1/ai/models", m); code != http.StatusCreated {
			t.Fatalf("create model %v: %d %s", m["name"], code, body)
		}
	}
	code, kb := e.admin(t, "POST", "/api/v1/ai/keys", map[string]any{"name": "matrix"})
	var created struct{ Key string }
	if err := json.Unmarshal(kb, &created); code != http.StatusCreated || err != nil || created.Key == "" {
		t.Fatalf("create key: %d %s", code, kb)
	}
	key := created.Key

	type pairRow struct {
		id       string
		caller   mxCaller
		model    string
		provider string // the stand-in that must be called
		format   string // what it speaks
		upstream string // the path it must be called on
		target   string // the model it must be asked for
		dropped  string // Burrow-Dropped for the requests this test sends, exactly
	}
	pairs := []pairRow{
		{"messages-chat", mxAnthropic, "t-chat", "chatonly", "chat", "/v1/chat/completions", "up-chat", ""},
		{"responses-chat", mxResponses, "t-chat", "chatonly", "chat", "/v1/chat/completions", "up-chat", ""},
		// Neither caller sends an output cap, and a Messages request needs
		// one: the pair sets its default and says so.
		{"chat-messages", mxChat, "t-msg", "msgonly", "messages", "/v1/messages", "up-msg", "max_tokens.default"},
		{"responses-messages", mxResponses, "t-msg", "msgonly", "messages", "/v1/messages", "up-msg", "max_tokens.default"},
	}
	// (The Messages caller sends max_tokens, and nothing in these requests is
	// left out toward a Chat Completions target: an empty list is exact.)

	// checkUpstream: the provider got the request on its own endpoint, in its
	// own format, with its own credential and nothing of the caller's.
	checkUpstream := func(t *testing.T, p pairRow) mxHistory {
		t.Helper()
		seen := e.up.last(t, p.provider)
		if seen.Path != p.upstream || seen.Model != p.target {
			t.Fatalf("the upstream was called on %s for model %q, want %s for %q", seen.Path, seen.Model, p.upstream, p.target)
		}
		if p.format == "messages" {
			if seen.Header.Get("Anthropic-Version") != "2023-06-01" || seen.Header.Get("X-Api-Key") != gwGammaSecret || seen.Header.Get("Authorization") != "" {
				t.Fatalf("headers sent to the Messages target: %v", seen.Header)
			}
		} else if seen.Header.Get("Authorization") != "Bearer "+gwAlphaSecret || seen.Header.Get("X-Api-Key") != "" || seen.Header.Get("Anthropic-Version") != "" {
			t.Fatalf("headers sent to the Chat Completions target: %v", seen.Header)
		}
		for name, vals := range seen.Header {
			if strings.Contains(strings.Join(vals, " "), key) {
				t.Fatalf("the gateway key reached the upstream in %s", name)
			}
		}
		h := mxReadUpstream(t, p.format, seen.Body)
		// Tool definitions: the name unchanged, the JSON Schema passed through.
		if schema, ok := h.tools["echo"]; !ok || len(h.tools) != 1 || !mxSameJSON(schema, []byte(mxSchema)) {
			t.Fatalf("tools sent upstream: %s", seen.Body)
		}
		return h
	}

	// checkRow: exactly one usage row, naming the pair, the caller's dialect
	// and the upstream's own token figures; the header and the row agree on
	// what was left out.
	checkRow := func(t *testing.T, p pairRow, h http.Header, stream, tool bool, before int) {
		t.Helper()
		id := h.Get("Burrow-Request-Id")
		u := e.usageFor(t, id) // fails on more than one row for the request
		want := mxUsage[mxKey(p.format, stream, tool)]
		wantStreamed := 0
		if stream {
			wantStreamed = 1
		}
		if u.Dialect != p.caller.dialect || u.Provider != p.provider || u.Target != p.target || u.Requested != p.model ||
			u.TokensIn != want.in || u.TokensOut != want.out || u.Status != 200 || u.Streamed != wantStreamed {
			t.Fatalf("usage row %+v, want dialect %s provider %s target %s tokens %d/%d streamed %d", u, p.caller.dialect, p.provider, p.target, want.in, want.out, wantStreamed)
		}
		tr, dropped := e.gwTranslation(t, id)
		if tr != p.id || dropped != h.Get("Burrow-Dropped") {
			t.Fatalf("usage row: translated %q dropped %q; headers: Burrow-Translated %q Burrow-Dropped %q", tr, dropped, h.Get("Burrow-Translated"), h.Get("Burrow-Dropped"))
		}
		if _, has := h["Burrow-Dropped"]; dropped != p.dropped || has != (p.dropped != "") {
			t.Fatalf("Burrow-Dropped %q (header present: %v), want exactly %q", dropped, has, p.dropped)
		}
		if after := e.settled(t); after != before+1 {
			t.Fatalf("%d usage rows for one request", after-before)
		}
		t.Logf("%s: Burrow-Dropped %q, usage %d/%d", p.id, dropped, u.TokensIn, u.TokensOut)
	}

	// The arguments are the provider's, byte for byte: the argument string
	// of a Chat or Responses answer, the joined partial_json of a Messages
	// stream, and the raw "input" of a Messages body.
	checkAnswer := func(t *testing.T, p pairRow, a mxAnswer, tool bool) {
		t.Helper()
		if tool {
			if a.Calls != 1 || a.ToolName != "echo" || a.ToolID == "" || a.Text != "" {
				t.Fatalf("tool call as the caller got it: %+v", a)
			}
			if a.ToolArgs != mxArgs {
				t.Fatalf("the tool call's arguments were altered on the way:\n got %q\nwant %q", a.ToolArgs, mxArgs)
			}
			wantStop := map[string]string{"messages": "tool_use", "chat": "tool_calls", "responses": "completed"}[p.caller.name]
			if a.Stop != wantStop {
				t.Fatalf("stop %q, want %q", a.Stop, wantStop)
			}
			return
		}
		wantStop := map[string]string{"messages": "end_turn", "chat": "stop", "responses": "completed"}[p.caller.name]
		if a.Calls != 0 || a.Text != "hi from "+p.provider || a.Stop != wantStop {
			t.Fatalf("text answer as the caller got it: %+v, want %q / %q", a, "hi from "+p.provider, wantStop)
		}
	}

	for _, p := range pairs {
		for _, stream := range []bool{false, true} {
			for _, tool := range []bool{false, true} {
				mode, kind := "buffered", "text"
				if stream {
					mode = "streamed"
				}
				if tool {
					kind = "tool call"
				}
				text := "hi"
				if tool {
					text = mxAsk
				}
				t.Run(p.id+"/"+mode+"/"+kind, func(t *testing.T) {
					before := e.settled(t)
					calls := e.up.calls(p.provider)
					hdr := p.caller.header(key)
					body := p.caller.request(p.model, text, stream)
					want := mxUsage[mxKey(p.format, stream, tool)]

					if !stream {
						r := e.call(t, "POST", p.caller.path, body, hdr...)
						if r.Status != 200 || r.Header.Get("Burrow-Translated") != p.id || r.Header.Get("Burrow-Provider") != p.provider ||
							r.Header.Get("Burrow-Model") != p.target || r.Header.Get("Burrow-Attempts") != "1" || r.Header.Get("Burrow-Error-Code") != "" {
							t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
						}
						if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
							t.Fatalf("Content-Type %q", r.Header.Get("Content-Type"))
						}
						a := p.caller.buffered(t, r.Body)
						checkAnswer(t, p, a, tool)
						// The caller sees the upstream's figures in its own shape.
						if a.In != want.in || a.Out != want.out {
							t.Fatalf("usage in the answer %d/%d, want the upstream's %d/%d", a.In, a.Out, want.in, want.out)
						}
						if e.up.calls(p.provider) != calls+1 {
							t.Fatalf("the provider was called %d times", e.up.calls(p.provider)-calls)
						}
						checkUpstream(t, p)
						checkRow(t, p, r.Header, stream, tool, before)
						return
					}

					// Streamed: the provider holds after its first piece. The
					// caller's first frame must be readable while it does.
					gate.arm()
					defer gate.release()
					resp := e.open(t, "POST", p.caller.path, body, hdr...)
					defer resp.Body.Close()
					if resp.StatusCode != 200 || resp.Header.Get("Burrow-Translated") != p.id || resp.Header.Get("Burrow-Provider") != p.provider ||
						resp.Header.Get("Burrow-Model") != p.target || resp.Header.Get("Burrow-Attempts") != "1" || resp.Header.Get("Burrow-Error-Code") != "" ||
						!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
						b, _ := io.ReadAll(resp.Body)
						t.Fatalf("status %d headers %v body %s", resp.StatusCode, resp.Header, b)
					}
					rd := bufio.NewReader(resp.Body)
					var got []byte
					for !strings.Contains(string(got), "\n\n") {
						line, err := rd.ReadBytes('\n')
						got = append(got, line...)
						if err != nil {
							t.Fatalf("the stream ended before its first frame: %v\n%s", err, got)
						}
					}
					held, finished, timedOut := gate.state()
					if !held || finished || timedOut {
						t.Fatalf("the first frame came with the provider held=%v finished=%v timedOut=%v; it must arrive while the provider is still answering", held, finished, timedOut)
					}
					gate.release()
					rest, err := io.ReadAll(rd)
					if err != nil {
						t.Fatalf("read the stream: %v", err)
					}
					got = append(got, rest...)
					if _, finished, timedOut := gate.state(); !finished || timedOut {
						t.Fatalf("the provider did not finish (finished=%v timedOut=%v)", finished, timedOut)
					}
					a := p.caller.streamed(t, got)
					checkAnswer(t, p, a, tool)
					if p.caller.name != "messages" && (a.In != want.in || a.Out != want.out) {
						// (A Messages stream names its input tokens in its
						// first frame, before the upstream has counted.)
						t.Fatalf("usage in the stream %d/%d, want the upstream's %d/%d", a.In, a.Out, want.in, want.out)
					}
					if p.caller.name == "messages" && a.Out != want.out {
						t.Fatalf("output tokens in the stream %d, want the upstream's %d", a.Out, want.out)
					}
					checkUpstream(t, p)
					checkRow(t, p, resp.Header, stream, tool, before)
				})
			}
		}
	}

	// A full tool round trip per pair, buffered and streamed: turn 1 gets the
	// call; turn 2 sends the result in the caller's format; what the provider
	// is sent for turn 2 is a valid request of its own format (the strict
	// validators) that holds the call and the result under one id.
	for _, p := range pairs {
		for _, stream := range []bool{false, true} {
			mode := "buffered"
			if stream {
				mode = "streamed"
			}
			t.Run(p.id+"/tool round trip/"+mode, func(t *testing.T) {
				hdr := p.caller.header(key)
				read := p.caller.buffered
				if stream {
					read = p.caller.streamed
				}
				r := e.call(t, "POST", p.caller.path, p.caller.request(p.model, mxAsk, stream), hdr...)
				if r.Status != 200 {
					t.Fatalf("turn 1: status %d body %s", r.Status, r.Body)
				}
				first := read(t, r.Body)
				checkAnswer(t, p, first, true)

				r = e.call(t, "POST", p.caller.path, p.caller.turnTwo(p.model, first.ToolID, stream), hdr...)
				if r.Status != 200 || r.Header.Get("Burrow-Translated") != p.id || r.Header.Get("Burrow-Dropped") != p.dropped {
					t.Fatalf("turn 2: status %d headers %v body %s", r.Status, r.Header, r.Body)
				}
				checkAnswer(t, p, read(t, r.Body), false)
				h := checkUpstream(t, p)
				sent := e.up.last(t, p.provider).Body
				validate := trchat.CheckRequest
				if p.format == "messages" {
					validate = trmessages.CheckRequest
				}
				if err := validate(sent); err != nil {
					t.Fatalf("turn 2 as the provider got it is not a valid request of its format: %v\n%s", err, sent)
				}
				if h.calls != 1 || h.results != 1 || h.callName != "echo" || h.result != mxToolResult {
					t.Fatalf("turn 2 as the provider got it: %+v\n%s", h, sent)
				}
				// A Chat Completions target gets the arguments as the string
				// they were; a Messages target as a JSON value.
				if p.format == "chat" && h.args != mxArgs || !mxEchoArgs(h.args) {
					t.Fatalf("the arguments in the history were altered: %q\n%s", h.args, sent)
				}
				if h.callID == "" || h.callID != h.resultID {
					t.Fatalf("the call's id %q and the result's id %q do not match", h.callID, h.resultID)
				}
				if h.messages != 3 {
					t.Fatalf("turn 2 reached the provider as %d messages, want 3 (user, assistant, tool result)", h.messages)
				}
				t.Logf("%s: call id from the caller %q, sent upstream as %q", p.id, first.ToolID, h.callID)
			})
		}
	}

	t.Run("translation off answers format_mismatch", func(t *testing.T) {
		calls := e.up.calls("chatonly")
		r := e.call(t, "POST", "/anthropic/v1/messages", mxAnthropic.request("t-off", "hi", false), mxAnthropic.header(key)...)
		if code, _, _ := anthropicErr(t, r); r.Status != 400 || code != "format_mismatch" || r.Header.Get("Burrow-Translated") != "" {
			t.Fatalf("status %d code %q headers %v", r.Status, code, r.Header)
		}
		r = e.call(t, "POST", "/openai/v1/responses", mxResponses.request("t-off", "hi", false), bearer(key)...)
		if r.Status != 400 || openAIErrCode(t, r) != "endpoint_unsupported" {
			t.Fatalf("responses: status %d body %s", r.Status, r.Body)
		}
		if e.up.calls("chatonly") != calls {
			t.Fatal("a refused request reached the provider")
		}
	})

	t.Run("a native target is not translated although translation is on", func(t *testing.T) {
		for _, c := range []struct {
			caller   mxCaller
			provider string
			path     string
		}{{mxAnthropic, "msgonly", "/v1/messages"}, {mxChat, "chatonly", "/v1/chat/completions"}} {
			body := c.caller.request("t-both", mxAsk, false)
			r := e.call(t, "POST", c.caller.path, body, c.caller.header(key)...)
			if r.Status != 200 || r.Header.Get("Burrow-Translated") != "" || r.Header.Get("Burrow-Dropped") != "" || r.Header.Get("Burrow-Provider") != c.provider {
				t.Fatalf("%s: status %d headers %v body %s", c.caller.name, r.Status, r.Header, r.Body)
			}
			if a := c.caller.buffered(t, r.Body); a.Calls != 1 || a.ToolArgs != mxArgs {
				t.Fatalf("%s: answer %+v", c.caller.name, a)
			}
			// Byte for byte the caller's request, apart from the model.
			seen := e.up.last(t, c.provider)
			wantBody := strings.Replace(body, `"model":"t-both"`, `"model":"`+seen.Model+`"`, 1)
			if seen.Path != c.path || string(seen.Body) != wantBody {
				t.Fatalf("%s: the native request was changed:\n%s\nwant\n%s", c.caller.name, seen.Body, wantBody)
			}
			if tr, dropped := e.gwTranslation(t, r.Header.Get("Burrow-Request-Id")); tr != "" || dropped != "" {
				t.Fatalf("%s: usage row of a native answer: translated %q dropped %q", c.caller.name, tr, dropped)
			}
		}
	})

	t.Run("counting tokens for a translated model is an estimate", func(t *testing.T) {
		before, calls := e.settled(t), e.up.calls("chatonly")
		r := e.call(t, "POST", "/anthropic/v1/messages/count_tokens", `{"model":"t-chat","messages":[{"role":"user","content":"12345678"}]}`, mxAnthropic.header(key)...)
		if r.Status != 200 || strings.TrimSpace(string(r.Body)) != `{"input_tokens":2}` || r.Header.Get("Burrow-Estimated") != "1" {
			t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
		}
		if e.up.calls("chatonly") != calls || e.settled(t) != before {
			t.Fatal("an estimate reached a provider or wrote a usage row")
		}
	})

	t.Run("the management API and the model lists agree with the above", func(t *testing.T) {
		code, body := e.admin(t, "GET", "/api/v1/ai/models", nil)
		var models []struct {
			Name             string
			Translate        bool
			DialectModes     map[string]string   `json:"dialect_modes"`
			ResponsesMode    string              `json:"responses_mode"`
			TranslationPairs map[string][]string `json:"translation_pairs"`
		}
		if err := json.Unmarshal(body, &models); code != 200 || err != nil {
			t.Fatalf("list models: %d %v %s", code, err, body)
		}
		want := map[string][3]string{ // openai, anthropic, responses
			"t-chat": {"native", "translated", "translated"},
			"t-msg":  {"translated", "native", "translated"},
			"t-off":  {"native", "not_served", "not_served"},
			"t-both": {"native", "native", "translated"},
		}
		wantPairs := map[string]map[string][]string{
			"t-chat": {"anthropic": {"messages-chat"}, "responses": {"responses-chat"}},
			"t-msg":  {"openai": {"chat-messages"}, "responses": {"responses-messages"}},
			"t-off":  {},
			"t-both": {"responses": {"responses-chat", "responses-messages"}},
		}
		var openai, anthropic []string
		seen := 0
		for _, m := range models {
			w, ok := want[m.Name]
			if !ok {
				continue
			}
			seen++
			if got := [3]string{m.DialectModes["openai"], m.DialectModes["anthropic"], m.ResponsesMode}; got != w {
				t.Errorf("%s: modes (openai, anthropic, responses) = %v, want %v", m.Name, got, w)
			}
			for _, k := range []string{"openai", "anthropic", "responses"} {
				if !reflect.DeepEqual(m.TranslationPairs[k], wantPairs[m.Name][k]) {
					t.Errorf("%s: translation_pairs[%s] = %v, want %v", m.Name, k, m.TranslationPairs[k], wantPairs[m.Name][k])
				}
			}
			if m.DialectModes["openai"] != "not_served" || m.ResponsesMode != "not_served" {
				openai = append(openai, m.Name)
			}
			if m.DialectModes["anthropic"] != "not_served" {
				anthropic = append(anthropic, m.Name)
			}
		}
		if seen != len(want) {
			t.Fatalf("the management API lists %d of the %d models: %s", seen, len(want), body)
		}
		// What a client is offered is what the management API says is served.
		sortStrings := func(s []string) string {
			for i := range s {
				for j := i + 1; j < len(s); j++ {
					if s[j] < s[i] {
						s[i], s[j] = s[j], s[i]
					}
				}
			}
			return strings.Join(s, ",")
		}
		if got := strings.Join(openAIModelIDs(t, e.call(t, "GET", "/openai/v1/models", "", bearer(key)...)), ","); got != sortStrings(openai) {
			t.Errorf("/openai/v1/models = %s, the management API serves %s", got, sortStrings(openai))
		}
		if got := strings.Join(anthropicModelIDs(t, e.call(t, "GET", "/anthropic/v1/models", "", mxAnthropic.header(key)...)), ","); got != sortStrings(anthropic) {
			t.Errorf("/anthropic/v1/models = %s, the management API serves %s", got, sortStrings(anthropic))
		}
	})
}
