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
	"strconv"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
)

// ---------------------------------------------------------------- helpers

func readFile(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	if !jsonEqual(got, []byte(want)) {
		t.Fatalf("JSON differs\n got: %s\nwant: %s", got, want)
	}
}

func messagesChat(t testing.TB) Pair {
	t.Helper()
	p, ok := Lookup(Messages, Chat)
	if !ok {
		t.Fatal("no released pair messages-chat")
	}
	return p
}

// call is one answer sent through the pair's response writer.
type call struct {
	rec     *httptest.ResponseRecorder
	w       ResponseWriter
	onError []string // "status code" per OnError call
}

func newCall(t testing.TB, stream bool) *call {
	c := &call{rec: httptest.NewRecorder()}
	c.w = messagesChat(t).Response(c.rec, ResponseOptions{Stream: stream, RequestedModel: "asked-for",
		OnError: func(status int, code string) { c.onError = append(c.onError, fmt.Sprint(status, " ", code)) }})
	return c
}

// upstream plays the provider: header, status, the body in pieces, Finish.
func (c *call) upstream(status int, contentType string, body []byte, piece int) *call {
	if contentType != "" {
		c.w.Header().Set("Content-Type", contentType)
	}
	c.w.WriteHeader(status)
	if piece < 1 {
		piece = max(len(body), 1)
	}
	for i := 0; i < len(body); i += piece {
		if _, err := c.w.Write(body[i:min(i+piece, len(body))]); err != nil {
			break
		}
	}
	c.w.Finish()
	return c
}

// anthropicError reads an error response in the Messages shape.
func (c *call) anthropicError(t *testing.T) (kind, message string) {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		BurrowCode string `json:"burrow_code"`
	}
	body := c.rec.Body.Bytes()
	if err := json.Unmarshal(body, &e); err != nil || e.Type != "error" || e.Error.Type == "" {
		t.Fatalf("not an Anthropic error body (%v): %s", err, body)
	}
	// The gateway's own Anthropic errors carry burrow_code; the writer's do too.
	if code, _ := c.w.Failure(); e.BurrowCode == "" || e.BurrowCode != code {
		t.Fatalf("burrow_code = %q, Failure = %q: %s", e.BurrowCode, code, body)
	}
	if ct := c.rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cl := c.rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q for %d bytes", cl, len(body))
	}
	return e.Error.Type, e.Error.Message
}

func sseOf(frames ...string) []byte {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	return []byte(b.String())
}

func chunk(delta string) string {
	return `{"id":"c1","model":"m-up","choices":[{"index":0,"delta":` + delta + `,"finish_reason":null}]}`
}

func finishChunk(reason string) string {
	return `{"id":"c1","model":"m-up","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`
}

type sseFrame struct {
	event string
	data  any
}

