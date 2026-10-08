package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/irtest"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// The tests of the half a caller needs: DecodeRequest, EncodeResponse,
// EncodeError, StreamEncoder and CheckStream.

// ---------------------------------------------------------------- helpers

func decodeReq(t *testing.T, body string) ir.Request {
	t.Helper()
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v\n%s", err, body)
	}
	return req
}

// chatReq is a request with the given messages and one piece of JSON after them.
func chatReq(messages, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return `{"model":"m","messages":[` + messages + `]` + extra + `}`
}

const hi = `{"role":"user","content":"hi"}`

func refusedReq(t *testing.T, body, field string) *ir.BadRequestError {
	t.Helper()
	req, err := DecodeRequest([]byte(body))
	var bad *ir.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a *ir.BadRequestError for %s\n%.200s", err, field, body)
	}
	if bad.Field != field || bad.Format != "chat" {
		t.Fatalf("Format %q, Field = %q (%s), want %q\n%.200s", bad.Format, bad.Field, bad.Reason, field, body)
	}
	if !reflect.DeepEqual(req, ir.Request{}) {
		t.Fatalf("a request came with the error: %+v", req)
	}
	return bad
}

func sorted(names []string) []string { return ir.Dropped(names) }

// ---------------------------------------------------------------- request

func TestDecodeRequest_ToolTurn(t *testing.T) {
	req := decodeReq(t, string(fixture(t, "req_tools.json")))
	temp := 0.2
	want := ir.Request{
		Model:  "burrow-medium",
		System: []ir.Part{text("You are a weather bot.")},
		Messages: []ir.Message{
			user(text("Weather in Oslo and Rome?")),
			assistant(text("Checking both."), toolUse("call_1", "get_weather", `{"city":"Oslo"}`), toolUse("call_2", "get_weather", `{ "city": "Rome" }`)),
			// The two tool messages and the user message after them are one user turn, results first.
			user(toolResult("call_1", "4°C"), toolResult("call_2", "19°C"), text("And tomorrow?")),
		},
		Tools:       []ir.Tool{{Name: "get_weather", Description: "Weather for a city", Schema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}},
		ToolChoice:  ir.ToolChoice{Mode: ir.ChoiceAuto},
		Temperature: &temp,
		Stream:      true,
	}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("\n got %+v\nwant %+v", req, want)
	}
	if !IncludeUsage(fixture(t, "req_tools.json")) || IncludeUsage([]byte(chatReq(hi, `"stream":true`))) || IncludeUsage([]byte(`{`)) ||
		IncludeUsage([]byte(chatReq(hi, `"stream_options":{"include_usage":"yes"}`))) {
		t.Fatal("IncludeUsage")
	}
}

func TestDecodeRequest_MessagesAndContent(t *testing.T) {
	for _, c := range []struct {
		name, messages string
		system         []ir.Part
		want           []ir.Message
		dropped        []string
	}{
		{"system and developer, as a string and as parts",
			`{"role":"system","content":"a"},{"role":"developer","content":[{"type":"text","text":"b"},{"type":"text","text":"c"}]},{"role":"system","content":""},` + hi,
			[]ir.Part{text("a"), text("b"), text("c")}, []ir.Message{user(text("hi"))}, nil},
		{"a system message after the conversation began loses its place",
			hi + `,{"role":"system","content":"late"}`,
			[]ir.Part{text("late")}, []ir.Message{user(text("hi"))}, []string{"system.position"}},
		{"user content as parts, with images",
			`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}},{"type":"image_url","image_url":{"url":"https://example.test/a.png","detail":"auto"}}]}`,
			nil, []ir.Message{user(text("look"), ir.Part{Kind: ir.Image, MediaType: "image/png", Data: "aGk="}, ir.Part{Kind: ir.Image, Data: "https://example.test/a.png"})}, nil},
		{"an image's detail is told",
			`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png","detail":"high"}}]}`,
			nil, []ir.Message{user(ir.Part{Kind: ir.Image, Data: "https://example.test/a.png"})}, []string{"image_url.detail"}},
		{"assistant content: a string, null, parts, a refusal, reasoning",
			hi + `,{"role":"assistant","content":"a"},` + hi + `,{"role":"assistant","content":null,"refusal":"no"},` + hi +
				`,{"role":"assistant","reasoning_content":"hm","content":[{"type":"text","text":"b"},{"type":"refusal","refusal":"c"}]}`,
			nil, []ir.Message{user(text("hi")), assistant(text("a")), user(text("hi")), assistant(text("no")), user(text("hi")),
				assistant(ir.Part{Kind: ir.Thinking, Text: "hm"}, text("b"), text("c"))}, nil},
		{"an assistant message that says nothing is no turn",
			hi + `,{"role":"assistant","content":""},{"role":"assistant"},` + hi,
			nil, []ir.Message{user(text("hi")), user(text("hi"))}, nil},
		{"two assistant messages in a row are one turn",
			hi + `,{"role":"assistant","content":"a"},{"role":"assistant","content":"b","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"t","content":"r"}`,
			nil, []ir.Message{user(text("hi")), assistant(text("a"), text("b"), toolUse("t", "f", `{}`)), user(toolResult("t", "r"))}, nil},
		{"names, audio and annotations of messages are told",
			`{"role":"user","name":"bob","content":"hi"},{"role":"assistant","content":"a","audio":{"id":"x"},"annotations":[{"type":"url_citation"}],"name":""}`,
			nil, []ir.Message{user(text("hi")), assistant(text("a"))}, []string{"messages.annotations", "messages.audio", "messages.name"}},
		{"unknown keys of a message and of a part, and cache_control",
			`{"role":"user","foo":1,"cache_control":{"type":"ephemeral"},"content":[{"type":"text","text":"hi","bar":2,"cache_control":{"type":"ephemeral"}},{"type":"image_url","image_url":{"url":"https://example.test/a.png","baz":3}}]}`,
			nil, []ir.Message{user(text("hi"), ir.Part{Kind: ir.Image, Data: "https://example.test/a.png"})},
			[]string{"cache_control", "unknown:content.bar", "unknown:content.image_url.baz", "unknown:messages.foo"}},
		{"a tool call of another type and its answer are left out",
			hi + `,{"role":"assistant","content":"a","tool_calls":[{"id":"c","type":"custom","custom":{"name":"x","input":"y"}}]},{"role":"tool","tool_call_id":"c","content":"r"},` + hi,
			nil, []ir.Message{user(text("hi")), assistant(text("a")), user(text("hi"))}, []string{"input:custom", "input:tool.orphan"}},
		{"tool content as parts, with an image the tool returned",
			hi + `,{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":""}}]},` +
				`{"role":"tool","tool_call_id":"t","content":[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]},{"role":"user","content":"see?"}`,
			nil, []ir.Message{user(text("hi")), assistant(toolUse("t", "f", `{}`)),
				user(toolResult("t", "a\nb"), text(ir.ToolImageNote("t")), ir.Part{Kind: ir.Image, MediaType: "image/png", Data: "aGk="}, text("see?"))}, nil},
		{"the legacy function_call and function role are carried as a tool call and its result",
			hi + `,{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{\"a\":1}"}},{"role":"function","name":"f","content":"r"},` + hi,
			nil, []ir.Message{user(text("hi")), assistant(toolUse("call_legacy_1", "f", `{"a":1}`)), user(toolResult("call_legacy_1", "r"), text("hi"))}, nil},
	} {
		req := decodeReq(t, chatReq(c.messages, ""))
		if !reflect.DeepEqual(req.System, c.system) || !reflect.DeepEqual(req.Messages, c.want) || !reflect.DeepEqual(sorted(req.Dropped), c.dropped) {
			t.Errorf("%s:\n system %+v\n got %+v\nwant %+v\n dropped %v", c.name, req.System, req.Messages, c.want, sorted(req.Dropped))
		}
	}
}

