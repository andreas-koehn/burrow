package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// ---------------------------------------------------------------- helpers

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON differs\n got: %s\nwant: %s", got, want)
	}
}

func user(parts ...ir.Part) ir.Message      { return ir.Message{Role: ir.User, Parts: parts} }
func assistant(parts ...ir.Part) ir.Message { return ir.Message{Role: ir.Assistant, Parts: parts} }
func text(s string) ir.Part                 { return ir.Part{Kind: ir.Text, Text: s} }
func toolUse(id, name, input string) ir.Part {
	return ir.Part{Kind: ir.ToolUse, ToolID: id, ToolName: name, Input: json.RawMessage(input)}
}
func toolResult(id, out string) ir.Part { return ir.Part{Kind: ir.ToolResult, ToolID: id, Text: out} }

// encode returns the body's "messages" (and the whole body) for a request.
func encode(t *testing.T, req ir.Request) (map[string]json.RawMessage, []string) {
	t.Helper()
	body, dropped, err := EncodeRequest(req, "m-chat")
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	return top, dropped
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decodeStream runs raw SSE bytes through the parser and the decoder in
// pieces of the given size and returns the events, Close's included.
func decodeStream(t testing.TB, raw []byte, piece int) ([]ir.Event, error) {
	t.Helper()
	p := sse.NewParser(MaxFrameBytes)
	d := NewStreamDecoder()
	var out []ir.Event
	feed := func(frames []sse.Frame) error {
		for _, f := range frames {
			evs, err := d.Feed(f.Data)
			out = append(out, evs...)
			if err != nil {
				return err
			}
		}
		return nil
	}
	for i := 0; i < len(raw); i += piece {
		frames, err := p.Feed(raw[i:min(i+piece, len(raw))])
		if ferr := feed(frames); ferr != nil {
			return out, ferr
		}
		if err != nil {
			return out, err
		}
	}
	if err := feed(p.Flush()); err != nil {
		return out, err
	}
	return append(out, d.Close()...), nil
}

func chunk(delta string) []byte {
	return []byte(`{"id":"c","model":"m","choices":[{"index":0,"delta":` + delta + `,"finish_reason":null}]}`)
}

func finishChunk(reason string) []byte {
	return []byte(`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}`)
}

// feedAll feeds frames and returns the events, stopping at the first error
// (whose events are included).
func feedAll(d *StreamDecoder, frames ...[]byte) ([]ir.Event, error) {
	var out []ir.Event
	for _, f := range frames {
		evs, err := d.Feed(f)
		out = append(out, evs...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func kinds(evs []ir.Event) string {
	var b strings.Builder
	for i, ev := range evs {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(string(ev.Kind))
	}
	return b.String()
}

// ---------------------------------------------------------------- request

func TestEncodeRequest_FullConversation(t *testing.T) {
	temp := 0.2
	req := ir.Request{
		System: []ir.Part{{Kind: ir.Text, Text: "You are terse."}, {Kind: ir.Text, Text: "Use tools."}},
		Messages: []ir.Message{
			{Role: "user", Parts: []ir.Part{{Kind: ir.Text, Text: "Read a.txt"}}},
			{Role: "assistant", Parts: []ir.Part{
				{Kind: ir.Thinking, Text: "I should read it."},
				{Kind: ir.Text, Text: "Reading."},
				{Kind: ir.ToolUse, ToolID: "call_a", ToolName: "read_file", Input: json.RawMessage(`{"path":"a.txt"}`)},
			}},
			{Role: "user", Parts: []ir.Part{
				{Kind: ir.ToolResult, ToolID: "call_a", Text: "contents of a"},
				{Kind: ir.Text, Text: "Now summarise."},
			}},
		},
		Tools:       []ir.Tool{{Name: "read_file", Description: "Reads a file", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}},
		ToolChoice:  ir.ToolChoice{Mode: "auto"},
		MaxTokens:   256,
		Temperature: &temp,
		Stop:        []string{"END"},
		Stream:      true,
		Dropped:     []string{"cache_control"},
	}
	body, dropped, err := EncodeRequest(req, "m-chat")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
	  "model":"m-chat",
	  "messages":[
	    {"role":"system","content":"You are terse.\n\nUse tools."},
	    {"role":"user","content":"Read a.txt"},
	    {"role":"assistant","content":"Reading.","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]},
	    {"role":"tool","tool_call_id":"call_a","content":"contents of a"},
	    {"role":"user","content":"Now summarise."}
	  ],
	  "tools":[{"type":"function","function":{"name":"read_file","description":"Reads a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],
	  "tool_choice":"auto",
	  "max_tokens":256,
	  "temperature":0.2,
	  "stop":["END"],
	  "stream":true,
	  "stream_options":{"include_usage":true}
	}`
	assertJSONEqual(t, body, want)
	if !reflect.DeepEqual(dropped, []string{"cache_control", "thinking"}) {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestEncodeRequest_ParallelToolResults(t *testing.T) {
	top, _ := encode(t, ir.Request{Messages: []ir.Message{
		user(text("go")),
		assistant(toolUse("a", "f", `{}`), toolUse("b", "g", `{"x":1}`)),
		user(toolResult("a", "one"), toolResult("b", "two")),
	}})
	assertJSONEqual(t, top["messages"], `[
	  {"role":"user","content":"go"},
	  {"role":"assistant","content":null,"tool_calls":[
	    {"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},
	    {"id":"b","type":"function","function":{"name":"g","arguments":"{\"x\":1}"}}]},
	  {"role":"tool","tool_call_id":"a","content":"one"},
	  {"role":"tool","tool_call_id":"b","content":"two"}
	]`)
}

func TestEncodeRequest_ToolResultsComeBeforeOtherContent(t *testing.T) {
	top, _ := encode(t, ir.Request{Messages: []ir.Message{
		user(text("first"), toolResult("a", "one"), text("second"), toolResult("b", "two")),
	}})
	assertJSONEqual(t, top["messages"], `[
	  {"role":"tool","tool_call_id":"a","content":"one"},
	  {"role":"tool","tool_call_id":"b","content":"two"},
	  {"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}
	]`)
}

func TestEncodeRequest_AssistantContent(t *testing.T) {
	top, dropped := encode(t, ir.Request{Messages: []ir.Message{
		user(text("go")),
		assistant(toolUse("a", "f", ``)),
		user(toolResult("a", "")),
		assistant(text("one"), text("two")),
		user(text("more")),
		assistant(ir.Part{Kind: ir.Thinking, Text: "only thinking"}),
		user(),
	}})
	assertJSONEqual(t, top["messages"], `[
	  {"role":"user","content":"go"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"a","content":""},
	  {"role":"assistant","content":"one\n\ntwo"},
	  {"role":"user","content":"more"},
	  {"role":"user","content":""}
	]`)
	if !reflect.DeepEqual(dropped, []string{"thinking"}) {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestEncodeRequest_ToolResultError(t *testing.T) {
	top, dropped := encode(t, ir.Request{Messages: []ir.Message{
		user(ir.Part{Kind: ir.ToolResult, ToolID: "a", Text: "no such file", IsError: true}),
	}})
	assertJSONEqual(t, top["messages"], `[{"role":"tool","tool_call_id":"a","content":"Error: no such file"}]`)
	if dropped != nil {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestEncodeRequest_Images(t *testing.T) {
	top, dropped := encode(t, ir.Request{Messages: []ir.Message{
		user(text("look"), ir.Part{Kind: ir.Image, MediaType: "image/png", Data: "aGk="},
			ir.Part{Kind: ir.Image, Data: "https://example.test/cat.png?a=1&b=2"}),
		assistant(text("ok")),
		user(ir.Part{Kind: ir.Image, MediaType: "image/jpeg", Data: "QUJD"}),
	}})
	assertJSONEqual(t, top["messages"], `[
	  {"role":"user","content":[
	    {"type":"text","text":"look"},
	    {"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}},
	    {"type":"image_url","image_url":{"url":"https://example.test/cat.png?a=1&b=2"}}]},
	  {"role":"assistant","content":"ok"},
	  {"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}}]}
	]`)
	if dropped != nil {
		t.Fatalf("an image was reported as dropped: %v", dropped)
	}
	if !bytes.Contains(top["messages"], []byte(`cat.png?a=1&b=2`)) {
		t.Fatalf("the URL was rewritten: %s", top["messages"])
	}

	img := ir.Part{Kind: ir.Image, MediaType: "image/png", Data: "aGk="}
	refused := map[string]ir.Request{
		"tool_result.image": {Messages: []ir.Message{user(ir.Part{Kind: ir.ToolResult, ToolID: "a", Text: "x", MediaType: "image/png", Data: "aGk="})}},
		"assistant.image":   {Messages: []ir.Message{user(text("x")), assistant(img)}},
		"system.image":      {System: []ir.Part{img}, Messages: []ir.Message{user(text("x"))}},
		"image.data":        {Messages: []ir.Message{user(ir.Part{Kind: ir.Image, MediaType: "image/png"})}},
	}
	for field, req := range refused {
		body, _, err := EncodeRequest(req, "m")
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), field) || body != nil {
			t.Errorf("%s: body %s, err = %v", field, body, err)
		}
	}
	if _, _, err := EncodeRequest(refused["tool_result.image"], "m"); !strings.Contains(err.Error(), "messages[0]") {
		t.Errorf("the error does not say where: %v", err)
	}
}

func TestEncodeRequest_ToolChoice(t *testing.T) {
	tools := []ir.Tool{{Name: "read_file"}, {Name: "list_dir", Schema: json.RawMessage(`{"type":"object"}`)}}
	msgs := []ir.Message{user(text("x"))}
	cases := []struct {
		choice  ir.ToolChoice
		tools   []ir.Tool
		want    string // "" = the key is absent
		dropped []string
	}{
		{ir.ToolChoice{}, tools, "", nil},
		{ir.ToolChoice{Mode: ir.ChoiceAuto}, tools, `"auto"`, nil},
		{ir.ToolChoice{Mode: ir.ChoiceNone}, tools, `"none"`, nil},
		{ir.ToolChoice{Mode: ir.ChoiceRequired}, tools, `"required"`, nil},
		{ir.ToolChoice{Mode: ir.ChoiceTool, Name: "read_file"}, tools, `{"type":"function","function":{"name":"read_file"}}`, nil},
		// Chat refuses a tool_choice without tools, and one that names a tool it was not given
		// (the caller's tool may have been left out as tool:<type>): the request still goes
		// through, and what was lost is named.
		{ir.ToolChoice{Mode: ir.ChoiceAuto}, nil, "", nil},
		{ir.ToolChoice{Mode: ir.ChoiceNone}, nil, "", nil},
		{ir.ToolChoice{Mode: ir.ChoiceRequired}, nil, "", []string{"tool_choice"}},
		{ir.ToolChoice{Mode: ir.ChoiceTool, Name: "read_file"}, nil, "", []string{"tool_choice"}},
		{ir.ToolChoice{Mode: ir.ChoiceTool, Name: "web_search"}, tools, "", []string{"tool_choice"}},
	}
	for _, c := range cases {
		top, dropped := encode(t, ir.Request{Messages: msgs, Tools: c.tools, ToolChoice: c.choice})
		got, present := top["tool_choice"]
		if present != (c.want != "") {
			t.Errorf("%+v: tool_choice present = %v (%s)", c.choice, present, got)
			continue
		}
		if present {
			assertJSONEqual(t, got, c.want)
		}
		if !reflect.DeepEqual(dropped, c.dropped) {
			t.Errorf("%+v: dropped = %v", c.choice, dropped)
		}
	}
	for _, bad := range []ir.ToolChoice{{Mode: ir.ChoiceTool}, {Mode: "sometimes"}} {
		if _, _, err := EncodeRequest(ir.Request{Messages: msgs, Tools: tools, ToolChoice: bad}, "m"); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "tool_choice") {
			t.Errorf("%+v: err = %v", bad, err)
		}
	}
}

func TestEncodeRequest_MinimalHasNoOptionalKeys(t *testing.T) {
	body, dropped, err := EncodeRequest(ir.Request{Model: "what-the-caller-asked-for", Messages: []ir.Message{user(text("hi"))}}, "m-chat")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"model":"m-chat","messages":[{"role":"user","content":"hi"}]}` {
		t.Fatalf("body = %s", body)
	}
	if dropped != nil {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestEncodeRequest_Parameters(t *testing.T) {
	temp, topP := 0.0, 0.95
	top, _ := encode(t, ir.Request{Messages: []ir.Message{user(text("hi"))},
		MaxTokens: 9007199254740993, Temperature: &temp, TopP: &topP, Stop: []string{"a", "\n\nHuman:"}})
	// max_tokens is written as the integer it is, not through a float.
	if string(top["max_tokens"]) != "9007199254740993" || string(top["temperature"]) != "0" || string(top["top_p"]) != "0.95" {
		t.Fatalf("max_tokens %s temperature %s top_p %s", top["max_tokens"], top["temperature"], top["top_p"])
	}
	assertJSONEqual(t, top["stop"], `["a","\n\nHuman:"]`)
	for _, k := range []string{"stream", "stream_options", "tools", "tool_choice"} {
		if _, ok := top[k]; ok {
			t.Errorf("%s is present", k)
		}
	}
}

func TestEncodeRequest_StopBeyondWhatChatTakes(t *testing.T) {
	top, dropped := encode(t, ir.Request{Messages: []ir.Message{user(text("hi"))}, Stop: []string{"a", "b", "c", "d", "e", "f"}})
	assertJSONEqual(t, top["stop"], `["a","b","c","d"]`)
	if !reflect.DeepEqual(dropped, []string{"stop.extra"}) {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestEncodeRequest_PassThroughJSONKeepsItsBytes(t *testing.T) {
	schema := `{ "type" : "object",  "properties":{"z":{"type":"integer","maximum":12345678901234567890123},"a":{"enum":[1.10,1E+2,"<b>&"]}},
	"required":["z"] }`
	input := `{ "z":12345678901234567890123, "a":1.10, "s":"é\u00e9\u0000 <&>" }`
	body, _, err := EncodeRequest(ir.Request{
		Messages: []ir.Message{user(text("x")), assistant(toolUse("c", "f", input)), user(toolResult("c", "ok"))},
		Tools:    []ir.Tool{{Name: "f", Schema: json.RawMessage(schema)}},
	}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"parameters":`+schema+`}`)) {
		t.Fatalf("the schema was rewritten:\n%s", body)
	}
	var out struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct{ Arguments string }
			} `json:"tool_calls"`
		}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.Messages[1].ToolCalls[0].Function.Arguments; got != input {
		t.Fatalf("arguments = %q", got)
	}
}

func TestEncodeRequest_StringsAreKeptExactly(t *testing.T) {
	s := "tab\t nul\x00 quote\" back\\slash \u2028 é 日本 😀 </script> &amp; \r\n end "
	top, _ := encode(t, ir.Request{
		System:   []ir.Part{text(s)},
		Messages: []ir.Message{user(text(s)), assistant(text(s), toolUse("id "+s, "f", "{}")), user(toolResult("id "+s, s), text(s), text(s))},
		Tools:    []ir.Tool{{Name: "f", Description: s}},
		Stop:     []string{s},
	})
	var msgs []struct {
		Content    any
		ToolCallID string                `json:"tool_call_id"`
		ToolCalls  []struct{ ID string } `json:"tool_calls"`
	}
	if err := json.Unmarshal(top["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	if msgs[0].Content != s || msgs[1].Content != s || msgs[2].Content != s || msgs[3].Content != s ||
		msgs[2].ToolCalls[0].ID != "id "+s || msgs[3].ToolCallID != "id "+s {
		t.Fatalf("a string changed: %s", top["messages"])
	}
	parts := msgs[4].Content.([]any)
	if parts[0].(map[string]any)["text"] != s || parts[1].(map[string]any)["text"] != s {
		t.Fatalf("a text part changed: %s", top["messages"])
	}
	var stop []string
	var tools []struct{ Function struct{ Description string } }
	if json.Unmarshal(top["stop"], &stop) != nil || stop[0] != s || json.Unmarshal(top["tools"], &tools) != nil || tools[0].Function.Description != s {
		t.Fatalf("stop or description changed")
	}
}

func TestEncodeRequest_Refused(t *testing.T) {
	const secret = "SECRET-PROMPT-CONTENT"
	ok := []ir.Message{user(text(secret))}
	nan := math.NaN()
	deep := json.RawMessage(strings.Repeat(`{"a":`, ir.MaxDepth+1) + `1` + strings.Repeat(`}`, ir.MaxDepth+1))
	cases := map[string]struct {
		req   ir.Request
		model string
		field string
	}{
		"no messages":             {ir.Request{}, "m", "messages"},
		"no model":                {ir.Request{Messages: ok}, "", "model"},
		"unknown role":            {ir.Request{Messages: []ir.Message{{Role: "system", Parts: []ir.Part{text(secret)}}}}, "m", "messages[0].role"},
		"tool use by the user":    {ir.Request{Messages: []ir.Message{user(toolUse("a", "f", "{}"))}}, "m", "messages[0]"},
		"thinking by the user":    {ir.Request{Messages: []ir.Message{user(ir.Part{Kind: ir.Thinking, Text: secret})}}, "m", "messages[0]"},
		"result by the assistant": {ir.Request{Messages: []ir.Message{user(text("x")), assistant(toolResult("a", secret))}}, "m", "messages[1]"},
		"unknown part kind":       {ir.Request{Messages: []ir.Message{user(ir.Part{Kind: "audio", Text: secret})}}, "m", "messages[0]"},
		"system tool use":         {ir.Request{System: []ir.Part{toolUse("a", "f", "{}")}, Messages: ok}, "m", "system"},
		"tool use without id":     {ir.Request{Messages: []ir.Message{user(text("x")), assistant(toolUse("", "f", "{}"))}}, "m", "tool_use.id"},
		"tool use without name":   {ir.Request{Messages: []ir.Message{user(text("x")), assistant(toolUse("a", "", "{}"))}}, "m", "tool_use.name"},
		"tool use input not JSON": {ir.Request{Messages: []ir.Message{user(text("x")), assistant(toolUse("a", "f", `{"`+secret))}}, "m", "tool_use.input"},
		"tool use input an array": {ir.Request{Messages: []ir.Message{user(text("x")), assistant(toolUse("a", "f", `[]`))}}, "m", "tool_use.input"},
		"result without id":       {ir.Request{Messages: []ir.Message{user(toolResult("", secret))}}, "m", "tool_result.id"},
		"tool without name":       {ir.Request{Messages: ok, Tools: []ir.Tool{{Description: secret}}}, "m", "tools[0].name"},
		"schema not JSON":         {ir.Request{Messages: ok, Tools: []ir.Tool{{Name: "f", Schema: json.RawMessage(`{"type":` + secret)}}}, "m", "tools[0].schema"},
		"schema not an object":    {ir.Request{Messages: ok, Tools: []ir.Tool{{Name: "f", Schema: json.RawMessage(`"string"`)}}}, "m", "tools[0].schema"},
		"schema closes the body":  {ir.Request{Messages: ok, Tools: []ir.Tool{{Name: "f", Schema: json.RawMessage(`{}}}],"model":"other","x":[{"y":{"z":{}`)}}}, "m", "tools[0].schema"},
		"schema too deep":         {ir.Request{Messages: ok, Tools: []ir.Tool{{Name: "f", Schema: deep}}}, "m", "tools[0].schema"},
		"negative max tokens":     {ir.Request{Messages: ok, MaxTokens: -1}, "m", "max_tokens"},
		"temperature NaN":         {ir.Request{Messages: ok, Temperature: &nan}, "m", "temperature"},
		"top_p NaN":               {ir.Request{Messages: ok, TopP: &nan}, "m", "top_p"},
	}
	for name, c := range cases {
		body, dropped, err := EncodeRequest(c.req, c.model)
		if err == nil || body != nil || dropped != nil {
			t.Errorf("%s: body %s, dropped %v, err %v", name, body, dropped, err)
			continue
		}
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: err = %v, want ErrUnsupported naming %q", name, err, c.field)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error carries request content: %v", name, err)
		}
	}
}

func TestEncodeRequest_Limits(t *testing.T) {
	many := func(n int, p ir.Part) []ir.Part {
		out := make([]ir.Part, n)
		for i := range out {
			out[i] = p
		}
		return out
	}
	msgs := make([]ir.Message, ir.MaxMessages+1)
	for i := range msgs {
		msgs[i] = user(text("x"))
	}
	calls := make([]ir.Part, ir.MaxToolCalls+1)
	for i := range calls {
		calls[i] = toolUse(fmt.Sprintf("c%d", i), "f", "{}")
	}
	tools := make([]ir.Tool, ir.MaxTools+1)
	for i := range tools {
		tools[i] = ir.Tool{Name: fmt.Sprintf("f%d", i)}
	}
	bigArgs := `{"a":"` + strings.Repeat("x", ir.MaxToolArgsBytes) + `"}`
	ok := []ir.Message{user(text("x"))}
	cases := map[string]ir.Request{
		"messages":      {Messages: msgs},
		"parts":         {Messages: []ir.Message{user(many(ir.MaxParts+1, text("x"))...)}},
		"system parts":  {System: many(ir.MaxParts+1, text("x")), Messages: ok},
		"tools":         {Messages: ok, Tools: tools},
		"tool calls":    {Messages: []ir.Message{user(text("x")), assistant(calls...)}},
		"tool argument": {Messages: []ir.Message{user(text("x")), assistant(toolUse("c", "f", bigArgs))}},
	}
	for name, req := range cases {
		if body, _, err := EncodeRequest(req, "m"); !errors.Is(err, ir.ErrLimit) || body != nil {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// At the limit it goes through.
	if _, _, err := EncodeRequest(ir.Request{Messages: msgs[:ir.MaxMessages], Tools: tools[:ir.MaxTools]}, "m"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EncodeRequest(ir.Request{Messages: []ir.Message{user(text("x")), assistant(calls[:ir.MaxToolCalls]...)}}, "m"); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeRequest_DoesNotChangeItsInput(t *testing.T) {
	dropped := make([]string, 1, 8)
	dropped[0] = "top_k"
	req := ir.Request{Messages: []ir.Message{user(text("x")), assistant(ir.Part{Kind: ir.Thinking, Text: "t"}, text("y"))}, Dropped: dropped}
	_, got, err := EncodeRequest(req, "m")
	if err != nil || !reflect.DeepEqual(got, []string{"thinking", "top_k"}) {
		t.Fatalf("dropped = %v, err = %v", got, err)
	}
	if len(req.Dropped) != 1 || dropped[:2][1] != "" {
		t.Fatalf("the request's list was written to: %q", dropped[:2])
	}
}

// ---------------------------------------------------------------- answer

func TestDecodeResponse(t *testing.T) {
	got, err := DecodeResponse(fixture(t, "resp_text.json"))
	want := ir.Response{ID: "chatcmpl-1", Model: "m-chat", Parts: []ir.Part{{Kind: ir.Text, Text: "Hello there."}},
		Stop: ir.StopEnd, Usage: ir.Usage{InputTokens: 12, OutputTokens: 3}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("resp_text: %+v, %v", got, err)
	}

	got, err = DecodeResponse(fixture(t, "resp_tools.json"))
	want = ir.Response{ID: "chatcmpl-2", Model: "m-chat", Stop: ir.StopToolUse, Usage: ir.Usage{InputTokens: 40, OutputTokens: 18},
		Parts: []ir.Part{
			{Kind: ir.ToolUse, ToolID: "call_a", ToolName: "read_file", Input: json.RawMessage(`{"path":"a.txt"}`)},
			{Kind: ir.ToolUse, ToolID: "call_b", ToolName: "read_file", Input: json.RawMessage(`{"path":"b.txt"}`)},
		}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("resp_tools: %+v, %v", got, err)
	}
}

func answer(message, finish string) []byte {
	return []byte(`{"id":"i","model":"m","choices":[{"index":0,"message":` + message + `,"finish_reason":` + finish + `}]}`)
}

func TestDecodeResponse_Mapping(t *testing.T) {
	cases := []struct {
		name  string
		body  []byte
		parts []ir.Part
		stop  ir.StopReason
	}{
		{"reasoning_content first", answer(`{"content":"hi","reasoning_content":"let me think"}`, `"stop"`),
			[]ir.Part{{Kind: ir.Thinking, Text: "let me think"}, {Kind: ir.Text, Text: "hi"}}, ir.StopEnd},
		{"reasoning", answer(`{"content":"hi","reasoning":"hm"}`, `"stop"`),
			[]ir.Part{{Kind: ir.Thinking, Text: "hm"}, {Kind: ir.Text, Text: "hi"}}, ir.StopEnd},
		{"reasoning that is no string is not text", answer(`{"content":"hi","reasoning":{"effort":"high"}}`, `"stop"`),
			[]ir.Part{{Kind: ir.Text, Text: "hi"}}, ir.StopEnd},
		{"length", answer(`{"content":"hi"}`, `"length"`), []ir.Part{{Kind: ir.Text, Text: "hi"}}, ir.StopMaxTokens},
		{"content_filter", answer(`{"content":""}`, `"content_filter"`), nil, ir.StopRefusal},
		{"weird", answer(`{"content":"hi"}`, `"weird"`), []ir.Part{{Kind: ir.Text, Text: "hi"}}, ir.StopUnknown},
		{"null finish", answer(`{"content":"hi"}`, `null`), []ir.Part{{Kind: ir.Text, Text: "hi"}}, ir.StopUnknown},
		// A stop reason that promises tools the answer does not hold would leave the caller waiting.
		{"tool_calls without a call", answer(`{"content":"x"}`, `"tool_calls"`), []ir.Part{{Kind: ir.Text, Text: "x"}}, ir.StopEnd},
		{"function_call without a call", answer(`{"content":null,"function_call":null}`, `"function_call"`), nil, ir.StopEnd},
		{"empty tool_calls", answer(`{"content":"x","tool_calls":[]}`, `"tool_calls"`), []ir.Part{{Kind: ir.Text, Text: "x"}}, ir.StopEnd},
		{"empty arguments", answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":""}}]}`, `"tool_calls"`),
			[]ir.Part{{Kind: ir.ToolUse, ToolID: "a", ToolName: "f", Input: json.RawMessage(`{}`)}}, ir.StopToolUse},
		{"no arguments", answer(`{"tool_calls":[{"id":"a","function":{"name":"f"}}]}`, `"tool_calls"`),
			[]ir.Part{{Kind: ir.ToolUse, ToolID: "a", ToolName: "f", Input: json.RawMessage(`{}`)}}, ir.StopToolUse},
		{"arguments as an object", answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":{ "n":12345678901234567890123 }}}]}`, `"tool_calls"`),
			[]ir.Part{{Kind: ir.ToolUse, ToolID: "a", ToolName: "f", Input: json.RawMessage(`{ "n":12345678901234567890123 }`)}}, ir.StopToolUse},
		{"arguments keep their bytes", answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{ \"n\":12345678901234567890123, \"f\":1.10,\"s\":\"\\u00e9\\u0000\" }"}}]}`, `"tool_calls"`),
			[]ir.Part{{Kind: ir.ToolUse, ToolID: "a", ToolName: "f", Input: json.RawMessage(`{ "n":12345678901234567890123, "f":1.10,"s":"\u00e9\u0000" }`)}}, ir.StopToolUse},
		// A provider that says "stop" although it asks for tools: the caller acts on the stop reason.
		{"tool calls with stop", answer(`{"content":"x","tool_calls":[{"id":"a","function":{"name":"f","arguments":"{}"}}]}`, `"stop"`),
			[]ir.Part{{Kind: ir.Text, Text: "x"}, {Kind: ir.ToolUse, ToolID: "a", ToolName: "f", Input: json.RawMessage(`{}`)}}, ir.StopToolUse},
		{"tool calls cut off stay cut off", answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{}"}}]}`, `"length"`),
			[]ir.Part{{Kind: ir.ToolUse, ToolID: "a", ToolName: "f", Input: json.RawMessage(`{}`)}}, ir.StopMaxTokens},
		{"content as text parts", answer(`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, `"stop"`),
			[]ir.Part{{Kind: ir.Text, Text: "ab"}}, ir.StopEnd},
		{"refusal", answer(`{"content":null,"refusal":"I cannot help with that."}`, `"stop"`),
			[]ir.Part{{Kind: ir.Text, Text: "I cannot help with that."}}, ir.StopRefusal},
		{"null everywhere", []byte(`{"id":null,"object":null,"model":null,"choices":[{"index":null,"message":{"role":null,"content":null,"refusal":null,"reasoning":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null}],"usage":null,"error":null}`),
			nil, ir.StopUnknown},
		{"text is kept exactly", answer(`{"content":" \u0000\ttab \"q\" é 日本 😀 \ud83d\ude00 \n"}`, `"stop"`),
			[]ir.Part{{Kind: ir.Text, Text: " \x00\ttab \"q\" é 日本 😀 😀 \n"}}, ir.StopEnd},
		{"only the first choice", []byte(`{"choices":[{"index":1,"message":{"content":"second"},"finish_reason":"length"},{"index":0,"message":{"content":"first"},"finish_reason":"stop"}]}`),
			[]ir.Part{{Kind: ir.Text, Text: "first"}}, ir.StopEnd},
	}
	for _, c := range cases {
		got, err := DecodeResponse(c.body)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got.Parts, c.parts) || got.Stop != c.stop {
			t.Errorf("%s: parts %+v, stop %q", c.name, got.Parts, got.Stop)
		}
	}
}

func TestDecodeResponse_Usage(t *testing.T) {
	cases := map[string]ir.Usage{
		`{"prompt_tokens":12,"completion_tokens":3}`:                               {InputTokens: 12, OutputTokens: 3},
		`{"prompt_tokens":9007199254740993,"completion_tokens":1}`:                 {InputTokens: 9007199254740993, OutputTokens: 1}, // not through a float
		`{"prompt_tokens":"12","completion_tokens":"3"}`:                           {InputTokens: 12, OutputTokens: 3},
		`{"prompt_tokens":12.0,"completion_tokens":3e1}`:                           {InputTokens: 12, OutputTokens: 30},
		`{"prompt_tokens":-5,"completion_tokens":1.5}`:                             {},
		`{"prompt_tokens":1e400,"completion_tokens":99999999999999999999999}`:      {},
		`{"prompt_tokens":null,"completion_tokens":[1]}`:                           {},
		`{"prompt_tokens":"many","completion_tokens":{"n":1},"total_tokens":true}`: {},
		`{}`:   {},
		`null`: {},
	}
	for usage, want := range cases {
		got, err := DecodeResponse([]byte(`{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":` + usage + `}`))
		if err != nil || got.Usage != want {
			t.Errorf("%s: %+v, %v", usage, got.Usage, err)
		}
	}
}

func TestDecodeResponse_Refused(t *testing.T) {
	const secret = "SECRET-ANSWER-CONTENT"
	calls := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `%s{"id":"c%d","function":{"name":"f","arguments":"{}"}}`, map[bool]string{true: ",", false: ""}[i > 0], i)
		}
		return `{"tool_calls":[` + b.String() + `]}`
	}
	if _, err := DecodeResponse(answer(calls(ir.MaxToolCalls), `"tool_calls"`)); err != nil {
		t.Fatalf("%d tool calls: %v", ir.MaxToolCalls, err)
	}
	malformed := map[string][]byte{
		"no choices":                 []byte(`{"choices":[]}`),
		"empty object":               []byte(`{}`),
		"not JSON":                   []byte(`nope ` + secret),
		"empty":                      nil,
		"null":                       []byte(`null`),
		"an array":                   []byte(`[]`),
		"a string":                   []byte(`"` + secret + `"`),
		"cut off":                    []byte(`{"choices":[{"message":{"content":"` + secret),
		"trailing data":              append(answer(`{"content":"x"}`, `"stop"`), "{}"...),
		"the legacy function_call":   answer(`{"content":null,"function_call":{"name":"f","arguments":"{}"}}`, `"function_call"`),
		"choices of another type":    []byte(`{"choices":{"message":{"content":"x"}}}`),
		"no message":                 []byte(`{"choices":[{"index":0,"finish_reason":"stop"}]}`),
		"no first choice":            []byte(`{"choices":[{"index":3,"message":{"content":"x"}}]}`),
		"content a number":           answer(`{"content":42}`, `"stop"`),
		"content an object":          answer(`{"content":{"text":"`+secret+`"}}`, `"stop"`),
		"content part of a new type": answer(`{"content":[{"type":"image_url","image_url":{"url":"`+secret+`"}}]}`, `"stop"`),
		"content part text a number": answer(`{"content":[{"type":"text","text":1}]}`, `"stop"`),
		"id a number":                []byte(`{"id":7,"choices":[{"message":{"content":"x"}}]}`),
		"finish_reason a number":     answer(`{"content":"x"}`, `7`),
		"refusal a number":           answer(`{"content":"x","refusal":7}`, `"stop"`),
		"tool_calls an object":       answer(`{"tool_calls":{"id":"a"}}`, `"tool_calls"`),
		"tool call a string":         answer(`{"tool_calls":["`+secret+`"]}`, `"tool_calls"`),
		"tool call without function": answer(`{"tool_calls":[{"id":"a"}]}`, `"tool_calls"`),
		"tool call without name":     answer(`{"tool_calls":[{"id":"a","function":{"arguments":"{}"}}]}`, `"tool_calls"`),
		"tool call with null name":   answer(`{"tool_calls":[{"id":"a","function":{"name":null,"arguments":"{}"}}]}`, `"tool_calls"`),
		"tool call without id":       answer(`{"tool_calls":[{"function":{"name":"f","arguments":"{}"}}]}`, `"tool_calls"`),
		"tool call id a number":      answer(`{"tool_calls":[{"id":1,"function":{"name":"f","arguments":"{}"}}]}`, `"tool_calls"`),
		"tool call name a number":    answer(`{"tool_calls":[{"id":"a","function":{"name":1,"arguments":"{}"}}]}`, `"tool_calls"`),
		"duplicate tool call ids":    answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{}"}},{"id":"a","function":{"name":"g","arguments":"{}"}}]}`, `"tool_calls"`),
		"arguments not JSON":         answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{not json `+secret+`"}}]}`, `"tool_calls"`),
		"arguments an array":         answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"[1]"}}]}`, `"tool_calls"`),
		"arguments a number":         answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":7}}]}`, `"tool_calls"`),
		"arguments two objects":      answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{}{}"}}]}`, `"tool_calls"`),
	}
	for name, body := range malformed {
		got, err := DecodeResponse(body)
		if !errors.Is(err, ErrMalformed) || !reflect.DeepEqual(got, ir.Response{}) {
			t.Errorf("%s: %+v, err = %v", name, got, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error carries answer content: %v", name, err)
		}
	}

	deepArgs := strings.Repeat(`{\"a\":`, ir.MaxDepth+1) + `1` + strings.Repeat(`}`, ir.MaxDepth+1)
	parts := `{"content":[` + strings.Repeat(`{"type":"text","text":"x"},`, ir.MaxParts) + `{"type":"text","text":"x"}]}`
	over := map[string][]byte{
		"too many tool calls":    answer(calls(ir.MaxToolCalls+1), `"tool_calls"`),
		"large arguments":        answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{\"a\":\"`+strings.Repeat("x", ir.MaxToolArgsBytes)+`\"}"}}]}`, `"tool_calls"`),
		"large object arguments": answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":{"a":"`+strings.Repeat("x", ir.MaxToolArgsBytes)+`"}}}]}`, `"tool_calls"`),
		"deep arguments":         answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"`+deepArgs+`"}}]}`, `"tool_calls"`),
		"deep body":              []byte(`{"choices":[{"message":{"content":"x"}}],"x":` + strings.Repeat("[", ir.MaxDepth+1) + strings.Repeat("]", ir.MaxDepth+1) + `}`),
		"very deep body":         []byte(strings.Repeat("[", 5_000_000)),
		"too many content parts": answer(parts, `"stop"`),
		"large body":             append(answer(`{"content":"x"}`, `"stop"`), bytes.Repeat([]byte(" "), MaxResponseBytes)...),
	}
	for name, body := range over {
		got, err := DecodeResponse(body)
		if !errors.Is(err, ir.ErrLimit) || !reflect.DeepEqual(got, ir.Response{}) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestDecodeResponse_ErrorObject(t *testing.T) {
	const secret = "PROVIDER-SAID-THIS"
	bodies := map[string]string{
		`{"error":{"message":"` + secret + `","type":"server_error"}}`:                   secret,
		`{"error":"` + secret + `"}`:                                                     secret,
		`{"error":{"code":500}}`:                                                         "the provider reported an error",
		`{"error":{"message":"` + secret + `"},"choices":[{"message":{"content":"x"}}]}`: secret,
		`{"error":{"message":"` + strings.Repeat("é", 400) + `"}}`:                       strings.Repeat("é", 150),
	}
	for body, want := range bodies {
		got, err := DecodeResponse([]byte(body))
		var se *ir.StreamError
		if !errors.As(err, &se) || !errors.Is(err, ir.ErrStream) || se.Message != want || !reflect.DeepEqual(got, ir.Response{}) {
			t.Errorf("%.50s: err = %v (%+v)", body, err, se)
			continue
		}
		// The message is for the caller, through the field; the error's own text is fixed.
		if strings.Contains(fmt.Sprintf("%v %s %q", err, err, err.Error()), secret) {
			t.Errorf("%.50s: the error text carries the provider's message", body)
		}
	}
	if _, err := DecodeResponse([]byte(`{"error":null,"choices":[{"message":{"content":"x"}}]}`)); err != nil {
		t.Fatalf("error null: %v", err)
	}
}

func TestDecodeResponse_TotalArgumentsLimit(t *testing.T) {
	arg := `{\"a\":\"` + strings.Repeat("x", ir.MaxToolArgsBytes-16) + `\"}`
	body := func(n int) []byte {
		var calls []string
		for i := 0; i < n; i++ {
			calls = append(calls, fmt.Sprintf(`{"id":"c%d","function":{"name":"f","arguments":"%s"}}`, i, arg))
		}
		return answer(`{"tool_calls":[`+strings.Join(calls, ",")+`]}`, `"tool_calls"`)
	}
	if _, err := DecodeResponse(body(4)); err != nil {
		t.Fatalf("four calls: %v", err)
	}
	if _, err := DecodeResponse(body(5)); !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("five calls: %v", err)
	}
}

func TestEncodeRequest_AssistantTurnWithNothingToSayIsLeftOut(t *testing.T) {
	// "content":"" without tool calls is refused by some servers. The thinking is reported;
	// two user messages in a row are fine for Chat Completions.
	thinking := ir.Part{Kind: ir.Thinking, Text: "hm"}
	body, dropped, err := EncodeRequest(ir.Request{Messages: []ir.Message{
		user(text("a")), assistant(thinking, thinking), user(text("b")), assistant(), user(text("c")),
	}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"user","content":"b"},{"role":"user","content":"c"}]}` {
		t.Fatalf("body = %s", body)
	}
	if !reflect.DeepEqual(dropped, []string{"thinking"}) {
		t.Fatalf("dropped = %v", dropped)
	}
	// Nothing left to send is a request without messages, with or without a system prompt.
	for _, req := range []ir.Request{
		{Messages: []ir.Message{assistant(thinking)}},
		{System: []ir.Part{text("s")}, Messages: []ir.Message{assistant(thinking), assistant()}},
	} {
		if body, _, err := EncodeRequest(req, "m"); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "messages") || body != nil {
			t.Fatalf("body %s, err = %v", body, err)
		}
	}
}

// ---------------------------------------------------------------- stream

func textEvents() []ir.Event {
	return []ir.Event{
		{Kind: ir.Start, ID: "chatcmpl-3", Model: "m-chat"},
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Text}},
		{Kind: ir.TextDelta, Index: 0, Text: "Hel"},
		{Kind: ir.TextDelta, Index: 0, Text: "lo."},
		{Kind: ir.PartStop, Index: 0},
		{Kind: ir.Finish, Stop: ir.StopEnd, Usage: ir.Usage{InputTokens: 12, OutputTokens: 2}},
	}
}

func toolsEvents() []ir.Event {
	return []ir.Event{
		{Kind: ir.Start, ID: "chatcmpl-4", Model: "m-chat"},
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Text}},
		{Kind: ir.TextDelta, Index: 0, Text: "Let me look."},
		{Kind: ir.PartStop, Index: 0},
		{Kind: ir.PartStart, Index: 1, Part: ir.Part{Kind: ir.ToolUse, ToolID: "call_a", ToolName: "read_file"}},
		{Kind: ir.ToolArgsDelta, Index: 1, ArgsJSON: `{"pa`},
		{Kind: ir.PartStart, Index: 2, Part: ir.Part{Kind: ir.ToolUse, ToolID: "call_b", ToolName: "list_dir"}},
		{Kind: ir.ToolArgsDelta, Index: 2, ArgsJSON: `{"dir":`},
		{Kind: ir.ToolArgsDelta, Index: 1, ArgsJSON: `th":"a.txt"}`},
		{Kind: ir.ToolArgsDelta, Index: 2, ArgsJSON: `"."}`},
		{Kind: ir.PartStop, Index: 1},
		{Kind: ir.PartStop, Index: 2},
		{Kind: ir.Finish, Stop: ir.StopToolUse, Usage: ir.Usage{InputTokens: 50, OutputTokens: 21}},
	}
}

func TestStreamDecoder_Text(t *testing.T) {
	raw := fixture(t, "stream_text.sse")
	for _, piece := range []int{len(raw), 1, 5, 64} {
		got, err := decodeStream(t, raw, piece)
		if err != nil || !reflect.DeepEqual(got, textEvents()) {
			t.Fatalf("piece %d: %v\n%+v", piece, err, got)
		}
	}
	// Close after a finished stream returns nothing, and so does a later Feed.
	d := NewStreamDecoder()
	if _, err := feedAll(d, chunk(`{"content":"x"}`), finishChunk("stop"), []byte("[DONE]")); err != nil {
		t.Fatal(err)
	}
	if evs := d.Close(); evs != nil {
		t.Fatalf("Close = %+v", evs)
	}
	if evs, err := d.Feed(chunk(`{"content":"late"}`)); evs != nil || err != nil {
		t.Fatalf("Feed after the finish = %+v, %v", evs, err)
	}
}

func TestStreamDecoder_InterleavedToolCalls(t *testing.T) {
	raw := fixture(t, "stream_tools.sse")
	for _, piece := range []int{len(raw), 1, 5, 64} {
		got, err := decodeStream(t, raw, piece)
		if err != nil || !reflect.DeepEqual(got, toolsEvents()) {
			t.Fatalf("piece %d: %v\n%+v", piece, err, got)
		}
	}
	resp, err := ir.Collect(toolsEvents())
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Parts[1].Input) != `{"path":"a.txt"}` || string(resp.Parts[2].Input) != `{"dir":"."}` {
		t.Fatalf("inputs = %s / %s", resp.Parts[1].Input, resp.Parts[2].Input)
	}
}

func TestStreamDecoder_CRLFAndComments(t *testing.T) {
	raw := bytes.ReplaceAll(fixture(t, "stream_tools.sse"), []byte("\n"), []byte("\r\n"))
	raw = append([]byte(": OPENROUTER PROCESSING\r\n\r\nthis line has no field\r\n\r\ndata:\r\n\r\ndata:   \r\n\r\n"), raw...)
	for _, piece := range []int{len(raw), 1, 7} {
		got, err := decodeStream(t, raw, piece)
		if err != nil || !reflect.DeepEqual(got, toolsEvents()) {
			t.Fatalf("piece %d: %v\n%+v", piece, err, got)
		}
	}
}

func TestStreamDecoder_Thinking(t *testing.T) {
	d := NewStreamDecoder()
	got, err := feedAll(d,
		chunk(`{"role":"assistant","reasoning_content":"Let me "}`),
		chunk(`{"reasoning":"think."}`),
		chunk(`{"reasoning_content":"","content":""}`),
		chunk(`{"content":"Done."}`),
		chunk(`{"reasoning_content":"More."}`),
		finishChunk("length"),
		[]byte("[DONE]"))
	want := []ir.Event{
		{Kind: ir.Start, ID: "c", Model: "m"},
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Thinking}},
		{Kind: ir.ThinkingDelta, Index: 0, Text: "Let me "},
		{Kind: ir.ThinkingDelta, Index: 0, Text: "think."},
		{Kind: ir.PartStop, Index: 0},
		{Kind: ir.PartStart, Index: 1, Part: ir.Part{Kind: ir.Text}},
		{Kind: ir.TextDelta, Index: 1, Text: "Done."},
		{Kind: ir.PartStop, Index: 1},
		{Kind: ir.PartStart, Index: 2, Part: ir.Part{Kind: ir.Thinking}},
		{Kind: ir.ThinkingDelta, Index: 2, Text: "More."},
		{Kind: ir.PartStop, Index: 2},
		{Kind: ir.Finish, Stop: ir.StopMaxTokens},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v\n%+v", err, got)
	}
}

