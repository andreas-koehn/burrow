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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/responses"
)

// The pair responses-chat: Codex and the OpenAI SDKs' Responses API on a Chat Completions target.

func responsesChat(t testing.TB) Pair {
	t.Helper()
	p, ok := Lookup(Responses, Chat)
	if !ok {
		t.Fatal("no released pair responses-chat")
	}
	return p
}

func newResponsesCall(t testing.TB, stream bool) *call {
	c := &call{rec: httptest.NewRecorder()}
	c.w = responsesChat(t).Response(c.rec, ResponseOptions{Stream: stream, RequestedModel: "asked-for",
		OnError: func(status int, code string) { c.onError = append(c.onError, fmt.Sprint(status, " ", code)) }})
	return c
}

// openaiError reads an error response in the shape of the gateway's own OpenAI errors.
func (c *call) openaiError(t *testing.T) (message string) {
	t.Helper()
	var e struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	body := c.rec.Body.Bytes()
	if err := json.Unmarshal(body, &e); err != nil || e.Error == nil || e.Error.Type != "burrow_error" || e.Error.Message == "" {
		t.Fatalf("not an OpenAI error body (%v): %s", err, body)
	}
	if code, _ := c.w.Failure(); e.Error.Code == "" || e.Error.Code != code {
		t.Fatalf("code = %q, Failure = %q: %s", e.Error.Code, code, body)
	}
	if ct := c.rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cl := c.rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q for %d bytes", cl, len(body))
	}
	return e.Error.Message
}

var createdAt = regexp.MustCompile(`"created_at":\d+`)

// atFixtureTime sets every created_at to the one the fixtures have.
func atFixtureTime(raw []byte) []byte {
	return createdAt.ReplaceAll(raw, []byte(`"created_at":1700000000`))
}

func TestResponsesChat_Lookup(t *testing.T) {
	p, ok := Lookup(Responses, Chat)
	if !ok || p.ID() != "responses-chat" || p.UpstreamPath() != "/v1/chat/completions" || !p.Released() {
		t.Fatalf("Lookup(Responses, Chat) = %v, %v", p, ok)
	}
	for _, pair := range [][2]Format{{Responses, Responses}, {Chat, Responses}, {Messages, Responses}, {Responses, Messages}} {
		if p, ok := Lookup(pair[0], pair[1]); ok || p != nil {
			t.Errorf("Lookup(%q, %q) found a pair", pair[0], pair[1])
		}
	}
	// Only POST /v1/responses itself is a Responses call: what works on a stored response
	// (retrieve, cancel, input_items, compact, input_tokens) has no translation.
	for _, path := range []string{"/v1/responses/resp_1", "/v1/responses/resp_1/cancel", "/v1/responses/resp_1/input_items",
		"/v1/responses/compact", "/v1/responses/input_tokens", "/v1/responses/"} {
		if f, ok := CallerFormat("openai", path); ok || f != "" {
			t.Errorf("CallerFormat(openai, %q) = %q, %v", path, f, ok)
		}
	}
}

func TestResponsesChat_Request_CodexTurn(t *testing.T) {
	body := readFile(t, "responses/testdata/req_codex.json")
	// What Codex adds to its requests in headers changes nothing.
	h := http.Header{}
	h.Set("OpenAI-Beta", "responses=experimental")
	h.Set("Session_id", "019a0000-aaaa-7bbb-8ccc-000000000001")
	for _, header := range []http.Header{nil, h} {
		out, stream, dropped, err := responsesChat(t).Request(body, header, "gpt-x")
		if err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out, string(readFile(t, "testdata/responses_chat_request.json")))
		if !stream {
			t.Fatal("stream = false")
		}
		want := []string{"client_metadata", "include", "input.namespace", "input.phase", "input:reasoning", "parallel_tool_calls",
			"prompt_cache_key", "reasoning", "stream_options", "text.verbosity", "tool:custom", "tool:local_shell", "tool:web_search"}
		if !reflect.DeepEqual(dropped, want) {
			t.Fatalf("dropped = %v, want %v", dropped, want)
		}
	}
	// Nothing dropped is nil, and a request that does not stream says so.
	out, stream, dropped, err := responsesChat(t).Request([]byte(`{"model":"m","input":"hi","max_output_tokens":5,"store":false}`), http.Header{}, "gpt-x")
	if err != nil || stream || dropped != nil || string(out) != `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"max_tokens":5}` {
		t.Fatalf("%s, %v, %v, %v", out, stream, dropped, err)
	}
	// No Responses state is kept: said, not silent.
	_, _, dropped, err = responsesChat(t).Request([]byte(`{"model":"m","input":"hi","previous_response_id":"resp_1","store":true}`), nil, "gpt-x")
	if err != nil || !reflect.DeepEqual(dropped, []string{"previous_response_id", "store"}) {
		t.Fatalf("%v, %v", dropped, err)
	}
	// A tool choice that names a tool which was left out is left out with it.
	out, _, dropped, err = responsesChat(t).Request([]byte(`{"model":"m","input":"hi","tools":[{"type":"web_search"}],"tool_choice":"required"}`), nil, "gpt-x")
	if err != nil || !reflect.DeepEqual(dropped, []string{"tool:web_search", "tool_choice"}) || bytes.Contains(out, []byte("tool")) {
		t.Fatalf("%s, %v, %v", out, dropped, err)
	}
}

