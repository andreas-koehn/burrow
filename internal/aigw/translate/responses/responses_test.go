package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// ---------------------------------------------------------------- helpers

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(got, &x); err != nil {
		t.Fatalf("got is not JSON (%v): %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &y); err != nil {
		t.Fatalf("want is not JSON (%v): %s", err, want)
	}
	if !reflect.DeepEqual(x, y) {
		t.Fatalf("JSON differs\n got: %s\nwant: %s", got, want)
	}
}

func text(s string) ir.Part { return ir.Part{Kind: ir.Text, Text: s} }
func use(id, name, input string) ir.Part {
	return ir.Part{Kind: ir.ToolUse, ToolID: id, ToolName: name, Input: json.RawMessage(input)}
}
func result(id, s string) ir.Part { return ir.Part{Kind: ir.ToolResult, ToolID: id, Text: s} }

func decode(t *testing.T, body string) ir.Request {
	t.Helper()
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v\n%.300s", err, body)
	}
	return req
}

// withInput is a request with the given items and further top-level fields.
func withInput(items string, extra ...string) string {
	body := `{"model":"m","input":[` + items + `]`
	for _, e := range extra {
		body += "," + e
	}
	return body + "}"
}

const userItem = `{"role":"user","content":"hi"}`

func refused(t *testing.T, body, field string) *ir.BadRequestError {
	t.Helper()
	req, err := DecodeRequest([]byte(body))
	var bad *ir.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a *ir.BadRequestError for %s\n%.200s", err, field, body)
	}
	if bad.Field != field || bad.Reason == "" || bad.Format != "responses" {
		t.Fatalf("refused %+v, want field %q\n%.200s", bad, field, body)
	}
	if !reflect.DeepEqual(req, ir.Request{}) {
		t.Fatalf("a request came with the error: %+v", req)
	}
	if !strings.HasPrefix(err.Error(), "responses: "+field+": ") {
		t.Fatalf("Error() = %q", err.Error())
	}
	return bad
}

func dropped(t *testing.T, body string) []string {
	t.Helper()
	return ir.Dropped(decode(t, body).Dropped)
}

// ---------------------------------------------------------------- request

func TestDecodeRequest_CodexTurn(t *testing.T) {
	req := decode(t, string(fixture(t, "testdata/req_codex.json")))
	want := ir.Request{
		Model:  "burrow-medium",
		System: []ir.Part{text("You are a coding agent."), text("Be careful.")},
		Messages: []ir.Message{
			{Role: ir.User, Parts: []ir.Part{text("List the files")}},
			{Role: ir.Assistant, Parts: []ir.Part{text("I'll list them."), use("call_1", "shell", `{"command":["ls"]}`)}},
			{Role: ir.User, Parts: []ir.Part{result("call_1", "a.txt\nb.txt")}},
			{Role: ir.User, Parts: []ir.Part{text("Now read a.txt")}},
		},
		Tools: []ir.Tool{{Name: "shell", Description: "Run a command",
			Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}},"required":["command"]}`)}},
		ToolChoice: ir.ToolChoice{Mode: ir.ChoiceAuto},
		Stream:     true,
	}
	got := req
	got.Dropped = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request\n got: %+v\nwant: %+v", got, want)
	}
	// Exactly these: "store": false and "strict": false ask for nothing.
	wantDropped := []string{"client_metadata", "include", "input.namespace", "input.phase", "input:reasoning", "parallel_tool_calls",
		"prompt_cache_key", "reasoning", "stream_options", "text.verbosity", "tool:custom", "tool:local_shell", "tool:web_search"}
	if d := ir.Dropped(req.Dropped); !reflect.DeepEqual(d, wantDropped) {
		t.Fatalf("dropped\n got: %v\nwant: %v", d, wantDropped)
	}
	// The request goes through to a Chat Completions target.
	if _, _, err := chat.EncodeRequest(req, "gpt-x"); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
}

func TestDecodeRequest_InputShapes(t *testing.T) {
	// A plain string is one user message.
	req := decode(t, `{"model":"m","input":"hello","instructions":"Be brief.","max_output_tokens":64,"temperature":0.5,"top_p":0.9}`)
	if !reflect.DeepEqual(req.Messages, []ir.Message{{Role: ir.User, Parts: []ir.Part{text("hello")}}}) ||
		!reflect.DeepEqual(req.System, []ir.Part{text("Be brief.")}) || req.MaxTokens != 64 ||
		*req.Temperature != 0.5 || *req.TopP != 0.9 || req.Stream || req.Dropped != nil {
		t.Fatalf("%+v", req)
	}
	// Items without "type" but with a role; every kind of content part.
	req = decode(t, withInput(`
	 {"role":"system","content":"S1"},
	 {"role":"developer","content":[{"type":"input_text","text":"D1"},{"type":"text","text":"D2"}]},
	 {"role":"user","content":[{"type":"input_text","text":"look"},
	   {"type":"input_image","image_url":"data:image/png;base64,QUJD","detail":"auto"},
	   {"type":"input_image","image_url":"https://example.org/a.png","detail":"high"}]},
	 {"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[
	   {"type":"output_text","text":"I see.","annotations":[],"logprobs":[]},{"type":"refusal","refusal":"No more."}]},
	 {"type":"message","role":"user","content":""}`))
	want := []ir.Message{
		{Role: ir.User, Parts: []ir.Part{text("look"),
			{Kind: ir.Image, MediaType: "image/png", Data: "QUJD"},
			{Kind: ir.Image, Data: "https://example.org/a.png"}}},
		{Role: ir.Assistant, Parts: []ir.Part{text("I see."), text("No more.")}},
		{Role: ir.User, Parts: []ir.Part{text("")}},
	}
	if !reflect.DeepEqual(req.Messages, want) {
		t.Fatalf("messages\n got: %+v\nwant: %+v", req.Messages, want)
	}
	if !reflect.DeepEqual(req.System, []ir.Part{text("S1"), text("D1"), text("D2")}) {
		t.Fatalf("system %+v", req.System)
	}
	if d := ir.Dropped(req.Dropped); !reflect.DeepEqual(d, []string{"input_image.detail"}) {
		t.Fatalf("dropped %v", d)
	}
}

func TestDecodeRequest_SystemMessagesAreHoisted(t *testing.T) {
	// Before the first turn: nothing is lost. After it: the place is, and that is reported.
	req := decode(t, withInput(`{"role":"developer","content":"first"},`+userItem+`,{"role":"system","content":"late"},{"role":"assistant","content":"ok"}`, `"instructions":"I"`))
	if !reflect.DeepEqual(req.System, []ir.Part{text("I"), text("first"), text("late")}) || len(req.Messages) != 2 {
		t.Fatalf("%+v", req)
	}
	if !reflect.DeepEqual(ir.Dropped(req.Dropped), []string{ir.DroppedSystemPosition}) {
		t.Fatalf("dropped %v", req.Dropped)
	}
	if d := dropped(t, withInput(`{"role":"developer","content":"a"},{"role":"system","content":"b"},`+userItem)); d != nil {
		t.Fatalf("dropped %v", d)
	}
	// An empty system message is nothing, wherever it stands.
	if d := dropped(t, withInput(userItem+`,{"role":"system","content":""}`)); d != nil {
		t.Fatalf("dropped %v", d)
	}
	refused(t, withInput(userItem+`,{"role":"system","content":[{"type":"input_image","image_url":"https://x/y.png"}]}`), "input[1].content[0]")
}

func TestDecodeRequest_CallsAndOutputsAreGrouped(t *testing.T) {
	call := func(id, args string) string {
		return fmt.Sprintf(`{"type":"function_call","id":"fc_x","status":"completed","call_id":%q,"name":"f","arguments":%q}`, id, args)
	}
	out := func(id, output string) string {
		return fmt.Sprintf(`{"type":"function_call_output","call_id":%q,"output":%s}`, id, output)
	}
	said := func(s string) string {
		return fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}`, s)
	}
	asst := func(parts ...ir.Part) ir.Message { return ir.Message{Role: ir.Assistant, Parts: parts} }
	user := func(parts ...ir.Part) ir.Message { return ir.Message{Role: ir.User, Parts: parts} }
	hi := user(text("hi"))
	image := func(data string) ir.Part { return ir.Part{Kind: ir.Image, Data: data} }
	const png = `{"type":"input_image","image_url":"data:image/png;base64,QUJD"}`
	cases := map[string]struct {
		items   []string
		want    []ir.Message
		dropped []string
		shape   string // the Chat messages: role and ids
	}{
		"several calls, then their outputs: one assistant turn, tool results in the order of the calls": {
			items: []string{said("Checking."), `{"type":"reasoning","id":"rs_1","summary":[]}`, call("a", `{"x": 1}`), call("b", ``),
				out("b", `"B"`), out("a", `[{"type":"input_text","text":"A1"},{"type":"output_text","text":"A2"}]`),
				call("c", `{}`), out("c", `""`), `{"role":"user","content":"thanks"}`},
			want: []ir.Message{hi, asst(text("Checking."), use("a", "f", `{"x": 1}`), use("b", "f", `{}`)), user(result("a", "A1\nA2"), result("b", "B")),
				asst(use("c", "f", `{}`)), user(result("c", "")), user(text("thanks"))},
			dropped: []string{"input:reasoning"},
			shape:   "user | assistant a b | tool a | tool b | assistant c | tool c | user",
		},
		"what the stream encoder itself writes: message, call, message, call, and the outputs after all of them": {
			items: []string{said("A"), call("a", `{}`), said("B"), call("b", `{}`), out("a", `"1"`), out("b", `"2"`)},
			want:  []ir.Message{hi, asst(text("A"), use("a", "f", `{}`), text("B"), use("b", "f", `{}`)), user(result("a", "1"), result("b", "2"))},
			shape: "user | assistant a b | tool a | tool b",
		},
		"a message between a call and its output": {
			items: []string{call("a", `{}`), said("x"), out("a", `"1"`), said("y")},
			want:  []ir.Message{hi, asst(use("a", "f", `{}`), text("x")), user(result("a", "1")), asst(text("y"))},
			shape: "user | assistant a | tool a | assistant",
		},
		"an output that comes after the next user message is moved up to its call": {
			items: []string{call("a", `{}`), `{"role":"user","content":"wait"}`, said("ok"), out("a", `"late"`), said("then")},
			want:  []ir.Message{hi, asst(use("a", "f", `{}`)), user(result("a", "late")), user(text("wait")), asst(text("ok"), text("then"))},
			shape: "user | assistant a | tool a | user | assistant",
		},
		"calls of two turns": {
			items: []string{call("a", `{}`), out("a", `"1"`), call("a", `{"again":true}`), out("a", `"2"`)},
			want:  []ir.Message{hi, asst(use("a", "f", `{}`)), user(result("a", "1")), asst(use("a", "f", `{"again":true}`)), user(result("a", "2"))},
			shape: "user | assistant a | tool a | assistant a | tool a",
		},
		"a call without an output gets one": {
			items:   []string{call("a", `{}`), call("b", `{}`), out("a", `"1"`), `{"role":"user","content":"go on"}`},
			want:    []ir.Message{hi, asst(use("a", "f", `{}`), use("b", "f", `{}`)), user(result("a", "1"), result("b", "[no output]")), user(text("go on"))},
			dropped: []string{"input:function_call.unanswered"},
			shape:   "user | assistant a b | tool a | tool b | user",
		},
		"a call without an output at the end": {
			items:   []string{call("a", `{}`)},
			want:    []ir.Message{hi, asst(use("a", "f", `{}`)), user(result("a", "[no output]"))},
			dropped: []string{"input:function_call.unanswered"},
			shape:   "user | assistant a | tool a",
		},
		"an output for no call, and a second output for one call, are left out": {
			items:   []string{out("zz", `"x"`), call("a", `{}`), out("a", `"1"`), out("a", `"2"`)},
			want:    []ir.Message{hi, asst(use("a", "f", `{}`)), user(result("a", "1"))},
			dropped: []string{"input:function_call_output.orphan"},
			shape:   "user | assistant a | tool a",
		},
		"an output that stands before its call is that call's output": {
			items:   []string{out("a", `"early"`), out("zz", `"never called"`), call("a", `{}`), call("b", `{}`), out("b", `"2"`), out("a", `"a second one"`)},
			want:    []ir.Message{hi, asst(use("a", "f", `{}`), use("b", "f", `{}`)), user(result("a", "early"), result("b", "2"))},
			dropped: []string{"input:function_call_output.orphan"},
			shape:   "user | assistant a b | tool a | tool b",
		},
		"a call id that is waiting already: the first call stays": {
			items:   []string{call("a", `{"first":1}`), call("a", `{"second":2}`), out("a", `"1"`)},
			want:    []ir.Message{hi, asst(use("a", "f", `{"first":1}`)), user(result("a", "1"))},
			dropped: []string{"input:function_call.duplicate"},
			shape:   "user | assistant a | tool a",
		},
		"arguments that are no JSON object (a call that was cut) are an empty object": {
			items: []string{call("a", `{"a":`), call("b", `[1]`), `{"type":"function_call","call_id":"c","name":"f","arguments":{"k":1}}`, call("d", `nul`),
				out("a", `"1"`), out("b", `"2"`), out("c", `"3"`), out("d", `"4"`)},
			want: []ir.Message{hi, asst(use("a", "f", `{}`), use("b", "f", `{}`), use("c", "f", `{}`), use("d", "f", `{}`)),
				user(result("a", "1"), result("b", "2"), result("c", "3"), result("d", "4"))},
			dropped: []string{"input:function_call.arguments"},
			shape:   "user | assistant a b c d | tool a | tool b | tool c | tool d",
		},
		"images in tool outputs follow the tool results as a user message": {
			items: []string{call("a", `{}`), call("b", `{}`), call("c", `{}`),
				out("b", `[{"type":"input_image","image_url":"https://example.org/b.png","detail":"auto"}]`),
				out("a", `[{"type":"input_text","text":"see"},`+png+`,`+png+`]`), out("c", `"plain"`), `{"role":"user","content":"and?"}`},
			want: []ir.Message{hi, asst(use("a", "f", `{}`), use("b", "f", `{}`), use("c", "f", `{}`)),
				user(result("a", "see"), result("b", "[image]"), result("c", "plain"),
					text("Image returned by tool call a:"), ir.Part{Kind: ir.Image, MediaType: "image/png", Data: "QUJD"}, ir.Part{Kind: ir.Image, MediaType: "image/png", Data: "QUJD"},
					text("Image returned by tool call b:"), image("https://example.org/b.png")),
				user(text("and?"))},
			shape: "user | assistant a b c | tool a | tool b | tool c | user | user",
		},
	}
	for name, c := range cases {
		req, err := DecodeRequest([]byte(withInput(userItem + "," + strings.Join(c.items, ","))))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(req.Messages, c.want) {
			t.Errorf("%s: messages\n got: %+v\nwant: %+v", name, req.Messages, c.want)
		}
		if d := ir.Dropped(req.Dropped); !reflect.DeepEqual(d, c.dropped) {
			t.Errorf("%s: dropped %v, want %v", name, d, c.dropped)
		}
		// What a strict Chat Completions server wants.
		body, _, err := chat.EncodeRequest(req, "m")
		if err != nil {
			t.Errorf("%s: EncodeRequest: %v", name, err)
			continue
		}
		if err := chat.CheckRequest(body); err != nil {
			t.Errorf("%s: %v\n%s", name, err, body)
		}
		var sent struct {
			Messages []struct {
				Role      string
				ToolCalls []struct{ ID string } `json:"tool_calls"`
				CallID    string                `json:"tool_call_id"`
			}
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatal(err)
		}
		var shape []string
		for _, m := range sent.Messages {
			s := m.Role
			for _, c := range m.ToolCalls {
				s += " " + c.ID
			}
			shape = append(shape, strings.TrimSpace(s+" "+m.CallID))
		}
		if got := strings.Join(shape, " | "); got != c.shape {
			t.Errorf("%s: chat messages: %s\nwant: %s", name, got, c.shape)
		}
	}
	// The image of a tool reaches the target as an image, after the tool messages.
	req := decode(t, withInput(userItem+","+call("a", `{}`)+","+out("a", `[`+png+`]`)))
	body, _, err := chat.EncodeRequest(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, `{"model":"m","messages":[{"role":"user","content":"hi"},
	 {"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]},
	 {"role":"tool","tool_call_id":"a","content":"[image]"},
	 {"role":"user","content":[{"type":"text","text":"Image returned by tool call a:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`)
}