func TestDecodeRequest_EveryTopLevelField(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		extra   string
		want    func(r *ir.Request)
		dropped []string
	}{
		{`"max_tokens":100`, func(r *ir.Request) { r.MaxTokens = 100 }, nil},
		{`"max_completion_tokens":200`, func(r *ir.Request) { r.MaxTokens = 200 }, nil},
		{`"max_tokens":100,"max_completion_tokens":200`, func(r *ir.Request) { r.MaxTokens = 200 }, nil},
		{`"temperature":1.5,"top_p":0.25`, func(r *ir.Request) { r.Temperature, r.TopP = f(1.5), f(0.25) }, nil},
		{`"stop":"END"`, func(r *ir.Request) { r.Stop = []string{"END"} }, nil},
		{`"stop":["a","b"]`, func(r *ir.Request) { r.Stop = []string{"a", "b"} }, nil},
		{`"stop":null,"stream":false,"tools":null,"tool_choice":null`, func(r *ir.Request) {}, nil},
		{`"stream":true`, func(r *ir.Request) { r.Stream = true }, nil},
		{`"stream":true,"stream_options":{"include_usage":true}`, func(r *ir.Request) { r.Stream = true }, nil},
		{`"stream_options":{"include_usage":false,"include_obfuscation":false}`, func(r *ir.Request) {}, []string{"unknown:stream_options.include_obfuscation"}},
		{`"n":1`, func(r *ir.Request) {}, nil},
		{`"n":3`, func(r *ir.Request) {}, []string{"n"}},
		{`"response_format":{"type":"text"}`, func(r *ir.Request) {}, nil},
		{`"response_format":{"type":"json_object"}`, func(r *ir.Request) {}, []string{"response_format"}},
		{`"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{}}}`, func(r *ir.Request) {}, []string{"response_format"}},
		{`"parallel_tool_calls":true`, func(r *ir.Request) {}, nil},
		{`"parallel_tool_calls":false`, func(r *ir.Request) {}, []string{"parallel_tool_calls"}},
		{`"seed":7,"logprobs":true,"top_logprobs":3,"logit_bias":{"50256":-100},"presence_penalty":0.5,"frequency_penalty":-1,"user":"u1","metadata":{"a":"b"},"store":true,` +
			`"reasoning_effort":"high","service_tier":"flex","prediction":{"type":"content","content":"x"},"modalities":["text","audio"],"audio":{"voice":"alloy"},` +
			`"web_search_options":{},"verbosity":"low","prompt_cache_key":"k","safety_identifier":"s"`,
			func(r *ir.Request) {},
			[]string{"audio", "frequency_penalty", "logit_bias", "logprobs", "metadata", "modalities", "prediction", "presence_penalty", "prompt_cache_key",
				"reasoning_effort", "safety_identifier", "seed", "service_tier", "store", "top_logprobs", "user", "verbosity"}},
		// What asks for nothing is not reported.
		{`"seed":null,"logprobs":false,"logit_bias":{},"presence_penalty":0,"frequency_penalty":0.0,"user":"","metadata":{},"store":false,"service_tier":"auto","modalities":["text"],"audio":null`,
			func(r *ir.Request) {}, nil},
		{`"service_tier":"default"`, func(r *ir.Request) {}, nil},
		{`"web_search_options":{"search_context_size":"low"}`, func(r *ir.Request) {}, []string{"web_search_options"}},
		{`"zeta":1,"more":true,"":0`, func(r *ir.Request) {}, []string{"unknown:", "unknown:more", "unknown:zeta"}},
		{`"functions":null,"function_call":null`, func(r *ir.Request) {}, nil},
	} {
		want := ir.Request{Model: "m", Messages: []ir.Message{user(text("hi"))}}
		c.want(&want)
		req := decodeReq(t, chatReq(hi, c.extra))
		got := sorted(req.Dropped)
		req.Dropped = nil
		if !reflect.DeepEqual(req, want) || !reflect.DeepEqual(got, c.dropped) {
			t.Errorf("%s:\n got %+v\nwant %+v\n dropped %v", c.extra, req, want, got)
		}
	}
}

func TestDecodeRequest_ToolsAndToolChoice(t *testing.T) {
	odd := `{ "type":"object", "properties":{"n":{"const":12345678901234567890123}} }`
	req := decodeReq(t, chatReq(hi, `"tools":[
 {"type":"function","function":{"name":"f","description":"d","parameters":`+odd+`,"strict":false}},
 {"type":"function","function":{"name":"g","strict":true,"extra":1},"also":2},
 {"type":"custom","custom":{"name":"patch"}},{"type":"web_search"},{"type":"more"},{"type":""}]`))
	want := []ir.Tool{{Name: "f", Description: "d", Schema: json.RawMessage(odd)}, {Name: "g"}}
	if !reflect.DeepEqual(req.Tools, want) {
		t.Fatalf("tools %+v", req.Tools)
	}
	if got := sorted(req.Dropped); !reflect.DeepEqual(got, []string{"tool:", "tool:custom", "tool:more", "tool:web_search", "tools.strict", "unknown:tools.also", "unknown:tools.extra"}) {
		t.Fatalf("dropped %v", got)
	}
	for choice, want := range map[string]ir.ToolChoice{
		`"auto"`:     {Mode: ir.ChoiceAuto},
		`"none"`:     {Mode: ir.ChoiceNone},
		`"required"`: {Mode: ir.ChoiceRequired},
		`{"type":"function","function":{"name":"f"}}`: {Mode: ir.ChoiceTool, Name: "f"},
	} {
		if req := decodeReq(t, chatReq(hi, `"tool_choice":`+choice)); req.ToolChoice != want || req.Dropped != nil {
			t.Errorf("%s: %+v, dropped %v", choice, req.ToolChoice, req.Dropped)
		}
	}
	for _, choice := range []string{`{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}`, `{"type":"custom","custom":{"name":"x"}}`} {
		if req := decodeReq(t, chatReq(hi, `"tool_choice":`+choice)); req.ToolChoice != (ir.ToolChoice{}) || !reflect.DeepEqual(req.Dropped, []string{"tool_choice"}) {
			t.Errorf("%s: %+v, dropped %v", choice, req.ToolChoice, req.Dropped)
		}
	}
}

func TestDecodeRequest_Refused(t *testing.T) {
	call := `{"role":"assistant","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{}"}}]}`
	for field, body := range map[string]string{
		"body":                                    `[]`,
		"model":                                   `{"model":5,"messages":[` + hi + `]}`,
		"messages":                                `{"model":"m"}`,
		"messages[0]":                             chatReq(`"hi"`, ""),
		"messages[0].role":                        chatReq(`{"role":"robot","content":"x"}`, ""),
		"messages[0].content":                     chatReq(`{"role":"user"}`, ""),
		"messages[1].content":                     chatReq(hi+`,{"role":"user","content":5}`, ""),
		"messages[0].content[0]":                  chatReq(`{"role":"user","content":["x"]}`, ""),
		"messages[0].content[0].type":             chatReq(`{"role":"user","content":[{"type":"video_url"}]}`, ""),
		"messages[0].content[1]":                  chatReq(`{"role":"user","content":[{"type":"text","text":"x"},{"type":"input_audio","input_audio":{"data":"SECRET","format":"wav"}}]}`, ""),
		"messages[0].content[0].text":             chatReq(`{"role":"user","content":[{"type":"text","text":5}]}`, ""),
		"messages[0].content[0].image_url":        chatReq(`{"role":"user","content":[{"type":"image_url"}]}`, ""),
		"messages[0].content[0].image_url.url":    chatReq(`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png,raw"}}]}`, ""),
		"messages[1].tool_calls":                  chatReq(hi+`,{"role":"assistant","tool_calls":"x"}`, ""),
		"messages[1].tool_calls[0].id":            chatReq(hi+`,{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f"}}]}`, ""),
		"messages[1].tool_calls[0].function.name": chatReq(hi+`,{"role":"assistant","tool_calls":[{"id":"t","type":"function","function":{}}]}`, ""),
		"messages[1].tool_calls[0].type":          chatReq(hi+`,{"role":"assistant","tool_calls":[{"id":"t"}]}`, ""),
		"messages[1].function_call.name":          chatReq(hi+`,{"role":"assistant","function_call":{"arguments":"{}"}}`, ""),
		"messages[2].tool_call_id":                chatReq(hi+`,`+call+`,{"role":"tool","content":"r"}`, ""),
		"messages[2].content[0]":                  chatReq(hi+`,`+call+`,{"role":"tool","tool_call_id":"t","content":[{"type":"file","file":{"file_id":"f"}}]}`, ""),
		"tools":                                   chatReq(hi, `"tools":{}`),
		"tools[0].type":                           chatReq(hi, `"tools":[{"name":"f"}]`),
		"tools[0].function":                       chatReq(hi, `"tools":[{"type":"function"}]`),
		"tools[0].function.name":                  chatReq(hi, `"tools":[{"type":"function","function":{"description":"d"}}]`),
		"tools[0].function.parameters":            chatReq(hi, `"tools":[{"type":"function","function":{"name":"f","parameters":[]}}]`),
		"tool_choice":                             chatReq(hi, `"tool_choice":"sometimes"`),
		"tool_choice.function.name":               chatReq(hi, `"tool_choice":{"type":"function","function":{}}`),
		"max_tokens":                              chatReq(hi, `"max_tokens":0`),
		"max_completion_tokens":                   chatReq(hi, `"max_completion_tokens":1.5`),
		"temperature":                             chatReq(hi, `"temperature":"hot"`),
		"top_p":                                   chatReq(hi, `"top_p":[1]`),
		"stop":                                    chatReq(hi, `"stop":5`),
		"stop[1]":                                 chatReq(hi, `"stop":["a",5]`),
		"stream":                                  chatReq(hi, `"stream":"yes"`),
		"stream_options":                          chatReq(hi, `"stream_options":true`),
		"n":                                       chatReq(hi, `"n":"two"`),
		"parallel_tool_calls":                     chatReq(hi, `"parallel_tool_calls":1`),
		// The legacy interface asks for an answer in a shape that is not written.
		"functions":     chatReq(hi, `"functions":[{"name":"f","parameters":{}}]`),
		"function_call": chatReq(hi, `"function_call":"auto"`),
	} {
		bad := refusedReq(t, body, field)
		if strings.Contains(bad.Error(), "SECRET") {
			t.Errorf("%s: the error holds content", field)
		}
	}
	if bad := refusedReq(t, chatReq(``, ""), "messages"); bad.Reason != "is empty" {
		t.Fatalf("reason %q", bad.Reason)
	}
	// A request in which no turn is left.
	refusedReq(t, chatReq(`{"role":"system","content":"only"}`, ""), "messages")
}