func TestStreamDecoder_BadEndings(t *testing.T) {
	const early = "the provider ended the stream early"

	// The upstream stops after two text chunks.
	d := NewStreamDecoder()
	if _, err := feedAll(d, chunk(`{"content":"Hel"}`), chunk(`{"content":"lo"}`)); err != nil {
		t.Fatal(err)
	}
	want := []ir.Event{{Kind: ir.PartStop, Index: 0}, {Kind: ir.Error, Err: early}}
	if got := d.Close(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Close = %+v", got)
	}
	if got := d.Close(); got != nil {
		t.Fatalf("second Close = %+v", got)
	}

	// It stops in the middle of two tool calls: both are closed, in order, then the error.
	d = NewStreamDecoder()
	if _, err := feedAll(d,
		chunk(`{"content":"x"}`),
		chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{"}}]}`),
		chunk(`{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":"{"}}]}`)); err != nil {
		t.Fatal(err)
	}
	want = []ir.Event{{Kind: ir.PartStop, Index: 1}, {Kind: ir.PartStop, Index: 2}, {Kind: ir.Error, Err: early}}
	if got := d.Close(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Close = %+v", got)
	}

	// No frame at all.
	d = NewStreamDecoder()
	if got := d.Close(); !reflect.DeepEqual(got, []ir.Event{{Kind: ir.Error, Err: early}}) {
		t.Fatalf("Close = %+v", got)
	}

	// [DONE] with nothing before it is not a finished answer either.
	d = NewStreamDecoder()
	if got, err := d.Feed([]byte("[DONE]")); err != nil || !reflect.DeepEqual(got, []ir.Event{{Kind: ir.Error, Err: early}}) {
		t.Fatalf("Feed = %+v, %v", got, err)
	}

	// [DONE] without a finish_reason: the model never said it was done.
	d = NewStreamDecoder()
	got, err := feedAll(d, chunk(`{"content":"Hel"}`), []byte("[DONE]"))
	if err != nil || kinds(got) != "start part_start text_delta part_stop error" {
		t.Fatalf("%v: %s", err, kinds(got))
	}

	// A frame that is not JSON.
	d = NewStreamDecoder()
	if _, err := feedAll(d, chunk(`{"content":"x"}`), []byte(`{"id":"c","choices":[{"delta":{"content":"SECRET`)); !errors.Is(err, ErrMalformed) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
	// After an error the decoder is finished: the caller ends the stream.
	if evs, err := d.Feed(chunk(`{"content":"y"}`)); evs != nil || err != nil {
		t.Fatalf("Feed after an error = %+v, %v", evs, err)
	}
	if evs := d.Close(); evs != nil {
		t.Fatalf("Close after an error = %+v", evs)
	}
}

func TestStreamDecoder_FinishReasonWithoutDone(t *testing.T) {
	// Several servers end the stream after the last chunk without "[DONE]". The model said
	// why it stopped, so the answer is complete.
	d := NewStreamDecoder()
	got, err := feedAll(d, chunk(`{"content":"Hi"}`), finishChunk("stop"),
		[]byte(`{"id":"c","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1}}`))
	if err != nil || kinds(got) != "start part_start text_delta part_stop" {
		t.Fatalf("%v: %s", err, kinds(got))
	}
	want := []ir.Event{{Kind: ir.Finish, Stop: ir.StopEnd, Usage: ir.Usage{InputTokens: 5, OutputTokens: 1}}}
	if got := d.Close(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Close = %+v", got)
	}
}

func TestStreamDecoder_ErrorObject(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes; a cut must not split a character
	cases := map[string]string{
		`{"error":{"message":"overloaded"}}`:                                                  "overloaded",
		`{"error":{"message":"overloaded","code":429},"choices":[{"delta":{"content":"x"}}]}`: "overloaded",
		`{"error":"quota exceeded"}`:                                                          "quota exceeded",
		`{"error":{"code":500}}`:                                                              "the provider reported an error",
		`{"error":{"message":7}}`:                                                             "the provider reported an error",
		`{"error":{"message":""}}`:                                                            "the provider reported an error",
		`{"error":true}`:                                                                      "the provider reported an error",
		`{"error":{"message":"` + long + `"}}`:                                                strings.Repeat("é", 150),
	}
	for frame, msg := range cases {
		d := NewStreamDecoder()
		got, err := feedAll(d, chunk(`{"content":"x"}`), []byte(frame), chunk(`{"content":"later"}`), []byte("[DONE]"))
		if err != nil || kinds(got) != "start part_start text_delta part_stop error" || got[4].Err != msg {
			t.Errorf("%.60s: %v: %s / %.80q", frame, err, kinds(got), got[len(got)-1].Err)
		}
		if evs := d.Close(); evs != nil {
			t.Errorf("%.60s: Close = %+v", frame, evs)
		}
	}
	// "error": null is no error.
	d := NewStreamDecoder()
	got, err := feedAll(d, []byte(`{"id":"c","error":null,"choices":[{"delta":{"content":"x"}}]}`))
	if err != nil || kinds(got) != "start part_start text_delta" {
		t.Fatalf("%v: %s", err, kinds(got))
	}
}

func TestStreamDecoder_ToolCallShapes(t *testing.T) {
	startA := ir.Event{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.ToolUse, ToolID: "a", ToolName: "f"}}
	startB := ir.Event{Kind: ir.PartStart, Index: 1, Part: ir.Part{Kind: ir.ToolUse, ToolID: "b", ToolName: "g"}}
	args := func(i int, s string) ir.Event { return ir.Event{Kind: ir.ToolArgsDelta, Index: i, ArgsJSON: s} }
	stop := func(i int) ir.Event { return ir.Event{Kind: ir.PartStop, Index: i} }
	begin := ir.Event{Kind: ir.Start, ID: "c", Model: "m"}
	end := ir.Event{Kind: ir.Finish, Stop: ir.StopToolUse}
	cases := []struct {
		name   string
		frames []string
		want   []ir.Event
	}{
		{"an absurd index is a key, not a length",
			[]string{`{"tool_calls":[{"index":1000000000,"id":"a","function":{"name":"f","arguments":"{}"}}]}`,
				`{"tool_calls":[{"index":1e9,"function":{"arguments":" "}}]}`},
			[]ir.Event{begin, startA, args(0, "{}"), args(0, " "), stop(0), end}},
		{"the index as a string",
			[]string{`{"tool_calls":[{"index":"0","id":"a","function":{"name":"f","arguments":"{"}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}`},
			[]ir.Event{begin, startA, args(0, "{"), args(0, "}"), stop(0), end}},
		{"no index: a new id is a new call, no id continues the last",
			[]string{`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{"}}]}`,
				`{"tool_calls":[{"function":{"arguments":"}"}}]}`,
				`{"tool_calls":[{"id":"b","function":{"name":"g","arguments":"{}"}}]}`,
				`{"tool_calls":[{"id":"a","function":{"arguments":" "}}]}`},
			[]ir.Event{begin, startA, args(0, "{"), args(0, "}"), startB, args(1, "{}"), args(0, " "), stop(0), stop(1), end}},
		{"every call numbered 0: a new id is a new call",
			[]string{`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}}]}`,
				`{"tool_calls":[{"index":0,"id":"b","function":{"name":"g","arguments":"{}"}}]}`},
			[]ir.Event{begin, startA, args(0, "{}"), startB, args(1, "{}"), stop(0), stop(1), end}},
		{"the id repeated on every chunk",
			[]string{`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{"}}]}`,
				`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"}"}}]}`},
			[]ir.Event{begin, startA, args(0, "{"), args(0, "}"), stop(0), end}},
		{"the name after the first arguments: the part opens when it can be named",
			[]string{`{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}}]}`,
				`{"tool_calls":[{"index":0,"id":"a","function":{"arguments":":1"}}]}`,
				`{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"}"}}]}`},
			[]ir.Event{begin, startA, args(0, `{"a":1}`), stop(0), end}},
		{"arguments as an object in one chunk",
			[]string{`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":{ "n":12345678901234567890123 }}}]}`},
			[]ir.Event{begin, startA, args(0, `{ "n":12345678901234567890123 }`), stop(0), end}},
		{"two calls in one chunk, nulls around them",
			[]string{`{"content":null,"tool_calls":[{"index":0,"id":"a","type":null,"function":{"name":"f","arguments":null}},{"index":1,"id":"b","function":{"name":"g"}}]}`,
				`{"tool_calls":null}`, `null`},
			[]ir.Event{begin, startA, startB, stop(0), stop(1), end}},
	}
	for _, c := range cases {
		d := NewStreamDecoder()
		var frames [][]byte
		for _, f := range c.frames {
			frames = append(frames, chunk(f))
		}
		frames = append(frames, finishChunk("tool_calls"), []byte("[DONE]"))
		got, err := feedAll(d, frames...)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v\n got %+v\nwant %+v", c.name, err, got, c.want)
		}
	}
}