func TestDecodeRequest_ItemsThatAreLeftOut(t *testing.T) {
	// Provider-side tools, references and reasoning: left out, reported by type, the request goes through.
	var items []string
	var want []string
	for _, kind := range []string{"reasoning", "item_reference", "web_search_call", "file_search_call", "computer_call",
		"computer_call_output", "code_interpreter_call", "local_shell_call", "local_shell_call_output", "mcp_call",
		"mcp_list_tools", "custom_tool_call", "custom_tool_call_output", "image_generation_call", "more", "what ever"} {
		items = append(items, fmt.Sprintf(`{"type":%q,"id":"x","call_id":"c"}`, kind))
		want = append(want, "input:"+kind)
	}
	req := decode(t, withInput(userItem+","+strings.Join(items, ",")))
	if len(req.Messages) != 1 || !reflect.DeepEqual(ir.Dropped(req.Dropped), ir.Dropped(want)) {
		t.Fatalf("%d messages, dropped %v", len(req.Messages), req.Dropped)
	}
	// Nothing left to send is a client error.
	refused(t, withInput(`{"type":"reasoning"}`), "input")
	refused(t, withInput(`{"role":"system","content":"only a system prompt"}`), "input")
}

func TestDecodeRequest_EveryTopLevelField(t *testing.T) {
	// field → the name it is reported by. Each asks for something.
	fields := map[string]string{
		`"previous_response_id":"resp_1"`:                "previous_response_id",
		`"store":true`:                                   "store",
		`"conversation":{"id":"conv_1"}`:                 "conversation",
		`"background":true`:                              "background",
		`"reasoning":{"effort":"high","summary":"auto"}`: "reasoning",
		`"include":["reasoning.encrypted_content"]`:      "include",
		`"metadata":{"k":"v"}`:                           "metadata",
		`"user":"u1"`:                                    "user",
		`"prompt_cache_key":"k"`:                         "prompt_cache_key",
		`"prompt_cache_retention":"24h"`:                 "prompt_cache_retention",
		`"safety_identifier":"s"`:                        "safety_identifier",
		`"prompt":{"id":"pmpt_1"}`:                       "prompt",
		`"max_tool_calls":3`:                             "max_tool_calls",
		`"top_logprobs":2`:                               "top_logprobs",
		`"stream_options":{"include_obfuscation":true}`:  "stream_options",
		`"truncation":"auto"`:                            "truncation",
		`"service_tier":"flex"`:                          "service_tier",
		`"parallel_tool_calls":false`:                    "parallel_tool_calls",
		`"text":{"format":{"type":"json_schema","name":"n","schema":{}}}`: "text.format",
		`"text":{"format":{"type":"json_object"}}`:                        "text.format",
		`"text":{"verbosity":"low"}`:                                      "text.verbosity",
		`"text":{"new":1}`:                                                "unknown:text.new",
		`"client_metadata":{"k":"v"}`:                                     "client_metadata",
		`"frobnicate":1`:                                                  "unknown:frobnicate",
		`"more":1`:                                                        "unknown:more",
		`"":1`:                                                            "unknown:",
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[]}`: "tool_choice",
		`"tool_choice":{"type":"web_search"}`:                             "tool_choice",
		`"tool_choice":{"type":"custom","name":"apply_patch"}`:            "tool_choice",
	}
	var all []string
	for field, name := range fields {
		if d := dropped(t, withInput(userItem, field)); !reflect.DeepEqual(d, []string{name}) {
			t.Errorf("%s: dropped %v, want [%s]", field, d, name)
		}
		all = append(all, name)
	}
	// What asks for nothing is not reported.
	for _, field := range []string{`"store":false`, `"store":null`, `"background":false`, `"previous_response_id":null`,
		`"include":[]`, `"metadata":{}`, `"metadata":null`, `"user":""`, `"truncation":"disabled"`, `"service_tier":"auto"`,
		`"parallel_tool_calls":true`, `"text":{"format":{"type":"text"}}`, `"text":{}`, `"reasoning":null`, `"tools":[]`,
		`"tool_choice":"auto"`, `"instructions":null`, `"max_output_tokens":null`, `"stream":false`} {
		if d := dropped(t, withInput(userItem, field)); d != nil {
			t.Errorf("%s: dropped %v", field, d)
		}
	}
	// Unknown keys at every depth the package looks at.
	req := decode(t, `{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"x","zz":1}],"yy":2},
	 {"type":"function_call","call_id":"c","name":"f","arguments":"{}","namespace":"n"},
	 {"type":"function_call_output","call_id":"c","output":"o","extra":1}],
	 "tools":[{"type":"function","name":"f","parameters":{},"strict":true,"defer_loading":true}],
	 "tool_choice":{"type":"function","name":"f","mode":"x"}}`)
	want := []string{"input.namespace", "tools.strict", "unknown:content.zz", "unknown:input.extra", "unknown:input.yy",
		"unknown:tool_choice.mode", "unknown:tools.defer_loading"}
	if d := ir.Dropped(req.Dropped); !reflect.DeepEqual(d, want) {
		t.Fatalf("dropped %v\nwant %v", d, want)
	}
	if req.ToolChoice != (ir.ToolChoice{Mode: ir.ChoiceTool, Name: "f"}) {
		t.Fatalf("tool choice %+v", req.ToolChoice)
	}
}

func TestDecodeRequest_ToolsAndToolChoice(t *testing.T) {
	req := decode(t, withInput(userItem, `"tools":[
	 {"type":"function","name":"a","description":"A","parameters":{"type":"object", "properties":{"n":{"type":"integer","minimum":10000000000000000000001}}},"strict":false},
	 {"type":"function","name":"b"},
	 {"type":"custom","name":"apply_patch","format":{"type":"grammar"}},
	 {"type":"web_search_preview"},{"type":"file_search","vector_store_ids":["v"]},{"type":"computer_use_preview"},
	 {"type":"code_interpreter","container":{"type":"auto"}},{"type":"mcp","server_label":"x"},{"type":"local_shell"},
	 {"type":"image_generation"},{"type":"shell"},{"type":"apply_patch"},{"type":"more"},{"type":""}]`, `"tool_choice":"required"`))
	want := []ir.Tool{
		// The schema's bytes are the caller's: spaces and a number no float holds included.
		{Name: "a", Description: "A", Schema: json.RawMessage(`{"type":"object", "properties":{"n":{"type":"integer","minimum":10000000000000000000001}}}`)},
		{Name: "b"},
	}
	if !reflect.DeepEqual(req.Tools, want) || req.ToolChoice.Mode != ir.ChoiceRequired {
		t.Fatalf("tools %+v, choice %+v", req.Tools, req.ToolChoice)
	}
	wantDropped := []string{"tool:", "tool:apply_patch", "tool:code_interpreter", "tool:computer_use_preview", "tool:custom",
		"tool:file_search", "tool:image_generation", "tool:local_shell", "tool:mcp", "tool:more", "tool:shell", "tool:web_search_preview"}
	if d := ir.Dropped(req.Dropped); !reflect.DeepEqual(d, wantDropped) {
		t.Fatalf("dropped %v", d)
	}
	for _, mode := range []ir.ToolChoiceMode{ir.ChoiceAuto, ir.ChoiceNone, ir.ChoiceRequired} {
		if got := decode(t, withInput(userItem, `"tool_choice":"`+string(mode)+`"`)).ToolChoice; got != (ir.ToolChoice{Mode: mode}) {
			t.Errorf("tool_choice %q: %+v", mode, got)
		}
	}
}

func TestDecodeRequest_ClientErrors(t *testing.T) {
	item := func(s string) string { return withInput(userItem + "," + s) }
	part := func(s string) string { return withInput(`{"role":"user","content":[` + s + `]}`) }
	asst := func(s string) string { return withInput(userItem + `,{"role":"assistant","content":[` + s + `]}`) }
	cases := map[string]string{ // body → the field that is named
		`null`:                                   "body",
		`[]`:                                     "body",
		`"x"`:                                    "body",
		`{"model":"m"}`:                          "input",
		`{"model":"m","input":null}`:             "input",
		`{"model":"m","input":7}`:                "input",
		`{"model":"m","input":{}}`:               "input",
		`{"model":"m","input":[]}`:               "input",
		`{"model":7,"input":"x"}`:                "model",
		`{"input":"x","instructions":["a"]}`:     "instructions",
		item(`7`):                                "input[1]",
		item(`{}`):                               "input[1]",
		item(`{"content":"x"}`):                  "input[1]",
		item(`{"type":7,"role":"user"}`):         "input[1].type",
		item(`{"type":"message","content":"x"}`): "input[1].role",
		item(`{"role":"tool","content":"x"}`):    "input[1].role",
		item(`{"role":"user"}`):                  "input[1].content",
		item(`{"role":"user","content":7}`):      "input[1].content",
		item(`{"role":"user","content":[7]}`):    "input[1].content[0]",
		part(`{"type":"input_text"}`):            "input[0].content[0].text",
		part(`{"type":"input_text","text":7}`):   "input[0].content[0].text",
		part(`{"type":"refusal"}`):               "input[0].content[0].refusal",
		// Content that cannot be carried is never left out.
		part(`{"type":"input_file","file_id":"f"}`):                                                                        "input[0].content[0]",
		part(`{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}`):                                           "input[0].content[0]",
		part(`{"type":"input_image","file_id":"file_1"}`):                                                                  "input[0].content[0].file_id",
		part(`{"type":"input_image"}`):                                                                                     "input[0].content[0].image_url",
		part(`{"type":"input_image","image_url":"data:image/png,abc"}`):                                                    "input[0].content[0].image_url",
		part(`{"type":"input_image","image_url":"data:;base64,abc"}`):                                                      "input[0].content[0].image_url",
		part(`{"type":"hologram"}`):                                                                                        "input[0].content[0].type",
		part(`{"text":"x"}`):                                                                                               "input[0].content[0].type",
		asst(`{"type":"input_image","image_url":"https://x/y.png"}`):                                                       "input[1].content[0]",
		item(`{"type":"function_call","name":"f","arguments":"{}"}`):                                                       "input[1].call_id",
		item(`{"type":"function_call","call_id":"c","arguments":"{}"}`):                                                    "input[1].name",
		item(`{"type":"function_call_output","output":"x"}`):                                                               "input[1].call_id",
		item(`{"type":"function_call","call_id":"c","name":"f"},{"type":"function_call_output","call_id":"c","output":7}`): "input[2].output",
		item(`{"type":"function_call","call_id":"c","name":"f"},{"type":"function_call_output","call_id":"c","output":[{"type":"input_file","file_id":"f"}]}`): "input[2].output[0]",
		withInput(userItem, `"tools":{}`):                                               "tools",
		withInput(userItem, `"tools":[7]`):                                              "tools[0]",
		withInput(userItem, `"tools":[{"name":"f"}]`):                                   "tools[0].type",
		withInput(userItem, `"tools":[{"type":7}]`):                                     "tools[0].type",
		withInput(userItem, `"tools":[{"type":"function"}]`):                            "tools[0].name",
		withInput(userItem, `"tools":[{"type":"function","name":"f","parameters":[]}]`): "tools[0].parameters",
		withInput(userItem, `"tools":[{"type":"function","name":"f","strict":"yes"}]`):  "tools[0].strict",
		withInput(userItem, `"tools":[{"type":"function","name":"f","description":7}]`): "tools[0].description",
		withInput(userItem, `"tool_choice":"sometimes"`):                                "tool_choice",
		withInput(userItem, `"tool_choice":7`):                                          "tool_choice",
		withInput(userItem, `"tool_choice":{"type":"function"}`):                        "tool_choice.name",
		withInput(userItem, `"max_output_tokens":0`):                                    "max_output_tokens",
		withInput(userItem, `"max_output_tokens":1.5`):                                  "max_output_tokens",
		withInput(userItem, `"max_output_tokens":"9"`):                                  "max_output_tokens",
		withInput(userItem, `"temperature":"hot"`):                                      "temperature",
		withInput(userItem, `"top_p":[]`):                                               "top_p",
		withInput(userItem, `"stream":"yes"`):                                           "stream",
		withInput(userItem, `"parallel_tool_calls":1`):                                  "parallel_tool_calls",
		withInput(userItem, `"text":"plain"`):                                           "text",
	}
	for body, field := range cases {
		refused(t, body, field)
	}
}

func TestDecodeRequest_ErrorsHoldNothingOfTheRequest(t *testing.T) {
	secret := "s3cr3t-content"
	for _, body := range []string{
		withInput(`{"role":"` + secret + `","content":"x"}`),
		withInput(`{"role":"user","content":[{"type":"` + secret + `"}]}`),
		withInput(userItem + `,{"type":"function_call","call_id":"` + secret + `","arguments":"` + secret + `"}`),
		withInput(userItem + `,{"type":"function_call","call_id":"c","name":"f"},{"type":"function_call_output","call_id":"c","output":[{"type":"` + secret + `"}]}`),
		withInput(userItem, `"tool_choice":"`+secret+`"`),
	} {
		_, err := DecodeRequest([]byte(body))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestDecodeRequest_Limits(t *testing.T) {
	limit := func(body, field string) {
		t.Helper()
		if bad := refused(t, body, field); !bad.Limit || !errors.Is(bad, ir.ErrLimit) {
			t.Fatalf("%s: not a limit error: %+v", field, bad)
		}
	}
	rep := func(s string, n int) string { return strings.TrimSuffix(strings.Repeat(s+",", n), ",") }
	limit(withInput(rep(userItem, ir.MaxMessages+1)), "input")
	limit(withInput(`{"role":"user","content":[`+rep(`{"type":"input_text","text":"x"}`, ir.MaxParts+1)+`]}`), "input[0].content")
	limit(withInput(userItem, `"tools":[`+rep(`{"type":"web_search"}`, ir.MaxTools+1)+`]`), "tools")
	limit(withInput(userItem+","+rep(`{"role":"system","content":"s"}`, ir.MaxParts+1)), "input")
	limit(withInput(userItem+`,{"type":"function_call","call_id":"c","name":"f","arguments":"{\"a\":\"`+strings.Repeat("x", ir.MaxToolArgsBytes)+`\"}"}`), "input[1].arguments")
	limit(`{"input":`+strings.Repeat("[", ir.MaxDepth+1)+strings.Repeat("]", ir.MaxDepth+1)+`}`, "body")
	var calls []string
	for i := 0; i <= ir.MaxToolCalls; i++ {
		calls = append(calls, fmt.Sprintf(`{"type":"function_call","call_id":"c%d","name":"f"}`, i))
	}
	limit(withInput(userItem+","+strings.Join(calls, ",")), fmt.Sprintf("input[%d]", ir.MaxToolCalls+1))
	// At the limits it goes through.
	decode(t, withInput(rep(userItem, ir.MaxMessages)))
}

func TestDecodeRequest_NeverPanics(t *testing.T) {
	values := []string{`null`, `[]`, `{}`, `7`, `"x"`, `true`, `[null]`, `[[]]`, `[{}]`, `{"type":null}`, `[{"type":"function"}]`, `{"format":7}`}
	keys := []string{"model", "input", "instructions", "tools", "tool_choice", "max_output_tokens", "temperature", "stream",
		"text", "reasoning", "store", "parallel_tool_calls", "truncation", "include"}
	items := func(v string) string {
		return `[{"role":"user","content":` + v + `},{"type":"function_call","call_id":` + v + `,"name":` + v + `,"arguments":` + v +
			`},{"type":"function_call_output","call_id":"c","output":` + v + `},{"type":` + v + `},{"role":` + v + `},` + v + `]`
	}
	for _, v := range values {
		for _, k := range keys {
			_, _ = DecodeRequest([]byte(`{"input":"x","` + k + `":` + v + `}`))
			_, _ = DecodeRequest([]byte(`{"input":` + items(v) + `,"` + k + `":` + v + `}`))
		}
	}
	// A megabyte wherever a string can stand.
	big := `"` + strings.Repeat("x", 1<<20) + `"`
	_, _ = DecodeRequest([]byte(`{"input":` + items(big) + `,"model":` + big + `,"instructions":` + big + `,"tool_choice":` + big + `,"truncation":` + big + `}`))
	if req, err := DecodeRequest([]byte(`{"input":` + big + `,"instructions":` + big + `}`)); err != nil || len(req.Messages[0].Parts[0].Text) != 1<<20 {
		t.Fatalf("a large input: %v", err)
	}
	for _, body := range []string{``, `{`, `{"input":`, `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:"}]}]}`} {
		_, _ = DecodeRequest([]byte(body))
	}
}