func TestDecodeRequest_Limits(t *testing.T) {
	deep := strings.Repeat(`{"a":`, ir.MaxDepth+1) + `1` + strings.Repeat(`}`, ir.MaxDepth+1)
	repeat := func(s string, n int) string { return strings.TrimSuffix(strings.Repeat(s+",", n), ",") }
	var calls []string
	for i := 0; i <= ir.MaxToolCalls; i++ {
		calls = append(calls, fmt.Sprintf(`{"id":"t%d","type":"function","function":{"name":"f","arguments":"{}"}}`, i))
	}
	big := string(mustJSON(`{"a":"` + strings.Repeat("x", ir.MaxToolArgsBytes) + `"}`))
	for field, body := range map[string]string{
		"body":                   `{"model":"m","messages":[` + hi + `],"x":` + deep + `}`,
		"messages":               chatReq(repeat(hi, ir.MaxMessages+1), ""),
		"messages[0].content":    chatReq(`{"role":"user","content":[`+repeat(`{"type":"text","text":"x"}`, ir.MaxParts+1)+`]}`, ""),
		"messages[1].tool_calls": chatReq(hi+`,{"role":"assistant","tool_calls":[`+strings.Join(calls, ",")+`]}`, ""),
		"messages[1].tool_calls[0].function.arguments": chatReq(hi+`,{"role":"assistant","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":`+big+`}}]}`, ""),
		"tools": chatReq(hi, `"tools":[`+repeat(`{"type":"web_search"}`, ir.MaxTools+1)+`]`),
		"stop":  chatReq(hi, `"stop":[`+repeat(`"x"`, ir.MaxParts+1)+`]`),
	} {
		if bad := refusedReq(t, body, field); !bad.Limit || !errors.Is(bad, ir.ErrLimit) {
			t.Errorf("%s: not a limit error: %v", field, bad)
		}
	}
}

func TestDecodeRequest_AHistoryNeverEndsASession(t *testing.T) {
	// The session rule: whatever a client holds as its history — a call that was cut, a call
	// without an answer, an answer without a call, answers out of order or after a later message —
	// is taken, becomes a conversation both targets accept, and what was repaired is told.
	call := func(id, args string) string {
		return `{"id":"` + id + `","type":"function","function":{"name":"f","arguments":` + string(mustJSON(args)) + `}}`
	}
	turn := func(calls ...string) string {
		return `{"role":"assistant","content":null,"tool_calls":[` + strings.Join(calls, ",") + `]}`
	}
	tool := func(id, out string) string {
		return `{"role":"tool","tool_call_id":"` + id + `","content":"` + out + `"}`
	}
	for _, c := range []struct {
		name, messages string
		want           []ir.Message
		dropped        []string
	}{
		{"a call without an answer, in the middle and at the end",
			hi + `,` + turn(call("a", `{}`), call("b", `{}`)) + `,` + tool("b", "rb") + `,` + hi + `,` + turn(call("c", `{}`)),
			[]ir.Message{user(text("hi")), assistant(toolUse("a", "f", `{}`), toolUse("b", "f", `{}`)),
				user(toolResult("a", ir.ToolNoOutput), toolResult("b", "rb"), text("hi")), assistant(toolUse("c", "f", `{}`)), user(toolResult("c", ir.ToolNoOutput))},
			[]string{"input:tool_call.unanswered"}},
		{"an answer without a call, and a second answer for one call",
			hi + `,` + tool("zz", "?") + `,` + turn(call("a", `{}`)) + `,` + tool("a", "ra") + `,` + tool("a", "again") + `,` + tool("a", "and again"),
			[]ir.Message{user(text("hi")), assistant(toolUse("a", "f", `{}`)), user(toolResult("a", "ra"))},
			[]string{"input:tool.orphan"}},
		{"answers in another order, and after a later user message",
			hi + `,` + turn(call("a", `{}`), call("b", `{}`)) + `,` + tool("b", "rb") + `,{"role":"user","content":"wait"},` + tool("a", "ra"),
			[]ir.Message{user(text("hi")), assistant(toolUse("a", "f", `{}`), toolUse("b", "f", `{}`)), user(toolResult("a", "ra"), toolResult("b", "rb"), text("wait"))},
			nil},
		{"an answer that stands before its call",
			hi + `,` + tool("a", "early") + `,` + turn(call("a", `{}`)),
			[]ir.Message{user(text("hi")), assistant(toolUse("a", "f", `{}`)), user(toolResult("a", "early"))},
			nil},
		{"an id that waits already, in one message and in the next",
			hi + `,` + turn(call("a", `{"n":1}`), call("a", `{"n":2}`)) + `,` + turn(call("a", `{"n":3}`)) + `,` + tool("a", "ra"),
			[]ir.Message{user(text("hi")), assistant(toolUse("a", "f", `{"n":1}`)), user(toolResult("a", "ra"))},
			[]string{"input:tool_call.duplicate"}},
		{"arguments that are no JSON object: a call that was cut",
			hi + `,` + turn(call("a", `{"city":`), call("b", `[1]`), call("c", `null`), call("d", ` `)) + `,` + tool("a", "ra") + `,` + tool("b", "rb") + `,` + tool("c", "rc") + `,` + tool("d", "rd"),
			[]ir.Message{user(text("hi")), assistant(toolUse("a", "f", `{}`), toolUse("b", "f", `{}`), toolUse("c", "f", `{}`), toolUse("d", "f", `{}`)),
				user(toolResult("a", "ra"), toolResult("b", "rb"), toolResult("c", "rc"), toolResult("d", "rd"))},
			[]string{"input:tool_call.arguments"}},
		{"arguments as an object instead of a string, as some clients send them",
			hi + `,{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":{"a": 1}}}]},` + tool("a", "ra"),
			[]ir.Message{user(text("hi")), assistant(toolUse("a", "f", `{"a": 1}`)), user(toolResult("a", "ra"))},
			nil},
		{"a legacy function message for nothing",
			hi + `,{"role":"function","name":"f","content":"r"}`,
			[]ir.Message{user(text("hi"))},
			[]string{"input:function.orphan"}},
		{"a conversation that starts with the assistant",
			`{"role":"assistant","content":"Hello!"},` + hi,
			[]ir.Message{assistant(text("Hello!")), user(text("hi"))},
			nil},
	} {
		req, err := DecodeRequest([]byte(chatReq(c.messages, "")))
		if err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(req.Messages, c.want) || !reflect.DeepEqual(sorted(req.Dropped), c.dropped) {
			t.Errorf("%s:\n got %+v\nwant %+v\n dropped %v", c.name, req.Messages, c.want, sorted(req.Dropped))
		}
		// Written for a Chat target it is a conversation a strict server takes.
		body, _, err := EncodeRequest(req, "m")
		if err != nil {
			t.Errorf("%s: EncodeRequest: %v", c.name, err)
		} else if err := CheckRequest(body); err != nil {
			t.Errorf("%s: %v\n%s", c.name, err, body)
		}
	}
}

