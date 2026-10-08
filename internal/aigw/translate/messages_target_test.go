package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
	"github.com/ankoehn/burrow/internal/aigw/translate/responses"
)

// The pairs with an Anthropic Messages target: chat-messages (the OpenAI SDKs on an
// Anthropic-format provider) and responses-messages (Codex on one).

func lookup(t testing.TB, from, to Format) Pair {
	t.Helper()
	p, ok := Lookup(from, to)
	if !ok {
		t.Fatalf("no released pair %s-%s", from, to)
	}
	return p
}

const askUsage = `{"stream":true,"stream_options":{"include_usage":true}}`

// newTargetCall is one answer of a Messages target sent through the pair from → Messages.
func newTargetCall(t testing.TB, from Format, stream bool, callerRequest string) *call {
	c := &call{rec: httptest.NewRecorder()}
	c.w = lookup(t, from, Messages).Response(c.rec, ResponseOptions{Stream: stream, RequestedModel: "asked-for", CallerRequest: []byte(callerRequest),
		OnError: func(status int, code string) { c.onError = append(c.onError, fmt.Sprint(status, " ", code)) }})
	return c
}

// anthropicSSE writes Messages stream frames from their data; the event name is the data's type.
func anthropicSSE(datas ...string) []byte {
	var b bytes.Buffer
	for _, data := range datas {
		var head struct{ Type string }
		_ = json.Unmarshal([]byte(data), &head)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", head.Type, data)
	}
	return b.Bytes()
}

const (
	aStart   = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m-up","content":[],"usage":{"input_tokens":9,"output_tokens":1}}}`
	aText    = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	aPar     = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`
	aStop0   = `{"type":"content_block_stop","index":0}`
	aEnd     = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`
	aToolEnd = `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`
	aDone    = `{"type":"message_stop"}`
	aError   = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
)

func aTool(i int, id, name string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, i, id, name)
}
func aArgs(i int, raw []byte) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":"%s"}}`, i, escapeBytes(raw))
}
func aStop(i int) string { return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i) }

// anthropicToolStream writes a Messages stream of several tool calls whose inputs are cut into
// random pieces of bytes (so characters are cut in two as well). Anthropic's blocks come one
// after the other; side by side, with the pieces interleaved, is what the neutral events allow
// and a caller's encoder must carry as well.
func anthropicToolStream(rng *rand.Rand, args []string, sideBySide bool) []byte {
	frames := []string{aStart, aText, aPar, aStop0}
	piece := func(i int, left *[]byte) {
		n := 1 + rng.Intn(min(len(*left), 9))
		frames = append(frames, aArgs(i+1, (*left)[:n]))
		*left = (*left)[n:]
	}
	left := make([][]byte, len(args))
	for i, a := range args {
		left[i] = []byte(a)
	}
	if !sideBySide {
		for i := range args {
			frames = append(frames, aTool(i+1, fmt.Sprint("call_", i), fmt.Sprint("fn", i)))
			for len(left[i]) > 0 {
				piece(i, &left[i])
			}
			frames = append(frames, aStop(i+1))
		}
	} else {
		for i := range args {
			frames = append(frames, aTool(i+1, fmt.Sprint("call_", i), fmt.Sprint("fn", i)))
		}
		for {
			var open []int
			for i := range left {
				if len(left[i]) > 0 {
					open = append(open, i)
				}
			}
			if len(open) == 0 {
				break
			}
			i := open[rng.Intn(len(open))]
			piece(i, &left[i])
		}
		for i := range args {
			frames = append(frames, aStop(i+1))
		}
	}
	return anthropicSSE(append(frames, aToolEnd, aDone)...)
}

// ---------------------------------------------------------------- registry

func TestMessagesTarget_LookupAndRelease(t *testing.T) {
	for _, c := range []struct {
		from, to Format
		id, path string
	}{
		{Messages, Chat, "messages-chat", "/v1/chat/completions"},
		{Responses, Chat, "responses-chat", "/v1/chat/completions"},
		{Chat, Messages, "chat-messages", "/v1/messages"},
		{Responses, Messages, "responses-messages", "/v1/messages"},
	} {
		p, ok := Lookup(c.from, c.to)
		if !ok || p.ID() != c.id || p.UpstreamPath() != c.path || !p.Released() {
			t.Errorf("Lookup(%s, %s) = %v, %v", c.from, c.to, p, ok)
		}
	}
	if len(pairs) != 4 {
		t.Fatalf("%d pairs", len(pairs))
	}
	if messages.Version != "2023-06-01" || messages.DefaultMaxTokens != 32000 {
		t.Fatal("the anthropic-version or the default max_tokens changed: the gateway and the docs name them")
	}
}