// ---------------------------------------------------------------- answer

var created = time.Unix(1_700_000_000, 0)

func TestEncodeResponse(t *testing.T) {
	// Text only.
	body, err := EncodeResponse(ir.Response{ID: "chatcmpl-1", Model: "m-up", Parts: []ir.Part{text("Hello <b>&\n")},
		Stop: ir.StopEnd, Usage: ir.Usage{InputTokens: 12, OutputTokens: 2}}, "asked-for", created)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, `{"id":"resp_chatcmpl-1","object":"response","created_at":1700000000,"status":"completed","error":null,
	 "incomplete_details":null,"model":"m-up","output":[{"id":"msg_0","type":"message","role":"assistant","status":"completed",
	 "content":[{"type":"output_text","text":"Hello <b>&\n","annotations":[]}]}],
	 "parallel_tool_calls":true,"tool_choice":"auto","tools":[],
	 "usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":14}}`)
	// "output_text" is the SDKs' own convenience, not a field of the wire object.
	if bytes.Contains(body, []byte(`"output_text":`)) {
		t.Fatalf("output_text is on the wire: %s", body)
	}
	if !bytes.Contains(body, []byte(`Hello <b>&\n`)) {
		t.Fatalf("text was escaped beyond what JSON needs: %s", body)
	}

	// Text, two calls, thinking; consecutive texts are one message. "arguments" is a JSON string
	// that holds the upstream's bytes.
	args := `{ "path": "a.txt", "n": 10000000000000000000001 }`
	body, err = EncodeResponse(ir.Response{Parts: []ir.Part{
		{Kind: ir.Thinking, Text: "hm"}, text("One."), text("Two."), use("call_a", "read_file", args), use("call_b", "list_dir", ``), text("After.")},
		Stop: ir.StopToolUse, Usage: ir.Usage{InputTokens: 40, OutputTokens: 18}}, "asked-for", created)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, `{"id":"resp_burrow","object":"response","created_at":1700000000,"status":"completed","error":null,
	 "incomplete_details":null,"model":"asked-for","output":[
	  {"id":"rs_0","type":"reasoning","summary":[{"type":"summary_text","text":"hm"}]},
	  {"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[
	    {"type":"output_text","text":"One.","annotations":[]},{"type":"output_text","text":"Two.","annotations":[]}]},
	  {"id":"fc_2","type":"function_call","call_id":"call_a","name":"read_file","arguments":"{ \"path\": \"a.txt\", \"n\": 10000000000000000000001 }","status":"completed"},
	  {"id":"fc_3","type":"function_call","call_id":"call_b","name":"list_dir","arguments":"{}","status":"completed"},
	  {"id":"msg_4","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"After.","annotations":[]}]}],
	 "parallel_tool_calls":true,"tool_choice":"auto","tools":[],
	 "usage":{"input_tokens":40,"input_tokens_details":{"cached_tokens":0},"output_tokens":18,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":58}}`)
	var got struct {
		Output []struct{ Arguments string }
	}
	if err := json.Unmarshal(body, &got); err != nil || got.Output[2].Arguments != args {
		t.Fatalf("arguments %q, want the upstream's bytes %q", got.Output[2].Arguments, args)
	}

	// Stop reasons; an id that is a response id already; an answer without parts.
	for stop, want := range map[ir.StopReason]string{
		ir.StopEnd: `"status":"completed","error":null,"incomplete_details":null`, ir.StopToolUse: `"status":"completed"`,
		ir.StopSequence: `"status":"completed"`, ir.StopUnknown: `"status":"completed"`,
		ir.StopMaxTokens: `"status":"incomplete","error":null,"incomplete_details":{"reason":"max_output_tokens"}`,
		ir.StopRefusal:   `"status":"incomplete","error":null,"incomplete_details":{"reason":"content_filter"}`,
	} {
		body, err := EncodeResponse(ir.Response{ID: "resp_7", Stop: stop}, "m", created)
		if err != nil || !bytes.Contains(body, []byte(want)) || !bytes.Contains(body, []byte(`"id":"resp_7"`)) || !bytes.Contains(body, []byte(`"output":[]`)) {
			t.Errorf("stop %q: %v\n%s", stop, err, body)
		}
	}

	// What cannot be written is the answer's fault, and the error holds nothing of it.
	for name, parts := range map[string][]ir.Part{
		"no id":         {use("", "f", `{}`)},
		"no name":       {use("c", "", `{}`)},
		"bad arguments": {use("c", "f", `{"secret"`)},
		"an image":      {{Kind: ir.Image, Data: "secret"}},
		"too many":      make([]ir.Part, ir.MaxParts+1),
	} {
		_, err := EncodeResponse(ir.Response{Parts: parts}, "m", created)
		if !errors.Is(err, ErrUnencodable) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestEncodeError(t *testing.T) {
	// The shape of the gateway's own OpenAI errors (internal/aigateway/errors.go).
	body := EncodeError(429, "upstream_error", "slow \"down\"\n")
	assertJSONEqual(t, body, `{"error":{"message":"slow \"down\"\n","type":"burrow_error","code":"upstream_error"}}`)
}

// ---------------------------------------------------------------- stream

func ev(kind ir.EventKind, index int) ir.Event { return ir.Event{Kind: kind, Index: index} }
func startEv() ir.Event                        { return ir.Event{Kind: ir.Start, ID: "chatcmpl-9", Model: "m-up"} }
func textStart(i int) ir.Event {
	return ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.Text}}
}
func thinkStart(i int) ir.Event {
	return ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.Thinking}}
}
func toolStart(i int, id, name string) ir.Event {
	return ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.ToolUse, ToolID: id, ToolName: name}}
}
func textDelta(i int, s string) ir.Event { return ir.Event{Kind: ir.TextDelta, Index: i, Text: s} }
func thinkDelta(i int, s string) ir.Event {
	return ir.Event{Kind: ir.ThinkingDelta, Index: i, Text: s}
}
func argsDelta(i int, s string) ir.Event {
	return ir.Event{Kind: ir.ToolArgsDelta, Index: i, ArgsJSON: s}
}
func finishEv(stop ir.StopReason, in, out int) ir.Event {
	return ir.Event{Kind: ir.Finish, Stop: stop, Usage: ir.Usage{InputTokens: in, OutputTokens: out}}
}