func TestStreamDecoder_StopReason(t *testing.T) {
	call := chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}}]}`)
	cases := []struct {
		frames [][]byte
		want   ir.StopReason
	}{
		{[][]byte{chunk(`{"content":"x"}`), finishChunk("stop")}, ir.StopEnd},
		{[][]byte{chunk(`{"content":"x"}`), finishChunk("length")}, ir.StopMaxTokens},
		{[][]byte{chunk(`{"content":"x"}`), finishChunk("content_filter")}, ir.StopRefusal},
		// A stop reason that promises tools although no call was opened.
		{[][]byte{chunk(`{"content":"x"}`), finishChunk("function_call")}, ir.StopEnd},
		{[][]byte{chunk(`{"content":"x"}`), finishChunk("tool_calls")}, ir.StopEnd},
		{[][]byte{chunk(`{"content":"x"}`), finishChunk("eos")}, ir.StopUnknown},
		{[][]byte{call, finishChunk("tool_calls")}, ir.StopToolUse},
		{[][]byte{call, finishChunk("stop")}, ir.StopToolUse}, // the caller acts on the stop reason
		{[][]byte{call, finishChunk("length")}, ir.StopMaxTokens},
		{[][]byte{chunk(`{"refusal":"No."}`), finishChunk("stop")}, ir.StopRefusal},
	}
	for i, c := range cases {
		d := NewStreamDecoder()
		got, err := feedAll(d, append(c.frames, []byte(" [DONE] \n"))...)
		if err != nil || len(got) == 0 || got[len(got)-1].Kind != ir.Finish || got[len(got)-1].Stop != c.want {
			t.Errorf("case %d: %v: %+v", i, err, got)
		}
	}
}

func TestStreamDecoder_Refused(t *testing.T) {
	open := chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{"}}]}`)
	malformed := map[string][][]byte{
		"not JSON":                      {[]byte(`{"choices":`)},
		"a string":                      {[]byte(`"hello"`)},
		"an array":                      {[]byte(`[]`)},
		"choices an object":             {[]byte(`{"id":"c","choices":{"delta":{}}}`)},
		"delta a string":                {[]byte(`{"id":"c","choices":[{"delta":"x"}]}`)},
		"content a number":              {chunk(`{"content":7}`)},
		"content an array":              {chunk(`{"content":[{"type":"text","text":"x"}]}`)},
		"tool_calls an object":          {chunk(`{"tool_calls":{"index":0}}`)},
		"tool call id a number":         {chunk(`{"tool_calls":[{"index":0,"id":7,"function":{"name":"f"}}]}`)},
		"tool call name a number":       {chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":7}}]}`)},
		"arguments a number":            {chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":7}}]}`)},
		"a negative index":              {chunk(`{"tool_calls":[{"index":-1,"id":"a","function":{"name":"f"}}]}`)},
		"an index that is no number":    {chunk(`{"tool_calls":[{"index":"first","id":"a","function":{"name":"f"}}]}`)},
		"an index with a fraction":      {chunk(`{"tool_calls":[{"index":0.5,"id":"a","function":{"name":"f"}}]}`)},
		"the same id under two indexes": {open, chunk(`{"tool_calls":[{"index":1,"id":"a","function":{"name":"g","arguments":"{}"}}]}`)},
		"a fragment never opened with a name, at the finish": {chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"arguments":"{}"}}]}`), finishChunk("tool_calls")},
		"the legacy function_call":                           {chunk(`{"function_call":{"name":"f","arguments":"{"}}`)},
		"arguments that are no JSON, at the finish":          {chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{not json"}}]}`), finishChunk("tool_calls")},
		"arguments that are no object, at the finish":        {chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"null"}}]}`), finishChunk("tool_calls")},
		"arguments cut off, at the finish":                   {open, finishChunk("tool_calls")},
		"two objects as arguments":                           {open, chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"}{}"}}]}`), finishChunk("tool_calls")},
		"a fragment for a call the finish closed":            {open, finishChunk("tool_calls"), chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}`)},
		"finish_reason a number":                             {[]byte(`{"id":"c","choices":[{"delta":{},"finish_reason":7}]}`)},
	}
	for name, frames := range malformed {
		d := NewStreamDecoder()
		got, err := feedAll(d, frames...)
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v", name, err)
		}
		// The decoder ended the answer itself: every open part stopped, then one Error with a fixed text.
		if n := len(got); n == 0 || got[n-1].Kind != ir.Error || got[n-1].Err != "the provider sent an answer that cannot be read" {
			t.Errorf("%s: events = %+v", name, got)
		}
		if cerr := checkEvents(got); cerr != nil {
			t.Errorf("%s: %v: %+v", name, cerr, got)
		}
		if evs := d.Close(); evs != nil {
			t.Errorf("%s: Close after an error = %+v", name, evs)
		}
	}
}

func TestStreamDecoder_Limits(t *testing.T) {
	// 64 tool calls pass, the 65th does not.
	d := NewStreamDecoder()
	var err error
	for i := 0; i <= ir.MaxToolCalls && err == nil; i++ {
		_, err = d.Feed(chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"c%d","function":{"name":"f","arguments":"{}"}}]}`, i, i)))
		if i < ir.MaxToolCalls && err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("%d tool calls: %v", ir.MaxToolCalls+1, err)
	}

	// 65 calls that never get a name are refused as well: nothing is kept for them without bound.
	d, err = NewStreamDecoder(), nil
	for i := 0; i <= ir.MaxToolCalls && err == nil; i++ {
		_, err = d.Feed(chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":"{}"}}]}`, i*1000)))
	}
	if !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("unnamed tool calls: %v", err)
	}

	// One call whose arguments add up to more than 1 MiB, in 4 KiB fragments. A call that cannot
	// be opened yet (no name, no id) is bounded the same way.
	for name, c := range map[string]struct {
		first string
		least int
	}{
		"open":         {`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{\"a\":\""}}]}`, ir.MaxToolArgsBytes / 4096},
		"never opened": {`{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":\""}}]}`, ir.MaxToolArgsBytes / 4096},
	} {
		first, least := c.first, c.least
		d, err = NewStreamDecoder(), nil
		if _, err = d.Feed(chunk(first)); err != nil {
			t.Fatal(err)
		}
		frag := chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"` + strings.Repeat("x", 4096) + `"}}]}`)
		n := 0
		for ; n <= ir.MaxToolArgsBytes/4096+1 && err == nil; n++ {
			_, err = d.Feed(frag)
		}
		if !errors.Is(err, ir.ErrLimit) || n < least || n > least+2 {
			t.Fatalf("%s: err = %v after %d fragments", name, err, n)
		}
	}

	// A frame over the frame limit, a frame nested too deep, and too many parts.
	d = NewStreamDecoder()
	if _, err := d.Feed(chunk(`{"content":"` + strings.Repeat("x", MaxFrameBytes) + `"}`)); !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("large frame: %v", err)
	}
	d = NewStreamDecoder()
	if _, err := d.Feed([]byte(`{"id":"c","x":` + strings.Repeat("[", ir.MaxDepth+1) + strings.Repeat("]", ir.MaxDepth+1) + `}`)); !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("deep frame: %v", err)
	}
	d, err = NewStreamDecoder(), nil
	n := 0
	for ; n <= ir.MaxParts && err == nil; n++ {
		if _, err = d.Feed(chunk(`{"content":"x"}`)); err == nil {
			_, err = d.Feed(chunk(`{"reasoning":"y"}`))
		}
	}
	if !errors.Is(err, ir.ErrLimit) || n < ir.MaxParts/2 {
		t.Fatalf("parts: err = %v after %d rounds", err, n)
	}
}

func TestDecoders_ListsOfMillionsAreRefusedBeforeTheyAreHeld(t *testing.T) {
	// A few megabytes of "{}," would decode into hundreds of megabytes of empty structs.
	bomb := strings.Repeat("{},", 1_300_000) + "{}"
	bodies := map[string]string{
		"choices":       `{"choices":[` + bomb + `]}`,
		"tool calls":    `{"choices":[{"message":{"tool_calls":[` + bomb + `]}}]}`,
		"content parts": `{"choices":[{"message":{"content":[` + bomb + `]}}]}`,
	}
	for name, body := range bodies {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err := DecodeResponse([]byte(body))
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ir.ErrLimit) {
			t.Errorf("%s: err = %v", name, err)
		}
		// Copies of the bytes on the way down are fine; a struct per element is not.
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8*uint64(len(body)) {
			t.Errorf("%s: %d bytes allocated for a body of %d", name, grew, len(body))
		}
	}
	frames := map[string]string{
		"choices":    `{"id":"c","choices":[` + bomb + `]}`,
		"tool calls": `{"id":"c","choices":[{"delta":{"tool_calls":[` + bomb + `]}}]}`,
	}
	for name, frame := range frames {
		if _, err := NewStreamDecoder().Feed([]byte(frame)); !errors.Is(err, ir.ErrLimit) {
			t.Errorf("stream %s: err = %v", name, err)
		}
	}
}

func TestStreamDecoder_MultiMegabyteLine(t *testing.T) {
	// One line of 8 MiB with no line ending: the parser refuses it at its limit, in pieces or whole.
	raw := append([]byte("data: "), bytes.Repeat([]byte("x"), 8<<20)...)
	for _, piece := range []int{len(raw), 4096} {
		if _, err := decodeStream(t, raw, piece); !errors.Is(err, sse.ErrTooLarge) {
			t.Fatalf("piece %d: err = %v", piece, err)
		}
	}
}

func TestStreamDecoder_QuietChunks(t *testing.T) {
	// Chunks that carry nothing produce nothing, and a first chunk without id, model or choice
	// (a content-filter preamble) does not start the answer.
	d := NewStreamDecoder()
	got, err := feedAll(d,
		[]byte(`{"id":"","model":"","choices":[],"prompt_filter_results":[{"prompt_index":0}]}`),
		[]byte(``), []byte("  \n"), []byte(`{}`), []byte(`null`),
		[]byte(`{"id":"c","model":"m","choices":null,"usage":null}`),
		[]byte(`{"id":"c2","model":"m2","choices":[{"index":1,"delta":{"content":"another choice"},"finish_reason":"stop"}]}`),
		[]byte(`{"choices":[{"index":0,"delta":null,"finish_reason":null}]}`),
		[]byte(`{"choices":[{"delta":{"role":"assistant","content":null,"refusal":null,"reasoning":null,"tool_calls":[]}}]}`),
		chunk(`{"content":"x"}`), finishChunk("stop"), []byte("[DONE]"))
	want := []ir.Event{
		{Kind: ir.Start, ID: "c", Model: "m"},
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Text}},
		{Kind: ir.TextDelta, Index: 0, Text: "x"},
		{Kind: ir.PartStop, Index: 0},
		{Kind: ir.Finish, Stop: ir.StopEnd},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v\n%+v", err, got)
	}
}

func TestStreamDecoder_TextIsKeptExactly(t *testing.T) {
	d := NewStreamDecoder()
	got, err := feedAll(d, chunk(`{"content":" \u0000\t\"q\" é 日本 \ud83d\ude00 \n"}`), chunk(`{"content":" "}`))
	if err != nil || got[2].Text != " \x00\t\"q\" é 日本 😀 \n" || got[3].Text != " " {
		t.Fatalf("%v: %+v", err, got)
	}
}

func TestStream_NoGoroutineAndNoStateLeftBehind(t *testing.T) {
	raw := fixture(t, "stream_tools.sse")
	before := runtime.NumGoroutine()
	for i := 0; i < 200; i++ {
		// Abandoned at every point: a decoder is plain memory, there is nothing to release.
		cut := len(raw) * (i % 20) / 20
		_, _ = decodeStream(t, raw[:cut], 16) // a cut inside a frame is an error; it must not leave anything running
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

func TestStreamDecoder_ErrorOrder(t *testing.T) {
	// On every way to fail: a PartStop for each open part, in part order, then exactly one Error.
	open := [][]byte{
		chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{"}}]}`),
		chunk(`{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":"{"}}]}`),
		chunk(`{"content":"text while the calls are open"}`),
	}
	const prefix = "start part_start tool_args_delta part_start tool_args_delta part_start text_delta "
	cases := []struct {
		name  string
		last  []byte // nil: Close
		msg   string
		wantE error
	}{
		{"the provider's error object", []byte(`{"error":{"message":"overloaded"}}`), "overloaded", nil},
		{"a frame that is not JSON", []byte(`{"choices":`), "the provider sent an answer that cannot be read", ErrMalformed},
		{"a limit", chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"` + strings.Repeat("x", ir.MaxToolArgsBytes) + `"}}]}`), "the provider's answer is too large", ir.ErrLimit},
		{"[DONE] without a finish_reason", []byte(`[DONE]`), "the provider ended the stream early", nil},
		{"the upstream is gone", nil, "the provider ended the stream early", nil},
		{"arguments that are no object at the finish", finishChunk("tool_calls"), "the provider sent an answer that cannot be read", ErrMalformed},
	}
	for _, c := range cases {
		d := NewStreamDecoder()
		got, err := feedAll(d, open...)
		if err != nil {
			t.Fatal(err)
		}
		if c.last == nil {
			got = append(got, d.Close()...)
		} else {
			evs, ferr := d.Feed(c.last)
			got = append(got, evs...)
			if c.wantE == nil && ferr != nil || c.wantE != nil && !errors.Is(ferr, c.wantE) {
				t.Errorf("%s: err = %v", c.name, ferr)
			}
		}
		if kinds(got) != prefix+"part_stop part_stop part_stop error" {
			t.Errorf("%s: %s", c.name, kinds(got))
			continue
		}
		n := len(got)
		if got[n-4].Index != 0 || got[n-3].Index != 1 || got[n-2].Index != 2 || got[n-1].Err != c.msg {
			t.Errorf("%s: %+v", c.name, got[n-4:])
		}
		if evs, err := d.Feed(chunk(`{"content":"later"}`)); evs != nil || err != nil || d.Close() != nil {
			t.Errorf("%s: something came after the Error", c.name)
		}
	}
	// Before anything started there is nothing to stop: just the Error.
	d := NewStreamDecoder()
	if got, err := d.Feed([]byte(`nope`)); !errors.Is(err, ErrMalformed) || kinds(got) != "error" {
		t.Fatalf("%v: %s", err, kinds(got))
	}
}

func TestStreamDecoder_ArgumentsAreOneObjectOrNothing(t *testing.T) {
	call := func(args string) []byte {
		return chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":` + string(mustJSON(args)) + `}}]}`)
	}
	more := func(args string) []byte {
		return chunk(`{"tool_calls":[{"index":0,"function":{"arguments":` + string(mustJSON(args)) + `}}]}`)
	}
	good := map[string][][]byte{
		``:              {call("")},
		`{}`:            {call("{}")},
		`{"a":[1,{}]} `: {call(` {"a":`), more(`[1,{}]} `)}, // white space before the object is not passed on
		`{"a":"} {"}`:   {call("  "), more("\n"), more(`{"a":"} {"}`)},
	}
	for want, frames := range good {
		d := NewStreamDecoder()
		got, err := feedAll(d, append(frames, finishChunk("tool_calls"), []byte("[DONE]"))...)
		if err != nil || got[len(got)-1].Kind != ir.Finish || checkEvents(got) != nil {
			t.Errorf("%q: %v: %+v", want, err, got)
			continue
		}
		var args string
		for _, ev := range got {
			args += ev.ArgsJSON
		}
		if args != want {
			t.Errorf("arguments %q, want %q", args, want)
		}
	}
	// What DecodeResponse refuses, the stream refuses too: no Finish, the part is stopped, then Error.
	for _, bad := range []string{`{not json`, `{"a":1`, `[1]`, `null`, `"x"`, `{}{}`, `{} x`, `{"a":tru}`} {
		d := NewStreamDecoder()
		got, err := feedAll(d, call(bad[:1]), more(bad[1:]), finishChunk("tool_calls"), []byte("[DONE]"))
		if !errors.Is(err, ErrMalformed) || !strings.HasSuffix(kinds(got), "part_stop error") || strings.Contains(kinds(got), "finish") {
			t.Errorf("%q: %v: %s", bad, err, kinds(got))
		}
		if _, berr := DecodeResponse(answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":`+string(mustJSON(bad))+`}}]}`, `"tool_calls"`)); berr == nil {
			t.Errorf("%q: the buffered path accepts it", bad)
		}
	}
}