func parseFrames(t *testing.T, raw []byte) []sseFrame {
	t.Helper()
	var out []sseFrame
	for _, block := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n\n") {
		eventLine, dataLine, _ := strings.Cut(block, "\n")
		f := sseFrame{event: strings.TrimPrefix(eventLine, "event: ")}
		if f.event == "ping" { // a sign of life on a slow machine, not content
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &f.data); err != nil {
			t.Fatalf("frame %q: %v", block, err)
		}
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------- registry

func TestLookupAndFormats(t *testing.T) {
	p, ok := Lookup(Messages, Chat)
	if !ok || p.ID() != "messages-chat" || p.UpstreamPath() != "/v1/chat/completions" || !p.Released() {
		t.Fatalf("Lookup(Messages, Chat) = %v, %v", p, ok)
	}
	for _, pair := range [][2]Format{{Chat, Chat}, {Messages, Messages}, {"x", Chat}, {Messages, "x"}, {"", ""}} {
		if p, ok := Lookup(pair[0], pair[1]); ok || p != nil {
			t.Errorf("Lookup(%q, %q) found a pair", pair[0], pair[1])
		}
	}
	for _, c := range []struct {
		dialect, path string
		want          Format
		ok            bool
	}{
		{"anthropic", "/v1/messages", Messages, true},
		{"openai", "/v1/chat/completions", Chat, true},
		{"openai", "/v1/responses", Responses, true},
		{"anthropic", "/v1/messages/count_tokens", "", false},
		{"openai", "/v1/embeddings", "", false},
		{"openai", "/v1/completions", "", false},
		{"openai", "/v1/models", "", false},
		{"anthropic", "/v1/models", "", false},
		{"openai", "/v1/messages", "", false},
		{"anthropic", "/v1/chat/completions", "", false},
		{"", "/v1/messages", "", false},
	} {
		if got, ok := CallerFormat(c.dialect, c.path); got != c.want || ok != c.ok {
			t.Errorf("CallerFormat(%q, %q) = %q, %v", c.dialect, c.path, got, ok)
		}
	}
	if TargetFormat("openai") != Chat || TargetFormat("anthropic") != Messages || TargetFormat("other") != "" {
		t.Fatal("TargetFormat")
	}
}

// ---------------------------------------------------------------- released

func TestReleased_IsDecidedByTheToolCallChecks(t *testing.T) {
	// The checks the registered pair is released by pass.
	registered := pairs[[2]Format{Messages, Chat}]
	if err := registered.check(registered); err != nil {
		t.Fatalf("tool-call checks of messages-chat: %v", err)
	}
	// A pair whose tool-call path is broken is not released, whatever broke, and Lookup does not
	// hand it out.
	broken := map[string]func(p *pair){
		"the stream encoder loses argument pieces": func(p *pair) {
			p.codec.newStreamEncoder = func(w io.Writer, model string) streamEncoder {
				return dropArgs{messages.NewStreamEncoder(w, model)}
			}
		},
		"the stream decoder reorders nothing but stops early": func(p *pair) {
			p.codec.newStreamDecoder = func() streamDecoder { return &stopsEarly{d: chat.NewStreamDecoder()} }
		},
		"the buffered answer loses its tool calls": func(p *pair) {
			p.codec.encodeResponse = func(r ir.Response, model string) ([]byte, error) {
				r.Parts = nil
				return messages.EncodeResponse(r, model)
			}
		},
		"the request loses the tool results": func(p *pair) {
			p.request = func(body []byte, h http.Header, model string) ([]byte, bool, []string, error) {
				out, stream, dropped, err := messagesToChat(body, h, model)
				return bytes.ReplaceAll(out, []byte(`"role":"tool"`), []byte(`"role":"user"`)), stream, dropped, err
			}
		},
		"the request fails": func(p *pair) {
			p.request = func([]byte, http.Header, string) ([]byte, bool, []string, error) {
				return nil, false, nil, errors.New("no")
			}
		},
	}
	for name, breakIt := range broken {
		p := newMessagesChat()
		breakIt(p)
		if p.Released() {
			t.Errorf("%s: the pair is released", name)
		}
		if err := p.check(p); err == nil {
			t.Errorf("%s: the checks pass", name)
		}
	}
	p := newMessagesChat()
	p.check = func(*pair) error { return errors.New("not yet") }
	pairs[[2]Format{"test-from", "test-to"}] = p
	defer delete(pairs, [2]Format{"test-from", "test-to"})
	if got, ok := Lookup("test-from", "test-to"); ok || got != nil {
		t.Fatal("Lookup handed out a pair that is not released")
	}
	if fresh := newMessagesChat(); !fresh.Released() || !fresh.Released() {
		t.Fatal("a sound pair is not released")
	}
}

type dropArgs struct{ streamEncoder }

func (d dropArgs) Write(ev ir.Event) error {
	if ev.Kind == ir.ToolArgsDelta && len(ev.ArgsJSON) > 2 {
		ev.ArgsJSON = ev.ArgsJSON[:len(ev.ArgsJSON)-1]
	}
	return d.streamEncoder.Write(ev)
}

type stopsEarly struct {
	d    streamDecoder
	seen int
}

func (s *stopsEarly) Feed(data []byte) ([]ir.Event, error) {
	if s.seen++; s.seen > 3 {
		return nil, nil
	}
	return s.d.Feed(data)
}
func (s *stopsEarly) Close() []ir.Event { return s.d.Close() }

// ---------------------------------------------------------------- request

func TestRequest_ClaudeCodeTurn(t *testing.T) {
	body := readFile(t, "messages/testdata/req_claude_code.json")
	out, stream, dropped, err := messagesChat(t).Request(body, nil, "gpt-x")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, out, string(readFile(t, "testdata/messages_chat_request.json")))
	if !stream {
		t.Fatal("stream = false")
	}
	want := []string{"cache_control", "metadata", "thinking", "thinking.signature", "tool:web_search_20250305", "top_k"}
	if !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped = %v, want %v", dropped, want)
	}
	// The anthropic-beta header is not sent on; it is reported.
	h := http.Header{}
	h.Add("anthropic-beta", "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14")
	_, _, dropped, err = messagesChat(t).Request(body, h, "gpt-x")
	if err != nil || !reflect.DeepEqual(dropped, append([]string{"anthropic-beta"}, want...)) {
		t.Fatalf("with the header: %v, dropped = %v", err, dropped)
	}
	// Nothing dropped is nil, and a request that does not stream says so.
	out, stream, dropped, err = messagesChat(t).Request([]byte(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`), http.Header{}, "gpt-x")
	if err != nil || stream || dropped != nil || string(out) != `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"max_tokens":5}` {
		t.Fatalf("%s, %v, %v, %v", out, stream, dropped, err)
	}
}

func TestRequest_ImagesInToolResultsDoNotEndASession(t *testing.T) {
	// Claude Code's Read tool returns images inside tool_result blocks, and the history is sent
	// again with every request: the images follow the tool messages as one user message.
	const png = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUFB"}}`
	out, _, dropped, err := messagesChat(t).Request([]byte(`{"model":"m","max_tokens":5,"messages":[
	 {"role":"user","content":"compare a.png and b.png"},
	 {"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Read","input":{"file_path":"a.png"}},{"type":"tool_use","id":"b","name":"Read","input":{"file_path":"b.png"}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[`+png+`]},
	  {"type":"tool_result","tool_use_id":"b","content":[{"type":"text","text":"b.png"},{"type":"image","source":{"type":"url","url":"https://example.org/b.png"}}]}]}]}`), nil, "gpt-x")
	if err != nil || dropped != nil {
		t.Fatalf("%v, dropped %v", err, dropped)
	}
	assertJSONEqual(t, out, `{"model":"gpt-x","max_tokens":5,"messages":[
	 {"role":"user","content":"compare a.png and b.png"},
	 {"role":"assistant","content":null,"tool_calls":[
	  {"id":"a","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"a.png\"}"}},
	  {"id":"b","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"b.png\"}"}}]},
	 {"role":"tool","tool_call_id":"a","content":"[image]"},
	 {"role":"tool","tool_call_id":"b","content":"b.png"},
	 {"role":"user","content":[
	  {"type":"text","text":"Image returned by tool call a:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUFB"}},
	  {"type":"text","text":"Image returned by tool call b:"},{"type":"image_url","image_url":{"url":"https://example.org/b.png"}}]}]}`)
	if err := chat.CheckRequest(out); err != nil {
		t.Fatal(err)
	}
}