// frozen is a clock that stands still.
func frozen() time.Time { return created }

// encodeEvents writes events through an encoder and returns the bytes and the first error.
func encodeEvents(events []ir.Event, closeIt bool) ([]byte, error) {
	var buf bytes.Buffer
	e := NewStreamEncoder(&buf, "asked-for", created)
	e.now = frozen // no keep-alives: the tests that want them move the clock themselves
	var first error
	for _, ev := range events {
		if err := e.Write(ev); err != nil && first == nil {
			first = err
		}
	}
	if closeIt {
		if err := e.Close(); err != nil && first == nil {
			first = err
		}
	}
	return buf.Bytes(), first
}

// mustStream encodes events that must be taken and checks the bytes the way a client reads them.
func mustStream(t *testing.T, events []ir.Event, closeIt bool) (Stream, []byte) {
	t.Helper()
	raw, err := encodeEvents(events, closeIt)
	if err != nil {
		t.Fatalf("encoder: %v\n%s", err, raw)
	}
	s, err := CheckStream(raw)
	if err != nil {
		t.Fatalf("not a well-formed Responses stream: %v\n%s", err, raw)
	}
	return s, raw
}

// frames reads a stream without CheckStream: the event names and the data as plain JSON values.
// It is the second pair of eyes of these tests.
func frames(t testing.TB, raw []byte) (names []string, data []map[string]any) {
	t.Helper()
	for _, block := range strings.Split(strings.TrimSuffix(string(raw), "\n\n"), "\n\n") {
		if block == "" || strings.HasPrefix(block, ":") {
			continue
		}
		eventLine, dataLine, _ := strings.Cut(block, "\n")
		var d map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &d); err != nil {
			t.Fatalf("frame %q: %v", block, err)
		}
		names = append(names, strings.TrimPrefix(eventLine, "event: "))
		data = append(data, d)
	}
	return names, data
}

func eventNames(t testing.TB, raw []byte) string {
	names, _ := frames(t, raw)
	return strings.ReplaceAll(strings.Join(names, " "), "response.", "")
}

// argumentViews returns, per function call in order of appearance and read without CheckStream:
// the joined argument deltas, the arguments of the done event, of the done item, and of the item in
// the final response.
func argumentViews(t testing.TB, raw []byte) (joined, done, item, final []string) {
	t.Helper()
	names, data := frames(t, raw)
	byItem := map[string]int{}
	for i, d := range data {
		switch names[i] {
		case "response.output_item.added":
			it := d["item"].(map[string]any)
			if it["type"] == "function_call" {
				byItem[it["id"].(string)] = len(joined)
				joined, done, item = append(joined, ""), append(done, "<none>"), append(item, "<none>")
			}
		case "response.function_call_arguments.delta":
			joined[byItem[d["item_id"].(string)]] += d["delta"].(string)
		case "response.function_call_arguments.done":
			done[byItem[d["item_id"].(string)]] = d["arguments"].(string)
		case "response.output_item.done":
			it := d["item"].(map[string]any)
			if it["type"] == "function_call" {
				item[byItem[it["id"].(string)]] = it["arguments"].(string)
			}
		case "response.completed", "response.incomplete", "response.failed":
			for _, o := range d["response"].(map[string]any)["output"].([]any) {
				if it := o.(map[string]any); it["type"] == "function_call" {
					final = append(final, it["arguments"].(string))
				}
			}
		}
	}
	return
}

func TestStreamEncoder_Text(t *testing.T) {
	s, raw := mustStream(t, []ir.Event{startEv(), textStart(0), textDelta(0, "Hel"), textDelta(0, "lo."), ev(ir.PartStop, 0), finishEv(ir.StopEnd, 12, 2)}, true)
	want := `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_chatcmpl-9","object":"response","created_at":1700000000,"status":"in_progress","error":null,"incomplete_details":null,"model":"m-up","output":[],"parallel_tool_calls":true,"tool_choice":"auto","tools":[],"usage":null}}

event: response.in_progress
data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_chatcmpl-9","object":"response","created_at":1700000000,"status":"in_progress","error":null,"incomplete_details":null,"model":"m-up","output":[],"parallel_tool_calls":true,"tool_choice":"auto","tools":[],"usage":null}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"msg_0","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":3,"item_id":"msg_0","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_0","output_index":0,"content_index":0,"delta":"Hel","logprobs":[]}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":5,"item_id":"msg_0","output_index":0,"content_index":0,"delta":"lo.","logprobs":[]}

event: response.output_text.done
data: {"type":"response.output_text.done","sequence_number":6,"item_id":"msg_0","output_index":0,"content_index":0,"text":"Hello.","logprobs":[]}

event: response.content_part.done
data: {"type":"response.content_part.done","sequence_number":7,"item_id":"msg_0","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hello.","annotations":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":8,"output_index":0,"item":{"id":"msg_0","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello.","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_chatcmpl-9","object":"response","created_at":1700000000,"status":"completed","error":null,"incomplete_details":null,"model":"m-up","output":[{"id":"msg_0","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello.","annotations":[]}]}],"parallel_tool_calls":true,"tool_choice":"auto","tools":[],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":14}}}

`
	if string(raw) != want {
		t.Fatalf("stream\n got:\n%s\nwant:\n%s", raw, want)
	}
	if !s.Completed || s.Status != "completed" || s.InputTokens != 12 || s.OutputTokens != 2 || s.TotalTokens != 14 {
		t.Fatalf("%+v", s)
	}
	// Sequence numbers 0..n without a gap, read without the checker.
	_, data := frames(t, raw)
	for i, d := range data {
		if d["sequence_number"] != float64(i) {
			t.Fatalf("frame %d has sequence_number %v", i, d["sequence_number"])
		}
	}
}

// chatEvents runs raw bytes through the SSE parser and the Chat stream decoder, as the response
// writer does, and returns the events.
func chatEvents(raw []byte, piece int) []ir.Event {
	if piece < 1 {
		piece = 1
	}
	p := sse.NewParser(chat.MaxFrameBytes)
	d := chat.NewStreamDecoder()
	var events []ir.Event
	feed := func(frames []sse.Frame) {
		for _, fr := range frames {
			evs, _ := d.Feed(fr.Data)
			events = append(events, evs...)
		}
	}
	for i := 0; i < len(raw); i += piece {
		frames, err := p.Feed(raw[i:min(i+piece, len(raw))])
		feed(frames)
		if err != nil {
			break
		}
	}
	feed(p.Flush())
	return append(events, d.Close()...)
}

func TestStreamEncoder_ChatFixtures(t *testing.T) {
	// The text stream of the Chat codec.
	s, raw := mustStream(t, chatEvents(fixture(t, "../chat/testdata/stream_text.sse"), 7), true)
	if got := eventNames(t, raw); got != "created in_progress output_item.added content_part.added output_text.delta output_text.delta output_text.done content_part.done output_item.done completed" {
		t.Fatalf("events: %s", got)
	}
	if len(s.Items) != 1 || s.Items[0].Text != "Hello." || s.InputTokens != 12 || s.OutputTokens != 2 || s.TotalTokens != 14 {
		t.Fatalf("%+v", s)
	}
	// The tool stream: two interleaved calls become the transcript, byte for byte.
	for _, piece := range []int{1, 5, 1 << 20} {
		_, raw = mustStream(t, chatEvents(fixture(t, "../chat/testdata/stream_tools.sse"), piece), true)
		if want := fixture(t, "../testdata/responses_chat_stream_tools.sse"); !bytes.Equal(raw, want) {
			t.Fatalf("piece %d: stream\n got:\n%s\nwant:\n%s", piece, raw, want)
		}
	}
}

// interleaved is an answer whose two tool calls are open side by side, with a text that opens
// while they are.
func interleaved() []ir.Event {
	return []ir.Event{startEv(),
		toolStart(0, "call_a", "fa"), argsDelta(0, `{"a":`),
		toolStart(1, "call_b", "fb"), argsDelta(1, `{"b"`), argsDelta(0, `"é\n"`), argsDelta(1, `:[1,2]`),
		textStart(2), textDelta(2, "mean"), argsDelta(0, `}`), textDelta(2, "while"), ev(ir.PartStop, 2),
		argsDelta(1, `}`),
		ev(ir.PartStop, 0), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 7, 9)}
}

func TestStreamEncoder_InterleavedToolCallsAreWrittenOneAfterTheOther(t *testing.T) {
	s, raw := mustStream(t, interleaved(), true)
	if got := eventNames(t, raw); got != "created in_progress "+
		"output_item.added function_call_arguments.delta function_call_arguments.delta function_call_arguments.delta function_call_arguments.done output_item.done "+
		"output_item.added function_call_arguments.delta function_call_arguments.delta function_call_arguments.delta function_call_arguments.done output_item.done "+
		"output_item.added content_part.added output_text.delta output_text.delta output_text.done content_part.done output_item.done completed" {
		t.Fatalf("events: %s", got)
	}
	a, b, c := s.Items[0], s.Items[1], s.Items[2]
	if a.ID != "fc_0" || a.CallID != "call_a" || a.Name != "fa" || a.Arguments != `{"a":"é\n"}` || !reflect.DeepEqual(a.Deltas, []string{`{"a":`, `"é\n"`, `}`}) ||
		b.ID != "fc_1" || b.CallID != "call_b" || b.Name != "fb" || b.Arguments != `{"b":[1,2]}` || !reflect.DeepEqual(b.Deltas, []string{`{"b"`, `:[1,2]`, `}`}) ||
		c.ID != "msg_2" || c.Text != "meanwhile" {
		t.Fatalf("%+v", s.Items)
	}
	// Review Focus 1, read without the checker: the deltas of each call add up to the upstream's
	// bytes, and the done event, the done item and the final response say the same.
	joined, done, item, final := argumentViews(t, raw)
	want := []string{`{"a":"é\n"}`, `{"b":[1,2]}`}
	for name, got := range map[string][]string{"joined deltas": joined, "done event": done, "done item": item, "final response": final} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	// ids are consistent: every item_id is the id of the item that is open.
	names, data := frames(t, raw)
	open, index := "", -1.0
	for i, d := range data {
		switch {
		case names[i] == "response.output_item.added":
			open, index = d["item"].(map[string]any)["id"].(string), d["output_index"].(float64)
		case d["item_id"] != nil:
			if d["item_id"] != open || d["output_index"] != index {
				t.Fatalf("frame %d (%s) names item %v at %v while %s at %v is open", i, names[i], d["item_id"], d["output_index"], open, index)
			}
		}
	}
}

func TestStreamEncoder_LiveItemIsNotHeldBack(t *testing.T) {
	var buf bytes.Buffer
	e := NewStreamEncoder(&buf, "m", created)
	e.now = frozen
	step := func(ev ir.Event) string {
		t.Helper()
		before := buf.Len()
		if err := e.Write(ev); err != nil {
			t.Fatal(err)
		}
		return eventNames(t, buf.Bytes()[before:])
	}
	for _, c := range []struct {
		ev   ir.Event
		want string
	}{
		{startEv(), "created in_progress"},
		{toolStart(0, "a", "f"), "output_item.added"},
		{argsDelta(0, `{"x":`), "function_call_arguments.delta"}, // live: at once
		{toolStart(1, "b", "g"), ""},                             // held
		{argsDelta(1, `{}`), ""},
		{argsDelta(0, `1}`), "function_call_arguments.delta"},
		{ev(ir.PartStop, 1), ""}, // held and stopped: still waits for the first
		{ev(ir.PartStop, 0), "function_call_arguments.done output_item.done output_item.added function_call_arguments.delta function_call_arguments.done output_item.done"},
		{textStart(2), "output_item.added content_part.added"},
		{textDelta(2, "x"), "output_text.delta"},
		{ev(ir.PartStop, 2), "output_text.done content_part.done output_item.done"},
		{finishEv(ir.StopToolUse, 1, 1), "completed"},
	} {
		if got := step(c.ev); got != c.want {
			t.Fatalf("%+v wrote %q, want %q", c.ev, got, c.want)
		}
	}
}

func TestStreamEncoder_ToolCallWithoutArguments(t *testing.T) {
	// Codex parses "arguments" as JSON, and the SDKs add the deltas to the "" of the added item: a
	// call without arguments is "{}" in all of them, as OpenAI itself sends it.
	for name, pieces := range map[string][]string{"none": nil, "white space": {" ", "\n"}} {
		events := []ir.Event{startEv(), toolStart(0, "call_a", "f")}
		upstream := ""
		for _, p := range pieces {
			events = append(events, argsDelta(0, p))
			upstream += p
		}
		events = append(events, ev(ir.PartStop, 0), finishEv(ir.StopToolUse, 1, 1))
		s, raw := mustStream(t, events, true)
		want := upstream + "{}"
		joined, done, item, final := argumentViews(t, raw)
		if joined[0] != want || done[0] != want || item[0] != want || final[0] != want || s.Items[0].Arguments != want {
			t.Fatalf("%s: joined %q, done %q, item %q, final %q; want %q", name, joined, done, item, final, want)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(final[0]), &obj); err != nil || len(obj) != 0 {
			t.Fatalf("%s: arguments %q are not the empty object", name, final[0])
		}
	}
}