func TestDecodeRequest_NeverPanics(t *testing.T) {
	for _, body := range []string{``, `null`, `5`, `"x"`, `{`, `{"messages":null}`, `{"messages":[null]}`, `{"messages":[{}]}`, `{"messages":[{"role":null}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[null]}]}`, `{"messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":null}]}]}`,
		`{"messages":[{"role":"tool"}]}`, `{"messages":[{"role":"user","content":[null]}]}`, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":null}]}]}`,
		`{"messages":[` + hi + `],"tools":[null]}`, `{"messages":[` + hi + `],"tool_choice":{}}`, `{"messages":[` + hi + `],"stream_options":null,"response_format":null}`,
		"\xff\xfe", strings.Repeat("[", 100000)} {
		req, err := DecodeRequest([]byte(body))
		var bad *ir.BadRequestError
		if err != nil && (!errors.As(err, &bad) || !reflect.DeepEqual(req, ir.Request{})) {
			t.Errorf("%.40s: err = %v", body, err)
		}
	}
}

// ---------------------------------------------------------------- answer

func frozen() time.Time { return time.Unix(1_700_000_000, 0) }

func TestEncodeResponse(t *testing.T) {
	resp := ir.Response{ID: "msg_01B", Model: "claude-x", Stop: ir.StopToolUse, Usage: ir.Usage{InputTokens: 131, OutputTokens: 17},
		Parts: []ir.Part{{Kind: ir.Thinking, Text: "hm"}, text("Checking both."),
			toolUse("toolu_1", "get_weather", `{"city":"Oslo","days":[1,2,3]}`), text("One moment."), toolUse("toolu_2", "get_weather", `{ "city": "Rome", "unit": "°C" }`)}}
	body, err := EncodeResponse(resp, "asked-for", frozen())
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, `{"id":"chatcmpl-msg_01B","object":"chat.completion","created":1700000000,"model":"claude-x",
"choices":[{"index":0,"message":{"role":"assistant","content":"Checking both.\n\nOne moment.","refusal":null,"reasoning_content":"hm","tool_calls":[
 {"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\",\"days\":[1,2,3]}"}},
 {"id":"toolu_2","type":"function","function":{"name":"get_weather","arguments":"{ \"city\": \"Rome\", \"unit\": \"°C\" }"}}]},
 "logprobs":null,"finish_reason":"tool_calls"}],
"usage":{"prompt_tokens":131,"completion_tokens":17,"total_tokens":148}}`)
	// What a Chat target would have answered reads back as the same answer.
	back, err := DecodeResponse(body)
	if err != nil || back.Stop != ir.StopToolUse || back.Usage != resp.Usage || len(back.Parts) != 4 || string(back.Parts[3].Input) != `{ "city": "Rome", "unit": "°C" }` {
		t.Fatalf("%v %+v", err, back)
	}

	// Nothing said: content is null, tool_calls and reasoning_content are not written.
	body, err = EncodeResponse(ir.Response{Stop: ir.StopEnd}, "asked-for", frozen())
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, `{"id":"chatcmpl-burrow","object":"chat.completion","created":1700000000,"model":"asked-for",
"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null},"logprobs":null,"finish_reason":"stop"}],
"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)

	for stop, want := range map[ir.StopReason]string{ir.StopEnd: "stop", ir.StopSequence: "stop", ir.StopUnknown: "stop", ir.StopMaxTokens: "length",
		ir.StopRefusal: "content_filter", ir.StopToolUse: "stop"} { // tool_use without a call is no tool turn
		body, _ := EncodeResponse(ir.Response{ID: "chatcmpl-1", Parts: []ir.Part{text("x")}, Stop: stop}, "m", frozen())
		if !bytes.Contains(body, []byte(`"finish_reason":"`+want+`"`)) || !bytes.Contains(body, []byte(`"id":"chatcmpl-1"`)) {
			t.Errorf("%q: %s", stop, body)
		}
	}
	// A tool call without arguments has "{}".
	body, _ = EncodeResponse(ir.Response{Parts: []ir.Part{{Kind: ir.ToolUse, ToolID: "t", ToolName: "f"}}, Stop: ir.StopToolUse}, "m", frozen())
	if !bytes.Contains(body, []byte(`"arguments":"{}"`)) || !bytes.Contains(body, []byte(`"content":null`)) {
		t.Fatalf("%s", body)
	}
	// Usage figures that cannot be: never negative, and the total does not wrap.
	body, _ = EncodeResponse(ir.Response{Usage: ir.Usage{InputTokens: -5, OutputTokens: 1<<63 - 1}}, "m", frozen())
	if !bytes.Contains(body, []byte(`"usage":{"prompt_tokens":0,"completion_tokens":9223372036854775807,"total_tokens":9223372036854775807}`)) {
		t.Fatalf("%s", body)
	}
}

func TestEncodeResponse_Refused(t *testing.T) {
	for name, resp := range map[string]ir.Response{
		"a tool call without an id":    {Parts: []ir.Part{{Kind: ir.ToolUse, ToolName: "f"}}},
		"a tool call without a name":   {Parts: []ir.Part{{Kind: ir.ToolUse, ToolID: "t"}}},
		"an input that is no object":   {Parts: []ir.Part{toolUse("t", "f", `[SECRET]`)}},
		"a part an answer cannot hold": {Parts: []ir.Part{toolResult("t", "x")}},
		"too many parts":               {Parts: make([]ir.Part, ir.MaxParts+1)},
	} {
		body, err := EncodeResponse(resp, "m", frozen())
		if !errors.Is(err, ErrUnencodable) || body != nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: err = %v, body %s", name, err, body)
		}
	}
}

func TestEncodeError(t *testing.T) {
	body := EncodeError(429, "upstream_error", "slow \"down\"\n")
	assertJSONEqual(t, body, `{"error":{"message":"slow \"down\"\n","type":"burrow_error","code":"upstream_error"}}`)
}

// ---------------------------------------------------------------- stream

func ev(kind ir.EventKind, index int) ir.Event { return ir.Event{Kind: kind, Index: index} }
func startEv() ir.Event {
	return ir.Event{Kind: ir.Start, ID: "msg_01B", Model: "claude-x", Usage: ir.Usage{InputTokens: 31}}
}
func textStart(i int) ir.Event {
	return ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.Text}}
}
func toolStart(i int, id, name string) ir.Event {
	return ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.ToolUse, ToolID: id, ToolName: name}}
}
func textDelta(i int, s string) ir.Event { return ir.Event{Kind: ir.TextDelta, Index: i, Text: s} }
func argsDelta(i int, s string) ir.Event {
	return ir.Event{Kind: ir.ToolArgsDelta, Index: i, ArgsJSON: s}
}
func finishEv(stop ir.StopReason, in, out int) ir.Event {
	return ir.Event{Kind: ir.Finish, Stop: stop, Usage: ir.Usage{InputTokens: in, OutputTokens: out}}
}

// encodeEvents writes events with a StreamEncoder; closeIt calls Close after them.
func encodeEvents(events []ir.Event, usage, closeIt bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := NewStreamEncoder(&buf, "asked-for", frozen(), usage)
	var first error
	for _, e := range events {
		if err := enc.Write(e); err != nil && first == nil {
			first = err
		}
	}
	if closeIt {
		if err := enc.Close(); err != nil && first == nil {
			first = err
		}
	}
	return buf.Bytes(), first
}

func mustStream(t *testing.T, events []ir.Event, usage, closeIt bool) (Stream, []byte) {
	t.Helper()
	raw, err := encodeEvents(events, usage, closeIt)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s, err := CheckStream(raw)
	if err != nil {
		t.Fatalf("a client refuses the stream: %v\n%s", err, raw)
	}
	return s, raw
}

func dataLines(raw []byte) []string {
	var out []string
	for _, frame := range strings.Split(strings.TrimSuffix(string(raw), "\n\n"), "\n\n") {
		out = append(out, strings.TrimPrefix(frame, "data: "))
	}
	return out
}

func TestStreamEncoder_Text(t *testing.T) {
	events := []ir.Event{startEv(), textStart(0), textDelta(0, "Hello"), textDelta(0, " \"there\".\n"), ev(ir.PartStop, 0), finishEv(ir.StopEnd, 0, 4)}
	_, raw := mustStream(t, events, true, true)
	head := `{"id":"chatcmpl-msg_01B","object":"chat.completion.chunk","created":1700000000,"model":"claude-x",`
	want := []string{
		head + `"choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}],"usage":null}`,
		head + `"choices":[{"index":0,"delta":{"content":"Hello"},"logprobs":null,"finish_reason":null}],"usage":null}`,
		head + `"choices":[{"index":0,"delta":{"content":" \"there\".\n"},"logprobs":null,"finish_reason":null}],"usage":null}`,
		head + `"choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}],"usage":null}`,
		// The input figure the start told stands when the finish has none.
		head + `"choices":[],"usage":{"prompt_tokens":31,"completion_tokens":4,"total_tokens":35}}`,
		`[DONE]`,
	}
	if got := dataLines(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %s\nwant %s", strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
	// Without include_usage no chunk speaks of usage, and none has empty choices.
	s, raw := mustStream(t, events, false, true)
	if bytes.Contains(raw, []byte("usage")) || bytes.Contains(raw, []byte(`"choices":[]`)) || s.HasUsage || !s.Done || s.Content != "Hello \"there\".\n" || s.FinishReason != "stop" {
		t.Fatalf("%+v\n%s", s, raw)
	}
	// One Write of the writer per frame, so that every frame can be flushed.
	var w countWrites
	enc := NewStreamEncoder(&w, "m", frozen(), true)
	for _, e := range events {
		enc.Write(e)
	}
	if w.n != 6 {
		t.Fatalf("%d writes for 6 frames", w.n)
	}
}