func TestResponsesChat_Request_ClientErrors(t *testing.T) {
	for body, field := range map[string]string{
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_data":"SECRET"}]}]}`: "input[0].content[0]",
		`{"model":"m"}`:   "input",
		`not json SECRET`: "body",
		`{"model":"m","input":[{"role":"user","content":"SECRET"},{"type":"function_call","call_id":"SECRET","arguments":"{}"}]}`: "input[1].name",
	} {
		out, _, _, err := responsesChat(t).Request([]byte(body), nil, "gpt-x")
		msg, ok := BadRequest(err)
		if err == nil || out != nil || !ok || !strings.Contains(msg, "responses: "+field+": ") || strings.Contains(msg, "SECRET") || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%.60s: out %s, err %v, BadRequest = %q, %v", body, out, err, msg, ok)
		}
	}
}

func TestResponsesChat_Request_AHistoryNeverEndsASession(t *testing.T) {
	// Codex sends its whole history with every request: a request that is refused for what an
	// earlier answer left in it is refused for ever. What a client can have recorded is repaired
	// and reported instead.
	const user = `{"role":"user","content":"go"}`
	for name, c := range map[string]struct {
		items   string
		dropped []string
		want    string // the Chat messages
	}{
		"a call whose arguments were cut": {
			`{"type":"function_call","call_id":"c","name":"f","arguments":"{\"a\":"},{"type":"function_call_output","call_id":"c","output":"failed to parse"}`,
			[]string{"input:function_call.arguments"},
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"failed to parse"}`},
		"a message between a call and its output": {
			`{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running."}]},{"type":"function_call_output","call_id":"c","output":"done"}`,
			nil,
			`{"role":"assistant","content":"Running.","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"done"}`},
		"a call without an output": {
			`{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"role":"user","content":"and?"}`,
			[]string{"input:function_call.unanswered"},
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"[no output]"},{"role":"user","content":"and?"}`},
		"an output without a call": {
			`{"type":"function_call_output","call_id":"gone","output":"x"},{"role":"user","content":"and?"}`,
			[]string{"input:function_call_output.orphan"},
			`{"role":"user","content":"and?"}`},
		"one call id twice": {
			`{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"x"}`,
			[]string{"input:function_call.duplicate"},
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"x"}`},
		"two tools that return images (Codex's view_image)": {
			`{"type":"function_call","call_id":"a","name":"view_image","arguments":"{\"path\":\"a.png\"}"},{"type":"function_call","call_id":"b","name":"view_image","arguments":"{\"path\":\"b.png\"}"},` +
				`{"type":"function_call_output","call_id":"a","output":[{"type":"input_image","image_url":"data:image/png;base64,QUFB"}]},` +
				`{"type":"function_call_output","call_id":"b","output":[{"type":"input_text","text":"b.png"},{"type":"input_image","image_url":"data:image/png;base64,QkJC"}]}`,
			nil,
			`{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"view_image","arguments":"{\"path\":\"a.png\"}"}},{"id":"b","type":"function","function":{"name":"view_image","arguments":"{\"path\":\"b.png\"}"}}]},` +
				`{"role":"tool","tool_call_id":"a","content":"[image]"},{"role":"tool","tool_call_id":"b","content":"b.png"},` +
				`{"role":"user","content":[{"type":"text","text":"Image returned by tool call a:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUFB"}},` +
				`{"type":"text","text":"Image returned by tool call b:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QkJC"}}]}`},
	} {
		out, _, dropped, err := responsesChat(t).Request([]byte(`{"model":"m","input":[`+user+`,`+c.items+`]}`), nil, "gpt-x")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(dropped, c.dropped) {
			t.Errorf("%s: dropped %v, want %v", name, dropped, c.dropped)
		}
		if !jsonEqual(out, []byte(`{"model":"gpt-x","messages":[`+user+`,`+c.want+`]}`)) {
			t.Errorf("%s:\n got %s\nwant %s", name, out, c.want)
		}
		if err := chat.CheckRequest(out); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// replayResponses builds the next request of a client that read a Responses stream — every done
// item, then an output for every done call — and sends it through the pair.
func replayResponses(t *testing.T, raw []byte) {
	t.Helper()
	items := []string{`{"role":"user","content":"go"}`}
	var outputs []string
	for _, f := range responseFrames(t, raw) {
		if f.event != "response.output_item.done" {
			continue
		}
		it := f.data.(map[string]any)["item"].(map[string]any)
		b, _ := json.Marshal(it)
		items = append(items, string(b))
		if it["type"] == "function_call" {
			id, _ := json.Marshal(it["call_id"])
			outputs = append(outputs, `{"type":"function_call_output","call_id":`+string(id)+`,"output":"ok"}`)
		}
	}
	body := `{"model":"m","input":[` + strings.Join(append(items, outputs...), ",") + `]}`
	out, _, dropped, err := responsesChat(t).Request([]byte(body), nil, "gpt-x")
	if err != nil || dropped != nil {
		t.Fatalf("the history of an answer is refused or repaired: %v, dropped %v\n%s", err, dropped, body)
	}
	if err := chat.CheckRequest(out); err != nil {
		t.Fatalf("the history of an answer is no valid Chat conversation: %v\n%s", err, out)
	}
}

func TestResponsesChat_TheHistoryOfAnyAnswerIsTaken(t *testing.T) {
	// Random Chat streams — text, then interleaved calls — whole and cut at a random byte, in
	// random pieces: whatever the caller got, it can send it back.
	rng := rand.New(rand.NewSource(11))
	args := []string{`{"path":"a.txt"}`, `{ "b":1.0, "a":[1, "<&>"] }`, `{}`, `{"s":"` + strings.Repeat("xy", 40) + `"}`}
	for round := 0; round < 400; round++ {
		n := 1 + rng.Intn(len(args))
		upstream := append(sseOf(chunk(`{"role":"assistant","content":"Let me look."}`)), chatToolStream(rng, args[:n])...)
		if round%4 != 0 {
			upstream = upstream[:rng.Intn(len(upstream))]
		}
		c := newResponsesCall(t, true).upstream(200, "text/event-stream", upstream, 1+rng.Intn(60))
		if !strings.HasPrefix(c.rec.Header().Get("Content-Type"), "text/event-stream") {
			continue // cut before the first event: an HTTP error, and nothing to send back
		}
		if _, err := responses.CheckStream(c.rec.Body.Bytes()); err != nil {
			t.Fatalf("round %d: %v\n%s", round, err, c.rec.Body)
		}
		replayResponses(t, c.rec.Body.Bytes())
	}
}

// responseBody is a Responses answer as a client reads it.
type responseBody struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"`
	Model     string `json:"model"`
	Output    []struct {
		ID, Type, Role, Status string
		CallID                 string `json:"call_id"`
		Name                   string
		Arguments              string
		Content                []struct {
			Type, Text  string
			Annotations []any
		}
	} `json:"output"`
	OutputText *string `json:"output_text"`
	Usage      struct {
		Input         int                       `json:"input_tokens"`
		Output        int                       `json:"output_tokens"`
		Total         int                       `json:"total_tokens"`
		InputDetails  *struct{ Cached *int }    `json:"input_tokens_details"`
		OutputDetails *struct{ Reasoning *int } `json:"output_tokens_details"`
	} `json:"usage"`
	Parallel   *bool                    `json:"parallel_tool_calls"`
	ToolChoice any                      `json:"tool_choice"`
	Tools      []any                    `json:"tools"`
	Incomplete *struct{ Reason string } `json:"incomplete_details"`
}