func TestResponse_TheHistoryOfAnyAnswerIsTaken(t *testing.T) {
	// Random Chat streams — text, then interleaved calls — whole and cut at a random byte: what the
	// caller got goes back as an assistant turn with a tool_result for every tool_use, and is a
	// conversation a strict Chat Completions server takes.
	rng := rand.New(rand.NewSource(11))
	args := []string{`{"path":"a.txt"}`, `{ "b":1.0, "a":[1, "<&>"] }`, `{}`, `{"s":"` + strings.Repeat("xy", 40) + `"}`}
	quote := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	for round := 0; round < 400; round++ {
		n := 1 + rng.Intn(len(args))
		upstream := append(sseOf(chunk(`{"role":"assistant","content":"Let me look."}`)), chatToolStream(rng, args[:n])...)
		if round%4 != 0 {
			upstream = upstream[:rng.Intn(len(upstream))]
		}
		c := newCall(t, true).upstream(200, "text/event-stream", upstream, 1+rng.Intn(60))
		if !strings.HasPrefix(c.rec.Header().Get("Content-Type"), "text/event-stream") {
			continue // cut before the first event: an HTTP error, and nothing to send back
		}
		s, err := messages.CheckStream(c.rec.Body.Bytes())
		if err != nil {
			t.Fatalf("round %d: %v\n%s", round, err, c.rec.Body)
		}
		var blocks, results []string
		for _, b := range s.Blocks {
			if b.Type != "tool_use" {
				blocks = append(blocks, `{"type":"text","text":`+quote(b.Text)+`}`)
				continue
			}
			input := b.Input // a client that cannot parse the partial JSON of a cut call has no input
			if ir.CheckObject([]byte(input)) != nil {
				input = "{}"
			}
			blocks = append(blocks, `{"type":"tool_use","id":`+quote(b.ToolID)+`,"name":`+quote(b.ToolName)+`,"input":`+input+`}`)
			results = append(results, `{"type":"tool_result","tool_use_id":`+quote(b.ToolID)+`,"content":"ok"}`)
		}
		body := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[` + strings.Join(blocks, ",") +
			`]},{"role":"user","content":[` + strings.Join(append(results, `{"type":"text","text":"go on"}`), ",") + `]}]}`
		out, _, dropped, err := messagesChat(t).Request([]byte(body), nil, "gpt-x")
		if err != nil || dropped != nil {
			t.Fatalf("round %d: %v, dropped %v\n%s", round, err, dropped, body)
		}
		if err := chat.CheckRequest(out); err != nil {
			t.Fatalf("round %d: %v\n%s", round, err, out)
		}
	}
}

func TestRequest_ClientErrors(t *testing.T) {
	for body, field := range map[string]string{
		`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"SECRET"}}]}]}`: "messages[0].content[0].type",
		`{"model":"m","max_tokens":5,"messages":[]}`: "messages",
		`not json SECRET`: "body",
		// Refused by the target's encoder: two tool calls with one id.
		`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"SECRET"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f"},{"type":"tool_use","id":"t","name":"f"}]}]}`: "messages[1].tool_use.id",
	} {
		out, _, _, err := messagesChat(t).Request([]byte(body), nil, "gpt-x")
		msg, ok := BadRequest(err)
		if err == nil || out != nil || !ok || !strings.Contains(msg, field) || strings.Contains(msg, "SECRET") || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%.60s: out %s, err %v, BadRequest = %q, %v", body, out, err, msg, ok)
		}
	}
	if msg, ok := BadRequest(errors.New("something else")); ok || msg != "" {
		t.Fatal("BadRequest took an error that is none")
	}
	if msg, ok := BadRequest(nil); ok || msg != "" {
		t.Fatal("BadRequest took nil")
	}
}

// ---------------------------------------------------------------- buffered answers

func TestResponse_Buffered(t *testing.T) {
	body := readFile(t, "chat/testdata/resp_tools.json")
	c := newCall(t, false)
	c.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	c.w.Header().Set("Content-Encoding", "identity")
	c.w.Header().Set("X-Request-Id", "req-7")
	c.upstream(200, "application/json; charset=utf-8", body, 100)
	if c.rec.Code != 200 || c.rec.Header().Get("Content-Type") != "application/json" || c.rec.Header().Get("X-Request-Id") != "req-7" || c.rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("status %d, header %v", c.rec.Code, c.rec.Header())
	}
	if cl := c.rec.Header().Get("Content-Length"); cl != strconv.Itoa(c.rec.Body.Len()) || c.rec.Body.Len() == len(body) {
		t.Fatalf("Content-Length = %q for %d bytes (upstream %d)", cl, c.rec.Body.Len(), len(body))
	}
	assertJSONEqual(t, c.rec.Body.Bytes(), `{"id":"msg_chatcmpl-2","type":"message","role":"assistant","model":"m-chat","content":[
		{"type":"tool_use","id":"call_a","name":"read_file","input":{"path":"a.txt"}},
		{"type":"tool_use","id":"call_b","name":"read_file","input":{"path":"b.txt"}}],
		"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":40,"output_tokens":18}}`)
	if code, mid := c.w.Failure(); code != "" || mid || c.onError != nil {
		t.Fatalf("Failure = %q, %v; OnError %v", code, mid, c.onError)
	}
	// Text, the stop reasons, the model that was asked for when the upstream names none.
	for finish, want := range map[string]string{"stop": "end_turn", "length": "max_tokens", "content_filter": "refusal", "weird": "end_turn"} {
		c := newCall(t, false).upstream(200, "application/json", []byte(`{"choices":[{"message":{"content":"Hi."},"finish_reason":"`+finish+`"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`), 0)
		assertJSONEqual(t, c.rec.Body.Bytes(), `{"id":"msg_burrow","type":"message","role":"assistant","model":"asked-for","content":[{"type":"text","text":"Hi."}],
			"stop_reason":"`+want+`","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":2}}`)
	}
	// A refusal text makes it a refusal.
	c = newCall(t, false).upstream(200, "application/json", []byte(`{"choices":[{"message":{"content":null,"refusal":"I cannot."},"finish_reason":"stop"}]}`), 0)
	if !strings.Contains(c.rec.Body.String(), `"stop_reason":"refusal"`) || !strings.Contains(c.rec.Body.String(), `"text":"I cannot."`) {
		t.Fatalf("refusal: %s", c.rec.Body)
	}
}

func TestResponse_Buffered_ArgumentBytesArriveUnchanged(t *testing.T) {
	args := `{ "b":1.0, "a":[12345678901234567890, "<&>", "\u00e9\ud83d\ude00"],  "n":null }`
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
		"tool_calls": []any{map[string]any{"id": "a", "function": map[string]any{"name": "f", "arguments": args}}}}}}})
	c := newCall(t, false).upstream(200, "application/json", body, 0)
	if !bytes.Contains(c.rec.Body.Bytes(), []byte(`"input":`+args+`}`)) {
		t.Fatalf("input changed: %s", c.rec.Body)
	}
}

// ---------------------------------------------------------------- streamed answers

func TestResponse_Streamed(t *testing.T) {
	upstream := readFile(t, "chat/testdata/stream_tools.sse")
	c := newCall(t, true)
	c.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	c.w.Header().Set("Content-Length", "99999")
	c.w.Header().Set("X-Request-Id", "req-8")
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
			if !c.rec.Flushed || !strings.HasPrefix(c.rec.Body.String(), "event: message_start\n") {
				t.Fatalf("the first frame was not flushed: %q", c.rec.Body)
			}
		}
	}
	c.w.Finish()
	if firstAt < 0 || firstAt > 200 {
		t.Fatalf("the first caller frame came after %d upstream bytes of %d", firstAt, len(upstream))
	}
	h := c.rec.Header()
	if c.rec.Code != 200 || h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" || h.Get("X-Request-Id") != "req-8" {
		t.Fatalf("status %d, header %v", c.rec.Code, h)
	}
	if _, has := h["Content-Length"]; has {
		t.Fatal("a Content-Length on a stream")
	}
	got, want := parseFrames(t, c.rec.Body.Bytes()), parseFrames(t, readFile(t, "testdata/messages_chat_stream_tools.sse"))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transcript\n got %v\nwant %v", got, want)
	}
	if _, err := messages.CheckStream(c.rec.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	if code, mid := c.w.Failure(); code != "" || mid || c.onError != nil {
		t.Fatalf("Failure = %q, %v; OnError %v", code, mid, c.onError)
	}
	// Text only, in pieces of every size.
	text := readFile(t, "chat/testdata/stream_text.sse")
	for _, piece := range []int{1, 2, 3, 5, 64, len(text)} {
		c := newCall(t, true).upstream(200, "text/event-stream", text, piece)
		s, err := messages.CheckStream(c.rec.Body.Bytes())
		if err != nil || len(s.Blocks) != 1 || s.Blocks[0].Text != "Hello." || s.StopReason != "end_turn" || s.InputTokens != 12 || s.OutputTokens != 2 || s.ID != "msg_chatcmpl-3" {
			t.Fatalf("piece %d: %v, %+v", piece, err, s)
		}
	}
}