func TestMessagesTarget_ReleasedIsDecidedByTheToolCallChecks(t *testing.T) {
	now := time.Now()
	for name, breakIt := range map[string]func(p *pair){
		"the stream encoder loses argument pieces": func(p *pair) {
			inner := p.codec.newStreamEncoder
			p.codec.newStreamEncoder = func(w io.Writer, o ResponseOptions) streamEncoder { return dropArgs{inner(w, o)} }
		},
		"the stream decoder stops early": func(p *pair) {
			p.codec.newStreamDecoder = func() streamDecoder { return &stopsEarly{d: messages.NewStreamDecoder()} }
		},
		"the buffered answer loses its tool calls": func(p *pair) {
			inner := p.codec.encodeResponse
			p.codec.encodeResponse = func(r ir.Response, model string) ([]byte, error) {
				r.Parts = r.Parts[:1]
				return inner(r, model)
			}
		},
		"the buffered answer rewrites the arguments": func(p *pair) {
			inner := p.codec.decodeResponse
			p.codec.decodeResponse = func(body []byte) (ir.Response, error) {
				return inner(bytes.ReplaceAll(body, []byte(`{ "city"`), []byte(`{"city"`)))
			}
		},
		"the cached input tokens are lost": func(p *pair) {
			inner := p.codec.decodeResponse
			p.codec.decodeResponse = func(body []byte) (ir.Response, error) {
				return inner(bytes.ReplaceAll(body, []byte(`"cache_read_input_tokens"`), []byte(`"x"`)))
			}
		},
		"the request puts the tool results in two messages": func(p *pair) {
			inner := p.request
			p.request = func(body []byte, h http.Header, model string) ([]byte, bool, []string, error) {
				out, stream, dropped, err := inner(body, h, model)
				return bytes.Replace(out, []byte(`},{"type":"tool_result","tool_use_id":"toolu_2"`), []byte(`}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2"`), 1), stream, dropped, err
			}
		},
		"the request loses the tool definitions": func(p *pair) {
			inner := p.request
			p.request = func(body []byte, h http.Header, model string) ([]byte, bool, []string, error) {
				out, stream, dropped, err := inner(body, h, model)
				return bytes.Replace(out, []byte(`"input_schema"`), []byte(`"schema"`), 1), stream, dropped, err
			}
		},
		"the request fails": func(p *pair) {
			p.request = func([]byte, http.Header, string) ([]byte, bool, []string, error) {
				return nil, false, nil, errors.New("no")
			}
		},
		"the request refuses a history with a call that was cut": func(p *pair) {
			inner := p.request
			p.request = func(body []byte, h http.Header, model string) ([]byte, bool, []string, error) {
				if bytes.Contains(body, []byte(`"arguments":"{\"city\":"}`)) {
					return nil, false, nil, &ir.BadRequestError{Format: "chat", Field: "messages[2].tool_calls[1].function.arguments", Reason: "is not the text of a JSON object"}
				}
				return inner(body, h, model)
			}
		},
	} {
		for _, fresh := range []func() *pair{newChatMessages, newResponsesMessages} {
			p := fresh()
			if err := p.check(p); err != nil {
				t.Fatalf("%s: the tool-call checks of the sound pair: %v", p.id, err)
			}
			p = fresh()
			breakIt(p)
			if p.Released() {
				t.Errorf("%s, %s: the pair is released", p.id, name)
			}
			if err := p.check(p); err == nil {
				t.Errorf("%s, %s: the checks pass", p.id, name)
			}
		}
	}
	// What only one of the two callers can get wrong.
	p := newChatMessages()
	p.codec.newStreamEncoder = func(w io.Writer, o ResponseOptions) streamEncoder {
		return chat.NewStreamEncoder(w, o.RequestedModel, now, true) // a usage chunk nobody asked for
	}
	if p.Released() {
		t.Error("chat-messages is released though it writes a usage chunk that was not asked for")
	}
	// (A Responses caller's encoder judges the arguments itself and fails such a stream.)
	p = newChatMessages()
	p.codec.newStreamDecoder = func() streamDecoder { return &finishesAnyway{d: messages.NewStreamDecoder()} }
	if p.Released() {
		t.Error("chat-messages is released though it takes a cut stream for a whole one")
	}
	p = newResponsesMessages()
	p.codec.newStreamEncoder = func(w io.Writer, o ResponseOptions) streamEncoder {
		return closesCalls{responses.NewStreamEncoder(w, o.RequestedModel, now)}
	}
	if p.Released() {
		t.Error("responses-messages is released though it hands over a call that was cut")
	}
}

// finishesAnyway ends a stream that was cut as if the model had said it was done.
type finishesAnyway struct{ d streamDecoder }

func (f *finishesAnyway) Feed(event string, data []byte) ([]ir.Event, error) {
	return f.d.Feed(event, data)
}
func (f *finishesAnyway) Close() []ir.Event {
	events := f.d.Close()
	for i := range events {
		if events[i].Kind == ir.Error {
			events[i] = ir.Event{Kind: ir.Finish, Stop: ir.StopToolUse}
		}
	}
	return events
}

// ---------------------------------------------------------------- request

func TestChatMessages_Request(t *testing.T) {
	body := readFile(t, "chat/testdata/req_tools.json")
	out, stream, dropped, err := lookup(t, Chat, Messages).Request(body, http.Header{"Openai-Organization": {"org"}}, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, out, string(readFile(t, "testdata/chat_messages_request.json")))
	if err := messages.CheckRequest(out); err != nil {
		t.Fatal(err)
	}
	// The caller sent no cap: Anthropic requires one, the default is sent and told. Its
	// temperature is not sent on, and that is told as well.
	if !stream || !reflect.DeepEqual(dropped, []string{"max_tokens.default", "temperature"}) {
		t.Fatalf("stream %v, dropped %v", stream, dropped)
	}
	// Nothing dropped is nil, and a request that does not stream says so.
	out, stream, dropped, err = lookup(t, Chat, Messages).Request([]byte(`{"model":"m","max_completion_tokens":5,"messages":[{"role":"user","content":"hi"}]}`), nil, "claude-x")
	if err != nil || stream || dropped != nil || string(out) != `{"model":"claude-x","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}` {
		t.Fatalf("%s, %v, %v, %v", out, stream, dropped, err)
	}
	// What the target cannot be given is told, by name, and what a caller's decoder reported stays.
	_, _, dropped, err = lookup(t, Chat, Messages).Request([]byte(`{"model":"m","n":2,"seed":7,"temperature":1.7,"top_p":0.9,"stop":["\n","END"],"parallel_tool_calls":false,"response_format":{"type":"json_object"},
"tools":[{"type":"web_search"}],"tool_choice":"required","zeta":1,
"messages":[{"role":"assistant","content":"Hello.","reasoning_content":"hm"},{"role":"user","content":"hi"}]}`), nil, "claude-x")
	want := []string{"max_tokens.default", "messages.start", "n", "parallel_tool_calls", "response_format", "seed", "stop.blank", "temperature", "thinking",
		"tool:web_search", "tool_choice", "top_p", "unknown:zeta"}
	if err != nil || !reflect.DeepEqual(dropped, want) {
		t.Fatalf("%v\n got %v\nwant %v", err, dropped, want)
	}
}

func TestResponsesMessages_Request(t *testing.T) {
	body := readFile(t, "responses/testdata/req_codex.json")
	out, stream, dropped, err := lookup(t, Responses, Messages).Request(body, nil, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, out, string(readFile(t, "testdata/responses_messages_request.json")))
	if err := messages.CheckRequest(out); err != nil {
		t.Fatal(err)
	}
	want := []string{"client_metadata", "include", "input.namespace", "input.phase", "input:reasoning", "max_tokens.default", "parallel_tool_calls",
		"prompt_cache_key", "reasoning", "stream_options", "text.verbosity", "tool:custom", "tool:local_shell", "tool:web_search"}
	if !stream || !reflect.DeepEqual(dropped, want) {
		t.Fatalf("stream %v, dropped = %v, want %v", stream, dropped, want)
	}
}