func TestResponsesChat_Buffered(t *testing.T) {
	before := time.Now().Unix()
	c := newResponsesCall(t, false)
	c.w.Header().Set("X-Request-Id", "req-1")
	c.w.Header().Set("Content-Length", "450")
	c.w.Header().Set("Set-Cookie", "a=b")
	c.upstream(200, "application/json", readFile(t, "chat/testdata/resp_tools.json"), 50)
	var got responseBody
	if err := json.Unmarshal(c.rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if c.rec.Code != 200 || got.ID != "resp_chatcmpl-2" || got.Object != "response" || got.Status != "completed" || got.Model != "m-chat" ||
		got.CreatedAt < before || got.CreatedAt > time.Now().Unix() || got.Usage.Input != 40 || got.Usage.Output != 18 || got.Usage.Total != 58 ||
		got.OutputText != nil || len(got.Output) != 2 {
		t.Fatalf("status %d: %s", c.rec.Code, c.rec.Body)
	}
	// What the SDKs' Response and ResponseUsage types require.
	if u := got.Usage; u.InputDetails == nil || u.OutputDetails == nil || got.Parallel == nil || !*got.Parallel || got.ToolChoice != "auto" || got.Tools == nil ||
		!bytes.Contains(c.rec.Body.Bytes(), []byte(`"input_tokens_details":{"cached_tokens":0}`)) || !bytes.Contains(c.rec.Body.Bytes(), []byte(`"output_tokens_details":{"reasoning_tokens":0}`)) {
		t.Fatalf("required fields are missing: %s", c.rec.Body)
	}
	for i, want := range []string{`{"path":"a.txt"}`, `{"path":"b.txt"}`} {
		o := got.Output[i]
		if o.Type != "function_call" || o.ID != fmt.Sprint("fc_", i) || o.CallID != fmt.Sprint("call_", string(rune('a'+i))) || o.Name != "read_file" || o.Arguments != want || o.Status != "completed" {
			t.Fatalf("output[%d] = %+v", i, o)
		}
	}
	if code, mid := c.w.Failure(); code != "" || mid || c.onError != nil {
		t.Fatalf("Failure = %q, %v; OnError %v", code, mid, c.onError)
	}
	h := c.rec.Header()
	if h.Get("Content-Type") != "application/json" || h.Get("Content-Length") != strconv.Itoa(c.rec.Body.Len()) || h.Get("X-Request-Id") != "req-1" || h.Get("Set-Cookie") != "" {
		t.Fatalf("header %v", h)
	}
	// Text: a message item with an output_text part and annotations, and no output_text field.
	c = newResponsesCall(t, false).upstream(200, "application/json", readFile(t, "chat/testdata/resp_text.json"), 0)
	got = responseBody{}
	if err := json.Unmarshal(c.rec.Body.Bytes(), &got); err != nil || len(got.Output) != 1 || got.Output[0].Type != "message" || got.Output[0].Role != "assistant" ||
		len(got.Output[0].Content) != 1 || got.Output[0].Content[0].Type != "output_text" || got.Output[0].Content[0].Text == "" || got.Output[0].Content[0].Annotations == nil ||
		got.OutputText != nil || bytes.Contains(c.rec.Body.Bytes(), []byte(`"output_text":`)) {
		t.Fatalf("%v: %s", err, c.rec.Body)
	}
	// Arguments arrive as the upstream's bytes; a cut answer is incomplete, not completed.
	args := `{ "b":1.0, "a":[12345678901234567890, "<&>"] }`
	quoted, _ := json.Marshal(args)
	c = newResponsesCall(t, false).upstream(200, "application/json", []byte(`{"id":"c","choices":[{"finish_reason":"length","message":{"content":"par","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":`+string(quoted)+`}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`), 0)
	got = responseBody{}
	if err := json.Unmarshal(c.rec.Body.Bytes(), &got); err != nil || got.Status != "incomplete" || got.Incomplete == nil || got.Incomplete.Reason != "max_output_tokens" ||
		len(got.Output) != 2 || got.Output[1].Arguments != args || got.Model != "asked-for" {
		t.Fatalf("%v: %s", err, c.rec.Body)
	}
}

func TestResponsesChat_Streamed(t *testing.T) {
	want := readFile(t, "testdata/responses_chat_stream_tools.sse")
	for _, piece := range []int{1, 5, 4096} {
		c := newResponsesCall(t, true)
		c.w.Header().Set("X-Request-Id", "req-2")
		c.upstream(200, "text/event-stream; charset=utf-8", readFile(t, "chat/testdata/stream_tools.sse"), piece)
		// The comments that are written while a call is held back depend on the clock.
		got := bytes.ReplaceAll(atFixtureTime(c.rec.Body.Bytes()), []byte(": keep-alive\n\n"), nil)
		if !bytes.Equal(got, want) {
			t.Fatalf("piece %d: stream\n got:\n%s\nwant:\n%s", piece, got, want)
		}
		h := c.rec.Header()
		if c.rec.Code != 200 || h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" || h.Get("X-Request-Id") != "req-2" || !c.rec.Flushed {
			t.Fatalf("piece %d: status %d, header %v, flushed %v", piece, c.rec.Code, h, c.rec.Flushed)
		}
		if code, mid := c.w.Failure(); code != "" || mid || c.onError != nil {
			t.Fatalf("Failure = %q, %v; OnError %v", code, mid, c.onError)
		}
	}
	// The stop reasons of a stream: the token cap and the content filter are not a completed
	// response, and they are not a failure of the gateway either.
	for reason, want := range map[string][2]string{"stop": {"completed", ""}, "length": {"incomplete", "max_output_tokens"}, "content_filter": {"incomplete", "content_filter"}} {
		c := newResponsesCall(t, true).upstream(200, "text/event-stream", sseOf(chunk(`{"content":"par"}`), finishChunk(reason), "[DONE]"), 0)
		s, err := responses.CheckStream(c.rec.Body.Bytes())
		if err != nil || s.Status != want[0] || s.IncompleteReason != want[1] || s.InputTokens != 9 || s.OutputTokens != 4 || s.TotalTokens != 13 || s.Model != "m-up" {
			t.Fatalf("%s: %v, %+v", reason, err, s)
		}
		if !bytes.HasSuffix(bytes.TrimRight(c.rec.Body.Bytes(), "\n"), []byte("}}")) || !bytes.Contains(c.rec.Body.Bytes(), []byte("event: response."+want[0]+"\n")) {
			t.Fatalf("%s: %s", reason, c.rec.Body)
		}
		if code, mid := c.w.Failure(); code != "" || mid {
			t.Fatalf("%s: Failure = %q, %v", reason, code, mid)
		}
	}
}