func TestStreamEncoder_ACallWithoutArgumentsWaitsForWhatFollows(t *testing.T) {
	// A decoder stops every open part before it reports a failure, so a call that stopped without
	// a byte of arguments may be a call that was cut before its first byte. Its "{}" and its done
	// events are held until an event shows the stream goes on; the stop of another part does not.
	var buf bytes.Buffer
	e := NewStreamEncoder(&buf, "m", created)
	e.now = frozen
	step := func(ev ir.Event) string {
		t.Helper()
		before := buf.Len()
		if err := e.Write(ev); err != nil {
			t.Fatal(err)
		}
		return eventNames(t, buf.Bytes()[before:])
	}
	const done = "function_call_arguments.delta function_call_arguments.done output_item.done"
	for _, c := range []struct {
		ev   ir.Event
		want string
	}{
		{startEv(), "created in_progress"},
		{toolStart(0, "a", "f"), "output_item.added"},
		{toolStart(1, "b", "g"), ""},
		{toolStart(2, "c", "h"), ""},
		{ev(ir.PartStop, 0), ""}, // no arguments: wait
		{ev(ir.PartStop, 1), ""}, // another stop says nothing
		{argsDelta(2, `{"k":1}`), done + " output_item.added " + done + " output_item.added function_call_arguments.delta"},
		{ev(ir.PartStop, 2), "function_call_arguments.done output_item.done"},
		{toolStart(3, "d", "i"), "output_item.added"},
		{ev(ir.PartStop, 3), ""},
		{finishEv(ir.StopToolUse, 1, 1), done + " completed"},
	} {
		if got := step(c.ev); got != c.want {
			t.Fatalf("%+v wrote %q, want %q", c.ev, got, c.want)
		}
	}
	s, err := CheckStream(buf.Bytes())
	if err != nil || !s.Completed || len(s.Items) != 4 || s.Items[0].Arguments != "{}" || s.Items[1].Arguments != "{}" || s.Items[2].Arguments != `{"k":1}` || s.Items[3].Arguments != "{}" {
		t.Fatalf("%v, %+v", err, s)
	}
}

func TestStreamEncoder_Thinking(t *testing.T) {
	// Nothing while it streams; the whole reasoning item when it stops.
	var buf bytes.Buffer
	e := NewStreamEncoder(&buf, "m", created)
	e.now = frozen
	for _, ev := range []ir.Event{startEv(), thinkStart(0), thinkDelta(0, "Let me "), thinkDelta(0, "think.")} {
		if err := e.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	if got := eventNames(t, buf.Bytes()); got != "created in_progress" {
		t.Fatalf("while thinking: %s", got)
	}
	s, raw := mustStream(t, []ir.Event{startEv(), thinkStart(0), thinkDelta(0, "Let me "), thinkDelta(0, "think."), ev(ir.PartStop, 0),
		textStart(1), textDelta(1, "Done."), ev(ir.PartStop, 1), thinkStart(2), ev(ir.PartStop, 2), finishEv(ir.StopEnd, 3, 4)}, true)
	if got := eventNames(t, raw); got != "created in_progress output_item.added output_item.done output_item.added content_part.added output_text.delta output_text.done content_part.done output_item.done output_item.added output_item.done completed" {
		t.Fatalf("events: %s", got)
	}
	if s.Items[0].Type != "reasoning" || s.Items[0].ID != "rs_0" || s.Items[0].Text != "Let me think." || s.Items[1].Text != "Done." || s.Items[2].Text != "" {
		t.Fatalf("%+v", s.Items)
	}
	_, data := frames(t, raw)
	item, _ := json.Marshal(data[2]["item"])
	assertJSONEqual(t, item, `{"id":"rs_0","type":"reasoning","summary":[{"type":"summary_text","text":"Let me think."}]}`)
	if !reflect.DeepEqual(data[2]["item"], data[3]["item"]) {
		t.Fatalf("added and done differ: %v / %v", data[2]["item"], data[3]["item"])
	}
}

func TestStreamEncoder_StopReasonsAndUsage(t *testing.T) {
	for stop, want := range map[ir.StopReason][2]string{
		ir.StopEnd: {"completed", ""}, ir.StopToolUse: {"completed", ""}, ir.StopSequence: {"completed", ""}, ir.StopUnknown: {"completed", ""},
		ir.StopMaxTokens: {"incomplete", "max_output_tokens"}, ir.StopRefusal: {"incomplete", "content_filter"},
	} {
		events := []ir.Event{startEv(), textStart(0), textDelta(0, "x"), ev(ir.PartStop, 0), finishEv(stop, 5, 6)}
		if stop == ir.StopToolUse {
			events = []ir.Event{startEv(), toolStart(0, "c", "f"), ev(ir.PartStop, 0), finishEv(stop, 5, 6)}
		}
		s, raw := mustStream(t, events, true)
		names, data := frames(t, raw)
		last := data[len(data)-1]["response"].(map[string]any)
		if s.Status != want[0] || s.IncompleteReason != want[1] || names[len(names)-1] != "response."+want[0] || last["status"] != want[0] ||
			s.Completed != (want[0] == "completed") || s.InputTokens != 5 || s.OutputTokens != 6 || s.TotalTokens != 11 {
			t.Errorf("stop %q: %+v, last event %s", stop, s, names[len(names)-1])
		}
	}
	// The start's input tokens stand when the finish has none; ids and model fall back.
	s, _ := mustStream(t, []ir.Event{{Kind: ir.Start, Usage: ir.Usage{InputTokens: 33}}, finishEv(ir.StopEnd, 0, 4)}, true)
	if s.ID != "resp_burrow" || s.Model != "asked-for" || s.InputTokens != 33 || s.OutputTokens != 4 || s.TotalTokens != 37 || len(s.Items) != 0 {
		t.Fatalf("%+v", s)
	}
	s, _ = mustStream(t, []ir.Event{{Kind: ir.Start, ID: "resp_abc", Model: "m"}, finishEv(ir.StopEnd, -5, -1)}, true)
	if s.ID != "resp_abc" || s.InputTokens != 0 || s.OutputTokens != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestStreamEncoder_BadEndings(t *testing.T) {
	// Review Focus 2: whatever goes wrong, what was opened is closed, the stream ends with
	// response.failed, and response.completed is never written.
	cases := map[string]struct {
		events  []ir.Event
		close   bool
		names   string
		message string
		status  []string // of the items
	}{
		"the provider's error after text": {
			events:  []ir.Event{startEv(), textStart(0), textDelta(0, "par"), ev(ir.PartStop, 0), {Kind: ir.Error, Err: "overloaded \"now\""}},
			names:   "created in_progress output_item.added content_part.added output_text.delta output_text.done content_part.done output_item.done failed",
			message: "overloaded \"now\"", status: []string{"completed"}},
		"an error without words": {
			events: []ir.Event{startEv(), {Kind: ir.Error}}, names: "created in_progress failed", message: errProvider},
		"the upstream went away inside a text": {
			events: []ir.Event{startEv(), textStart(0), textDelta(0, "par")}, close: true,
			names:   "created in_progress output_item.added content_part.added output_text.delta output_text.done content_part.done output_item.done failed",
			message: errEarlyEnd, status: []string{"incomplete"}},
		"the upstream went away inside two calls: the cut one is never done, the whole one is": {
			events: []ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, `{"x`), toolStart(1, "b", "g"), argsDelta(1, `{"y":1}`)}, close: true,
			names: "created in_progress output_item.added function_call_arguments.delta " +
				"output_item.added function_call_arguments.delta function_call_arguments.done output_item.done failed",
			message: errEarlyEnd, status: []string{"", "completed"}},
		"calls stopped by a failing decoder: cut arguments are not a done call": {
			events: []ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, `{"x":1}`), toolStart(1, "b", "g"), argsDelta(1, `{"y":`),
				ev(ir.PartStop, 0), ev(ir.PartStop, 1), {Kind: ir.Error, Err: errEarlyEnd}},
			names: "created in_progress output_item.added function_call_arguments.delta function_call_arguments.done output_item.done " +
				"output_item.added function_call_arguments.delta failed",
			message: errEarlyEnd, status: []string{"completed", ""}},
		"a held call that was cut is not written at all, and one without arguments is not done": {
			events: []ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, `{}`), toolStart(1, "b", "g"), argsDelta(1, `{"y":`), toolStart(2, "c", "h"),
				ev(ir.PartStop, 1), ev(ir.PartStop, 2), ev(ir.PartStop, 0), {Kind: ir.Error, Err: errEarlyEnd}},
			names:   "created in_progress output_item.added function_call_arguments.delta function_call_arguments.done output_item.done output_item.added failed",
			message: errEarlyEnd, status: []string{"completed", ""}},
		"a call that stopped without arguments, then the failure: it was cut before its first byte": {
			events: []ir.Event{startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), {Kind: ir.Error, Err: errEarlyEnd}},
			names:  "created in_progress output_item.added failed", message: errEarlyEnd, status: []string{""}},
		"the same, ended by Close": {
			events: []ir.Event{startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0)}, close: true,
			names: "created in_progress output_item.added failed", message: errEarlyEnd, status: []string{""}},
		"two calls without arguments, stopped in a row, then the failure: only the whole call is done": {
			events: []ir.Event{startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), toolStart(2, "c", "h"), argsDelta(2, `{"k":1}`),
				ev(ir.PartStop, 0), ev(ir.PartStop, 1), ev(ir.PartStop, 2), {Kind: ir.Error, Err: errEarlyEnd}},
			names:   "created in_progress output_item.added output_item.added function_call_arguments.delta function_call_arguments.done output_item.done failed",
			message: errEarlyEnd, status: []string{"", "completed"}},
		"a call without arguments that never stopped is not a call": {
			events: []ir.Event{startEv(), toolStart(0, "a", "f")}, close: true,
			names: "created in_progress output_item.added failed", message: errEarlyEnd, status: []string{""}},
		"no finish at all": {
			events: []ir.Event{startEv()}, close: true, names: "created in_progress failed", message: errEarlyEnd},
		"nothing at all": {close: true, names: "created failed", message: errEarlyEnd},
		"an error before the start": {
			events: []ir.Event{{Kind: ir.Error, Err: "first thing"}}, names: "created failed", message: "first thing"},
		"thinking that never stopped": {
			events: []ir.Event{startEv(), thinkStart(0), thinkDelta(0, "hm")}, close: true,
			names: "created in_progress output_item.added output_item.done failed", message: errEarlyEnd, status: []string{""}},
	}
	for name, c := range cases {
		raw, err := encodeEvents(c.events, c.close)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		s, err := CheckStream(raw)
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, raw)
			continue
		}
		var status []string
		for _, it := range s.Items {
			status = append(status, it.Status)
		}
		if got := eventNames(t, raw); got != c.names || s.Completed || s.Status != "failed" || s.ErrCode != "server_error" || s.ErrMessage != c.message || !reflect.DeepEqual(status, c.status) {
			t.Errorf("%s: events %q, stream %+v, item status %v", name, got, s, status)
		}
		if bytes.Contains(raw, []byte("response.completed")) || bytes.Contains(raw, []byte(`"status":"completed","error"`)) {
			t.Errorf("%s: a completed response in a failed stream\n%s", name, raw)
		}
		// Read without the checker: every done function_call, and every one in the final output, has
		// arguments that are one JSON object. Codex records and runs each of them.
		_, done, item, final := argumentViews(t, raw)
		for _, args := range append(append([]string{}, item...), final...) {
			if args != "<none>" && ir.CheckObject([]byte(args)) != nil {
				t.Errorf("%s: a call with arguments %q is handed over (done events %q)", name, args, done)
			}
		}
	}
}

func TestStreamEncoder_ArgumentsThatAreNoObjectAreNeverDone(t *testing.T) {
	// The decoder judges arguments before its Finish. The encoder does not rely on it: a call whose
	// pieces are not one JSON object gets no done event, is not in the final output, and a Finish
	// after it fails the answer.
	for _, args := range []string{`{"a":`, `[1]`, `{"a":1}{`, `nul`, "{\"a\":\"\xff\"}"} {
		raw, err := encodeEvents([]ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, args), ev(ir.PartStop, 0),
			toolStart(1, "b", "g"), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1)}, true)
		if !errors.Is(err, ir.ErrSequence) {
			t.Fatalf("%q: err = %v", args, err)
		}
		s, err := CheckStream(raw)
		if err != nil || s.Completed || s.Status != "failed" || s.Items[0].Done || !s.Items[1].Done || s.Items[1].ID != "fc_1" || s.ErrMessage != errUnreadable {
			t.Fatalf("%q: %v, %+v\n%s", args, err, s, raw)
		}
		names, data := frames(t, raw)
		if got := eventNames(t, raw); got != "created in_progress output_item.added function_call_arguments.delta output_item.added function_call_arguments.delta function_call_arguments.done output_item.done failed" {
			t.Fatalf("%q: events %s", args, got)
		}
		if final := data[len(data)-1]["response"].(map[string]any)["output"].([]any); len(final) != 1 || final[0].(map[string]any)["call_id"] != "b" || names[len(names)-1] != "response.failed" {
			t.Fatalf("%q: final output %v", args, final)
		}
	}
}