func TestMessagesTarget_Request_ClientErrors(t *testing.T) {
	for _, c := range []struct {
		from       Format
		body, want string
	}{
		{Chat, `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"SECRET","format":"wav"}}]}]}`, "chat: messages[0].content[0]: audio cannot be translated"},
		{Chat, `{"model":"m","messages":[]}`, "chat: messages: is empty"},
		// An image the target cannot take is a 400 that names it, never a missing image.
		{Chat, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/tiff;base64,SECRET"}}]}]}`,
			"messages: cannot be expressed in Anthropic Messages: messages[0].image.media_type"},
		{Chat, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"ftp://SECRET/a.png"}}]}]}`,
			"messages: cannot be expressed in Anthropic Messages: messages[0].image.url"},
		{Responses, `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/bmp;base64,SECRET"}]}]}`,
			"messages: cannot be expressed in Anthropic Messages: messages[0].image.media_type"},
		{Chat, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"array"}}}]}`,
			"messages: cannot be expressed in Anthropic Messages: tools[0].input_schema (ir: not a JSON object)"},
		// A tool declared under a name Anthropic refuses.
		{Chat, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"SECRET.tool"}}]}`,
			"messages: tools[0].name: is not a name of 1 to 128 of the characters a-z, A-Z, 0-9, _ and - (Anthropic takes no other)"},
	} {
		out, _, _, err := lookup(t, c.from, Messages).Request([]byte(c.body), nil, "claude-x")
		message, ok := BadRequest(err)
		if out != nil || !ok || message != c.want || strings.Contains(message, "SECRET") {
			t.Errorf("%s:\n got %q (%v)\nwant %q", c.body, message, err, c.want)
		}
	}
	if _, ok := BadRequest(errors.New("other")); ok {
		t.Fatal("BadRequest took an error of another kind")
	}
}

func TestMessagesTarget_Request_ImagesInToolResults(t *testing.T) {
	// The image a tool returned travels inside its tool_result again, whichever caller sent it.
	want := `[{"role":"user","content":[{"type":"text","text":"read it"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}]`
	for from, body := range map[Format]string{
		Chat: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"read it"},
 {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
 {"role":"tool","tool_call_id":"call_1","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}]}`,
		Responses: `{"model":"m","max_output_tokens":5,"input":[{"role":"user","content":"read it"},
 {"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"},
 {"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"data:image/png;base64,aGk="}]}]}`,
	} {
		out, _, dropped, err := lookup(t, from, Messages).Request([]byte(body), nil, "claude-x")
		if err != nil || dropped != nil {
			t.Fatalf("%s: %v, dropped %v", from, err, dropped)
		}
		var got struct{ Messages json.RawMessage }
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, got.Messages, want)
		if err := messages.CheckRequest(out); err != nil {
			t.Fatalf("%s: %v", from, err)
		}
	}
}

// ---------------------------------------------------------------- answers

type completion struct {
	ID, Object, Model string
	Created           int64
	Choices           []struct {
		Index   int
		Message struct {
			Role             string
			Content          *string
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID, Type string
				Function struct{ Name, Arguments string }
			} `json:"tool_calls"`
		}
		FinishReason string `json:"finish_reason"`
	}
	Usage struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Total      int `json:"total_tokens"`
	}
}

func TestChatMessages_Buffered(t *testing.T) {
	c := newTargetCall(t, Chat, false, "")
	c.w.Header().Set("Content-Type", "application/json")
	c.w.Header().Set("Request-Id", "req_011")
	c.w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "7")
	c.w.Header().Set("Set-Cookie", "a=b")
	c.upstream(200, "", readFile(t, "messages/testdata/resp_tools.json"), 50)
	var got completion
	if err := json.Unmarshal(c.rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, c.rec.Body)
	}
	if c.rec.Code != 200 || got.ID != "chatcmpl-msg_01B" || got.Object != "chat.completion" || got.Model != "claude-x" || got.Created < 1_700_000_000 || len(got.Choices) != 1 {
		t.Fatalf("%d %+v", c.rec.Code, got)
	}
	// The cached input tokens are input: 31 + 100 read from the cache.
	if got.Usage.Prompt != 131 || got.Usage.Completion != 17 || got.Usage.Total != 148 {
		t.Fatalf("usage %+v", got.Usage)
	}
	msg := got.Choices[0].Message
	if got.Choices[0].FinishReason != "tool_calls" || msg.Content == nil || *msg.Content != "Checking both." || len(msg.ToolCalls) != 2 {
		t.Fatalf("%+v", got.Choices[0])
	}
	// "arguments" is the upstream's input, byte for byte.
	if one, two := msg.ToolCalls[0], msg.ToolCalls[1]; one.ID != "toolu_1" || one.Type != "function" || one.Function.Name != "get_weather" || one.Function.Arguments != `{"city":"Oslo","days":[1,2,3]}` ||
		two.ID != "toolu_2" || two.Function.Arguments != `{ "city": "Rome", "unit": "°C" }` {
		t.Fatalf("%+v", msg.ToolCalls)
	}
	h := c.rec.Header()
	if h.Get("Content-Type") != "application/json" || h.Get("Request-Id") != "req_011" || h.Get("Anthropic-Ratelimit-Requests-Remaining") != "7" || h.Get("Set-Cookie") != "" {
		t.Fatalf("header %v", h)
	}
	if code, mid := c.w.Failure(); code != "" || mid || c.onError != nil {
		t.Fatalf("Failure = %q, %v", code, mid)
	}
	// Thinking is reasoning_content; an answer that says nothing has a null content.
	c = newTargetCall(t, Chat, false, "").upstream(200, "application/json",
		[]byte(`{"type":"message","content":[{"type":"thinking","thinking":"hm","signature":"sig"},{"type":"redacted_thinking","data":"x"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":2}}`), 0)
	got = completion{}
	if err := json.Unmarshal(c.rec.Body.Bytes(), &got); err != nil || got.Model != "asked-for" || got.Choices[0].Message.Content != nil ||
		got.Choices[0].Message.ReasoningContent != "hm" || got.Choices[0].FinishReason != "length" || bytes.Contains(c.rec.Body.Bytes(), []byte("tool_calls")) {
		t.Fatalf("%v: %s", err, c.rec.Body)
	}
}