// responseFrames reads the frames of a Responses stream without responses.CheckStream; the
// comments that are written while a call is held back are no frames.
func responseFrames(t *testing.T, raw []byte) []sseFrame {
	t.Helper()
	return parseFrames(t, bytes.ReplaceAll(raw, []byte(": keep-alive\n\n"), nil))
}

// functionCalls reads the function calls of a Responses stream without responses.CheckStream:
// per call the joined argument deltas and the arguments of the done event, the done item and the
// final response.
func functionCalls(t *testing.T, raw []byte) (joined, done, item, final []string) {
	t.Helper()
	index := map[string]int{}
	for _, f := range responseFrames(t, raw) {
		d := f.data.(map[string]any)
		switch f.event {
		case "response.output_item.added":
			if it := d["item"].(map[string]any); it["type"] == "function_call" {
				index[it["id"].(string)] = len(joined)
				joined, done, item = append(joined, ""), append(done, "<none>"), append(item, "<none>")
			}
		case "response.function_call_arguments.delta":
			joined[index[d["item_id"].(string)]] += d["delta"].(string)
		case "response.function_call_arguments.done":
			done[index[d["item_id"].(string)]] = d["arguments"].(string)
		case "response.output_item.done":
			if it := d["item"].(map[string]any); it["type"] == "function_call" {
				item[index[it["id"].(string)]] = it["arguments"].(string)
			}
		case "response.completed":
			for _, o := range d["response"].(map[string]any)["output"].([]any) {
				if it := o.(map[string]any); it["type"] == "function_call" {
					final = append(final, it["arguments"].(string))
				}
			}
		}
	}
	return
}