type countWrites struct{ n int }

func (c *countWrites) Write(p []byte) (int, error) { c.n++; return len(p), nil }

func TestStreamEncoder_StartFallbacksAndStops(t *testing.T) {
	s, _ := mustStream(t, []ir.Event{{Kind: ir.Start}, finishEv(ir.StopMaxTokens, 9, 0)}, true, false)
	if s.ID != "chatcmpl-burrow" || s.Model != "asked-for" || s.Created != 1700000000 || s.FinishReason != "length" || s.PromptTokens != 9 || s.TotalTokens != 9 || !s.Done {
		t.Fatalf("%+v", s)
	}
	for stop, want := range map[ir.StopReason]string{ir.StopEnd: "stop", ir.StopSequence: "stop", ir.StopUnknown: "stop", ir.StopRefusal: "content_filter", ir.StopToolUse: "stop"} {
		if s, _ := mustStream(t, []ir.Event{startEv(), finishEv(stop, 1, 1)}, false, false); s.FinishReason != want {
			t.Errorf("%q: %q", stop, s.FinishReason)
		}
	}
}

func TestStreamEncoder_ThinkingAndSeveralTexts(t *testing.T) {
	thinking := ir.Event{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Thinking}}
	s, _ := mustStream(t, []ir.Event{startEv(), thinking, {Kind: ir.ThinkingDelta, Index: 0, Text: "hm"}, ev(ir.PartStop, 0),
		textStart(1), textDelta(1, "a"), textDelta(1, "b"), ev(ir.PartStop, 1),
		toolStart(2, "t", "f"), ev(ir.PartStop, 2),
		textStart(3), ev(ir.PartStop, 3), // a text part that says nothing leaves no trace
		textStart(4), textDelta(4, "c"), ev(ir.PartStop, 4), finishEv(ir.StopToolUse, 1, 1)}, false, false)
	// A second text is set apart by a blank line, as in a whole answer.
	if s.Reasoning != "hm" || s.Content != "ab\n\nc" || len(s.Calls) != 1 || s.FinishReason != "tool_calls" {
		t.Fatalf("%+v", s)
	}
}

// interleaved is an answer with a text and two tool calls whose argument pieces alternate.
func interleaved() []ir.Event {
	return []ir.Event{startEv(), textStart(0), textDelta(0, "Checking."), ev(ir.PartStop, 0),
		toolStart(1, "toolu_1", "get_weather"), toolStart(2, "toolu_2", "get_time"),
		argsDelta(2, `{ "tz"`), argsDelta(1, `{"city":`), argsDelta(2, `: "CET"`), argsDelta(1, `"Oslo"`), argsDelta(1, `}`), argsDelta(2, ` }`),
		ev(ir.PartStop, 1), ev(ir.PartStop, 2), finishEv(ir.StopToolUse, 31, 17)}
}

func TestStreamEncoder_InterleavedToolCallsGoOutAsTheyCome(t *testing.T) {
	s, raw := mustStream(t, interleaved(), true, false)
	if len(s.Calls) != 2 || s.FinishReason != "tool_calls" || s.Content != "Checking." || s.CompletionTokens != 17 {
		t.Fatalf("%+v", s)
	}
	one, two := s.Calls[0], s.Calls[1]
	if one.ID != "toolu_1" || one.Name != "get_weather" || one.Arguments != `{"city":"Oslo"}` || !reflect.DeepEqual(one.Deltas, []string{`{"city":`, `"Oslo"`, `}`}) ||
		two.ID != "toolu_2" || two.Name != "get_time" || two.Arguments != `{ "tz": "CET" }` || !reflect.DeepEqual(two.Deltas, []string{`{ "tz"`, `: "CET"`, ` }`}) {
		t.Fatalf("%+v", s.Calls)
	}
	// Nothing is held back: every event that carries something is one frame, in the order it came.
	lines := dataLines(raw)
	if len(lines) != 2+2+6+3 {
		t.Fatalf("%d frames:\n%s", len(lines), raw)
	}
	for i, want := range []string{
		`"delta":{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`,
		`"delta":{"tool_calls":[{"index":1,"id":"toolu_2","type":"function","function":{"name":"get_time","arguments":""}}]}`,
		`"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{ \"tz\""}}]}`,
		`"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`,
	} {
		if !strings.Contains(lines[2+i], want) {
			t.Errorf("frame %d: %s\n want %s", 2+i, lines[2+i], want)
		}
	}
}

func TestStreamEncoder_ToolCallWithoutArguments(t *testing.T) {
	// A client parses the joined arguments as JSON: a call that got none gets "{}" when it stops.
	s, _ := mustStream(t, []ir.Event{startEv(), toolStart(0, "t", "f"), ev(ir.PartStop, 0), finishEv(ir.StopToolUse, 1, 1)}, false, false)
	if len(s.Calls) != 1 || s.Calls[0].Arguments != "{}" {
		t.Fatalf("%+v", s.Calls)
	}
	// Not when the stream fails while the call is open: nothing says that call was whole.
	s, _ = mustStream(t, []ir.Event{startEv(), toolStart(0, "t", "f"), {Kind: ir.Error, Err: "gone"}}, false, false)
	if len(s.Calls) != 1 || s.Calls[0].Arguments != "" || s.Done || s.ErrMessage != "gone" {
		t.Fatalf("%+v", s)
	}
}

func TestStreamEncoder_BadEndings(t *testing.T) {
	open := []ir.Event{startEv(), textStart(0), textDelta(0, "par"), ev(ir.PartStop, 0), toolStart(1, "t", "f"), argsDelta(1, `{"a":`)}
	for _, c := range []struct {
		name    string
		events  []ir.Event
		closeIt bool
		message string
	}{
		{"the provider's error", append(open[:len(open):len(open)], ev(ir.PartStop, 1), ir.Event{Kind: ir.Error, Err: "Overloaded"}), false, "Overloaded"},
		{"an error without words", append(open[:len(open):len(open)], ir.Event{Kind: ir.Error}), false, errProvider},
		{"the events stop coming", open, true, errEarlyEnd},
		{"the events stop after the start", []ir.Event{startEv()}, true, errEarlyEnd},
		{"an error before the start", []ir.Event{{Kind: ir.Error, Err: "first"}}, true, "first"},
		{"nothing at all", nil, true, errEarlyEnd},
	} {
		for _, usage := range []bool{false, true} {
			s, raw := mustStream(t, c.events, usage, c.closeIt)
			// The stream ends with the error object, in the OpenAI shape: no finish_reason, no usage
			// chunk and no [DONE] that would let a client take the answer for complete.
			if s.Done || s.FinishReason != "" || s.HasUsage || s.ErrMessage != c.message || s.ErrType == "" || bytes.Contains(raw, []byte("[DONE]")) {
				t.Errorf("%s: %+v\n%s", c.name, s, raw)
			}
			lines := dataLines(raw)
			var last struct {
				Error struct{ Message, Type, Code string }
			}
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Error.Message != c.message || last.Error.Type != "server_error" || last.Error.Code != "upstream_error" {
				t.Errorf("%s: last frame %s", c.name, lines[len(lines)-1])
			}
		}
	}
}