func TestChatMessages_Streamed(t *testing.T) {
	upstream := readFile(t, "messages/testdata/stream_tools.sse")
	c := newTargetCall(t, Chat, true, askUsage)
	c.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	c.w.Header().Set("Request-Id", "req_8")
	c.w.WriteHeader(200)
	if c.rec.Body.Len() != 0 || c.rec.Flushed {
		t.Fatal("something reached the caller before the first event")
	}
	firstAt := -1
	for i := 0; i < len(upstream); i += 7 {
		if _, err := c.w.Write(upstream[i:min(i+7, len(upstream))]); err != nil {
			t.Fatal(err)
		}
		if firstAt < 0 && c.rec.Body.Len() > 0 {
			firstAt = i
			if !c.rec.Flushed || !strings.HasPrefix(c.rec.Body.String(), `data: {"id":"chatcmpl-msg_01B","object":"chat.completion.chunk"`) {
				t.Fatalf("the first frame was not flushed: %q", c.rec.Body)
			}
		}
	}
	c.w.Finish()
	if firstAt < 0 || firstAt > 400 {
		t.Fatalf("the first caller frame came after %d upstream bytes of %d", firstAt, len(upstream))
	}
	h := c.rec.Header()
	if c.rec.Code != 200 || h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" || h.Get("Request-Id") != "req_8" {
		t.Fatalf("status %d, header %v", c.rec.Code, h)
	}
	raw := c.rec.Body.Bytes()
	s, err := chat.CheckStream(raw)
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if !s.Done || s.FinishReason != "tool_calls" || s.Content != "Checking both." || s.Model != "claude-x" || len(s.Calls) != 2 ||
		!s.HasUsage || s.PromptTokens != 131 || s.CompletionTokens != 17 || s.TotalTokens != 148 {
		t.Fatalf("%+v", s)
	}
	// The fragments of each call, concatenated per index, are the upstream's two inputs — and
	// they are the upstream's fragments, one chunk each.
	one, two := s.Calls[0], s.Calls[1]
	if one.ID != "toolu_1" || one.Name != "get_weather" || one.Arguments != `{"city":"Oslo","days":[1,2,3]}` ||
		!reflect.DeepEqual(one.Deltas, []string{`{"ci`, `ty":"Os`, `lo","days":[1,2`, `,3]}`}) ||
		two.ID != "toolu_2" || two.Arguments != `{ "city": "Rome", "unit": "°C" }` || !reflect.DeepEqual(two.Deltas, []string{`{ "city": "Ro`, `me", "unit": "°`, `C" }`}) {
		t.Fatalf("%+v", s.Calls)
	}
	// The stream ends with the finish chunk, the usage chunk and [DONE], and with nothing else.
	if !bytes.HasSuffix(raw, []byte(`"finish_reason":"tool_calls"}],"usage":null}`+"\n\n"+
		`data: {"id":"chatcmpl-msg_01B","object":"chat.completion.chunk","created":`+fmt.Sprint(s.Created)+`,"model":"claude-x","choices":[],"usage":{"prompt_tokens":131,"completion_tokens":17,"total_tokens":148}}`+
		"\n\ndata: [DONE]\n\n")) {
		t.Fatalf("the end of the stream:\n%s", raw[max(0, len(raw)-500):])
	}
	if code, mid := c.w.Failure(); code != "" || mid || c.onError != nil {
		t.Fatalf("Failure = %q, %v; OnError %v", code, mid, c.onError)
	}
	// A caller that did not ask for the usage gets no chunk without choices.
	c = newTargetCall(t, Chat, true, `{"stream":true}`).upstream(200, "text/event-stream", upstream, 64)
	if s, err := chat.CheckStream(c.rec.Body.Bytes()); err != nil || !s.Done || s.HasUsage || bytes.Contains(c.rec.Body.Bytes(), []byte("usage")) {
		t.Fatalf("%v %+v", err, s)
	}
	// Text only, in pieces of every size.
	text := readFile(t, "messages/testdata/stream_text.sse")
	for _, piece := range []int{1, 2, 3, 5, 64, len(text)} {
		c := newTargetCall(t, Chat, true, askUsage).upstream(200, "text/event-stream", text, piece)
		s, err := chat.CheckStream(c.rec.Body.Bytes())
		if err != nil || !s.Done || s.Content != "Hello there." || s.FinishReason != "stop" || s.PromptTokens != 20 || s.CompletionTokens != 4 || s.ID != "chatcmpl-msg_01A" {
			t.Fatalf("piece %d: %v, %+v", piece, err, s)
		}
	}
}

func TestResponsesMessages_Streamed(t *testing.T) {
	upstream := readFile(t, "messages/testdata/stream_tools.sse")
	for _, piece := range []int{1, 7, len(upstream)} {
		c := newTargetCall(t, Responses, true, "").upstream(200, "text/event-stream", upstream, piece)
		s, err := responses.CheckStream(c.rec.Body.Bytes())
		if err != nil {
			t.Fatalf("piece %d: %v\n%s", piece, err, c.rec.Body)
		}
		// The stream ends in response.completed, with both function_call items.
		if !s.Completed || s.Status != "completed" || s.Model != "claude-x" || s.InputTokens != 131 || s.OutputTokens != 17 || len(s.Items) != 3 ||
			!bytes.Contains(c.rec.Body.Bytes(), []byte("event: response.completed\n")) {
			t.Fatalf("piece %d: %+v", piece, s)
		}
		text, one, two := s.Items[0], s.Items[1], s.Items[2]
		if text.Type != "message" || text.Text != "Checking both." ||
			one.Type != "function_call" || one.CallID != "toolu_1" || one.Name != "get_weather" || one.Arguments != `{"city":"Oslo","days":[1,2,3]}` || !one.Done ||
			two.Type != "function_call" || two.CallID != "toolu_2" || two.Arguments != `{ "city": "Rome", "unit": "°C" }` || !two.Done {
			t.Fatalf("piece %d: %+v", piece, s.Items)
		}
		if code, mid := c.w.Failure(); code != "" || mid {
			t.Fatalf("Failure = %q, %v", code, mid)
		}
	}
	// A whole answer.
	c := newTargetCall(t, Responses, false, "").upstream(200, "application/json", readFile(t, "messages/testdata/resp_tools.json"), 0)
	var body responseBody
	if err := json.Unmarshal(c.rec.Body.Bytes(), &body); err != nil || body.Status != "completed" || len(body.Output) != 3 ||
		body.Output[2].Arguments != `{ "city": "Rome", "unit": "°C" }` || body.Usage.Input != 131 {
		t.Fatalf("%v: %s", err, c.rec.Body)
	}
}