func TestResponsesChat_Streamed_ArgumentsReassembleToTheUpstreamsBytes(t *testing.T) {
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
		c := newResponsesCall(t, true).upstream(200, "text/event-stream", upstream, 1+rng.Intn(40))
		s, err := responses.CheckStream(c.rec.Body.Bytes())
		if err != nil {
			t.Fatalf("round %d: %v\n%s", round, err, c.rec.Body)
		}
		if len(s.Items) != n || !s.Completed || s.InputTokens != 9 || s.OutputTokens != 4 || s.TotalTokens != 13 {
			t.Fatalf("round %d: %+v", round, s)
		}
		for i, it := range s.Items {
			if it.CallID != fmt.Sprint("call_", i) || it.Name != fmt.Sprint("fn", i) || it.Arguments != args[i] || strings.Join(it.Deltas, "") != args[i] {
				t.Fatalf("round %d call %d:\n got %q\nwant %q", round, i, it.Arguments, args[i])
			}
		}
		// The same, read without the checker.
		joined, done, item, final := functionCalls(t, c.rec.Body.Bytes())
		for name, got := range map[string][]string{"joined deltas": joined, "done event": done, "done item": item, "final response": final} {
			if !reflect.DeepEqual(got, args) {
				t.Fatalf("round %d, %s:\n got %q\nwant %q", round, name, got, args)
			}
		}
	}
	// A call without arguments is "{}" everywhere: Codex parses the string as JSON.
	for _, arguments := range []string{`""`, `null`} {
		c := newResponsesCall(t, true).upstream(200, "text/event-stream", sseOf(
			chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":`+arguments+`}}]}`), finishChunk("tool_calls"), "[DONE]"), 0)
		joined, done, item, final := functionCalls(t, c.rec.Body.Bytes())
		if want := []string{"{}"}; !reflect.DeepEqual(joined, want) || !reflect.DeepEqual(done, want) || !reflect.DeepEqual(item, want) || !reflect.DeepEqual(final, want) {
			t.Fatalf("no arguments (%s): %q %q %q %q\n%s", arguments, joined, done, item, final, c.rec.Body)
		}
	}
}

func TestResponsesChat_Streamed_BadEndings(t *testing.T) {
	// Review Focus 2. After the first byte the caller's stream ends with every item done and
	// one response.failed; there is no response.completed.
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
		call := newResponsesCall(t, true).upstream(200, "text/event-stream", c.upstream, 4096)
		raw := call.rec.Body.Bytes()
		s, err := responses.CheckStream(raw)
		if err != nil {
			t.Errorf("%s: %v\n%.600s", name, err, raw)
			continue
		}
		if call.rec.Code != 200 || s.Completed || s.Status != "failed" || s.ErrCode != "server_error" || s.ErrMessage != c.message {
			t.Errorf("%s: status %d, stream %+v", name, call.rec.Code, s)
		}
		if strings.Contains(string(raw), "response.completed") || strings.Contains(string(raw), "response.incomplete") || strings.Contains(string(raw), "more") {
			t.Errorf("%s: the stream pretends to go on\n%.600s", name, raw)
		}
		// The failure is the last thing written, read without the checker.
		frames := responseFrames(t, raw)
		last := frames[len(frames)-1]
		resp, _ := last.data.(map[string]any)["response"].(map[string]any)
		failure, _ := resp["error"].(map[string]any)
		if last.event != "response.failed" || resp["status"] != "failed" || failure["message"] != c.message || failure["code"] != "server_error" {
			t.Errorf("%s: the last event is %s: %v", name, last.event, last.data)
		}
		for _, f := range frames[:len(frames)-1] {
			if f.event == "response.failed" || f.event == "response.completed" {
				t.Errorf("%s: %s before the end", name, f.event)
			}
		}
		if code, mid := call.w.Failure(); code != c.code || !mid {
			t.Errorf("%s: Failure = %q, %v", name, code, mid)
		}
		if want := []string{"200 " + c.code}; !reflect.DeepEqual(call.onError, want) {
			t.Errorf("%s: OnError %v, want %v", name, call.onError, want)
		}
		if len(s.Items) == 0 || s.Items[0].Text != "par" || s.Items[0].Status != "completed" {
			t.Errorf("%s: what had arrived is gone: %+v", name, s.Items)
		}
		// A call that was cut is never handed over: Codex would run it and send it back for ever.
		for _, it := range s.Items[1:] {
			if it.Type == "function_call" && it.Done {
				t.Errorf("%s: a cut call is done, with arguments %.40q", name, it.Arguments)
			}
		}
		if bytes.Contains(raw, []byte("function_call_arguments.done")) {
			t.Errorf("%s: a done event for a cut call\n%.600s", name, raw)
		}
		replayResponses(t, raw)
	}
}