func TestStreamEncoder_AfterTheEndNothingIsWritten(t *testing.T) {
	for _, end := range []ir.Event{finishEv(ir.StopEnd, 1, 1), {Kind: ir.Error, Err: "x"}} {
		var buf bytes.Buffer
		e := NewStreamEncoder(&buf, "m", created)
		for _, ev := range []ir.Event{startEv(), end} {
			if err := e.Write(ev); err != nil {
				t.Fatal(err)
			}
		}
		before := buf.Len()
		for _, ev := range []ir.Event{startEv(), textStart(0), textDelta(0, "x"), finishEv(ir.StopEnd, 1, 1), {Kind: ir.Error, Err: "y"}} {
			if err := e.Write(ev); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.Close(); err != nil || buf.Len() != before {
			t.Fatalf("after %s: err %v, %d more bytes", end.Kind, err, buf.Len()-before)
		}
	}
}

func TestStreamEncoder_MalformedSequencesEndInAFailure(t *testing.T) {
	cases := map[string][]ir.Event{
		"no start":                      {textStart(0)},
		"two starts":                    {startEv(), startEv()},
		"a part out of order":           {startEv(), textStart(1)},
		"a delta for no part":           {startEv(), textDelta(0, "x")},
		"a text delta for a tool call":  {startEv(), toolStart(0, "a", "f"), textDelta(0, "x")},
		"an args delta for a text":      {startEv(), textStart(0), argsDelta(0, "{}")},
		"a delta after the stop":        {startEv(), textStart(0), ev(ir.PartStop, 0), textDelta(0, "x")},
		"a stop for no part":            {startEv(), ev(ir.PartStop, 3)},
		"a negative index":              {startEv(), ev(ir.PartStop, -1)},
		"a finish while a part is open": {startEv(), textStart(0), finishEv(ir.StopEnd, 1, 1)},
		"a finish while a part is held": {startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "f"), ev(ir.PartStop, 1), finishEv(ir.StopEnd, 1, 1)},
		"a tool call without an id":     {startEv(), toolStart(0, "", "f")},
		"a tool call without a name":    {startEv(), toolStart(0, "a", "")},
		"a tool call id twice":          {startEv(), toolStart(0, "a", "f"), toolStart(1, "a", "f")},
		"a part of no kind":             {startEv(), {Kind: ir.PartStart, Part: ir.Part{Kind: ir.Image}}},
		"an event of no kind":           {startEv(), {Kind: "what"}},
	}
	for name, events := range cases {
		raw, err := encodeEvents(events, false)
		if !errors.Is(err, ir.ErrSequence) {
			t.Errorf("%s: err = %v", name, err)
			continue
		}
		s, err := CheckStream(raw)
		if err != nil || s.Completed || s.Status != "failed" || s.ErrMessage != errUnreadable {
			t.Errorf("%s: %v, %+v\n%s", name, err, s, raw)
		}
	}
}

func TestStreamEncoder_Limits(t *testing.T) {
	// The streams are large: only their last event is read, and that no other one ends them.
	tooLarge := func(name string, events []ir.Event) (items int) {
		t.Helper()
		raw, err := encodeEvents(events, false)
		if !errors.Is(err, ir.ErrLimit) {
			t.Fatalf("%s: err = %v", name, err)
		}
		at := bytes.LastIndex(raw, []byte("event: "))
		names, data := frames(t, raw[at:])
		resp := data[0]["response"].(map[string]any)
		if names[0] != "response.failed" || resp["status"] != "failed" || resp["error"].(map[string]any)["message"] != errTooLarge ||
			bytes.Contains(raw[:at], []byte("event: response.failed")) || bytes.Contains(raw, []byte("event: response.completed")) {
			t.Fatalf("%s: the stream ends with %s: %.300s", name, names[0], raw[at:])
		}
		return len(resp["output"].([]any))
	}
	chunk := strings.Repeat("x", 1<<16)
	// One item over MaxItemBytes.
	events := []ir.Event{startEv(), textStart(0)}
	for i := 0; i <= MaxItemBytes/len(chunk); i++ {
		events = append(events, textDelta(0, chunk))
	}
	tooLarge("one text", events)
	// Thinking is kept until it stops: it is bounded in the same way.
	events = []ir.Event{startEv(), thinkStart(0)}
	for i := 0; i <= MaxItemBytes/len(chunk); i++ {
		events = append(events, thinkDelta(0, chunk))
	}
	tooLarge("one thinking", events)
	// The answer as a whole over MaxOutputBytes: the final event repeats all of it.
	events = []ir.Event{startEv()}
	for p := 0; p*(MaxItemBytes-len(chunk)) <= MaxOutputBytes; p++ {
		events = append(events, textStart(p))
		for i := 0; i < MaxItemBytes/len(chunk)-1; i++ {
			events = append(events, textDelta(p, chunk))
		}
		events = append(events, ev(ir.PartStop, p))
	}
	tooLarge("the whole answer", events)
	// The size is counted as it is written: text that JSON has to escape counts six-fold.
	events = []ir.Event{startEv(), textStart(0)}
	for i := 0; i <= MaxItemBytes/6/len(chunk); i++ {
		events = append(events, textDelta(0, strings.Repeat("\x01", len(chunk))))
	}
	tooLarge("escaped text", events)
	// The ir limits on tool calls.
	events = []ir.Event{startEv(), toolStart(0, "a", "f")}
	for i := 0; i <= ir.MaxToolArgsBytes/len(chunk); i++ {
		events = append(events, argsDelta(0, chunk))
	}
	tooLarge("one call's arguments", events)
	events = []ir.Event{startEv()}
	for i := 0; i <= ir.MaxToolCalls; i++ {
		events = append(events, toolStart(i, fmt.Sprint("c", i), "f"))
	}
	// None of those calls stopped: none of them is handed over.
	if n := tooLarge("too many calls", events); n != 0 {
		t.Fatalf("%d items", n)
	}
	events = []ir.Event{startEv()}
	for i := 0; i <= ir.MaxParts; i++ {
		events = append(events, textStart(i), ev(ir.PartStop, i))
	}
	tooLarge("too many parts", events)
	// What is held back is bounded by ir.MaxTotalToolArgsBytes.
	events = []ir.Event{startEv(), toolStart(0, "live", "f")}
	for i := 1; i <= 6; i++ {
		events = append(events, toolStart(i, fmt.Sprint("held", i), "f"))
		for n := 0; n < ir.MaxToolArgsBytes/len(chunk); n++ {
			events = append(events, argsDelta(i, chunk))
		}
	}
	tooLarge("held back", events)
}

func TestStreamEncoder_HeldPiecesAreNotKeptOneByOne(t *testing.T) {
	// A held part fed byte by byte: its first maxHeldPieces deltas are replayed as they came, the
	// rest as one. The bytes are the same.
	args := `{"k":"` + strings.Repeat("v", 5000) + `"}`
	events := []ir.Event{startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g")}
	for i := range args {
		events = append(events, argsDelta(1, args[i:i+1]))
	}
	events = append(events, ev(ir.PartStop, 0), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1))
	s, _ := mustStream(t, events, true)
	if b := s.Items[1]; b.Arguments != args || len(b.Deltas) != maxHeldPieces+1 {
		t.Fatalf("%d deltas, arguments equal: %v", len(b.Deltas), b.Arguments == args)
	}
}