// chatToolStream writes a Chat stream of several tool calls whose argument texts are cut into
// random pieces that are sent interleaved, as providers do for parallel calls.
func chatToolStream(rng *rand.Rand, args []string) []byte {
	var frames []string
	left := make([][]byte, len(args))
	for i, a := range args {
		left[i] = []byte(a)
		frames = append(frames, chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"call_%d","type":"function","function":{"name":"fn%d","arguments":""}}]}`, i, i, i)))
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
		n := 1 + rng.Intn(min(len(left[i]), 9)) // bytes, so characters are cut in two as well
		// The bytes as they are: json.Marshal would repair a character that is cut in two.
		raw := append(append([]byte{'"'}, escapeBytes(left[i][:n])...), '"')
		left[i] = left[i][n:]
		frames = append(frames, chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}`, i, raw)))
	}
	frames = append(frames, finishChunk("tool_calls"), "[DONE]")
	return sseOf(frames...)
}

// escapeBytes escapes what a JSON string must escape and leaves every other byte alone.
func escapeBytes(b []byte) []byte {
	var out []byte
	for _, c := range b {
		switch {
		case c == '"' || c == '\\':
			out = append(out, '\\', c)
		case c < 0x20:
			out = append(out, fmt.Sprintf(`\u%04x`, c)...)
		default:
			out = append(out, c)
		}
	}
	return out
}

func TestResponse_Streamed_ArgumentsReassembleToTheUpstreamsBytes(t *testing.T) {
	// Review Focus 1, through the whole pair: one call and several interleaved calls.
	rng := rand.New(rand.NewSource(7))
	tricky := []string{
		`{"path":"a.txt"}`,
		`{ "b":1.0, "a":[12345678901234567890, "<&>"],` + "\n\t" + ` "n":null }`,
		`{"text":"zażółć \"gęślą\" jaźń 😀 \\n \u2028","nested":{"deep":[[[{"k":"v"}]]]}}`,
		`{"s":"` + strings.Repeat("0123456789", 300) + `"}`,
		`{}`,
	}
	for round := 0; round < 60; round++ {
		n := 1 + round%len(tricky)
		args := append([]string(nil), tricky[:n]...)
		rng.Shuffle(n, func(i, j int) { args[i], args[j] = args[j], args[i] })
		upstream := chatToolStream(rng, args)
		c := newCall(t, true).upstream(200, "text/event-stream", upstream, 1+rng.Intn(40))
		s, err := messages.CheckStream(c.rec.Body.Bytes())
		if err != nil {
			t.Fatalf("round %d: %v\n%s", round, err, c.rec.Body)
		}
		if len(s.Blocks) != n || s.StopReason != "tool_use" || !s.Stopped || s.InputTokens != 9 || s.OutputTokens != 4 {
			t.Fatalf("round %d: %+v", round, s)
		}
		for i, b := range s.Blocks {
			if b.ToolID != fmt.Sprint("call_", i) || b.ToolName != fmt.Sprint("fn", i) || b.PartialJSON != args[i] || b.Input != args[i] {
				t.Fatalf("round %d call %d:\n got %q\nwant %q", round, i, b.PartialJSON, args[i])
			}
			if !json.Valid([]byte(b.PartialJSON)) {
				t.Fatalf("round %d call %d: not JSON", round, i)
			}
		}
	}
	// A call without arguments: "input":{} in the start and one partial_json "{}".
	for _, arguments := range []string{`""`, `null`} {
		c := newCall(t, true).upstream(200, "text/event-stream", sseOf(
			chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":`+arguments+`}}]}`), finishChunk("tool_calls"), "[DONE]"), 0)
		s, err := messages.CheckStream(c.rec.Body.Bytes())
		if err != nil || len(s.Blocks) != 1 || !reflect.DeepEqual(s.Blocks[0].Deltas, []string{"{}"}) || s.Blocks[0].Input != "{}" {
			t.Fatalf("no arguments (%s): %v, %+v", arguments, err, s.Blocks)
		}
	}
}

func TestResponse_Streamed_BadEndings(t *testing.T) {
	// Review Focus 2. After the first byte the caller's stream ends with every block stopped
	// and one error event; there is no message_delta and no message_stop.
	start := chunk(`{"role":"assistant","content":"par"}`)
	tool := chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{\"k\":"}}]}`)
	for name, c := range map[string]struct {
		upstream []byte
		code     string
		message  string
	}{
		"the upstream closes early":              {sseOf(start), CodeUpstreamInvalid, "the provider ended the stream early"},
		"the upstream closes inside a tool call": {sseOf(start, tool), CodeUpstreamInvalid, "the provider ended the stream early"},
		"[DONE] without a finish_reason":         {sseOf(start, "[DONE]"), CodeUpstreamError, "the provider ended the stream early"},
		"an error object mid-stream":             {sseOf(start, tool, `{"error":{"message":"overloaded, try later","type":"server_error"}}`, chunk(`{"content":"more"}`)), CodeUpstreamError, "overloaded, try later"},
		"a malformed frame":                      {sseOf(start, `{"choices":[{"delta":`, chunk(`{"content":"more"}`), finishChunk("stop"), "[DONE]"), CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"arguments that are no object":           {sseOf(start, tool, finishChunk("tool_calls"), "[DONE]"), CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"a limit is exceeded":                    {sseOf(start, tool, chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"`+strings.Repeat("x", ir.MaxToolArgsBytes)+`\"}"}}]}`), finishChunk("tool_calls"), "[DONE]"), CodeUpstreamInvalid, "the provider's answer is too large"},
		"a frame over the frame limit":           {append(sseOf(start), []byte("data: "+strings.Repeat("y", chat.MaxFrameBytes+10)+"\n\n")...), CodeUpstreamInvalid, "the provider's answer is too large"},
	} {
		call := newCall(t, true).upstream(200, "text/event-stream", c.upstream, 4096)
		raw := call.rec.Body.Bytes()
		s, err := messages.CheckStream(raw)
		if err != nil {
			t.Errorf("%s: %v\n%.600s", name, err, raw)
			continue
		}
		if call.rec.Code != 200 || s.Stopped || s.StopReason != "" || s.ErrType != "api_error" || s.ErrMessage != c.message {
			t.Errorf("%s: status %d, stream %+v", name, call.rec.Code, s)
		}
		if strings.Contains(string(raw), "message_delta") || strings.Contains(string(raw), "message_stop") || strings.Contains(string(raw), "more") {
			t.Errorf("%s: the stream pretends to go on\n%.600s", name, raw)
		}
		if !bytes.HasSuffix(raw, []byte("event: error\ndata: "+`{"type":"error","error":{"type":"api_error","message":"`+c.message+`"}}`+"\n\n")) {
			t.Errorf("%s: the error event is not the last thing written\n%.600s", name, raw[max(0, len(raw)-300):])
		}
		if code, mid := call.w.Failure(); code != c.code || !mid {
			t.Errorf("%s: Failure = %q, %v", name, code, mid)
		}
		if want := []string{"200 " + c.code}; !reflect.DeepEqual(call.onError, want) {
			t.Errorf("%s: OnError %v, want %v", name, call.onError, want)
		}
		if len(s.Blocks) == 0 || s.Blocks[0].Text != "par" {
			t.Errorf("%s: what had arrived is gone: %+v", name, s.Blocks)
		}
	}
}