func TestResponsesChat_Streamed_ACallCutBeforeItsFirstArgumentByte(t *testing.T) {
	// The Chat decoder stops every open call before it reports the failure. A call that had its
	// name and not one byte of arguments yet must not be handed over as a call with "{}": Codex
	// would run it.
	nameOnly := func(index int) string {
		return chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"cut","type":"function","function":{"name":"f","arguments":""}}]}`, index))
	}
	whole := func(index int) string {
		return chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"whole","type":"function","function":{"name":"g","arguments":"{\"k\":1}"}}]}`, index))
	}
	for name, c := range map[string]struct {
		upstream  []byte
		wholeDone bool
	}{
		"then the upstream closes":               {sseOf(nameOnly(0)), false},
		"then an error frame":                    {sseOf(nameOnly(0), `{"error":{"message":"overloaded"}}`), false},
		"then [DONE] without a finish_reason":    {sseOf(nameOnly(0), "[DONE]"), false},
		"beside a complete call that came later": {sseOf(nameOnly(0), whole(1)), true},
		"beside a complete call that came first": {sseOf(whole(0), nameOnly(1), `{"error":{"message":"overloaded"}}`), true},
	} {
		for _, piece := range []int{1, 4096} {
			call := newResponsesCall(t, true).upstream(200, "text/event-stream", c.upstream, piece)
			raw := call.rec.Body.Bytes()
			s, err := responses.CheckStream(raw)
			if err != nil || s.Status != "failed" {
				t.Fatalf("%s: %v, %+v\n%s", name, err, s, raw)
			}
			wholeDone := false
			for _, it := range s.Items {
				if it.CallID == "cut" && (it.Done || len(it.Deltas) > 0) {
					t.Errorf("%s: the cut call is handed over: %+v", name, it)
				}
				wholeDone = wholeDone || (it.CallID == "whole" && it.Done && it.Arguments == `{"k":1}`)
			}
			if wholeDone != c.wholeDone || bytes.Contains(raw, []byte(`"arguments":"{}"`)) {
				t.Errorf("%s: the whole call is done: %v\n%s", name, wholeDone, raw)
			}
			if code, mid := call.w.Failure(); code == "" || !mid {
				t.Errorf("%s: Failure = %q, %v", name, code, mid)
			}
			replayResponses(t, raw)
		}
	}
	// The same call in a stream that ends well is a call without arguments.
	call := newResponsesCall(t, true).upstream(200, "text/event-stream", sseOf(nameOnly(0), whole(1), finishChunk("tool_calls"), "[DONE]"), 7)
	s, err := responses.CheckStream(call.rec.Body.Bytes())
	if err != nil || !s.Completed || len(s.Items) != 2 || s.Items[0].CallID != "cut" || s.Items[0].Arguments != "{}" || s.Items[1].Arguments != `{"k":1}` {
		t.Fatalf("%v, %+v", err, s)
	}
}

func TestResponsesChat_Streamed_FailureBeforeTheFirstByteIsAnHTTPError(t *testing.T) {
	// Nothing was sent yet, so the caller gets an error RESPONSE in the OpenAI shape (and the
	// gateway a status it can act on) instead of a stream that begins with a failure.
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
		call := newResponsesCall(t, true)
		call.w.Header().Set("X-Request-Id", "req-9")
		call.upstream(200, "text/event-stream", c.upstream, 5)
		message := call.openaiError(t)
		if call.rec.Code != 502 || message != c.message || call.rec.Header().Get("X-Request-Id") != "req-9" {
			t.Errorf("%s: status %d: %q", name, call.rec.Code, message)
		}
		if code, mid := call.w.Failure(); code != c.code || mid {
			t.Errorf("%s: Failure = %q, %v", name, code, mid)
		}
		if want := []string{"502 " + c.code}; !reflect.DeepEqual(call.onError, want) {
			t.Errorf("%s: OnError %v, want %v", name, call.onError, want)
		}
	}
}