func TestStreamDecoder_TotalArgumentsLimit(t *testing.T) {
	d := NewStreamDecoder()
	frag := strings.Repeat("x", 64<<10)
	var err error
	sent := 0
	for i := 0; i < ir.MaxToolCalls && err == nil; i++ {
		_, err = d.Feed(chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"c%d","function":{"name":"f","arguments":"{\"a\":\""}}]}`, i, i)))
		for n := 0; n < ir.MaxToolArgsBytes-(128<<10) && err == nil; n += len(frag) {
			_, err = d.Feed(chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":"%s"}}]}`, i, frag)))
			sent += len(frag)
		}
	}
	if !errors.Is(err, ir.ErrLimit) || sent < ir.MaxTotalToolArgsBytes-len(frag) || sent > ir.MaxTotalToolArgsBytes+len(frag) {
		t.Fatalf("err = %v after %d bytes", err, sent)
	}
}

func TestStreamDecoder_CharacterSplitAcrossChunks(t *testing.T) {
	// A provider that cuts its output by bytes sends the halves of a character in two chunks.
	// Each half alone is not UTF-8; together they are the character.
	const smile = "\xF0\x9F\x98\x80"
	content := func(key string, raw string) []byte {
		return []byte(`{"id":"c","choices":[{"delta":{"` + key + `":"` + raw + `"}}]}`)
	}
	args := func(index int, id, name, raw string) []byte {
		if id != "" {
			id = `"id":"` + id + `",`
		}
		if name != "" {
			name = `"name":"` + name + `",`
		}
		return []byte(fmt.Sprintf(`{"id":"c","choices":[{"delta":{"tool_calls":[{"index":%d,%s"function":{%s"arguments":"%s"}}]}}]}`, index, id, name, raw))
	}
	for cut := 1; cut < len(smile); cut++ {
		a, b := smile[:cut], smile[cut:]
		for _, key := range []string{"content", "reasoning_content", "refusal"} {
			d := NewStreamDecoder()
			got, err := feedAll(d, content(key, "x"+a), content(key, b+"y"), content(key, a), content(key, ""), content(key, b), finishChunk("stop"), []byte("[DONE]"))
			if err != nil || checkEvents(got) != nil {
				t.Fatalf("%s cut %d: %v: %+v", key, cut, err, got)
			}
			var s string
			for _, ev := range got {
				s += ev.Text
			}
			if s != "x"+smile+"y"+smile {
				t.Fatalf("%s cut %d: text %q", key, cut, s)
			}
		}

		// In arguments, the pieces of two calls interleaved: each call keeps its own half.
		d := NewStreamDecoder()
		got, err := feedAll(d,
			args(0, "a", "", `{\"a\":\"`+a),
			args(1, "b", "", `{\"b\":\"é`+a),
			args(0, "", "", b+`\"}`),
			args(1, "", "", b+`\"}`),
			[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"f"}},{"index":1,"function":{"name":"g"}}]}}]}`),
			finishChunk("tool_calls"), []byte("[DONE]"))
		if err != nil || checkEvents(got) != nil {
			t.Fatalf("arguments cut %d: %v: %+v", cut, err, got)
		}
		resp, err := ir.Collect(got)
		if err != nil || string(resp.Parts[0].Input) != `{"a":"`+smile+`"}` || string(resp.Parts[1].Input) != `{"b":"é`+smile+`"}` {
			t.Fatalf("arguments cut %d: %v: %+v", cut, err, resp.Parts)
		}
		// The same with both calls open, so that the halves travel as deltas.
		d = NewStreamDecoder()
		got, err = feedAll(d,
			args(0, "a", "f", ``),
			args(1, "b", "g", ``),
			args(0, "", "", `{\"a\":\"`+a), args(1, "", "", `{\"b\":\"é`+a), args(0, "", "", b+`\"}`), args(1, "", "", b+`\"}`),
			finishChunk("tool_calls"), []byte("[DONE]"))
		if err != nil || checkEvents(got) != nil {
			t.Fatalf("arguments cut %d: %v: %+v", cut, err, got)
		}
		resp, err = ir.Collect(got)
		if err != nil || string(resp.Parts[0].Input) != `{"a":"`+smile+`"}` || string(resp.Parts[1].Input) != `{"b":"é`+smile+`"}` {
			t.Fatalf("open arguments cut %d: %v: %+v", cut, err, resp.Parts)
		}

		// A character that never gets its second half is an error at the part's stop, not U+FFFD.
		for name, frames := range map[string][][]byte{
			"text":      {content("content", "x"+a), finishChunk("stop")},
			"then call": {content("content", "x"+a), args(0, "a", "f", `{}`)},
			"then kind": {content("content", "x"+a), content("reasoning", "y")},
			"arguments": {args(0, "a", "f", `{\"a\":\"`+a), finishChunk("tool_calls")},
		} {
			d := NewStreamDecoder()
			got, err := feedAll(d, frames...)
			if !errors.Is(err, ErrMalformed) || checkEvents(got) != nil || got[len(got)-1].Kind != ir.Error {
				t.Fatalf("%s cut %d: %v: %+v", name, cut, err, got)
			}
			for _, ev := range got {
				if strings.ContainsRune(ev.Text+ev.ArgsJSON, utf8.RuneError) {
					t.Fatalf("%s cut %d: a replacement character was emitted: %+v", name, cut, got)
				}
			}
		}
	}

	// The same for a pair of escaped surrogates, which a server that cuts by UTF-16 units sends.
	d := NewStreamDecoder()
	got, err := feedAll(d, content("content", `a\ud83d`), content("content", `\ude00b\ud83d\ude00`), finishChunk("stop"), []byte("[DONE]"))
	if err != nil || got[2].Text != "a" || got[3].Text != smile+"b"+smile {
		t.Fatalf("%v: %+v", err, got)
	}
}