func TestResponse_Streamed_FailureBeforeTheFirstByteIsAnHTTPError(t *testing.T) {
	// Nothing was sent yet, so the caller gets an error RESPONSE in the Anthropic shape (and the
	// gateway a status it can act on) instead of a stream that begins with an error.
	for name, c := range map[string]struct {
		upstream []byte
		code     string
		message  string
	}{
		"nothing at all":                     {nil, CodeUpstreamInvalid, "the provider ended the stream early"},
		"only comments":                      {[]byte(": ping\n\n: ping\n\n"), CodeUpstreamInvalid, "the provider ended the stream early"},
		"an error object as first frame":     {sseOf(`{"error":{"message":"model is loading"}}`), CodeUpstreamError, "model is loading"},
		"garbage as first frame":             {sseOf(`{not json`), CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"[DONE] alone":                       {sseOf("[DONE]"), CodeUpstreamError, "the provider ended the stream early"},
		"a JSON error body sent as a stream": {[]byte(`{"error":{"message":"SECRET"}}`), CodeUpstreamInvalid, "the provider ended the stream early"},
	} {
		call := newCall(t, true)
		call.w.Header().Set("X-Request-Id", "req-9")
		call.upstream(200, "text/event-stream", c.upstream, 5)
		kind, message := call.anthropicError(t)
		if call.rec.Code != 502 || kind != "api_error" || message != c.message || call.rec.Header().Get("X-Request-Id") != "req-9" {
			t.Errorf("%s: status %d, %s: %q", name, call.rec.Code, kind, message)
		}
		if code, mid := call.w.Failure(); code != c.code || mid {
			t.Errorf("%s: Failure = %q, %v", name, code, mid)
		}
		if want := []string{"502 " + c.code}; !reflect.DeepEqual(call.onError, want) {
			t.Errorf("%s: OnError %v, want %v", name, call.onError, want)
		}
	}
}

// ---------------------------------------------------------------- upstream errors

func TestResponse_UpstreamErrorsKeepTheirStatus(t *testing.T) {
	html := "<html><body>Bad Gateway SECRET</body></html>"
	for _, stream := range []bool{false, true} {
		for _, c := range []struct {
			status            int
			contentType, body string
			wantType, wantMsg string
		}{
			{429, "application/json", `{"error":{"message":"Rate limit reached for requests","type":"requests","code":"rate_limit_exceeded"}}`, "rate_limit_error", "Rate limit reached for requests"},
			{400, "application/json", `{"error":{"message":"max_tokens is too large","type":"invalid_request_error"}}`, "invalid_request_error", "max_tokens is too large"},
			{401, "application/json", `{"error":{"message":"Incorrect API key provided"}}`, "authentication_error", "Incorrect API key provided"},
			{403, "application/json", `{"error":"forbidden"}`, "permission_error", "forbidden"},
			{404, "application/json", `{"message":"no such model"}`, "not_found_error", "no such model"},
			{413, "text/plain", `too big`, "request_too_large", "the provider answered 413"},
			{500, "application/json", `{"error":{"code":500}}`, "api_error", "the provider answered 500"},
			{502, "text/html", html, "api_error", "the provider answered 502"},
			{503, "", ``, "overloaded_error", "the provider answered 503"},
			{529, "text/event-stream", "data: {\"error\":{\"message\":\"in a frame\"}}\n\n", "overloaded_error", "the provider answered 529"},
		} {
			call := newCall(t, stream)
			call.w.Header().Set("Retry-After", "7")
			call.w.Header().Set("Content-Length", strconv.Itoa(len(c.body)))
			call.upstream(c.status, c.contentType, []byte(c.body), 9)
			kind, message := call.anthropicError(t)
			if call.rec.Code != c.status || kind != c.wantType || message != c.wantMsg {
				t.Errorf("%d (stream %v): status %d, %s: %q", c.status, stream, call.rec.Code, kind, message)
			}
			if call.rec.Header().Get("Retry-After") != "7" {
				t.Errorf("%d: Retry-After was not passed on", c.status)
			}
			if code, mid := call.w.Failure(); code != CodeUpstreamError || mid {
				t.Errorf("%d: Failure = %q, %v", c.status, code, mid)
			}
			if want := []string{fmt.Sprint(c.status, " ", CodeUpstreamError)}; !reflect.DeepEqual(call.onError, want) {
				t.Errorf("%d: OnError %v", c.status, call.onError)
			}
		}
	}
	// A long message is cut, a long body is not held.
	long := newCall(t, false).upstream(500, "application/json", []byte(`{"error":{"message":"`+strings.Repeat("m", 5000)+`"}}`), 0)
	if _, message := long.anthropicError(t); len(message) != 300 {
		t.Fatalf("message of %d bytes", len(message))
	}
	huge := newCall(t, false)
	huge.w.WriteHeader(500)
	block := bytes.Repeat([]byte("z"), 1<<20)
	for i := 0; i < 8; i++ {
		_, _ = huge.w.Write(block)
	}
	if held := cap(huge.w.(*writer).buf); held > 2*maxErrorBody {
		t.Fatalf("%d bytes of an error body are held", held)
	}
	huge.w.Finish()
	if _, message := huge.anthropicError(t); message != "the provider answered 500" {
		t.Fatalf("message = %q", message)
	}
}

func TestResponse_AnswersThatAreNoAnswer(t *testing.T) {
	for name, c := range map[string]struct {
		stream      bool
		contentType string
		body        string
		code        string
		message     string
	}{
		"not a chat completion":                        {false, "application/json", `{"hello":"SECRET"}`, CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"not JSON":                                     {false, "text/html", `<html>SECRET</html>`, CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"empty":                                        {false, "application/json", ``, CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"an error object with status 200":              {false, "application/json", `{"error":{"message":"quota exceeded"}}`, CodeUpstreamError, "quota exceeded"},
		"a tool call without a name":                   {false, "application/json", `{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"arguments":"{}"}}]}}]}`, CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"arguments that are no object":                 {false, "application/json", `{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"[1]"}}]}}]}`, CodeUpstreamInvalid, "the provider sent an answer that cannot be read"},
		"a stream where none was asked for":            {false, "text/event-stream", string(sseOf(chunk(`{"content":"x"}`), finishChunk("stop"), "[DONE]")), CodeUpstreamInvalid, "the provider did not answer in the form that was asked for"},
		"a body where a stream was asked for":          {true, "application/json", `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`, CodeUpstreamInvalid, "the provider did not answer in the form that was asked for"},
		"an error object where a stream was asked for": {true, "application/json", `{"error":{"message":"quota exceeded"}}`, CodeUpstreamError, "quota exceeded"},
	} {
		call := newCall(t, c.stream).upstream(200, c.contentType, []byte(c.body), 0)
		kind, message := call.anthropicError(t)
		if call.rec.Code != 502 || kind != "api_error" || message != c.message {
			t.Errorf("%s: status %d, %s: %q", name, call.rec.Code, kind, message)
		}
		if code, mid := call.w.Failure(); code != c.code || mid {
			t.Errorf("%s: Failure = %q, %v", name, code, mid)
		}
		if want := []string{"502 " + c.code}; !reflect.DeepEqual(call.onError, want) {
			t.Errorf("%s: OnError %v", name, call.onError)
		}
	}
	// A compressed answer cannot be read: refused instead of guessed at.
	call := newCall(t, false)
	call.w.Header().Set("Content-Encoding", "gzip")
	call.upstream(200, "application/json", []byte("\x1f\x8b\x08\x00"), 0)
	if _, message := call.anthropicError(t); call.rec.Code != 502 || message != "the provider sent an answer that cannot be read" || call.rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("gzip: status %d, %q, header %v", call.rec.Code, message, call.rec.Header())
	}
	// The upstream handler returned without answering.
	call = newCall(t, true)
	call.w.Finish()
	if _, message := call.anthropicError(t); call.rec.Code != 502 || message != "the provider did not answer" {
		t.Fatalf("no answer: status %d, %q", call.rec.Code, message)
	}
}

func TestResponse_OnlyAllowedUpstreamHeadersPass(t *testing.T) {
	blocked := map[string]string{
		"Etag": `"abc"`, "Content-Range": "bytes 0-1/2", "Content-Md5": "Q2hlY2s=", "Trailer": "X-Sum", "Set-Cookie": "sid=SECRET",
		"Connection": "keep-alive", "Keep-Alive": "timeout=5", "Location": "https://elsewhere.test/", "Content-Length": "5",
		"Content-Encoding": "identity", "Content-Language": "de", "Transfer-Encoding": "chunked", "Server": "nginx", "Via": "1.1 x",
		"Openai-Organization": "org-SECRET", "X-Custom": "1", "Cache-Control": "public, max-age=60", "Last-Modified": "now", "Vary": "Origin",
	}
	allowed := map[string]string{
		"Retry-After": "7", "X-Request-Id": "req-1", "Request-Id": "req-2", "Openai-Request-Id": "req-3", "X-Amzn-Requestid": "req-4",
		"X-Ratelimit-Remaining-Requests": "9", "x-ratelimit-reset-tokens": "1s", "Anthropic-Ratelimit-Requests-Remaining": "8",
	}
	play := map[string]func() *call{
		"a buffered answer": func() *call { return newCall(t, false) },
		"a stream":          func() *call { return newCall(t, true) },
		"an error":          func() *call { return newCall(t, false) },
		"a redirect":        func() *call { return newCall(t, false) },
	}
	for name, make := range play {
		c := make()
		for k, v := range blocked {
			c.w.Header().Set(k, v)
		}
		for k, v := range allowed {
			c.w.Header()[k] = []string{v} // as written, also in lower case
		}
		switch name {
		case "a buffered answer":
			c.upstream(200, "application/json", readFile(t, "chat/testdata/resp_text.json"), 0)
		case "a stream":
			c.upstream(200, "text/event-stream", readFile(t, "chat/testdata/stream_text.sse"), 0)
		case "an error":
			c.upstream(429, "application/json", []byte(`{"error":{"message":"slow"}}`), 0)
		case "a redirect":
			c.upstream(302, "text/html", nil, 0)
		}
		got := c.rec.Header()
		for k, v := range allowed {
			if vs := got[k]; len(vs) != 1 || vs[0] != v {
				t.Errorf("%s: %s was not passed on: %v", name, k, got)
			}
		}
		for k := range blocked {
			switch k {
			case "Content-Length":
				if name != "a stream" && got.Get(k) == strconv.Itoa(c.rec.Body.Len()) {
					continue // the writer's own
				}
			case "Cache-Control":
				if name == "a stream" && got.Get(k) == "no-cache" {
					continue // the writer's own
				}
			}
			if _, has := got[k]; has {
				t.Errorf("%s: the upstream's %s reached the caller: %q", name, k, got[k])
			}
		}
		if len(got) != len(allowed)+2 { // + Content-Type and Content-Length or Cache-Control
			t.Errorf("%s: header = %v", name, got)
		}
	}
}

// flushCounter is a caller's writer that tells a flush with nothing new apart.
type flushCounter struct {
	*httptest.ResponseRecorder
	flushes, empty, lastLen int
}

func (f *flushCounter) Flush() {
	f.flushes++
	if f.Body.Len() == f.lastLen {
		f.empty++
	}
	f.lastLen = f.Body.Len()
}

func TestResponse_Streamed_NoFlushWithoutBytes(t *testing.T) {
	// A second tool call is held back while the first is open: the upstream writes that add
	// nothing to the caller's stream must not flush it.
	under := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	w := messagesChat(t).Response(under, ResponseOptions{Stream: true})
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	w.(http.Flusher).Flush() // the proxy's own flush before anything was written
	frames := []string{
		chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}}]}`),
		chunk(`{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":"{\"k\":\""}}]}`),
	}
	for i := 0; i < 50; i++ {
		frames = append(frames, chunk(`{"tool_calls":[{"index":1,"function":{"arguments":"`+strings.Repeat("v", 1000)+`"}}]}`))
	}
	frames = append(frames, chunk(`{"tool_calls":[{"index":1,"function":{"arguments":"\"}"}}]}`), finishChunk("tool_calls"), "[DONE]")
	for _, f := range frames {
		if _, err := w.Write(sseOf(f)); err != nil {
			t.Fatal(err)
		}
		w.(http.Flusher).Flush()
	}
	w.Finish()
	if under.empty != 0 || under.flushes == 0 || under.flushes > 12 {
		t.Fatalf("%d flushes, %d of them with nothing new", under.flushes, under.empty)
	}
	s, err := messages.CheckStream(under.Body.Bytes())
	if err != nil || len(s.Blocks) != 2 || len(s.Blocks[1].PartialJSON) != 50_000+8 || !s.Stopped {
		t.Fatalf("%v, %d blocks", err, len(s.Blocks))
	}
}

func TestResponse_Streamed_OverLimitFrameAfterTheFinishReason(t *testing.T) {
	// The model said it was done, but the answer did not end: what follows is over the frame
	// limit. That is the limit error, not a clean finish with usage 0/0.
	body := append(sseOf(chunk(`{"content":"par"}`), `{"id":"c1","model":"m-up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		[]byte("data: "+strings.Repeat("y", chat.MaxFrameBytes+10)+"\n\ndata: [DONE]\n\n")...)
	c := newCall(t, true).upstream(200, "text/event-stream", body, 8192)
	raw := c.rec.Body.Bytes()
	s, err := messages.CheckStream(raw)
	if err != nil || s.Stopped || s.StopReason != "" || s.ErrMessage != "the provider's answer is too large" {
		t.Fatalf("%v, %+v\n%s", err, s, raw)
	}
	if code, mid := c.w.Failure(); code != CodeUpstreamInvalid || !mid || !reflect.DeepEqual(c.onError, []string{"200 " + CodeUpstreamInvalid}) {
		t.Fatalf("Failure = %q, %v; OnError %v", code, mid, c.onError)
	}
	if !bytes.HasSuffix(raw, []byte(`"message":"the provider's answer is too large"}}`+"\n\n")) {
		t.Fatalf("something follows the error event: %s", raw)
	}
	// Once the finish has reached the caller, a late over-limit frame changes nothing.
	body = append(sseOf(chunk(`{"content":"par"}`), finishChunk("stop"), "[DONE]"), []byte("data: "+strings.Repeat("y", chat.MaxFrameBytes+10)+"\n\n")...)
	c = newCall(t, true).upstream(200, "text/event-stream", body, 8192)
	s, err = messages.CheckStream(c.rec.Body.Bytes())
	if code, _ := c.w.Failure(); err != nil || !s.Stopped || s.OutputTokens != 4 || code != "" {
		t.Fatalf("%v, %+v, %q", err, s, code)
	}
}