func TestStreamEncoder_WorkIsLinear(t *testing.T) {
	// Arguments and a text, each in 200 000 one-byte deltas: the done events and the final response
	// repeat them once each. What the encoder allocates is a small multiple of what it writes;
	// assembling the text anew for every delta would allocate tens of gigabytes here.
	const pieces = 200_000
	var n countWriter
	e := NewStreamEncoder(&n, "m", created)
	e.now = frozen
	write := func(ev ir.Event) {
		if err := e.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	write(startEv())
	write(toolStart(0, "a", "f"))
	write(toolStart(1, "b", "g")) // held back, byte by byte
	write(argsDelta(0, `{"k":"`))
	write(argsDelta(1, `{"k":"`))
	for i := 0; i < pieces; i++ {
		write(argsDelta(0, "v"))
		write(argsDelta(1, "w"))
	}
	write(argsDelta(0, `"}`))
	write(argsDelta(1, `"}`))
	write(ev(ir.PartStop, 0))
	write(ev(ir.PartStop, 1))
	write(textStart(2))
	for i := 0; i < pieces; i++ {
		write(textDelta(2, "t"))
	}
	write(ev(ir.PartStop, 2))
	write(finishEv(ir.StopToolUse, 1, 1))
	runtime.ReadMemStats(&after)
	if n.last < 3*pieces {
		t.Fatalf("the final event has %d bytes", n.last)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16*uint64(n.total) {
		t.Fatalf("%d bytes allocated for %d bytes written", allocated, n.total)
	}
}

type countWriter struct{ last, total int }

func (c *countWriter) Write(p []byte) (int, error) {
	c.last = len(p)
	c.total += len(p)
	return len(p), nil
}

func TestStreamEncoder_KeepAliveWhileNothingCanBeWritten(t *testing.T) {
	var buf bytes.Buffer
	e := NewStreamEncoder(&buf, "m", created)
	now := created
	e.now = func() time.Time { return now }
	write := func(ev ir.Event) string {
		t.Helper()
		before := buf.Len()
		if err := e.Write(ev); err != nil {
			t.Fatal(err)
		}
		return buf.String()[before:]
	}
	const alive = ": keep-alive\n\n"
	write(startEv())
	write(toolStart(0, "a", "f"))
	write(toolStart(1, "b", "g")) // held
	if got := write(argsDelta(1, `{"x":`)); got != "" {
		t.Fatalf("a keep-alive at once: %q", got)
	}
	now = now.Add(pingAfter)
	if got := write(argsDelta(1, `1`)); got != alive {
		t.Fatalf("after a second of holding back: %q", got)
	}
	if got := write(argsDelta(1, `}`)); got != "" { // at most one per second
		t.Fatalf("a second keep-alive: %q", got)
	}
	now = now.Add(pingAfter)
	if got := write(argsDelta(0, `{}`)); strings.Contains(got, alive) { // something was written: no need
		t.Fatalf("a keep-alive next to a frame: %q", got)
	}
	write(ev(ir.PartStop, 0))
	write(ev(ir.PartStop, 1))
	// Thinking writes nothing until it stops: the same sign of life.
	write(thinkStart(2))
	now = now.Add(pingAfter)
	if got := write(thinkDelta(2, "hm")); got != alive {
		t.Fatalf("while thinking: %q", got)
	}
	write(ev(ir.PartStop, 2))
	now = now.Add(pingAfter)
	if got := write(finishEv(ir.StopToolUse, 1, 1)); strings.Contains(got, alive) {
		t.Fatalf("a keep-alive at the end: %q", got)
	}
	// A comment is no event: clients skip it, and so does the checker.
	s, err := CheckStream(buf.Bytes())
	if err != nil || !s.Completed || len(s.Items) != 3 {
		t.Fatalf("%v, %+v", err, s)
	}
}

type failingWriter struct {
	left int
	n    int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.left == 0 {
		return 0, errors.New("client gone")
	}
	w.left--
	w.n++
	return len(p), nil
}

func TestStreamEncoder_WriteErrorIsReturnedAndSticks(t *testing.T) {
	for allowed := 0; allowed < 12; allowed++ {
		w := &failingWriter{left: allowed}
		e := NewStreamEncoder(w, "m", created)
		e.now = frozen
		var first error
		for _, ev := range interleaved() {
			if err := e.Write(ev); err != nil && first == nil {
				first = err
			}
		}
		if first == nil || first.Error() != "client gone" || e.Close() == nil || w.n != allowed {
			t.Fatalf("allowed %d: err %v, %d writes", allowed, first, w.n)
		}
	}
}

func TestStreamEncoder_OneWritePerFrame(t *testing.T) {
	var w framesWriter
	e := NewStreamEncoder(&w, "m", created)
	e.now = frozen
	for _, ev := range interleaved() {
		if err := e.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range w.writes {
		if !strings.HasPrefix(p, "event: ") || !strings.HasSuffix(p, "\n\n") || strings.Count(p, "\n\n") != 1 {
			t.Fatalf("a write that is not one frame: %q", p)
		}
	}
}

type framesWriter struct{ writes []string }

func (f *framesWriter) Write(p []byte) (int, error) {
	f.writes = append(f.writes, string(p))
	return len(p), nil
}

// ---------------------------------------------------------------- property

type genPart struct {
	kind   ir.PartKind
	pieces []string
}

// genEvents makes a well-formed event sequence of up to 5 parts whose deltas are interleaved at
// random, as ir.Event allows: tool calls side by side, a text opening while they are open.
func genEvents(rng *rand.Rand) ([]ir.Event, []genPart) {
	cut := func(s string) []string {
		var out []string
		for len(s) > 0 {
			n := 1 + rng.Intn(len(s))
			out = append(out, s[:n])
			s = s[n:]
		}
		return out
	}
	parts := make([]genPart, rng.Intn(6))
	for i := range parts {
		switch rng.Intn(4) {
		case 0:
			parts[i] = genPart{ir.Text, cut(strings.Repeat("text ", rng.Intn(4)))}
		case 1:
			parts[i] = genPart{ir.Thinking, cut(strings.Repeat("hm ", rng.Intn(3)))}
		default:
			args := ""
			if rng.Intn(4) > 0 {
				args = fmt.Sprintf(`{"n":%d,"s":"%s"}`, rng.Intn(1000), strings.Repeat("ab", rng.Intn(5)))
			}
			parts[i] = genPart{ir.ToolUse, cut(args)}
		}
	}
	events := []ir.Event{startEv()}
	sent := make([]int, len(parts))
	var open []int
	opened, textOpen := 0, false
	for opened < len(parts) || len(open) > 0 {
		canOpen := opened < len(parts) && !textOpen
		if canOpen && (len(open) == 0 || rng.Intn(3) == 0) {
			i := opened
			opened++
			p := ir.Part{Kind: parts[i].kind}
			if p.Kind == ir.ToolUse {
				p.ToolID, p.ToolName = fmt.Sprint("call_", i), fmt.Sprint("fn", i)
			} else {
				textOpen = true
			}
			events = append(events, ir.Event{Kind: ir.PartStart, Index: i, Part: p})
			open = append(open, i)
			continue
		}
		k := rng.Intn(len(open))
		i := open[k]
		if sent[i] < len(parts[i].pieces) {
			piece := parts[i].pieces[sent[i]]
			sent[i]++
			switch parts[i].kind {
			case ir.Text:
				events = append(events, textDelta(i, piece))
			case ir.Thinking:
				events = append(events, thinkDelta(i, piece))
			default:
				events = append(events, argsDelta(i, piece))
			}
			continue
		}
		events = append(events, ev(ir.PartStop, i))
		open = append(open[:k], open[k+1:]...)
		if parts[i].kind != ir.ToolUse {
			textOpen = false
		}
	}
	stop := ir.StopEnd
	for _, p := range parts {
		if p.kind == ir.ToolUse {
			stop = ir.StopToolUse
		}
	}
	return append(events, finishEv(stop, 5, 9)), parts
}

// replay sends what a client holds after a stream back as the history of its next request, the
// way Codex does: every done item of the response, then a function_call_output for every done
// call. Whatever the stream was, the request must go through and be one a strict Chat Completions
// server takes: a session must survive every answer the encoder can write.
func replay(t testing.TB, raw []byte) {
	t.Helper()
	names, data := frames(t, raw)
	items := []string{userItem}
	var outputs []string
	for i, d := range data {
		if names[i] != "response.output_item.done" {
			continue
		}
		it := d["item"].(map[string]any)
		b, _ := json.Marshal(it)
		items = append(items, string(b))
		if it["type"] == "function_call" {
			id, _ := json.Marshal(it["call_id"])
			outputs = append(outputs, `{"type":"function_call_output","call_id":`+string(id)+`,"output":"ok"}`)
		}
	}
	body := withInput(strings.Join(append(items, outputs...), ","))
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("the history is refused: %v\n%s", err, body)
	}
	for _, name := range req.Dropped {
		if name != "input:reasoning" {
			t.Fatalf("the history loses %s\n%s", name, body)
		}
	}
	out, _, err := chat.EncodeRequest(req, "m")
	if err != nil {
		t.Fatalf("the history cannot be sent: %v\n%s", err, body)
	}
	if err := chat.CheckRequest(out); err != nil {
		t.Fatalf("the history is no valid Chat conversation: %v\n%s\n%s", err, out, body)
	}
}

func TestProperty_ItemsAreSequentialAndBytesAreKept(t *testing.T) {
	rng := rand.New(rand.NewSource(20261007))
	itemType := map[ir.PartKind]string{ir.Text: "message", ir.Thinking: "reasoning", ir.ToolUse: "function_call"}
	for round := 0; round < 3000; round++ {
		events, parts := genEvents(rng)
		// Whole, and cut short at a random place (then ended by Close).
		n := len(events)
		whole := round%3 != 0
		if !whole {
			n = rng.Intn(len(events))
		}
		raw, err := encodeEvents(events[:n], true)
		if err != nil {
			t.Fatalf("round %d: %v\n%+v", round, err, events[:n])
		}
		// Items one after the other, output_index 0,1,2…, sequence numbers without a gap, and a
		// final response that repeats what the deltas added up to: see CheckStream.
		s, err := CheckStream(raw)
		if err != nil {
			t.Fatalf("round %d: %v\n%s\n%+v", round, err, raw, events[:n])
		}
		if whole != s.Completed || whole == (s.Status == "failed") {
			t.Fatalf("round %d: whole %v, stream %+v", round, whole, s)
		}
		replay(t, raw)
		// What each part had got when the events stopped.
		got := map[int]string{}
		stopped := map[int]bool{}
		started := 0
		for _, ev := range events[:n] {
			switch ev.Kind {
			case ir.PartStart:
				started++
			case ir.TextDelta, ir.ThinkingDelta:
				got[ev.Index] += ev.Text
			case ir.ToolArgsDelta:
				got[ev.Index] += ev.ArgsJSON
			case ir.PartStop:
				stopped[ev.Index] = true
			}
		}
		// The final output, read without the checker: the done items, in order.
		names, data := frames(t, raw)
		final := data[len(data)-1]["response"].(map[string]any)["output"].([]any)
		if (names[len(names)-1] == "response.completed") != whole {
			t.Fatalf("round %d: the last event is %s", round, names[len(names)-1])
		}
		// Every part is on the wire in part order with exactly the bytes it had got — but for a call
		// that was cut: it is not done (and not written at all when it was still held back).
		at, doneAt := 0, 0
		for i := 0; i < started; i++ {
			want := got[i]
			call := parts[i].kind == ir.ToolUse
			if call && want == "" && stopped[i] {
				want = "{}"
			}
			whole := !call || ir.CheckObject([]byte(want)) == nil
			// A call that stopped without arguments in a stream that failed: done only when an
			// event after its stop showed that the stream went on.
			unsure := call && got[i] == "" && stopped[i] && n < len(events)
			if at == len(s.Items) || (call && s.Items[at].CallID != fmt.Sprint("call_", i)) {
				if whole && !unsure {
					t.Fatalf("round %d: part %d is missing\n%s", round, i, raw)
				}
				continue
			}
			it := s.Items[at]
			at++
			if it.Type != itemType[parts[i].kind] || (it.Done != whole && !unsure) {
				t.Fatalf("round %d part %d: %+v for a %s part, whole %v", round, i, it, parts[i].kind, whole)
			}
			if !it.Done {
				if unsure {
					want = ""
				}
				if strings.Join(it.Deltas, "") != want {
					t.Fatalf("round %d part %d: deltas %q, want %q", round, i, it.Deltas, want)
				}
				continue
			}
			inFinal := final[doneAt].(map[string]any)
			doneAt++
			if inFinal["id"] != it.ID {
				t.Fatalf("round %d part %d: item %s is %v in the final output", round, i, it.ID, inFinal["id"])
			}
			switch parts[i].kind {
			case ir.ToolUse:
				if it.Name != fmt.Sprint("fn", i) || strings.Join(it.Deltas, "") != want || it.Arguments != want || inFinal["arguments"] != want || it.Status != "completed" {
					t.Fatalf("round %d part %d: %+v, want arguments %q", round, i, it, want)
				}
			case ir.Text:
				if wantStatus := map[bool]string{true: "completed", false: "incomplete"}[stopped[i]]; it.Status != wantStatus {
					t.Fatalf("round %d part %d: status %q, want %q", round, i, it.Status, wantStatus)
				}
				if it.Text != want || inFinal["content"].([]any)[0].(map[string]any)["text"] != want {
					t.Fatalf("round %d part %d: %+v, want text %q", round, i, it, want)
				}
			default:
				if it.Text != want {
					t.Fatalf("round %d part %d: %+v, want text %q", round, i, it, want)
				}
			}
		}
		if at != len(s.Items) || doneAt != len(final) {
			t.Fatalf("round %d: %d items and %d done ones on the wire, %d and %d accounted for", round, len(s.Items), len(final), at, doneAt)
		}
	}
}

// ---------------------------------------------------------------- CheckStream itself

func TestCheckStream_RefusesWhatAClientWould(t *testing.T) {
	good, err := encodeEvents(interleaved(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckStream(good); err != nil {
		t.Fatal(err)
	}
	blocks := strings.SplitAfter(string(good), "\n\n")
	blocks = blocks[:len(blocks)-1]
	join := func(b []string) []byte { return []byte(strings.Join(b, "")) }
	without := func(i int) []byte {
		return join(append(append([]string{}, blocks[:i]...), blocks[i+1:]...))
	}
	// Every frame is needed: without any one of them the stream is refused (sequence numbers
	// alone see to that), and so is a stream cut after any frame but the last.
	for i := range blocks {
		if _, err := CheckStream(without(i)); err == nil {
			t.Errorf("accepted without frame %d: %s", i, blocks[i])
		}
		if i < len(blocks)-1 {
			if _, err := CheckStream(join(blocks[:i+1])); err == nil {
				t.Errorf("accepted when cut after frame %d", i)
			}
		}
	}
	// Changes a client would trip over, each applied to the good stream.
	changes := map[string][2]string{
		"an item's arguments differ in the final response": {`"arguments":"{\"b\":[1,2]}","status":"completed"},{"id":"msg_2"`, `"arguments":"{\"b\":[1,3]}","status":"completed"},{"id":"msg_2"`},
		"the done event's arguments differ":                {`"name":"fa","arguments":"{\"a\":\"é\\n\"}"`, `"name":"fa","arguments":"{\"a\":\"e\\n\"}"`},
		"a delta for another item":                         {`"item_id":"fc_1","output_index":1,"delta":"}"`, `"item_id":"fc_0","output_index":1,"delta":"}"`},
		"a wrong output_index":                             {`"output_index":2,"item":{"id":"msg_2","type":"message","role":"assistant","status":"in_progress"`, `"output_index":3,"item":{"id":"msg_2","type":"message","role":"assistant","status":"in_progress"`},
		"a call_id that changes":                           {`"call_id":"call_b","name":"fb","arguments":"{\"b\":[1,2]}","status":"completed"}}`, `"call_id":"call_x","name":"fb","arguments":"{\"b\":[1,2]}","status":"completed"}}`},
		"the text differs in the done event":               {`"text":"meanwhile","logprobs"`, `"text":"meanwhil","logprobs"`},
		"a total that is not the sum":                      {`"total_tokens":16`, `"total_tokens":17`},
		"a completed response without usage":               {`"usage":{"input_tokens":7,"input_tokens_details":{"cached_tokens":0},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":16}`, `"usage":null`},
		"a usage without the details the SDKs require":     {`"input_tokens_details":{"cached_tokens":0},`, ``},
		"a response without the fields the SDKs require":   {`"parallel_tool_calls":true,"tool_choice":"auto","tools":[],`, ``},
		"a type that is not the event":                     {`data: {"type":"response.in_progress"`, `data: {"type":"response.created"`},
		"an event nobody knows":                            {"event: response.in_progress\ndata: {\"type\":\"response.in_progress\"", "event: response.wat\ndata: {\"type\":\"response.wat\""},
		"an item that starts with arguments":               {`"name":"fa","arguments":"","status":"in_progress"`, `"name":"fa","arguments":"{","status":"in_progress"`},
		"two data lines":                                   {`,"output_index":0,"delta":"}"}`, ",\ndata: \"output_index\":0,\"delta\":\"}\"}"},
	}
	for name, c := range changes {
		if !strings.Contains(string(good), c[0]) {
			t.Fatalf("%s: the good stream does not hold %s", name, c[0])
		}
		if _, err := CheckStream([]byte(strings.Replace(string(good), c[0], c[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A completed response whose call has arguments that are no object; anything after the end;
	// a failure without a message; an item left open at the end.
	for name, raw := range map[string][]byte{
		"frames after the end":    append(append([]byte{}, good...), blocks[len(blocks)-1]...),
		"a comment after the end": append(append([]byte{}, good...), ": keep-alive\n\n"...),
		"no end":                  join(blocks[:len(blocks)-1]),
		"nothing":                 nil,
		"no blank line":           []byte("event: response.created\ndata: {}"),
	} {
		if _, err := CheckStream(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	bad, _ := encodeEvents([]ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, `{"x":1}`), ev(ir.PartStop, 0), finishEv(ir.StopToolUse, 1, 1)}, true)
	for name, c := range map[string][2]string{
		"completed with arguments that are no object": {`{\"x\":1}`, `{\"x\":1`},
		"a done call that is not completed":           {`"status":"completed"}`, `"status":"incomplete"}`},
	} {
		if _, err := CheckStream(bytes.ReplaceAll(bad, []byte(c[0]), []byte(c[1]))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	failed, _ := encodeEvents([]ir.Event{startEv(), {Kind: ir.Error, Err: "x"}}, true)
	for name, c := range map[string][2]string{
		"a failure without an error":  {`"error":{"code":"server_error","message":"x"}`, `"error":null`},
		"a failure that is completed": {`"status":"failed"`, `"status":"completed"`},
	} {
		if _, err := CheckStream(bytes.Replace(failed, []byte(c[0]), []byte(c[1]), 1)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ---------------------------------------------------------------- fuzz

func FuzzDecodeRequest(f *testing.F) {
	f.Add(fixture(f, "testdata/req_codex.json"))
	f.Add([]byte(`{"model":"m","input":"hi","instructions":"be brief","max_output_tokens":9,"store":true,"previous_response_id":"r"}`))
	f.Add([]byte(withInput(userItem+`,{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},{"type":"function_call","call_id":"b","name":"g","arguments":""},{"type":"function_call_output","call_id":"b","output":[{"type":"input_text","text":"x"}]},{"type":"function_call_output","call_id":"a","output":"y"}`,
		`"tools":[{"type":"function","name":"f","parameters":{},"strict":true},{"type":"custom","name":"p"}]`, `"tool_choice":{"type":"function","name":"f"}`)))
	f.Add([]byte(withInput(`{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,QQ==","detail":"low"},{"type":"input_file","file_id":"f"}]}`)))
	f.Add([]byte(withInput(`{"type":"item_reference","id":"x"},{"role":"developer","content":"d"},{"type":"more"}`)))
	f.Add([]byte(withInput(userItem + `,{"type":"function_call","call_id":"a","name":"f","arguments":"{\"cut\":"},{"role":"assistant","content":"x"},{"type":"function_call","call_id":"a","name":"f"},{"type":"function_call","call_id":"b","name":"f"},{"role":"user","content":"u"},{"type":"function_call_output","call_id":"b","output":[{"type":"input_image","image_url":"https://x/y.png"}]},{"type":"function_call_output","call_id":"zz","output":"o"}`)))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := DecodeRequest(body)
		if err != nil {
			var bad *ir.BadRequestError
			if !errors.As(err, &bad) || bad.Field == "" || bad.Reason == "" || bad.Format != "responses" {
				t.Fatalf("an error that is no usable *ir.BadRequestError: %#v", err)
			}
			if !reflect.DeepEqual(req, ir.Request{}) {
				t.Fatalf("a request came with the error: %+v", req)
			}
			return
		}
		// What is accepted keeps the limits and the rules of the neutral form.
		if len(req.Messages) == 0 || len(req.Messages) > ir.MaxMessages || len(req.System) > ir.MaxParts || len(req.Tools) > ir.MaxTools || req.MaxTokens < 0 {
			t.Fatalf("limits: %d messages, %d system parts, %d tools, max_tokens %d", len(req.Messages), len(req.System), len(req.Tools), req.MaxTokens)
		}
		for _, p := range req.System {
			if p.Kind != ir.Text {
				t.Fatalf("system part %+v", p)
			}
		}
		// Every call is answered by the turn that follows it, and by nothing else.
		var waiting map[string]bool
		for i, m := range req.Messages {
			if (m.Role != ir.User && m.Role != ir.Assistant) || len(m.Parts) > ir.MaxParts {
				t.Fatalf("message %q with %d parts", m.Role, len(m.Parts))
			}
			results := 0
			for seen, p := range m.Parts {
				switch p.Kind {
				case ir.Text:
				case ir.Image:
					if m.Role != ir.User || p.Data == "" {
						t.Fatalf("image %+v in a %s turn", p, m.Role)
					}
				case ir.ToolResult:
					if m.Role != ir.User || !waiting[p.ToolID] || p.Data != "" || p.MediaType != "" || results != seen {
						t.Fatalf("message %d: tool result %+v in a %s turn after %d other parts, waiting %v", i, p, m.Role, seen-results, waiting)
					}
					delete(waiting, p.ToolID)
					results++
				case ir.ToolUse:
					if m.Role != ir.Assistant || p.ToolID == "" || p.ToolName == "" || ir.CheckObject(p.Input) != nil || len(p.Input) > ir.MaxToolArgsBytes {
						t.Fatalf("tool call %+v in a %s turn", p, m.Role)
					}
				default:
					t.Fatalf("part of kind %q", p.Kind)
				}
			}
			if len(waiting) > 0 {
				t.Fatalf("message %d: calls left unanswered %v", i, waiting)
			}
			waiting = map[string]bool{}
			calls := 0
			for _, p := range m.Parts {
				if p.Kind == ir.ToolUse {
					if waiting[p.ToolID] {
						t.Fatalf("message %d: call id %q twice", i, p.ToolID)
					}
					waiting[p.ToolID] = true
					calls++
				}
			}
			if calls > ir.MaxToolCalls {
				t.Fatalf("%d calls in one turn", calls)
			}
		}
		if len(waiting) > 0 {
			t.Fatalf("calls left unanswered at the end: %v", waiting)
		}
		for _, tool := range req.Tools {
			if tool.Name == "" || (tool.Schema != nil && ir.CheckObject(tool.Schema) != nil) {
				t.Fatalf("tool %+v", tool)
			}
		}
		// Every dropped name is fixed or carries a prefix; none is empty or the reserved "more".
		for _, name := range req.Dropped {
			if name == "" || name == "more" {
				t.Fatalf("dropped name %q", name)
			}
		}
		// The target's encoder takes it or refuses it with a client error; it never panics. What it
		// takes has every tool call answered where a strict server wants the answers.
		out, _, err := chat.EncodeRequest(req, "m")
		if err != nil && !errors.Is(err, chat.ErrUnsupported) {
			t.Fatalf("EncodeRequest: %v", err)
		}
		if err == nil {
			if err := chat.CheckPairing(out); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}
	})
}

// FuzzStreamEncoder drives the encoder with whatever the Chat stream decoder makes of arbitrary
// bytes. Oracle: the bytes written are a well-formed Responses stream (CheckStream) that agrees
// with the events: success exactly when they end in a Finish, and then the same content.
func FuzzStreamEncoder(f *testing.F) {
	f.Add(fixture(f, "../chat/testdata/stream_text.sse"), 7, 1000)
	f.Add(fixture(f, "../chat/testdata/stream_tools.sse"), 64, 1000)
	f.Add(fixture(f, "../chat/testdata/stream_tools.sse"), 3, 9)
	f.Add([]byte("data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\"}},{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"g\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"late text\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"), 5, 1000)
	f.Add([]byte("data: {\"choices\":[{\"delta\":{\"reasoning\":\"r\",\"content\":\"c\",\"refusal\":\"no\"}}]}\n\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n"), 1, 1000)
	f.Add([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\",\"arguments\":\" \"}}]},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"), 9, 1000)
	f.Add([]byte("data: {\"error\":\"first thing\"}\n\n"), 4, 1000)
	f.Fuzz(func(t *testing.T, raw []byte, piece, keep int) {
		events := chatEvents(raw, piece)
		// keep cuts the events short: the upstream handler went away and only Close is left.
		if keep >= 0 && keep < len(events) {
			events = events[:keep]
		}
		out, err := encodeEvents(events, true)
		if err != nil {
			t.Fatalf("encoder: %v\n%+v", err, events)
		}
		s, err := CheckStream(out)
		if err != nil {
			t.Fatalf("not a well-formed Responses stream: %v\n%s\n%+v", err, out, events)
		}
		replay(t, out)
		finished := len(events) > 0 && events[len(events)-1].Kind == ir.Finish
		if finished == (s.Status == "failed") || (s.Status == "failed") != (s.ErrMessage != "") {
			t.Fatalf("finished %v, stream status %q, error %q\n%+v", finished, s.Status, s.ErrMessage, events)
		}
		if !finished {
			return
		}
		want, err := ir.Collect(events)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		incomplete := want.Stop == ir.StopMaxTokens || want.Stop == ir.StopRefusal
		if s.Completed == incomplete || len(s.Items) != len(want.Parts) {
			t.Fatalf("status %q for stop %q, %d items for %d parts", s.Status, want.Stop, len(s.Items), len(want.Parts))
		}
		for i, p := range want.Parts {
			it := s.Items[i]
			switch p.Kind {
			case ir.ToolUse:
				// Review Focus 1: the caller's pieces add up to the upstream's bytes ("{}" is added
				// to nothing, and to nothing but white space).
				upstream := ""
				for _, ev := range events {
					if ev.Kind == ir.ToolArgsDelta && ev.Index == i {
						upstream += ev.ArgsJSON
					}
				}
				if strings.TrimSpace(upstream) == "" {
					upstream += "{}"
				}
				if it.Type != "function_call" || it.CallID != p.ToolID || it.Name != p.ToolName || it.Arguments != upstream || strings.Join(it.Deltas, "") != upstream {
					t.Fatalf("item %d: %+v, want %+v with arguments %q", i, it, p, upstream)
				}
				var a, b any
				if json.Unmarshal([]byte(it.Arguments), &a) != nil || json.Unmarshal(p.Input, &b) != nil || !reflect.DeepEqual(a, b) {
					t.Fatalf("item %d: arguments %q are not the call's input %q", i, it.Arguments, p.Input)
				}
			default:
				if (it.Type == "reasoning") != (p.Kind == ir.Thinking) || it.Text != p.Text {
					t.Fatalf("item %d: %+v, want %+v", i, it, p)
				}
			}
		}
		if s.InputTokens != max(want.Usage.InputTokens, 0) || s.OutputTokens != max(want.Usage.OutputTokens, 0) {
			t.Fatalf("usage %d/%d, want %+v", s.InputTokens, s.OutputTokens, want.Usage)
		}
	})
}

// FuzzStreamEncoderAnyEvents hands the encoder event sequences no decoder would emit. Oracle:
// whatever comes in, what goes out is a well-formed Responses stream that ended, and it reports
// an answer only when the events were a well-formed one, tool arguments included.
func FuzzStreamEncoderAnyEvents(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2, 0, 3, 0, 5, 0, 6, 0})
	f.Add([]byte{0, 0, 4, 0, 4, 1, 3, 1, 1, 2, 3, 0, 5, 0, 5, 1, 5, 2, 6, 0})
	f.Add([]byte{0, 0, 4, 0, 9, 0, 6, 0, 7, 0})
	f.Add([]byte{7, 0})
	f.Fuzz(func(t *testing.T, script []byte) {
		var events []ir.Event
		for i := 0; i+1 < len(script) && len(events) < 4096; i += 2 {
			index := int(script[i+1] % 8)
			piece := strings.Repeat(string(rune('a'+script[i+1]%26)), 1+int(script[i+1]/32))
			switch script[i] % 10 {
			case 0:
				events = append(events, startEv())
			case 1:
				events = append(events, textStart(index))
			case 2:
				events = append(events, thinkStart(index))
			case 3:
				events = append(events, textDelta(index, piece))
			case 4:
				events = append(events, toolStart(index, fmt.Sprint("id", len(events)), "f"))
			case 5:
				events = append(events, argsDelta(index, piece))
			case 6:
				events = append(events, ev(ir.PartStop, index))
			case 7:
				events = append(events, finishEv(ir.StopEnd, 1, 2))
			case 8:
				events = append(events, ir.Event{Kind: ir.Error, Err: piece})
			case 9:
				events = append(events, argsDelta(index, `{"k":1}`))
			}
		}
		var buf bytes.Buffer
		e := NewStreamEncoder(&buf, "m", created)
		for _, ev := range events {
			if err := e.Write(ev); err != nil && !errors.Is(err, ir.ErrSequence) && !errors.Is(err, ir.ErrLimit) {
				t.Fatalf("an error of no known kind: %v", err)
			}
		}
		if err := e.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		before := buf.Len()
		_ = e.Write(startEv())
		_ = e.Close()
		if buf.Len() != before {
			t.Fatal("bytes after the end")
		}
		s, err := CheckStream(buf.Bytes())
		if err != nil {
			t.Fatalf("not a well-formed Responses stream: %v\n%s\n%+v", err, buf.Bytes(), events)
		}
		replay(t, buf.Bytes())
		if s.Status != "failed" {
			// An answer is reported only for a prefix that is a well-formed answer up to its Finish.
			n := 0
			for n < len(events) && events[n].Kind != ir.Finish {
				n++
			}
			if n == len(events) {
				t.Fatalf("an answer without a Finish: %+v", events)
			}
			if _, err := ir.Collect(events[:n+1]); err != nil {
				t.Fatalf("an answer for a sequence that is not well formed (%v): %+v", err, events[:n+1])
			}
		}
	})
}