func TestStreamEncoder_AfterTheEndNothingIsWritten(t *testing.T) {
	for _, events := range [][]ir.Event{
		{startEv(), finishEv(ir.StopEnd, 1, 1)},
		{startEv(), {Kind: ir.Error, Err: "x"}},
	} {
		var buf bytes.Buffer
		enc := NewStreamEncoder(&buf, "m", frozen(), true)
		for _, e := range events {
			enc.Write(e)
		}
		n := buf.Len()
		for _, e := range []ir.Event{startEv(), textStart(0), textDelta(0, "late"), finishEv(ir.StopEnd, 1, 1), {Kind: ir.Error, Err: "late"}} {
			if err := enc.Write(e); err != nil {
				t.Fatalf("Write after the end: %v", err)
			}
		}
		if err := enc.Close(); err != nil || buf.Len() != n {
			t.Fatalf("something was written after the end (%v):\n%s", err, buf.Bytes()[n:])
		}
	}
}

func TestStreamEncoder_MalformedSequencesEndInAnError(t *testing.T) {
	big := strings.Repeat("x", ir.MaxToolArgsBytes/2+1)
	var manyCalls []ir.Event
	for i := 0; i <= ir.MaxToolCalls; i++ {
		manyCalls = append(manyCalls, toolStart(i, fmt.Sprint("t", i), "f"))
	}
	for _, c := range []struct {
		name   string
		events []ir.Event
		want   error
	}{
		{"no start", []ir.Event{textStart(0)}, ir.ErrSequence},
		{"two starts", []ir.Event{startEv(), startEv()}, ir.ErrSequence},
		{"a part out of order", []ir.Event{startEv(), textStart(1)}, ir.ErrSequence},
		{"a delta for a part that is not open", []ir.Event{startEv(), textDelta(0, "x")}, ir.ErrSequence},
		{"a text delta for a tool call", []ir.Event{startEv(), toolStart(0, "t", "f"), textDelta(0, "x")}, ir.ErrSequence},
		{"an argument delta for a text", []ir.Event{startEv(), textStart(0), argsDelta(0, "{}")}, ir.ErrSequence},
		{"a delta after the stop", []ir.Event{startEv(), textStart(0), ev(ir.PartStop, 0), textDelta(0, "x")}, ir.ErrSequence},
		{"a second stop", []ir.Event{startEv(), textStart(0), ev(ir.PartStop, 0), ev(ir.PartStop, 0)}, ir.ErrSequence},
		{"a tool call without a name", []ir.Event{startEv(), toolStart(0, "t", "")}, ir.ErrSequence},
		{"a tool call without an id", []ir.Event{startEv(), toolStart(0, "", "f")}, ir.ErrSequence},
		{"one id twice", []ir.Event{startEv(), toolStart(0, "t", "f"), toolStart(1, "t", "f")}, ir.ErrSequence},
		{"a part of an unknown kind", []ir.Event{startEv(), {Kind: ir.PartStart, Part: ir.Part{Kind: ir.Image}}}, ir.ErrSequence},
		{"the finish while a part is open", []ir.Event{startEv(), toolStart(0, "t", "f"), finishEv(ir.StopToolUse, 1, 1)}, ir.ErrSequence},
		{"an event of an unknown kind", []ir.Event{startEv(), {Kind: "new"}}, ir.ErrSequence},
		{"too many tool calls", append([]ir.Event{startEv()}, manyCalls...), ir.ErrLimit},
		{"arguments over the limit", []ir.Event{startEv(), toolStart(0, "t", "f"), argsDelta(0, big), argsDelta(0, big)}, ir.ErrLimit},
	} {
		raw, err := encodeEvents(append(c.events, finishEv(ir.StopEnd, 1, 1)), true, true)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		s, cerr := CheckStream(raw)
		want := errUnreadable
		if c.want == ir.ErrLimit {
			want = errTooLarge
		}
		if cerr != nil || s.Done || s.ErrMessage != want {
			t.Errorf("%s: %v, %+v\n%.300s", c.name, cerr, s, raw)
		}
	}
}

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after--; w.after < 0 {
		return 0, errors.New("client gone")
	}
	return len(p), nil
}

func TestStreamEncoder_WriteErrorIsReturnedAndSticks(t *testing.T) {
	enc := NewStreamEncoder(&failingWriter{after: 1}, "m", frozen(), false)
	if err := enc.Write(startEv()); err != nil {
		t.Fatal(err)
	}
	for _, e := range []ir.Event{textStart(0), textDelta(0, "x"), textDelta(0, "y"), finishEv(ir.StopEnd, 1, 1)} {
		if err := enc.Write(e); e.Kind == ir.TextDelta && (err == nil || err.Error() != "client gone") {
			t.Fatalf("err = %v", err)
		}
	}
	if err := enc.Close(); err == nil || err.Error() != "client gone" {
		t.Fatalf("Close = %v", err)
	}
}

func TestCheckStream_RefusesWhatAClientWould(t *testing.T) {
	good, _ := encodeEvents(interleaved(), true, false)
	if _, err := CheckStream(good); err != nil {
		t.Fatal(err)
	}
	lines := dataLines(good)
	join := func(lines ...string) []byte {
		var b bytes.Buffer
		for _, l := range lines {
			b.WriteString("data: " + l + "\n\n")
		}
		return b.Bytes()
	}
	without := func(i int) []string { return append(append([]string(nil), lines[:i]...), lines[i+1:]...) }
	replace := func(i int, old, new string) []string {
		out := append([]string(nil), lines...)
		if !strings.Contains(out[i], old) {
			t.Fatalf("frame %d does not hold %s: %s", i, old, out[i])
		}
		out[i] = strings.Replace(out[i], old, new, 1)
		return out
	}
	last := len(lines) - 1 // [DONE]; before it the usage chunk, before that the finish
	for name, raw := range map[string][]byte{
		"no end":                           join(lines[:last-2]...),
		"no [DONE]":                        join(lines[:last]...),
		"no finish_reason":                 join(without(last - 2)...),
		"no first chunk with the role":     join(lines[1:]...),
		"a frame after [DONE]":             join(append(append([]string(nil), lines...), lines[1])...),
		"an event line":                    append([]byte("event: message\n"), good...),
		"data that is no JSON":             join(replace(1, `{"id"`, `{id`)...),
		"another id":                       join(replace(1, `chatcmpl-msg_01B`, `chatcmpl-other`)...),
		"another object":                   join(replace(1, `chat.completion.chunk`, `chat.completion`)...),
		"a call that starts without an id": join(replace(2, `"id":"toolu_1",`, ``)...),
		"a call that starts without name":  join(replace(2, `"name":"get_weather",`, ``)...),
		"a call index out of order":        join(replace(2, `"index":0`, `"index":5`)...),
		"arguments for an unknown call":    join(replace(4, `"index":1`, `"index":7`)...),
		"arguments that are cut":           join(without(last - 4)...),
		"an unknown finish_reason":         join(replace(last-2, `"finish_reason":"tool_calls"`, `"finish_reason":"done"`)...),
		"tool_calls without a call":        join(lines[0], lines[1], lines[last-2], lines[last-1], lines[last]),
		"a usage chunk with choices":       join(replace(last-1, `"choices":[]`, `"choices":[{"index":0,"delta":{}}]`)...),
		"an error after the finish":        join(append(append([]string(nil), lines[:last]...), `{"error":{"message":"x","type":"server_error"}}`)...),
		"an error and then [DONE]":         join(lines[0], `{"error":{"message":"x","type":"server_error"}}`, `[DONE]`),
		"an error without a message":       join(lines[0], `{"error":{"type":"server_error"}}`),
		"empty":                            nil,
	} {
		if s, err := CheckStream(raw); err == nil {
			t.Errorf("%s: accepted: %+v", name, s)
		}
	}
}