func TestStreamDecoder_IDAfterTheArgumentsWithoutAnIndex(t *testing.T) {
	d := NewStreamDecoder()
	got, err := feedAll(d,
		chunk(`{"tool_calls":[{"function":{"name":"f","arguments":"{\"a\""}}]}`),
		chunk(`{"tool_calls":[{"id":"a","function":{"arguments":":1}"}}]}`),
		chunk(`{"tool_calls":[{"id":"b","function":{"name":"g","arguments":"{}"}}]}`),
		finishChunk("tool_calls"), []byte("[DONE]"))
	want := []ir.Event{
		{Kind: ir.Start, ID: "c", Model: "m"},
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.ToolUse, ToolID: "a", ToolName: "f"}},
		{Kind: ir.ToolArgsDelta, Index: 0, ArgsJSON: `{"a":1}`},
		{Kind: ir.PartStart, Index: 1, Part: ir.Part{Kind: ir.ToolUse, ToolID: "b", ToolName: "g"}},
		{Kind: ir.ToolArgsDelta, Index: 1, ArgsJSON: `{}`},
		{Kind: ir.PartStop, Index: 0},
		{Kind: ir.PartStop, Index: 1},
		{Kind: ir.Finish, Stop: ir.StopToolUse},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v\n%+v", err, got)
	}
}