func TestResponse_BufferedBodyOverTheLimit(t *testing.T) {
	call := newCall(t, false)
	call.w.Header().Set("Content-Type", "application/json")
	call.w.WriteHeader(200)
	block := bytes.Repeat([]byte(" "), 1<<20)
	for i := 0; i < chat.MaxResponseBytes>>20+4; i++ {
		if n, err := call.w.Write(block); n != len(block) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if held := cap(call.w.(*writer).buf); held > chat.MaxResponseBytes*5/4+2<<20 {
			t.Fatalf("%d bytes are held", held)
		}
	}
	if held := cap(call.w.(*writer).buf); held != 0 {
		t.Fatalf("%d bytes are still held after the limit was passed", held)
	}
	call.w.Finish()
	if _, message := call.anthropicError(t); call.rec.Code != 502 || message != "the provider's answer is too large" {
		t.Fatalf("status %d, %q", call.rec.Code, message)
	}
	if code, _ := call.w.Failure(); code != CodeUpstreamInvalid {
		t.Fatalf("Failure = %q", code)
	}
}

// ---------------------------------------------------------------- the writer as a writer

func TestResponse_FinishAndLateWrites(t *testing.T) {
	c := newCall(t, false).upstream(200, "application/json", readFile(t, "chat/testdata/resp_text.json"), 0)
	before := c.rec.Body.String()
	c.w.Finish()
	c.w.WriteHeader(500)
	if n, err := c.w.Write([]byte("late")); n != 4 || err != nil {
		t.Fatalf("late Write = %d, %v", n, err)
	}
	c.w.Finish()
	if c.rec.Body.String() != before || c.rec.Code != 200 {
		t.Fatalf("something was written after Finish: %d %s", c.rec.Code, c.rec.Body)
	}
	// A Write without WriteHeader is a 200; a second WriteHeader does not count; 1xx is not an answer.
	c = newCall(t, false)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(103)
	_, _ = c.w.Write(readFile(t, "chat/testdata/resp_text.json"))
	c.w.WriteHeader(500)
	c.w.Finish()
	if c.rec.Code != 200 || !strings.Contains(c.rec.Body.String(), `"text":"Hello there."`) {
		t.Fatalf("status %d: %s", c.rec.Code, c.rec.Body)
	}
	// Nothing reaches the caller before Finish on the buffered paths; Flush is a no-op there.
	c = newCall(t, false)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(200)
	_, _ = c.w.Write([]byte(`{"choices":`))
	c.w.(http.Flusher).Flush()
	if c.rec.Flushed || c.rec.Body.Len() != 0 || len(c.rec.Header()) != 0 {
		t.Fatalf("the caller got something early: %v %q", c.rec.Header(), c.rec.Body)
	}
}