// ---------------------------------------------------------------- properties and fuzz

// genEvents makes a random event sequence that follows the rules of ir.Event: text and thinking
// parts one after the other, tool calls open side by side with their pieces interleaved, and a
// text that opens while calls are open. It returns what the answer adds up to as well.
func genEvents(rng *rand.Rand) ([]ir.Event, ir.Response) {
	resp := ir.Response{ID: "chatcmpl-" + fmt.Sprint(rng.Intn(1000)), Model: genText(rng), Usage: ir.Usage{InputTokens: rng.Intn(1 << 20), OutputTokens: rng.Intn(1 << 20)}}
	events := []ir.Event{{Kind: ir.Start, ID: resp.ID, Model: resp.Model, Usage: ir.Usage{InputTokens: resp.Usage.InputTokens}}}
	type open struct {
		index  int
		pieces []string
	}
	var calls []open
	step := func() { // one piece of one open call
		i := rng.Intn(len(calls))
		c := &calls[i]
		if len(c.pieces) == 0 {
			events = append(events, ev(ir.PartStop, c.index))
			calls = append(calls[:i], calls[i+1:]...)
			return
		}
		events = append(events, argsDelta(c.index, c.pieces[0]))
		c.pieces = c.pieces[1:]
	}
	for n := rng.Intn(6); n > 0; n-- {
		index := len(resp.Parts)
		switch rng.Intn(3) {
		case 0:
			kind, delta := ir.Text, ir.TextDelta
			if rng.Intn(3) == 0 {
				kind, delta = ir.Thinking, ir.ThinkingDelta
			}
			s := genText(rng)
			resp.Parts = append(resp.Parts, ir.Part{Kind: kind, Text: s})
			events = append(events, ir.Event{Kind: ir.PartStart, Index: index, Part: ir.Part{Kind: kind}})
			for _, piece := range cutRunes(rng, s) {
				events = append(events, ir.Event{Kind: delta, Index: index, Text: piece})
				if len(calls) > 0 && rng.Intn(2) == 0 {
					step()
				}
			}
			events = append(events, ev(ir.PartStop, index))
		default:
			input := ""
			if rng.Intn(5) > 0 {
				input = genObject(rng, 3)
			}
			p := ir.Part{Kind: ir.ToolUse, ToolID: fmt.Sprintf("call_%d", index), ToolName: genText(rng), Input: json.RawMessage(input)}
			if input == "" {
				p.Input = json.RawMessage("{}")
			}
			resp.Parts = append(resp.Parts, p)
			events = append(events, toolStart(index, p.ToolID, p.ToolName))
			calls = append(calls, open{index: index, pieces: cutRunes(rng, input)})
		}
		for len(calls) > 0 && rng.Intn(3) > 0 {
			step()
		}
	}
	for len(calls) > 0 {
		step()
	}
	hasCall := false
	for _, p := range resp.Parts {
		hasCall = hasCall || p.Kind == ir.ToolUse
	}
	resp.Stop = []ir.StopReason{ir.StopEnd, ir.StopMaxTokens, ir.StopRefusal}[rng.Intn(3)]
	if hasCall && rng.Intn(4) > 0 {
		resp.Stop = ir.StopToolUse
	}
	return append(events, ir.Event{Kind: ir.Finish, Stop: resp.Stop, Usage: resp.Usage}), resp
}

// chatShape is what a Chat Completions answer can say of an answer: one reasoning text, one
// content text (several are set apart by a blank line), then the tool calls.
func chatShape(r ir.Response) ir.Response { return chatShapeWith(r, "\n\n") }

func chatShapeWith(r ir.Response, gap string) ir.Response {
	var thinking, texts []string
	var calls []ir.Part
	for _, p := range r.Parts {
		switch p.Kind {
		case ir.Thinking:
			thinking = append(thinking, p.Text)
		case ir.Text:
			texts = append(texts, p.Text)
		default:
			calls = append(calls, p)
		}
	}
	r.Parts = nil
	if len(thinking) > 0 {
		r.Parts = append(r.Parts, ir.Part{Kind: ir.Thinking, Text: strings.Join(thinking, gap)})
	}
	if len(texts) > 0 {
		r.Parts = append(r.Parts, text(strings.Join(texts, gap)))
	}
	r.Parts = append(r.Parts, calls...)
	return r
}

func TestProperty_StreamKeepsEveryByteAndEveryCall(t *testing.T) {
	rng := rand.New(rand.NewSource(20261007))
	for i := 0; i < 1500; i++ {
		events, want := genEvents(rng)
		raw, err := encodeEvents(events, true, rng.Intn(2) == 0)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		s, err := CheckStream(raw)
		if err != nil || !s.Done {
			t.Fatalf("case %d: %v\n%s", i, err, raw)
		}
		// Review Focus 1: per index, the pieces are the upstream's pieces and add up to its bytes.
		var calls []ir.Part
		pieces := map[int][]string{}
		for _, e := range events {
			if e.Kind == ir.ToolArgsDelta {
				pieces[e.Index] = append(pieces[e.Index], e.ArgsJSON)
			}
		}
		for n, p := range want.Parts {
			if p.Kind == ir.ToolUse {
				c := s.Calls[len(calls)]
				if c.ID != p.ToolID || c.Name != p.ToolName || c.Arguments != string(p.Input) {
					t.Fatalf("case %d: call %d is %+v, want %+v", i, len(calls), c, p)
				}
				if sent := pieces[n]; len(sent) > 0 && !reflect.DeepEqual(c.Deltas, sent) {
					t.Fatalf("case %d: pieces %q, want %q", i, c.Deltas, sent)
				}
				calls = append(calls, p)
			}
		}
		if len(calls) != len(s.Calls) || s.PromptTokens != want.Usage.InputTokens || s.CompletionTokens != want.Usage.OutputTokens {
			t.Fatalf("case %d: %+v", i, s)
		}
		// What the decoder of the target half reads from it is the same answer, in Chat's shape.
		decoded, err := decodeStream(t, raw, 1+rng.Intn(200))
		if err != nil {
			t.Fatalf("case %d: decode: %v", i, err)
		}
		got, err := irtest.Collect(decoded)
		shape := chatShape(want)
		if len(calls) > 0 && shape.Stop == ir.StopEnd {
			shape.Stop = ir.StopToolUse // the target half reads "stop" with tool calls as a tool turn
		}
		// The decoder opens a new text part after a tool call; the texts are the same.
		got = chatShapeWith(got, "")
		if err != nil || !reflect.DeepEqual(got, shape) {
			t.Fatalf("case %d: %v\n got %+v\nwant %+v\n%s", i, err, got, shape, raw)
		}
	}
}

func FuzzDecodeRequest(f *testing.F) {
	f.Add(fixture(f, "req_tools.json"))
	f.Add([]byte(chatReq(`{"role":"developer","content":[{"type":"text","text":"s"}]},{"role":"tool","tool_call_id":"a","content":"early"},`+
		`{"role":"assistant","content":null,"refusal":"no","reasoning_content":"r","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{\"a\":"}},{"id":"a","type":"function","function":{"name":"f","arguments":{}}},{"id":"c","type":"custom"}]},`+
		`{"role":"assistant","function_call":{"name":"g","arguments":"{}"}},{"role":"function","name":"g","content":"r"},`+
		`{"role":"user","name":"n","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk=","detail":"low"}}]},{"role":"system","content":"late"}`,
		`"n":2,"stop":"x","max_completion_tokens":5,"tool_choice":{"type":"function","function":{"name":"f"}},"tools":[{"type":"function","function":{"name":"f","strict":true}},{"type":"x"}],"stream_options":{"include_usage":true,"z":1},"seed":1,"zz":null`)))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := DecodeRequest(body)
		_ = IncludeUsage(body)
		if err != nil {
			var bad *ir.BadRequestError
			if !errors.As(err, &bad) || bad.Format != "chat" || bad.Field == "" || bad.Reason == "" || !reflect.DeepEqual(req, ir.Request{}) {
				t.Fatalf("err = %#v", err)
			}
			return
		}
		// No name in the dropped list stands alone as the client chose it.
		for _, name := range req.Dropped {
			if name == "" || name == "more" {
				t.Fatalf("dropped %q", name)
			}
		}
		if len(req.Messages) == 0 || len(req.Messages) > ir.MaxMessages || len(req.Tools) > ir.MaxTools || len(req.System) > ir.MaxParts {
			t.Fatalf("over a limit or empty: %d messages", len(req.Messages))
		}
		// The session rule: whatever was taken can be written for a Chat target, and a strict
		// server accepts how its tool calls and results are paired.
		out, _, err := EncodeRequest(req, "m")
		if err != nil {
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("EncodeRequest: %v", err)
			}
			return
		}
		if err := irtest.CheckPairing(out); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	})
}