func TestMessagesTarget_Streamed_ArgumentsReassembleToTheUpstreamsBytes(t *testing.T) {
	// Review Focus 1, through the whole pairs: one call and several, one after the other as
	// Anthropic sends them and side by side, cut at random bytes and delivered in random pieces.
	rng := rand.New(rand.NewSource(7))
	pool := []string{`{"path":"a.txt"}`, `{ "b":1.0, "a":[1, "<&>"], "n":12345678901234567890123 }`, `{}`, `{"s":"日本語 😀 é \u00e9 \ud83d\ude00"}`,
		`{"cmd":"printf \"%s\\n\" \"x\"","deep":{"a":{"b":[{}, [], null, true]}}}`, `{"s":"` + strings.Repeat("xy", 300) + `"}`}
	for round := 0; round < 300; round++ {
		n := 1 + rng.Intn(4)
		args := make([]string, n)
		for i := range args {
			args[i] = pool[rng.Intn(len(pool))]
		}
		upstream := anthropicToolStream(rng, args, round%2 == 1)
		piece := 1 + rng.Intn(200)

		c := newTargetCall(t, Chat, true, askUsage).upstream(200, "text/event-stream", upstream, piece)
		s, err := chat.CheckStream(c.rec.Body.Bytes())
		if err != nil || !s.Done || s.FinishReason != "tool_calls" || len(s.Calls) != n || s.Content != "par" {
			t.Fatalf("round %d, chat: %v, %+v\n%s", round, err, s, c.rec.Body)
		}
		for i, call := range s.Calls {
			if call.ID != fmt.Sprint("call_", i) || call.Name != fmt.Sprint("fn", i) || call.Arguments != args[i] {
				t.Fatalf("round %d, chat: call %d is %+v, want %s", round, i, call, args[i])
			}
		}

		c = newTargetCall(t, Responses, true, "").upstream(200, "text/event-stream", upstream, piece)
		rs, err := responses.CheckStream(c.rec.Body.Bytes())
		if err != nil || !rs.Completed || len(rs.Items) != n+1 {
			t.Fatalf("round %d, responses: %v, %+v\n%s", round, err, rs, c.rec.Body)
		}
		for i, item := range rs.Items[1:] {
			if item.CallID != fmt.Sprint("call_", i) || item.Arguments != args[i] || strings.Join(item.Deltas, "") != args[i] || !item.Done {
				t.Fatalf("round %d, responses: item %d is %+v, want %s", round, i, item, args[i])
			}
		}
	}
}