func TestResponsesChat_UpstreamErrorsKeepTheirStatus(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, c := range []struct {
			status            int
			contentType, body string
			wantMsg           string
		}{
			{429, "application/json", `{"error":{"message":"Rate limit reached for requests","type":"requests","code":"rate_limit_exceeded"}}`, "Rate limit reached for requests"},
			{400, "application/json", `{"error":{"message":"max_tokens is too large","type":"invalid_request_error"}}`, "max_tokens is too large"},
			{401, "application/json", `{"error":{"message":"Incorrect API key provided"}}`, "Incorrect API key provided"},
			{404, "application/json", `{"message":"no such model"}`, "no such model"},
			{502, "text/html", "<html><body>Bad Gateway SECRET</body></html>", "the provider answered 502"},
			{503, "", ``, "the provider answered 503"},
		} {
			call := newResponsesCall(t, stream)
			call.w.Header().Set("Retry-After", "7")
			call.upstream(c.status, c.contentType, []byte(c.body), 9)
			if message := call.openaiError(t); call.rec.Code != c.status || message != c.wantMsg {
				t.Errorf("%d (stream %v): status %d: %q", c.status, stream, call.rec.Code, message)
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
	// The body is exactly the gateway's OpenAI error shape; a long message is cut.
	c := newResponsesCall(t, false).upstream(429, "application/json", []byte(`{"error":{"message":"slow down"}}`), 0)
	assertJSONEqual(t, c.rec.Body.Bytes(), `{"error":{"message":"slow down","type":"burrow_error","code":"upstream_error"}}`)
	long := newResponsesCall(t, false).upstream(500, "application/json", []byte(`{"error":{"message":"`+strings.Repeat("m", 5000)+`"}}`), 0)
	if message := long.openaiError(t); len(message) != 300 {
		t.Fatalf("message of %d bytes", len(message))
	}
	// Answers that are no answer.
	for name, c := range map[string]struct {
		stream      bool
		contentType string
		body        string
		code        string
	}{
		"a whole answer where a stream was asked for": {true, "application/json", string(readFile(t, "chat/testdata/resp_tools.json")), CodeUpstreamInvalid},
		"a stream where a whole answer was asked for": {false, "text/event-stream", string(readFile(t, "chat/testdata/stream_tools.sse")), CodeUpstreamInvalid},
		"an error object with status 200":             {false, "application/json", `{"error":{"message":"quota"}}`, CodeUpstreamError},
		"no JSON":                                     {false, "application/json", `<html>`, CodeUpstreamInvalid},
	} {
		call := newResponsesCall(t, c.stream).upstream(200, c.contentType, []byte(c.body), 0)
		call.openaiError(t)
		if code, mid := call.w.Failure(); call.rec.Code != 502 || code != c.code || mid {
			t.Errorf("%s: status %d, Failure = %q, %v", name, call.rec.Code, code, mid)
		}
	}
}

func TestResponsesChat_NothingIsLogged(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() { log.SetOutput(os.Stderr); slog.SetDefault(old) }()

	_, _, _, err := responsesChat(t).Request([]byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_data":"SECRET"}]}]}`), nil, "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
	newResponsesCall(t, false).upstream(429, "application/json", []byte(`{"error":{"message":"SECRET key sk-live-123"}}`), 0)
	newResponsesCall(t, false).upstream(200, "application/json", []byte(`{"SECRET":1}`), 0)
	newResponsesCall(t, true).upstream(200, "text/event-stream", sseOf(chunk(`{"content":"SECRET"}`), `{"error":{"message":"SECRET"}}`), 0)
	newResponsesCall(t, true).upstream(200, "text/event-stream", sseOf(chunk(`{"content":"SECRET"}`), `{SECRET`), 0)
	if logged.Len() != 0 {
		t.Fatalf("something was logged: %s", logged.String())
	}
}

func TestResponsesChat_ReleasedIsDecidedByTheToolCallChecks(t *testing.T) {
	registered := pairs[[2]Format{Responses, Chat}]
	if err := registered.check(registered); err != nil {
		t.Fatalf("tool-call checks of responses-chat: %v", err)
	}
	now := time.Now()
	broken := map[string]func(p *pair){
		"the stream encoder loses argument pieces": func(p *pair) {
			p.codec.newStreamEncoder = func(w io.Writer, model string) streamEncoder {
				return dropArgs{responses.NewStreamEncoder(w, model, now)}
			}
		},
		"the stream decoder stops early": func(p *pair) {
			p.codec.newStreamDecoder = func() streamDecoder { return &stopsEarly{d: chat.NewStreamDecoder()} }
		},
		"the buffered answer loses its tool calls": func(p *pair) {
			p.codec.encodeResponse = func(r ir.Response, model string) ([]byte, error) {
				r.Parts = r.Parts[:1]
				return responses.EncodeResponse(r, model, now)
			}
		},
		"the buffered answer rewrites the arguments": func(p *pair) {
			p.codec.encodeResponse = func(r ir.Response, model string) ([]byte, error) {
				out, err := responses.EncodeResponse(r, model, now)
				return bytes.ReplaceAll(out, []byte(`{ \"city\"`), []byte(`{\"city\"`)), err
			}
		},
		"the request splits the calls of one turn": func(p *pair) {
			p.request = func(body []byte, h http.Header, model string) ([]byte, bool, []string, error) {
				out, stream, dropped, err := responsesToChat(body, h, model)
				return bytes.Replace(out, []byte(`}},{"id":"toolu_2"`), []byte(`}}]},{"role":"assistant","content":null,"tool_calls":[{"id":"toolu_2"`), 1), stream, dropped, err
			}
		},
		"the request loses the tool results": func(p *pair) {
			p.request = func(body []byte, h http.Header, model string) ([]byte, bool, []string, error) {
				out, stream, dropped, err := responsesToChat(body, h, model)
				return bytes.ReplaceAll(out, []byte(`"role":"tool"`), []byte(`"role":"user"`)), stream, dropped, err
			}
		},
		"the request fails": func(p *pair) {
			p.request = func([]byte, http.Header, string) ([]byte, bool, []string, error) {
				return nil, false, nil, errors.New("no")
			}
		},
		"the request refuses a history with a call that was cut": func(p *pair) {
			p.request = func(body []byte, h http.Header, model string) ([]byte, bool, []string, error) {
				if bytes.Contains(body, []byte(`"arguments":"{\"city\":"}`)) {
					return nil, false, nil, &ir.BadRequestError{Format: "responses", Field: "input[4].arguments", Reason: "is not the text of a JSON object"}
				}
				return responsesToChat(body, h, model)
			}
		},
		"the stream encoder hands over a call that was cut": func(p *pair) {
			p.codec.newStreamEncoder = func(w io.Writer, model string) streamEncoder {
				return closesCalls{responses.NewStreamEncoder(w, model, now)}
			}
		},
	}
	for name, breakIt := range broken {
		p := newResponsesChat()
		breakIt(p)
		if p.Released() {
			t.Errorf("%s: the pair is released", name)
		}
		if err := p.check(p); err == nil {
			t.Errorf("%s: the checks pass", name)
		}
	}
	if fresh := newResponsesChat(); !fresh.Released() || !fresh.Released() {
		t.Fatal("a sound pair is not released")
	}
}

// closesCalls completes the arguments of every call that stops, whatever had arrived.
type closesCalls struct{ streamEncoder }

func (c closesCalls) Write(ev ir.Event) error {
	if ev.Kind == ir.PartStop {
		_ = c.streamEncoder.Write(ir.Event{Kind: ir.ToolArgsDelta, Index: ev.Index, ArgsJSON: `"x"}`})
	}
	return c.streamEncoder.Write(ev)
}

// FuzzResponsesChatWriter plays an arbitrary upstream answer through the pair. Oracle: the caller
// gets exactly one of a Responses body, an OpenAI error response, or a well-formed Responses
// stream that ended — and what the writer reports about it is true.
func FuzzResponsesChatWriter(f *testing.F) {
	f.Add(readFile(f, "chat/testdata/stream_tools.sse"), 7, true, 200, "text/event-stream")
	f.Add(readFile(f, "chat/testdata/stream_text.sse"), 1, true, 200, "text/event-stream")
	f.Add(readFile(f, "chat/testdata/resp_tools.json"), 50, false, 200, "application/json")
	f.Add([]byte(`{"error":{"message":"no"}}`), 3, false, 429, "application/json")
	f.Add(sseOf(chunk(`{"content":"x"}`), `{"error":"late"}`), 4, true, 200, "text/event-stream")
	f.Add(sseOf(chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{\"k\":"}}]}`), finishChunk("tool_calls")), 4, true, 200, "text/event-stream")
	f.Add(sseOf(`{"error":"first"}`), 4, true, 200, "text/event-stream")
	f.Add([]byte("<html>"), 2, true, 502, "text/html")
	f.Fuzz(func(t *testing.T, body []byte, piece int, stream bool, status int, contentType string) {
		if status < 200 || status > 599 {
			status = 200
		}
		c := newResponsesCall(t, stream).upstream(status, contentType, body, piece)
		code, mid := c.w.Failure()
		out := c.rec.Body.Bytes()
		switch {
		case mid:
			s, err := responses.CheckStream(out)
			if err != nil || s.Status != "failed" || s.ErrMessage == "" || code == "" || c.rec.Code != status {
				t.Fatalf("a failed stream: %v, %+v, code %q, status %d\n%s", err, s, code, c.rec.Code, out)
			}
			replayResponses(t, out)
		case code != "":
			var e struct {
				Error struct{ Message, Type, Code string }
			}
			if err := json.Unmarshal(out, &e); err != nil || e.Error.Type != "burrow_error" || e.Error.Code != code || e.Error.Message == "" {
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
			replayResponses(t, out)
		default:
			var r responseBody
			if err := json.Unmarshal(out, &r); err != nil || r.Object != "response" || r.ID == "" || (r.Status != "completed" && r.Status != "incomplete") ||
				r.Usage.Total != r.Usage.Input+r.Usage.Output || stream || c.rec.Code != status {
				t.Fatalf("a response: %v\n%s", err, out)
			}
			for _, o := range r.Output {
				if o.Type == "function_call" && (o.CallID == "" || o.Name == "" || ir.CheckObject([]byte(o.Arguments)) != nil) {
					t.Fatalf("a call that cannot be acted on: %+v", o)
				}
			}
		}
		if (code != "") != (len(c.onError) == 1) {
			t.Fatalf("code %q, OnError %v", code, c.onError)
		}
	})
}