type clientGone struct {
	http.ResponseWriter
	writes int
}

func (c *clientGone) Write(p []byte) (int, error) {
	if c.writes++; c.writes > 2 {
		return 0, errors.New("broken pipe")
	}
	return c.ResponseWriter.Write(p)
}

func TestResponse_Streamed_ClientGoneStopsTheUpstream(t *testing.T) {
	rec := httptest.NewRecorder()
	w := messagesChat(t).Response(&clientGone{ResponseWriter: rec}, ResponseOptions{Stream: true})
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	var failed error
	for i := 0; i < 50 && failed == nil; i++ {
		_, failed = w.Write(sseOf(chunk(`{"content":"x"}`)))
	}
	if failed == nil {
		t.Fatal("Write kept succeeding after the client went away: the upstream would be read to its end")
	}
	before := rec.Body.Len()
	w.Finish()
	if rec.Body.Len() != before {
		t.Fatal("Finish wrote to a client that is gone")
	}
}

type noting struct {
	*httptest.ResponseRecorder
	timeouts int
}

func (n *noting) NoteUpstreamTimeout() { n.timeouts++ }

func TestResponse_ForwardsWhatTheGatewayNeeds(t *testing.T) {
	under := &noting{ResponseRecorder: httptest.NewRecorder()}
	w := messagesChat(t).Response(under, ResponseOptions{Stream: true})
	if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok || u.Unwrap() != http.ResponseWriter(under) {
		t.Fatal("Unwrap does not give the caller's writer")
	}
	w.(interface{ NoteUpstreamTimeout() }).NoteUpstreamTimeout()
	if under.timeouts != 1 {
		t.Fatal("NoteUpstreamTimeout was not passed on")
	}
	// A writer that takes no note does not break it.
	messagesChat(t).Response(httptest.NewRecorder(), ResponseOptions{}).(interface{ NoteUpstreamTimeout() }).NoteUpstreamTimeout()
	// A write that reached the caller flushes it; http.ResponseController finds the wrapper's
	// own Flush (and gets no "not supported"), which has nothing new to flush then.
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	_, _ = w.Write(sseOf(chunk(`{"content":"x"}`)))
	if !under.Flushed {
		t.Fatal("a write that reached the caller was not flushed")
	}
	under.Flushed = false
	if err := http.NewResponseController(w).Flush(); err != nil || under.Flushed {
		t.Fatalf("Flush through the controller: %v, flushed again %v", err, under.Flushed)
	}
}