// ---------------------------------------------------------------- parked for X03

func toolAnswer(arguments string) []byte {
	return answer(`{"tool_calls":[{"id":"a","function":{"name":"f","arguments":`+arguments+`}}]}`, `"tool_calls"`)
}

func toolStream(arguments string) [][]byte {
	return [][]byte{
		chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":` + arguments + `}}]}`),
		finishChunk("tool_calls"), []byte("[DONE]"),
	}
}

func TestToolArguments_TheTextNullIsRefusedOnBothPaths(t *testing.T) {
	for _, arguments := range []string{`"null"`, `" null "`} {
		if resp, err := DecodeResponse(toolAnswer(arguments)); !errors.Is(err, ErrMalformed) || len(resp.Parts) != 0 {
			t.Errorf("buffered %s: err = %v, parts %+v", arguments, err, resp.Parts)
		}
		evs, err := feedAll(NewStreamDecoder(), toolStream(arguments)...)
		if !errors.Is(err, ErrMalformed) || evs[len(evs)-1].Kind != ir.Error {
			t.Errorf("stream %s: err = %v, events %s", arguments, err, kinds(evs))
		}
	}
	// "Nothing" stays nothing on both paths.
	for _, arguments := range []string{`null`, `""`, `" \n\t "`} {
		resp, err := DecodeResponse(toolAnswer(arguments))
		if err != nil || len(resp.Parts) != 1 || string(resp.Parts[0].Input) != "{}" {
			t.Errorf("buffered %s: err = %v, parts %+v", arguments, err, resp.Parts)
		}
		evs, err := feedAll(NewStreamDecoder(), toolStream(arguments)...)
		if err != nil || kinds(evs) != "start part_start part_stop finish" {
			t.Errorf("stream %s: err = %v, events %s", arguments, err, kinds(evs))
		}
		streamed, err := ir.Collect(evs)
		if err != nil || !reflect.DeepEqual(streamed.Parts, resp.Parts) {
			t.Errorf("stream %s: %v: %+v, buffered %+v", arguments, err, streamed.Parts, resp.Parts)
		}
	}
}