// FuzzStreamEncoder feeds an encoder any event sequence. Oracle: it never panics, what it wrote
// is a stream a client reads to an end, and it ended well only when the sequence was sound.
func FuzzStreamEncoder(f *testing.F) {
	f.Add([]byte{0, 1, 2, 2, 5, 6}, true, true)
	f.Add([]byte{0, 3, 4, 3, 4, 4, 5, 5, 6}, false, false)
	f.Add([]byte{0, 1, 2, 7}, true, false)
	f.Add([]byte{7}, true, true)
	f.Add([]byte{1, 0, 0, 6, 6}, false, true)
	f.Fuzz(func(t *testing.T, ops []byte, usage, closeIt bool) {
		if len(ops) > 4096 {
			ops = ops[:4096]
		}
		var events []ir.Event
		parts := 0
		pieces := []string{`{"a":`, `1}`, `{}`, "é", " ", `"`, `[`}
		for i, op := range ops {
			index := int(op >> 4)
			if parts > 0 {
				index %= parts + 1
			}
			switch op & 0x0f {
			case 0:
				events = append(events, ir.Event{Kind: ir.Start, ID: "id", Model: "m", Usage: ir.Usage{InputTokens: int(op)}})
			case 1:
				events = append(events, textStart(parts))
				parts++
			case 2:
				events = append(events, textDelta(index, pieces[i%len(pieces)]))
			case 3:
				events = append(events, toolStart(parts, fmt.Sprint("t", i%70), []string{"f", "g", ""}[i%3]))
				parts++
			case 4:
				events = append(events, argsDelta(index, pieces[i%len(pieces)]))
			case 5:
				events = append(events, ev(ir.PartStop, index))
			case 6:
				events = append(events, finishEv([]ir.StopReason{ir.StopEnd, ir.StopToolUse, ir.StopMaxTokens, ""}[i%4], i, i))
			case 7:
				events = append(events, ir.Event{Kind: ir.Error, Err: []string{"gone", ""}[i%2]})
			case 8:
				events = append(events, ir.Event{Kind: ir.PartStart, Index: parts, Part: ir.Part{Kind: ir.Thinking}})
				parts++
			case 9:
				events = append(events, ir.Event{Kind: ir.ThinkingDelta, Index: index, Text: "hm"})
			default:
				events = append(events, ir.Event{Kind: ir.EventKind(fmt.Sprint("k", op)), Index: index - 1})
			}
		}
		raw, err := encodeEvents(events, usage, closeIt)
		if err != nil && !errors.Is(err, ir.ErrSequence) && !errors.Is(err, ir.ErrLimit) {
			t.Fatalf("an error of no known kind: %v", err)
		}
		if !utf8.Valid(raw) {
			t.Fatal("the stream is not UTF-8")
		}
		s, cerr := checkStream(raw, false)
		_, sound := irtest.Collect(events)
		switch {
		case len(raw) == 0:
			// Nothing was handed in that had to be written (and Close was not called).
			if closeIt || err != nil {
				t.Fatalf("nothing written: closeIt %v, err %v", closeIt, err)
			}
		case errors.Is(cerr, errNotEnded):
			if closeIt || err != nil {
				t.Fatalf("a stream without an end though it was closed or failed: %v\n%s", err, raw)
			}
		case cerr != nil:
			t.Fatalf("a client refuses the stream: %v\n%s", cerr, raw)
		case s.Done:
			// It ended well: then the events up to the Finish were a sound answer — but for the
			// arguments, which the encoder passes on and the decoder before it judges.
			if err != nil || (sound != nil && !errors.Is(sound, ir.ErrBadJSON) && !errors.Is(sound, ir.ErrSequence)) {
				t.Fatalf("ended well: err %v, Collect %v\n%s", err, sound, raw)
			}
			if s.HasUsage != usage {
				t.Fatalf("usage chunk %v, asked %v", s.HasUsage, usage)
			}
		default:
			if s.ErrMessage == "" {
				t.Fatalf("ended badly without a message\n%s", raw)
			}
		}
	})
}

// FuzzChatStreamRoundTrip: any stream the target-half decoder reads to a Finish is written by
// the encoder as a stream a client accepts, with the same calls and bytes.
func FuzzChatStreamRoundTrip(f *testing.F) {
	// Short seeds: the fuzzer minimises every input that reaches new code, and a long one costs it minutes.
	f.Add([]byte("data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"delta\":{\"reasoning\":\"r\",\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3}}\n\ndata: [DONE]\n\n"), 7)
	f.Add([]byte("data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}},{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"g\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"), 64)
	f.Add([]byte("data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: {\"error\":{\"message\":\"gone\"}}\n\n"), 3)
	f.Fuzz(func(t *testing.T, raw []byte, piece int) {
		if piece < 1 {
			piece = 1
		}
		p := sse.NewParser(MaxFrameBytes)
		d := NewStreamDecoder()
		var buf bytes.Buffer
		enc := NewStreamEncoder(&buf, "m", frozen(), true)
		var events []ir.Event
		write := func(evs []ir.Event) {
			for _, e := range evs {
				events = append(events, e)
				if err := enc.Write(e); err != nil {
					t.Fatalf("the encoder refused what the decoder emitted: %v (%+v)", err, e)
				}
			}
		}
		for i := 0; i < len(raw); i += piece {
			frames, err := p.Feed(raw[i:min(i+piece, len(raw))])
			for _, fr := range frames {
				evs, _ := d.Feed(fr.Data)
				write(evs)
			}
			if err != nil {
				break
			}
		}
		for _, fr := range p.Flush() {
			evs, _ := d.Feed(fr.Data)
			write(evs)
		}
		write(d.Close())
		if err := enc.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := CheckStream(buf.Bytes())
		if err != nil {
			t.Fatalf("%v\n%s", err, buf.Bytes())
		}
		want, cerr := irtest.Collect(events)
		if s.Done != (cerr == nil) {
			t.Fatalf("Done %v, Collect %v", s.Done, cerr)
		}
		if !s.Done {
			return
		}
		var ids []string
		for _, p := range want.Parts {
			if p.Kind == ir.ToolUse {
				c := s.Calls[len(ids)]
				if c.ID != p.ToolID || c.Name != p.ToolName || c.Arguments != string(p.Input) {
					t.Fatalf("call %+v, want %+v", c, p)
				}
				ids = append(ids, p.ToolID)
			}
		}
		sort.Strings(ids)
		if len(ids) != len(s.Calls) {
			t.Fatalf("%d calls, want %d", len(s.Calls), len(ids))
		}
	})
}

// An id of a tool call in replayed history that is longer than a tool id may
// be is not refused (no history may end a session): it is cut, the same way
// on the call and on its result, so the two still pair, and reported.
func TestDecodeRequest_LongToolIDInHistoryIsCut(t *testing.T) {
	long := strings.Repeat("x", ir.MaxToolIDBytes+1)
	req := decodeReq(t, chatReq(hi+`,{"role":"assistant","content":null,"tool_calls":[{"id":"`+long+`","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"`+long+`","content":"r"}`, ""))
	var call, result string
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			switch p.Kind {
			case ir.ToolUse:
				call = p.ToolID
			case ir.ToolResult:
				result = p.ToolID
				if p.Text != "r" {
					t.Fatalf("the result's text is %q", p.Text)
				}
			}
		}
	}
	if call == "" || call != result || len(call) > ir.MaxToolIDBytes {
		t.Fatalf("call id %d bytes, result id %d bytes, equal %v", len(call), len(result), call == result)
	}
	if got := strings.Join(ir.Dropped(req.Dropped), ","); got != "input:tool_call.id" {
		t.Fatalf("dropped = %s", got)
	}
}