func TestMessagesTarget_Streamed_BadEndings(t *testing.T) {
	// Review Focus 2 and 5: no failure of a stream that had begun is reported as a success, to
	// either caller.
	for name, c := range map[string]struct {
		body    []byte
		code    string
		message string
	}{
		"the upstream closes mid-text":      {anthropicSSE(aStart, aText, aPar), CodeUpstreamInvalid, "the provider ended the stream early"},
		"the upstream closes mid-arguments": {anthropicSSE(aStart, aText, aPar, aStop0, aTool(1, "t", "f"), aArgs(1, []byte(`{"a":`))), CodeUpstreamInvalid, "the provider ended the stream early"},
		"no message_stop":                   {anthropicSSE(aStart, aText, aPar, aStop0, aEnd), CodeUpstreamInvalid, "the provider ended the stream early"},
		"an error event (overloaded_error)": {anthropicSSE(aStart, aText, aPar, aError, aStop0, aEnd, aDone), CodeUpstreamError, "Overloaded"},
		"a delta for an index never started": {anthropicSSE(aStart, aText, aPar, `{"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"x"}}`, aStop0, aEnd, aDone),
			CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"a second message_start":         {anthropicSSE(aStart, aText, aPar, aStart, aStop0, aEnd, aDone), CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"a tool_use block without an id": {anthropicSSE(aStart, aText, aPar, aStop0, aTool(1, "", "f"), aStop(1), aToolEnd, aDone), CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"a tool_use block without a name": {anthropicSSE(aStart, aText, aPar, aStop0, aTool(1, "t", ""), aStop(1), aToolEnd, aDone), CodeUpstreamInvalid,
			"the provider sent an answer that cannot be read"},
		"arguments that are no JSON object": {anthropicSSE(aStart, aText, aPar, aStop0, aTool(1, "t", "f"), aArgs(1, []byte(`{"a":]`)), aStop(1), aToolEnd, aDone), CodeUpstreamInvalid,
			"the provider sent an answer that cannot be read"},
		"invalid JSON in a frame": {append(anthropicSSE(aStart, aText, aPar), "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",SECRET\n\n"...), CodeUpstreamInvalid,
			"the provider sent an answer that cannot be read"},
		"an enormous argument string": {anthropicSSE(aStart, aText, aPar, aStop0, aTool(1, "t", "f"), aArgs(1, []byte(`{"a":"`+strings.Repeat("x", ir.MaxToolArgsBytes)+`"}`)), aStop(1), aToolEnd, aDone),
			CodeUpstreamInvalid, "the provider's answer is too large"},
		"a frame over the frame limit": {append(anthropicSSE(aStart, aText, aPar), []byte("data: "+strings.Repeat("y", messages.MaxFrameBytes+10)+"\n\n")...), CodeUpstreamInvalid, "the provider's answer is too large"},
	} {
		for _, piece := range []int{0, 1, 11} {
			if piece == 1 && len(c.body) > 1<<20 {
				continue
			}
			// A Chat caller: the error object, no finish_reason, no usage chunk, no [DONE].
			call := newTargetCall(t, Chat, true, askUsage).upstream(200, "text/event-stream", c.body, piece)
			raw := call.rec.Body.Bytes()
			s, err := chat.CheckStream(raw)
			if err != nil {
				t.Errorf("%s (piece %d): a client refuses the stream: %v\n%.600s", name, piece, err, raw)
				continue
			}
			if s.Done || s.FinishReason != "" || s.HasUsage || s.ErrMessage != c.message || s.ErrType != "server_error" || s.Content != "par" || bytes.Contains(raw, []byte("[DONE]")) {
				t.Errorf("%s (piece %d): %+v", name, piece, s)
			}
			if code, mid := call.w.Failure(); code != c.code || !mid || call.rec.Code != 200 || !reflect.DeepEqual(call.onError, []string{"200 " + c.code}) {
				t.Errorf("%s (piece %d): Failure = %q, %v; status %d; OnError %v", name, piece, code, mid, call.rec.Code, call.onError)
			}
			// A Responses caller: response.failed, and no call handed over that was cut.
			call = newTargetCall(t, Responses, true, "").upstream(200, "text/event-stream", c.body, piece)
			rs, err := responses.CheckStream(call.rec.Body.Bytes())
			if err != nil || rs.Completed || rs.Status != "failed" || rs.ErrMessage != c.message {
				t.Errorf("%s (piece %d), responses: %v, %+v", name, piece, err, rs)
				continue
			}
			for _, it := range rs.Items {
				if it.Type == "function_call" && it.Done && ir.CheckObject([]byte(it.Arguments)) != nil {
					t.Errorf("%s (piece %d), responses: a call that was cut is handed over: %+v", name, piece, it)
				}
			}
			if code, mid := call.w.Failure(); code != c.code || !mid {
				t.Errorf("%s (piece %d), responses: Failure = %q, %v", name, piece, code, mid)
			}
		}
	}
}

func TestMessagesTarget_Streamed_FailureBeforeTheFirstByteIsAnHTTPError(t *testing.T) {
	for name, c := range map[string]struct {
		contentType string
		body        []byte
		code        string
		message     string
	}{
		"an error event first":                   {"text/event-stream", anthropicSSE(aError), CodeUpstreamError, "Overloaded"},
		"an empty stream":                        {"text/event-stream", nil, CodeUpstreamInvalid, "the provider ended the stream early"},
		"nothing but pings":                      {"text/event-stream", anthropicSSE(`{"type":"ping"}`, `{"type":"ping"}`), CodeUpstreamInvalid, "the provider ended the stream early"},
		"a Chat Completions stream":              {"text/event-stream", sseOf(chunk(`{"content":"x"}`), finishChunk("stop")), CodeUpstreamInvalid, "the provider ended the stream early"},
		"a Chat Completions stream with [DONE]":  {"text/event-stream", sseOf(chunk(`{"content":"x"}`), finishChunk("stop"), "[DONE]"), CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"garbage first":                          {"text/event-stream", []byte("event: message_start\ndata: {SECRET\n\n"), CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"content before message_start":           {"text/event-stream", anthropicSSE(aText, aPar), CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"an error object where a stream was due": {"application/json", []byte(aError), CodeUpstreamError, "Overloaded"},
		"a whole message where a stream was due": {"application/json", readFile(t, "messages/testdata/resp_text.json"), CodeUpstreamInvalid, "the provider did not answer in the form that was asked for"},
	} {
		call := newTargetCall(t, Chat, true, askUsage).upstream(200, c.contentType, c.body, 5)
		if message := call.openaiError(t); call.rec.Code != 502 || message != c.message {
			t.Errorf("%s: status %d, message %q", name, call.rec.Code, message)
		}
		if code, mid := call.w.Failure(); code != c.code || mid || !reflect.DeepEqual(call.onError, []string{"502 " + c.code}) {
			t.Errorf("%s: Failure = %q, %v; OnError %v", name, code, mid, call.onError)
		}
		if strings.Contains(call.rec.Body.String(), "SECRET") {
			t.Errorf("%s: the caller got the provider's bytes: %s", name, call.rec.Body)
		}
	}
}

func TestMessagesTarget_UpstreamErrorsKeepTheirStatus(t *testing.T) {
	for _, c := range []struct {
		status  int
		body    string
		message string
	}{
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "slow down"},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, "Overloaded"},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 4096 > 1024"}}`, "max_tokens: 4096 > 1024"},
		{401, `{"error":{"message":"openai shape"}}`, "openai shape"},
		{500, `<html>oops</html>`, "the provider answered 500"},
		{503, ``, "the provider answered 503"},
	} {
		for _, stream := range []bool{false, true} {
			call := newTargetCall(t, Chat, stream, askUsage)
			call.w.Header().Set("Retry-After", "7")
			call.upstream(c.status, "application/json", []byte(c.body), 3)
			if message := call.openaiError(t); call.rec.Code != c.status || message != c.message || call.rec.Header().Get("Retry-After") != "7" {
				t.Errorf("%d (stream %v): status %d, message %q, header %v", c.status, stream, call.rec.Code, message, call.rec.Header())
			}
			if code, mid := call.w.Failure(); code != CodeUpstreamError || mid || !reflect.DeepEqual(call.onError, []string{fmt.Sprint(c.status, " ", CodeUpstreamError)}) {
				t.Errorf("%d: Failure = %q, %v; OnError %v", c.status, code, mid, call.onError)
			}
		}
	}
	// A 200 that is no answer.
	for name, c := range map[string]struct {
		body    string
		code    string
		message string
	}{
		"an error object":   {aError, CodeUpstreamError, "Overloaded"},
		"not JSON":          {`<html>`, CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"a chat completion": {`{"id":"c","choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`, CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"a tool call without a name": {`{"type":"message","content":[{"type":"tool_use","id":"t","input":{}}],"stop_reason":"tool_use"}`, CodeUpstreamInvalid,
			"the provider sent an answer that cannot be read"},
		"an enormous input": {`{"type":"message","content":[{"type":"tool_use","id":"t","name":"f","input":{"a":"` + strings.Repeat("x", ir.MaxToolArgsBytes) + `"}}],"stop_reason":"tool_use"}`,
			CodeUpstreamInvalid, "the provider's answer is too large"},
	} {
		call := newTargetCall(t, Chat, false, "").upstream(200, "application/json", []byte(c.body), 0)
		if message := call.openaiError(t); call.rec.Code != 502 || message != c.message {
			t.Errorf("%s: status %d, message %q", name, call.rec.Code, message)
		}
		if code, _ := call.w.Failure(); code != c.code {
			t.Errorf("%s: Failure = %q", name, code)
		}
	}
}

func TestMessagesTarget_TheHistoryOfAnyAnswerIsTaken(t *testing.T) {
	// Random Messages streams — text, then calls — whole and cut at a random byte: what a Chat
	// caller got goes back as an assistant message with a tool message for every call, as an
	// OpenAI SDK client replays it, and is a conversation Anthropic accepts. A session never ends
	// on its own history.
	rng := rand.New(rand.NewSource(11))
	args := []string{`{"path":"a.txt"}`, `{ "b":1.0, "a":[1, "<&>"] }`, `{}`, `{"s":"` + strings.Repeat("xy", 40) + `"}`}
	quote := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	replayed := 0
	for round := 0; round < 400; round++ {
		n := 1 + rng.Intn(len(args))
		upstream := anthropicToolStream(rng, args[:n], round%3 == 0)
		if round%4 != 0 {
			upstream = upstream[:rng.Intn(len(upstream))]
		}
		whole := bytes.Contains(upstream, []byte(aDone)) // the model said it was done: only the blank line after it may be missing
		c := newTargetCall(t, Chat, true, askUsage).upstream(200, "text/event-stream", upstream, 1+rng.Intn(60))
		if !strings.HasPrefix(c.rec.Header().Get("Content-Type"), "text/event-stream") {
			continue // cut before the first event: an HTTP error, and nothing to send back
		}
		s, err := chat.CheckStream(c.rec.Body.Bytes())
		if err != nil {
			t.Fatalf("round %d: %v\n%s", round, err, c.rec.Body)
		}
		if s.Done != whole {
			t.Fatalf("round %d: Done = %v for a stream that was whole: %v\n%s", round, s.Done, whole, c.rec.Body)
		}
		var calls, results []string
		for _, call := range s.Calls {
			// What an SDK accumulated: the arguments string as far as it came.
			calls = append(calls, `{"id":`+quote(call.ID)+`,"type":"function","function":{"name":`+quote(call.Name)+`,"arguments":`+quote(call.Arguments)+`}}`)
			results = append(results, `{"role":"tool","tool_call_id":`+quote(call.ID)+`,"content":"ok"}`)
		}
		assistant := `{"role":"assistant","content":` + quote(s.Content)
		if len(calls) > 0 {
			assistant += `,"tool_calls":[` + strings.Join(calls, ",") + `]`
		}
		body := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"go"},` + assistant + `},` + strings.Join(append(results, `{"role":"user","content":"go on"}`), ",") + `]}`
		out, _, dropped, err := lookup(t, Chat, Messages).Request([]byte(body), nil, "claude-x")
		if err != nil {
			t.Fatalf("round %d: the history is refused: %v\n%s", round, err, body)
		}
		if whole && dropped != nil {
			t.Fatalf("round %d: dropped %v for the history of a whole answer", round, dropped)
		}
		if err := messages.CheckRequest(out); err != nil {
			t.Fatalf("round %d: %v\n%s", round, err, out)
		}
		replayed++
	}
	if replayed < 200 {
		t.Fatalf("only %d histories were replayed", replayed)
	}
}

func TestMessagesTarget_NothingIsLogged(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() { log.SetOutput(os.Stderr); slog.SetDefault(old) }()

	for _, from := range []Format{Chat, Responses} {
		_, _, _, err := lookup(t, from, Messages).Request([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"SECRET"}}]}],"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_data":"SECRET"}]}]}`), nil, "x")
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("err = %v", err)
		}
		newTargetCall(t, from, false, "").upstream(429, "application/json", []byte(`{"type":"error","error":{"message":"SECRET key sk-ant-123"}}`), 0)
		newTargetCall(t, from, false, "").upstream(200, "application/json", []byte(`{"SECRET":1}`), 0)
		newTargetCall(t, from, true, "").upstream(200, "text/event-stream", anthropicSSE(aStart, aText, aPar, `{"type":"error","error":{"message":"SECRET"}}`), 0)
		newTargetCall(t, from, true, "").upstream(200, "text/event-stream", append(anthropicSSE(aStart, aText, aPar), "data: {SECRET\n\n"...), 0)
	}
	if logged.Len() != 0 {
		t.Fatalf("something was logged: %s", logged.String())
	}
}