func TestToolArguments_AreNeverRepaired(t *testing.T) {
	for name, arguments := range map[string]string{
		"a lone continuation byte":     "\"{\\\"a\\\":\\\"\x80\\\"}\"",
		"a lead byte without its rest": "\"{\\\"a\\\":\\\"\xc3\\\"}\"",
		"a lone high surrogate escape": `"{\"a\":\"\ud83d\"}"`,
		"a lone low surrogate escape":  `"{\"a\":\"\ude00\"}"`,
	} {
		if resp, err := DecodeResponse(toolAnswer(arguments)); !errors.Is(err, ErrMalformed) || len(resp.Parts) != 0 {
			t.Errorf("buffered, %s: err = %v, parts %+v", name, err, resp.Parts)
		}
		evs, err := feedAll(NewStreamDecoder(), toolStream(arguments)...)
		if !errors.Is(err, ErrMalformed) || len(evs) == 0 || evs[len(evs)-1].Kind != ir.Error {
			t.Errorf("stream, %s: err = %v, events %s", name, err, kinds(evs))
		}
		for _, ev := range evs {
			if strings.Contains(ev.ArgsJSON, "�") {
				t.Errorf("stream, %s: repaired arguments were sent on: %q", name, ev.ArgsJSON)
			}
		}
	}
	// What is valid passes byte for byte: a real U+FFFD, a surrogate pair written as escapes, and
	// an escape that belongs to the arguments' own JSON text.
	for _, want := range []string{"{\"a\":\"�\"}", "{\"a\":\"\U0001F600\"}", `{"a":"\ud83d"}`} {
		arguments := string(mustJSON(want))
		if want == "{\"a\":\"\U0001F600\"}" {
			arguments = `"{\"a\":\"😀\"}"`
		}
		resp, err := DecodeResponse(toolAnswer(arguments))
		if err != nil || string(resp.Parts[0].Input) != want {
			t.Errorf("buffered %s: %v, %+v", arguments, err, resp.Parts)
		}
		evs, err := feedAll(NewStreamDecoder(), toolStream(arguments)...)
		streamed, cerr := ir.Collect(evs)
		if err != nil || cerr != nil || string(streamed.Parts[0].Input) != want {
			t.Errorf("stream %s: %v, %v, %+v", arguments, err, cerr, streamed.Parts)
		}
	}
	// Text may keep U+FFFD.
	resp, err := DecodeResponse(answer("{\"content\":\"a\x80b\"}", `"stop"`))
	if err != nil || resp.Parts[0].Text != "a�b" {
		t.Errorf("text: %v, %+v", err, resp.Parts)
	}
}