func TestResponse_NothingIsLogged(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() { log.SetOutput(os.Stderr); slog.SetDefault(old) }()

	_, _, _, err := messagesChat(t).Request([]byte(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"document","data":"SECRET"}]}]}`), nil, "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
	newCall(t, false).upstream(429, "application/json", []byte(`{"error":{"message":"SECRET key sk-live-123"}}`), 0)
	newCall(t, false).upstream(200, "application/json", []byte(`{"SECRET":1}`), 0)
	newCall(t, true).upstream(200, "text/event-stream", sseOf(chunk(`{"content":"SECRET"}`), `{"error":{"message":"SECRET"}}`), 0)
	newCall(t, true).upstream(200, "text/event-stream", sseOf(chunk(`{"content":"SECRET"}`), `{SECRET`), 0)
	if logged.Len() != 0 {
		t.Fatalf("something was logged: %s", logged.String())
	}
}

// ---------------------------------------------------------------- fuzz

// FuzzResponseWriter plays an arbitrary upstream answer through the pair. Oracle: the caller
// gets exactly one of an Anthropic message, an Anthropic error response, or a well-formed
// Anthropic stream that ended — and what the writer reports about it is true.
func FuzzResponseWriter(f *testing.F) {
	f.Add(readFile(f, "chat/testdata/stream_tools.sse"), 7, true, 200, "text/event-stream")
	f.Add(readFile(f, "chat/testdata/stream_text.sse"), 1, true, 200, "text/event-stream")
	f.Add(readFile(f, "chat/testdata/resp_tools.json"), 50, false, 200, "application/json")
	f.Add([]byte(`{"error":{"message":"no"}}`), 3, false, 429, "application/json")
	f.Add(sseOf(chunk(`{"content":"x"}`), `{"error":"late"}`), 4, true, 200, "text/event-stream")
	f.Add(sseOf(`{"error":"first"}`), 4, true, 200, "text/event-stream")
	f.Add([]byte("<html>"), 2, true, 502, "text/html")
	f.Fuzz(func(t *testing.T, body []byte, piece int, stream bool, status int, contentType string) {
		if status < 200 || status > 599 {
			status = 200
		}
		c := newCall(t, stream).upstream(status, contentType, body, piece)
		code, mid := c.w.Failure()
		out := c.rec.Body.Bytes()
		switch {
		case mid:
			s, err := messages.CheckStream(out)
			if err != nil || s.Stopped || s.ErrType == "" || code == "" || c.rec.Code != status {
				t.Fatalf("a failed stream: %v, %+v, code %q, status %d\n%s", err, s, code, c.rec.Code, out)
			}
		case code != "":
			var e struct {
				Type  string
				Error struct{ Type, Message string }
			}
			if err := json.Unmarshal(out, &e); err != nil || e.Type != "error" || e.Error.Type == "" || e.Error.Message == "" {
				t.Fatalf("an error response: %v\n%s", err, out)
			}
			if c.rec.Code < 400 || (status >= 400 && c.rec.Code != status) || len(c.onError) != 1 {
				t.Fatalf("status %d for upstream %d, OnError %v", c.rec.Code, status, c.onError)
			}
		case strings.HasPrefix(c.rec.Header().Get("Content-Type"), "text/event-stream"):
			s, err := messages.CheckStream(out)
			if err != nil || !s.Stopped || s.ErrType != "" || !stream || c.rec.Code != status {
				t.Fatalf("a stream: %v, %+v\n%s", err, s, out)
			}
		default:
			var m struct {
				Type, Role string
				Content    []json.RawMessage
				StopReason string `json:"stop_reason"`
			}
			if err := json.Unmarshal(out, &m); err != nil || m.Type != "message" || m.Role != "assistant" || m.StopReason == "" || stream || c.rec.Code != status {
				t.Fatalf("a message: %v\n%s", err, out)
			}
		}
		if (code != "") != (len(c.onError) == 1) {
			t.Fatalf("code %q, OnError %v", code, c.onError)
		}
	})
}