// ---------------------------------------------------------------- fuzz

// FuzzChatMessagesWriter plays an arbitrary upstream answer through the pair chat-messages.
// Oracle: the caller gets exactly one of a chat completion, an OpenAI error response, or a
// well-formed Chat Completions stream that ended — and what the writer reports about it is true.
func FuzzChatMessagesWriter(f *testing.F) {
	f.Add(anthropicSSE(aStart, aText, aPar, aStop0, aTool(1, "t", "f"), aArgs(1, []byte(`{"a":1}`)), aStop(1), aToolEnd, aDone), 7, true, 200, "text/event-stream", true)
	f.Add(anthropicSSE(aStart, aText, aPar, aStop0, aEnd, aDone), 1, true, 200, "text/event-stream", false)
	f.Add([]byte(`{"type":"message","content":[{"type":"tool_use","id":"t","name":"f","input":{"a":1}}],"stop_reason":"tool_use","usage":{"input_tokens":1}}`), 50, false, 200, "application/json", false)
	f.Add([]byte(aError), 3, false, 529, "application/json", false)
	f.Add(anthropicSSE(aStart, aText, aPar, aError), 4, true, 200, "text/event-stream", true)
	f.Add(anthropicSSE(aError), 4, true, 200, "text/event-stream", true)
	f.Add([]byte("<html>"), 2, true, 502, "text/html", false)
	f.Fuzz(func(t *testing.T, body []byte, piece int, stream bool, status int, contentType string, usage bool) {
		if status < 200 || status > 599 {
			status = 200
		}
		request := ""
		if usage {
			request = askUsage
		}
		c := newTargetCall(t, Chat, stream, request).upstream(status, contentType, body, piece)
		code, mid := c.w.Failure()
		out := c.rec.Body.Bytes()
		switch {
		case mid:
			s, err := chat.CheckStream(out)
			if err != nil || s.Done || s.ErrMessage == "" || code == "" || c.rec.Code != status {
				t.Fatalf("a failed stream: %v, %+v, code %q, status %d\n%s", err, s, code, c.rec.Code, out)
			}
		case code != "":
			var e struct {
				Error *struct{ Message, Type, Code string }
			}
			if err := json.Unmarshal(out, &e); err != nil || e.Error == nil || e.Error.Message == "" || e.Error.Code != code {
				t.Fatalf("an error response: %v\n%s", err, out)
			}
			if c.rec.Code < 400 || (status >= 400 && c.rec.Code != status) || len(c.onError) != 1 {
				t.Fatalf("status %d for upstream %d, OnError %v", c.rec.Code, status, c.onError)
			}
		case strings.HasPrefix(c.rec.Header().Get("Content-Type"), "text/event-stream"):
			s, err := chat.CheckStream(out)
			if err != nil || !s.Done || s.ErrMessage != "" || !stream || c.rec.Code != status || s.HasUsage != usage {
				t.Fatalf("a stream: %v, %+v\n%s", err, s, out)
			}
		default:
			var m completion
			if err := json.Unmarshal(out, &m); err != nil || m.Object != "chat.completion" || len(m.Choices) != 1 || m.Choices[0].FinishReason == "" || stream || c.rec.Code != status {
				t.Fatalf("a completion: %v\n%s", err, out)
			}
			for _, call := range m.Choices[0].Message.ToolCalls {
				if call.ID == "" || call.Function.Name == "" || ir.CheckObject([]byte(call.Function.Arguments)) != nil {
					t.Fatalf("a tool call a client cannot act on: %+v", call)
				}
			}
		}
		if (code != "") != (len(c.onError) == 1) {
			t.Fatalf("code %q, OnError %v", code, c.onError)
		}
	})
}