func TestEncodeRequest_ThinkingAndEmptyTextIsNoTurn(t *testing.T) {
	thinking := ir.Part{Kind: ir.Thinking, Text: "hm"}
	body, dropped, err := EncodeRequest(ir.Request{Messages: []ir.Message{
		user(text("a")), assistant(thinking, text("")), user(text("b")), assistant(text("")), user(text("c")),
	}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"user","content":"b"},{"role":"user","content":"c"}]}` {
		t.Fatalf("body = %s", body)
	}
	if !reflect.DeepEqual(dropped, []string{"thinking"}) {
		t.Fatalf("dropped = %v", dropped)
	}
	// With a tool call the turn stays.
	top, _ := encode(t, ir.Request{Messages: []ir.Message{user(text("a")), assistant(text(""), toolUse("t", "f", `{}`)), user(toolResult("t", "r"))}})
	if !strings.Contains(string(top["messages"]), `"tool_calls"`) {
		t.Fatalf("messages = %s", top["messages"])
	}
}

func TestStreamDecoder_CloseTellsTooLargeFromUnreadable(t *testing.T) {
	// A call that begins after the finish_reason is judged by Close when "[DONE]" never comes.
	late := func(arguments string) []ir.Event {
		d := NewStreamDecoder()
		evs, err := feedAll(d, chunk(`{"content":"x"}`), finishChunk("stop"),
			chunk(`{"tool_calls":[{"index":7,"id":"late","function":{"name":"f","arguments":`+string(mustJSON(arguments))+`}}]}`))
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		return append(evs, d.Close()...)
	}
	deep := late(`{"a":` + strings.Repeat("[", ir.MaxDepth+1) + strings.Repeat("]", ir.MaxDepth+1) + `}`)
	if last := deep[len(deep)-1]; last.Kind != ir.Error || last.Err != errTooLarge {
		t.Fatalf("too deep: %+v", last)
	}
	broken := late(`{"a":`)
	if last := broken[len(broken)-1]; last.Kind != ir.Error || last.Err != errUnreadable {
		t.Fatalf("broken: %+v", last)
	}
}

func TestDecodeError(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"message":"rate limited","type":"x"}}`: "rate limited",
		`{"error":"plain"}`:       "plain",
		`{"message":"top level"}`: "top level",
		`{"type":"error","error":{"type":"api_error","message":"anthropic shape"}}`: "anthropic shape",
		`{"error":{"code":7}}`: "",
		`<html>502</html>`:     "",
		``:                     "",
		`[1]`:                  "",
		`{"error":{"message":"` + strings.Repeat("é", 400) + `"}}`: strings.Repeat("é", 150),
		strings.Repeat("[", 200) + strings.Repeat("]", 200):        "",
	} {
		if got := DecodeError([]byte(body)); got != want {
			t.Errorf("DecodeError(%.40q) = %q, want %q", body, got, want)
		}
	}
}

func TestCheckRequest(t *testing.T) {
	msgs := func(m string) []byte { return []byte(`{"model":"m","messages":[` + m + `]}`) }
	const (
		user  = `{"role":"user","content":"hi"}`
		calls = `{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"f","arguments":"{\"k\":1}"}}]}`
		toolA = `{"role":"tool","tool_call_id":"a","content":"x"}`
		toolB = `{"role":"tool","tool_call_id":"b","content":""}`
		said  = `{"role":"assistant","content":"ok"}`
	)
	for _, good := range []string{
		user,
		`{"role":"system","content":"s"},` + user + "," + said + "," + user,
		user + "," + calls + "," + toolB + "," + toolA + "," + said,
		user + "," + calls + "," + toolA + "," + toolB + `,{"role":"user","content":[{"type":"text","text":"x"}]}`,
	} {
		if err := CheckRequest(msgs(good)); err != nil {
			t.Errorf("%v\n%s", err, good)
		}
	}
	for name, bad := range map[string]string{
		"no messages":                    ``,
		"a call without an answer":       user + "," + calls + "," + toolA,
		"a message before the answers":   user + "," + calls + "," + toolA + "," + user + "," + toolB,
		"a tool message for no call":     user + "," + toolA,
		"an id answered twice":           user + "," + calls + "," + toolA + "," + toolA + "," + toolB,
		"a tool message for another id":  user + "," + calls + "," + toolA + `,{"role":"tool","tool_call_id":"z","content":"x"}`,
		"two assistant messages":         user + "," + said + "," + said,
		"an assistant that says nothing": user + `,{"role":"assistant","content":""}`,
		"a user that says nothing":       `{"role":"user","content":""}`,
		"arguments that are no object":   user + `,{"role":"assistant","content":null,"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{"}}]},` + toolA,
		"one call id twice":              user + `,{"role":"assistant","tool_calls":[{"id":"a","function":{"name":"f","arguments":"{}"}},{"id":"a","function":{"name":"f","arguments":"{}"}}]},` + toolA,
		"a role nobody knows":            `{"role":"function","content":"x"}`,
	} {
		if err := CheckRequest(msgs(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if CheckRequest([]byte(`[]`)) == nil {
		t.Error("a body that is no object: accepted")
	}
}