// FuzzResponsesMessagesWriter is the same for the pair responses-messages.
func FuzzResponsesMessagesWriter(f *testing.F) {
	f.Add(anthropicSSE(aStart, aText, aPar, aStop0, aTool(1, "t", "f"), aArgs(1, []byte(`{"a":1}`)), aStop(1), aToolEnd, aDone), 7, true, 200, "text/event-stream")
	f.Add(anthropicSSE(aStart, aTool(0, "a", "f"), aTool(1, "b", "g"), aArgs(1, []byte(`{}`)), aArgs(0, []byte(`{"k":`))), 3, true, 200, "text/event-stream")
	f.Add([]byte(`{"type":"message","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn"}`), 50, false, 200, "application/json")
	f.Add(anthropicSSE(aStart, aText, aPar, aError), 4, true, 200, "text/event-stream")
	f.Add([]byte(aError), 3, false, 429, "application/json")
	f.Fuzz(func(t *testing.T, body []byte, piece int, stream bool, status int, contentType string) {
		if status < 200 || status > 599 {
			status = 200
		}
		c := newTargetCall(t, Responses, stream, "").upstream(status, contentType, body, piece)
		code, mid := c.w.Failure()
		out := c.rec.Body.Bytes()
		switch {
		case mid:
			s, err := responses.CheckStream(out)
			if err != nil || s.Completed || s.Status != "failed" || code == "" || c.rec.Code != status {
				t.Fatalf("a failed stream: %v, %+v, code %q, status %d\n%s", err, s, code, c.rec.Code, out)
			}
		case code != "":
			var e struct {
				Error *struct{ Message, Type, Code string }
			}
			if err := json.Unmarshal(out, &e); err != nil || e.Error == nil || e.Error.Message == "" || e.Error.Code != code {
				t.Fatalf("an error response: %v\n%s", err, out)
			}
			if c.rec.Code < 400 || (status >= 400 && c.rec.Code != status) || len(c.onError) != 1 {
				t.Fatalf("status %d for upstream %d, OnError %v", c.rec.Code, status, c.onError)
			}
		case strings.HasPrefix(c.rec.Header().Get("Content-Type"), "text/event-stream"):
			s, err := responses.CheckStream(out)
			if err != nil || s.Status == "failed" || !stream || c.rec.Code != status {
				t.Fatalf("a stream: %v, %+v\n%s", err, s, out)
			}
		default:
			var m responseBody
			if err := json.Unmarshal(out, &m); err != nil || m.Object != "response" || stream || c.rec.Code != status {
				t.Fatalf("a response: %v\n%s", err, out)
			}
		}
		if (code != "") != (len(c.onError) == 1) {
			t.Fatalf("code %q, OnError %v", code, c.onError)
		}
	})
}

func TestChatMessages_Request_OneCallIDInEveryTurn(t *testing.T) {
	// A history from a provider that numbers its calls anew in every answer: five turns, each
	// with the call "call_0". Every replay of it must be a request Anthropic accepts.
	var msgs []string
	for turn := 0; turn < 5; turn++ {
		msgs = append(msgs, `{"role":"user","content":"go"}`,
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_0","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
			fmt.Sprintf(`{"role":"tool","tool_call_id":"call_0","content":"r%d"}`, turn))
	}
	body := `{"model":"m","max_tokens":5,"messages":[` + strings.Join(append(msgs, `{"role":"user","content":"and now?"}`), ",") + `]}`
	out, _, dropped, err := lookup(t, Chat, Messages).Request([]byte(body), nil, "claude-x")
	if err != nil || dropped != nil {
		t.Fatalf("%v, dropped %v", err, dropped)
	}
	if err := messages.CheckRequest(out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for turn, id := range []string{"call_0", "call_0_2", "call_0_3", "call_0_4", "call_0_5"} {
		want := fmt.Sprintf(`{"type":"tool_result","tool_use_id":%q,"content":"r%d"}`, id, turn)
		if !bytes.Contains(out, []byte(want)) || bytes.Count(out, []byte(`"id":"`+id+`"`)) != 1 {
			t.Fatalf("turn %d: no %s in\n%s", turn, want, out)
		}
	}
}
