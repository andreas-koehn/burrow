package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
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
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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

func text(s string) ir.Part { return ir.Part{Kind: ir.Text, Text: s} }

// decode reads a request that must be accepted.
func decode(t *testing.T, body string) ir.Request {
	t.Helper()
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v\n%s", err, body)
	}
	return req
}

// minimal is a request around one piece of JSON that is put after the
// required fields.
func minimal(extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]` + extra + `}`
}

// withContent is a request whose first message holds the given blocks.
func withContent(role, blocks string) string {
	return `{"model":"m","max_tokens":10,"messages":[{"role":"` + role + `","content":[` + blocks + `]}]}`
}

func refused(t *testing.T, body, field string) *BadRequestError {
	t.Helper()
	req, err := DecodeRequest([]byte(body))
	var bad *BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a *BadRequestError for %s\n%.200s", err, field, body)
	}
	if bad.Field != field {
		t.Fatalf("Field = %q (%s), want %q\n%.200s", bad.Field, bad.Reason, field, body)
	}
	if !reflect.DeepEqual(req, ir.Request{}) {
		t.Fatalf("a request came with the error: %+v", req)
	}
	return bad
}

// ---------------------------------------------------------------- request

func TestDecodeRequest_ClaudeCodeTurn(t *testing.T) {
	req := decode(t, string(fixture(t, "req_claude_code.json")))
	one := 1.0
	want := ir.Request{
		Model:  "burrow-medium",
		System: []ir.Part{text("You are a coding assistant.")},
		Messages: []ir.Message{
			{Role: ir.User, Parts: []ir.Part{text("Open README.md")}},
			{Role: ir.Assistant, Parts: []ir.Part{
				{Kind: ir.Thinking, Text: "I need the file."},
				text("I'll read it."),
				{Kind: ir.ToolUse, ToolID: "toolu_1", ToolName: "Read", Input: json.RawMessage(`{"file_path":"README.md"}`)},
			}},
			{Role: ir.User, Parts: []ir.Part{
				{Kind: ir.ToolResult, ToolID: "toolu_1", Text: "# Title\nbody"},
				text("Summarise it."),
			}},
		},
		Tools: []ir.Tool{{Name: "Read", Description: "Read a file",
			Schema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)}},
		ToolChoice:  ir.ToolChoice{Mode: ir.ChoiceAuto},
		MaxTokens:   4096,
		Temperature: &one,
		Stream:      true,
	}
	dropped := ir.Dropped(req.Dropped)
	req.Dropped = nil
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("request\n got %+v\nwant %+v", req, want)
	}
	if want := []string{"cache_control", "metadata", "thinking", "thinking.signature", "tool:web_search_20250305", "top_k"}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped = %v, want %v", dropped, want)
	}
}

func TestDecodeRequest_SystemAndContentShapes(t *testing.T) {
	req := decode(t, minimal(`"system":"be brief"`))
	if !reflect.DeepEqual(req.System, []ir.Part{text("be brief")}) || req.Dropped != nil {
		t.Fatalf("system = %+v, dropped %v", req.System, req.Dropped)
	}
	if !reflect.DeepEqual(req.Messages, []ir.Message{{Role: ir.User, Parts: []ir.Part{text("hi")}}}) {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.MaxTokens != 10 || req.Model != "m" || req.Stream || req.Temperature != nil || req.TopP != nil || req.ToolChoice != (ir.ToolChoice{}) {
		t.Fatalf("request = %+v", req)
	}
	// No system, an empty one, null.
	for _, extra := range []string{``, `"system":""`, `"system":null`, `"system":[]`} {
		if req := decode(t, minimal(extra)); len(req.System) != 0 || req.Dropped != nil {
			t.Fatalf("%s: system %+v, dropped %v", extra, req.System, req.Dropped)
		}
	}
	req = decode(t, minimal(`"system":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"},"citations":[]}]`))
	if !reflect.DeepEqual(req.System, []ir.Part{text("a"), text("b")}) {
		t.Fatalf("system = %+v", req.System)
	}
	if got := ir.Dropped(req.Dropped); !reflect.DeepEqual(got, []string{"cache_control", "unknown:content.citations"}) {
		t.Fatalf("dropped = %v", got)
	}
	// Text is kept exactly.
	exact := " a\n\t\"b\" \u00e9\u2028 "
	body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 1, "system": exact,
		"messages": []any{map[string]any{"role": "user", "content": exact}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": exact}}}}})
	req = decode(t, string(body))
	if req.System[0].Text != exact || req.Messages[0].Parts[0].Text != exact || req.Messages[1].Parts[0].Text != exact {
		t.Fatalf("text changed: %+v", req)
	}
}

func TestDecodeRequest_EveryTopLevelField(t *testing.T) {
	// Carried.
	req := decode(t, minimal(`"temperature":0.5,"top_p":0.25,"stop_sequences":["a","b"],"stream":true`))
	if *req.Temperature != 0.5 || *req.TopP != 0.25 || !reflect.DeepEqual(req.Stop, []string{"a", "b"}) || !req.Stream || req.Dropped != nil {
		t.Fatalf("request = %+v", req)
	}
	req = decode(t, minimal(`"temperature":null,"top_p":null,"stop_sequences":null,"stream":false,"top_k":null,"metadata":null,"thinking":null,"tools":null,"tool_choice":null`))
	if req.Temperature != nil || req.TopP != nil || req.Stop != nil || req.Stream || req.Dropped != nil {
		t.Fatalf("nulls: %+v", req)
	}
	// Reported: each by its fixed name, anything else with the prefix.
	for extra, want := range map[string]string{
		`"top_k":40`:                 "top_k",
		`"metadata":{"user_id":"u"}`: "metadata",
		`"thinking":{"type":"enabled","budget_tokens":1024}`: "thinking",
		`"thinking":{"type":"adaptive"}`:                     "thinking",
		`"service_tier":"auto"`:                              "service_tier",
		`"container":"c1"`:                                   "container",
		`"context_management":{"edits":[]}`:                  "context_management",
		`"mcp_servers":[{"name":"x"}]`:                       "mcp_servers",
		`"output_config":{"effort":"high"}`:                  "output_config",
		`"cache_control":{"type":"ephemeral"}`:               "cache_control",
		`"foo":1`:                                            "unknown:foo",
		`"more":true`:                                        "unknown:more",
		`"":0`:                                               "unknown:",
		`"output_format":{"type":"json_schema"}`:             "unknown:output_format",
	} {
		if got := ir.Dropped(decode(t, minimal(extra)).Dropped); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("%s: dropped = %v, want [%s]", extra, got, want)
		}
	}
	// Thinking switched off loses nothing.
	if got := decode(t, minimal(`"thinking":{"type":"disabled"}`)).Dropped; got != nil {
		t.Errorf("thinking disabled: dropped = %v", got)
	}
	// Unknown fields of a message, a block and a tool are named by where they stand.
	req = decode(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","name":"x","content":[{"type":"text","text":"a","extra":1}]}],
		"tools":[{"name":"f","input_schema":{},"strict":true,"cache_control":{"type":"ephemeral"}}],"tool_choice":{"type":"auto","x":1}}`)
	if got, want := ir.Dropped(req.Dropped), []string{"cache_control", "unknown:content.extra", "unknown:messages.name", "unknown:tool_choice.x", "unknown:tools.strict"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dropped = %v, want %v", got, want)
	}
}

func TestDecodeRequest_RequiredAndMistyped(t *testing.T) {
	for body, field := range map[string]string{
		`null`:      "body",
		`[]`:        "body",
		`"x"`:       "body",
		``:          "body",
		`{"a":1} x`: "body",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`:                                               "max_tokens",
		`{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`:                                "max_tokens",
		`{"model":"m","max_tokens":-4,"messages":[{"role":"user","content":"hi"}]}`:                               "max_tokens",
		`{"model":"m","max_tokens":1.5,"messages":[{"role":"user","content":"hi"}]}`:                              "max_tokens",
		`{"model":"m","max_tokens":"9","messages":[{"role":"user","content":"hi"}]}`:                              "max_tokens",
		`{"model":"m","max_tokens":1e30,"messages":[{"role":"user","content":"hi"}]}`:                             "max_tokens",
		`{"model":"m","max_tokens":1}`:                                                                            "messages",
		`{"model":"m","max_tokens":1,"messages":[]}`:                                                              "messages",
		`{"model":"m","max_tokens":1,"messages":null}`:                                                            "messages",
		`{"model":"m","max_tokens":1,"messages":"x"}`:                                                             "messages",
		`{"model":"m","max_tokens":1,"messages":["x"]}`:                                                           "messages[0]",
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"a"},{"role":"system","content":"x"}]}`: "messages[1].role",
		`{"model":"m","max_tokens":1,"messages":[{"content":"x"}]}`:                                               "messages[0].role",
		`{"model":"m","max_tokens":1,"messages":[{"role":"user"}]}`:                                               "messages[0].content",
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":7}]}`:                                   "messages[0].content",
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[7]}]}`:                                 "messages[0].content[0]",
		`{"model":7,"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`:                                   "model",
		minimal(`"system":7`):                                                    "system",
		minimal(`"system":[{"type":"image"}]`):                                   "system[0].type",
		minimal(`"system":[{"type":"text","text":7}]`):                           "system[0].text",
		minimal(`"temperature":"hot"`):                                           "temperature",
		minimal(`"top_p":[]`):                                                    "top_p",
		minimal(`"stream":"yes"`):                                                "stream",
		minimal(`"stop_sequences":"a"`):                                          "stop_sequences",
		minimal(`"stop_sequences":[1]`):                                          "stop_sequences[0]",
		minimal(`"tools":{}`):                                                    "tools",
		minimal(`"tools":[7]`):                                                   "tools[0]",
		minimal(`"tools":[{"input_schema":{}}]`):                                 "tools[0].name",
		minimal(`"tools":[{"name":"f","type":7}]`):                               "tools[0].type",
		minimal(`"tools":[{"name":"f","input_schema":[]}]`):                      "tools[0].input_schema",
		minimal(`"tools":[{"name":"f","description":7}]`):                        "tools[0].description",
		minimal(`"tool_choice":"auto"`):                                          "tool_choice",
		minimal(`"tool_choice":{"type":"sometimes"}`):                            "tool_choice.type",
		minimal(`"tool_choice":{}`):                                              "tool_choice.type",
		minimal(`"tool_choice":{"type":"tool"}`):                                 "tool_choice.name",
		minimal(`"tool_choice":{"type":"auto","disable_parallel_tool_use":"y"}`): "tool_choice.disable_parallel_tool_use",
	} {
		refused(t, body, field)
	}
}

func TestDecodeRequest_ToolChoice(t *testing.T) {
	tools := `"tools":[{"name":"f","input_schema":{"type":"object"}}],`
	for choice, want := range map[string]ir.ToolChoice{
		`{"type":"auto"}`:            {Mode: ir.ChoiceAuto},
		`{"type":"any"}`:             {Mode: ir.ChoiceRequired},
		`{"type":"tool","name":"f"}`: {Mode: ir.ChoiceTool, Name: "f"},
		`{"type":"none"}`:            {Mode: ir.ChoiceNone},
		`{"type":"auto","disable_parallel_tool_use":false}`: {Mode: ir.ChoiceAuto},
	} {
		req := decode(t, minimal(tools+`"tool_choice":`+choice))
		if req.ToolChoice != want || req.Dropped != nil {
			t.Errorf("%s: %+v, dropped %v", choice, req.ToolChoice, req.Dropped)
		}
	}
	req := decode(t, minimal(tools+`"tool_choice":{"type":"any","disable_parallel_tool_use":true}`))
	if req.ToolChoice.Mode != ir.ChoiceRequired || !reflect.DeepEqual(req.Dropped, []string{"tool_choice.disable_parallel_tool_use"}) {
		t.Fatalf("%+v, dropped %v", req.ToolChoice, req.Dropped)
	}
}

func TestDecodeRequest_Tools(t *testing.T) {
	schema := `{ "type":"object", "properties":{"n":{"type":"integer","maximum":12345678901234567890}},
	"required":["n"] }`
	req := decode(t, minimal(`"tools":[
		{"name":"plain","description":"d","input_schema":`+schema+`},
		{"type":"custom","name":"custom","input_schema":{"type":"object"}},
		{"name":"bare"},
		{"type":"web_search_20250305","name":"web_search","max_uses":3},
		{"type":"bash_20250124","name":"bash"},
		{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"},
		{"type":"computer_20250124","name":"computer","display_width_px":1,"display_height_px":1},
		{"type":"code_execution_20250825","name":"code_execution"},
		{"type":"more","name":"x"},
		{"type":"","name":"empty"}
	]`))
	want := []ir.Tool{
		{Name: "plain", Description: "d", Schema: json.RawMessage(schema)},
		{Name: "custom", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "bare"},
	}
	if !reflect.DeepEqual(req.Tools, want) {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if got, want := ir.Dropped(req.Dropped), []string{"tool:", "tool:bash_20250124", "tool:code_execution_20250825", "tool:computer_20250124",
		"tool:more", "tool:text_editor_20250728", "tool:web_search_20250305"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dropped = %v", got)
	}
}

func TestDecodeRequest_Blocks(t *testing.T) {
	// Images.
	req := decode(t, withContent("user", `{"type":"text","text":"look"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},
		{"type":"image","source":{"type":"url","url":"https://example.test/a.png"},"cache_control":{"type":"ephemeral"}}`))
	want := []ir.Part{text("look"), {Kind: ir.Image, MediaType: "image/png", Data: "QUJD"}, {Kind: ir.Image, Data: "https://example.test/a.png"}}
	if !reflect.DeepEqual(req.Messages[0].Parts, want) || !reflect.DeepEqual(req.Dropped, []string{"cache_control"}) {
		t.Fatalf("parts = %+v, dropped %v", req.Messages[0].Parts, req.Dropped)
	}
	// Tool calls: the input's bytes are kept, an absent one is {}.
	input := `{ "b":1.0, "a":[12345678901234567890, "<&>"] }`
	req = decode(t, withContent("assistant", `{"type":"tool_use","id":"t1","name":"f","input":`+input+`},{"type":"tool_use","id":"t2","name":"g"},{"type":"tool_use","id":"t3","name":"h","input":null,"caller":{"type":"direct"}}`))
	want = []ir.Part{
		{Kind: ir.ToolUse, ToolID: "t1", ToolName: "f", Input: json.RawMessage(input)},
		{Kind: ir.ToolUse, ToolID: "t2", ToolName: "g", Input: json.RawMessage(`{}`)},
		{Kind: ir.ToolUse, ToolID: "t3", ToolName: "h", Input: json.RawMessage(`{}`)},
	}
	// (The calls have no results here: each gets one, and that is reported.)
	if !reflect.DeepEqual(req.Messages[0].Parts, want) || !reflect.DeepEqual(ir.Dropped(req.Dropped), []string{"input:tool_use.unanswered", "unknown:content.caller"}) {
		t.Fatalf("parts = %+v, dropped %v", req.Messages[0].Parts, req.Dropped)
	}
	// Tool results: a string, text blocks, nothing, an error. They stand first, in the order of the
	// calls they answer; the text next to them is kept.
	calls := `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f"},{"type":"tool_use","id":"t2","name":"f"},{"type":"tool_use","id":"t3","name":"f"},{"type":"tool_use","id":"t4","name":"f"}]},`
	req = decode(t, strings.Replace(withContent("user", `{"type":"text","text":"before"},
		{"type":"tool_result","tool_use_id":"t1","content":"plain"},
		{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}],"is_error":true},
		{"type":"tool_result","tool_use_id":"t3"},
		{"type":"tool_result","tool_use_id":"t4","content":[],"is_error":false},
		{"type":"text","text":"after"}`), `"messages":[`, `"messages":[`+calls, 1))
	want = []ir.Part{
		{Kind: ir.ToolResult, ToolID: "t1", Text: "plain"},
		{Kind: ir.ToolResult, ToolID: "t2", Text: "a\nb", IsError: true},
		{Kind: ir.ToolResult, ToolID: "t3"},
		{Kind: ir.ToolResult, ToolID: "t4"},
		text("before"),
		text("after"),
	}
	if !reflect.DeepEqual(req.Messages[1].Parts, want) || !reflect.DeepEqual(req.Dropped, []string{"cache_control"}) {
		t.Fatalf("parts = %+v, dropped %v", req.Messages[1].Parts, req.Dropped)
	}
	// Thinking: the text is carried (the target's encoder decides), a signature is reported;
	// redacted thinking is left out and reported.
	req = decode(t, withContent("assistant", `{"type":"thinking","thinking":"hm"},{"type":"thinking","thinking":"hm2","signature":""},{"type":"redacted_thinking","data":"AAAA"},{"type":"text","text":"x"}`))
	want = []ir.Part{{Kind: ir.Thinking, Text: "hm"}, {Kind: ir.Thinking, Text: "hm2"}, text("x")}
	if !reflect.DeepEqual(req.Messages[0].Parts, want) || !reflect.DeepEqual(req.Dropped, []string{"thinking"}) {
		t.Fatalf("parts = %+v, dropped %v", req.Messages[0].Parts, req.Dropped)
	}
	req = decode(t, withContent("assistant", `{"type":"thinking","thinking":"hm","signature":"c2ln"}`))
	if !reflect.DeepEqual(req.Dropped, []string{"thinking.signature"}) {
		t.Fatalf("dropped %v", req.Dropped)
	}
	// History of a tool the provider ran itself: left out, reported with the prefix.
	req = decode(t, withContent("assistant", `{"type":"text","text":"searching"},
		{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"q"}},
		{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]},
		{"type":"mcp_tool_use","id":"m1","name":"x","server_name":"s","input":{}},
		{"type":"mcp_tool_result","tool_use_id":"m1","content":"r"},
		{"type":"code_execution_tool_result","tool_use_id":"srvtoolu_2","content":{}}`))
	if !reflect.DeepEqual(req.Messages[0].Parts, []ir.Part{text("searching")}) {
		t.Fatalf("parts = %+v", req.Messages[0].Parts)
	}
	if got, want := ir.Dropped(req.Dropped), []string{"input:code_execution_tool_result", "input:mcp_tool_result", "input:mcp_tool_use", "input:server_tool_use", "input:web_search_tool_result"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dropped = %v", got)
	}
}

func TestDecodeRequest_BlocksThatAreRefused(t *testing.T) {
	secret := "SECRET-user-content"
	cases := map[string]string{
		// An image is carried or the request fails.
		withContent("user", `{"type":"image","source":{"type":"file","file_id":"f"}}`):                "messages[0].content[0].source.type",
		withContent("user", `{"type":"image"}`):                                                       "messages[0].content[0].source",
		withContent("user", `{"type":"image","source":{"type":"base64","media_type":"image/png"}}`):   "messages[0].content[0].source.data",
		withContent("user", `{"type":"image","source":{"type":"base64","data":"QUJD"}}`):              "messages[0].content[0].source.media_type",
		withContent("user", `{"type":"image","source":{"type":"url"}}`):                               "messages[0].content[0].source.url",
		withContent("assistant", `{"type":"image","source":{"type":"url","url":"https://x.test/a"}}`): "messages[0].content[0]",
		// Content that would vanish.
		withContent("user", `{"type":"text","text":"a"},{"type":"document","source":{"type":"text","media_type":"text/plain","data":"`+secret+`"}}`): "messages[0].content[1].type",
		withContent("user", `{"type":"`+secret+`"}`):                                                    "messages[0].content[0].type",
		withContent("user", `{"text":"no type"}`):                                                       "messages[0].content[0].type",
		withContent("user", `{"type":"search_result","source":"s","title":"t","content":[]}`):           "messages[0].content[0].type",
		withContent("user", `{"type":"tool_result","tool_use_id":"t","content":[{"type":"document"}]}`): "messages[0].content[0].content[0]",
		withContent("user", `{"type":"tool_result","tool_use_id":"t","content":7}`):                     "messages[0].content[0].content",
		withContent("user", `{"type":"tool_result","content":"x"}`):                                     "messages[0].content[0].tool_use_id",
		withContent("user", `{"type":"tool_result","tool_use_id":"t","is_error":"no"}`):                 "messages[0].content[0].is_error",
		// Blocks in the wrong turn.
		withContent("assistant", `{"type":"tool_result","tool_use_id":"t","content":"x"}`): "messages[0].content[0]",
		withContent("user", `{"type":"tool_use","id":"t","name":"f","input":{}}`):          "messages[0].content[0]",
		withContent("user", `{"type":"thinking","thinking":"`+secret+`"}`):                 "messages[0].content[0]",
		// Tool calls a client could not act on.
		withContent("assistant", `{"type":"tool_use"}`):                                          "messages[0].content[0].id",
		withContent("assistant", `{"type":"tool_use","id":"t"}`):                                 "messages[0].content[0].name",
		withContent("assistant", `{"type":"tool_use","id":"t","name":"f","input":[]}`):           "messages[0].content[0].input",
		withContent("assistant", `{"type":"tool_use","id":"t","name":"f","input":"`+secret+`"}`): "messages[0].content[0].input",
		withContent("assistant", `{"type":"text","text":7}`):                                     "messages[0].content[0].text",
		withContent("assistant", `{"type":"thinking","thinking":7}`):                             "messages[0].content[0].thinking",
	}
	for body, field := range cases {
		bad := refused(t, body, field)
		if msg := bad.Error(); strings.Contains(msg, secret) || !strings.Contains(msg, field) {
			t.Errorf("Error() = %q", msg)
		}
	}
	// The reason says what is wrong in fixed words.
	// An image in a tool result is carried (see the test below); one that cannot be read is refused
	// like any other image.
	bad := refused(t, withContent("user", `{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{}}]}`), "messages[0].content[0].content[0].source.type")
	if !strings.Contains(bad.Reason, "image source") {
		t.Fatalf("Reason = %q", bad.Reason)
	}
	bad = refused(t, withContent("user", `{"type":"document"}`), "messages[0].content[0].type")
	if !strings.Contains(bad.Reason, "document") {
		t.Fatalf("Reason = %q", bad.Reason)
	}
}

func TestDecodeRequest_TurnEmptiedByDroppingIsLeftOut(t *testing.T) {
	// A turn whose only blocks were left out is no turn: it must not reach the target as
	// {"role":"user","content":""}. The drop is reported.
	req := decode(t, `{"model":"m","max_tokens":9,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"mcp_tool_use","id":"m1","name":"x","server_name":"s","input":{}}]},
		{"role":"user","content":[{"type":"mcp_tool_result","tool_use_id":"m1","content":"r"}]},
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"AAAA"}]},
		{"role":"user","content":"on"}]}`)
	want := []ir.Message{{Role: ir.User, Parts: []ir.Part{text("go")}}, {Role: ir.User, Parts: []ir.Part{text("on")}}}
	if !reflect.DeepEqual(req.Messages, want) {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if got := ir.Dropped(req.Dropped); !reflect.DeepEqual(got, []string{"input:mcp_tool_result", "input:mcp_tool_use", "thinking"}) {
		t.Fatalf("dropped = %v", got)
	}
	body, _, err := chat.EncodeRequest(req, "gpt-x")
	if err != nil || string(body) != `{"model":"gpt-x","messages":[{"role":"user","content":"go"},{"role":"user","content":"on"}],"max_tokens":9}` {
		t.Fatalf("%v: %s", err, body)
	}
	// A turn that was empty to begin with is the caller's own and stays.
	req = decode(t, `{"model":"m","max_tokens":9,"messages":[{"role":"user","content":[]},{"role":"user","content":""}]}`)
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	// Nothing left to send is a request without messages.
	refused(t, withContent("user", `{"type":"mcp_tool_result","tool_use_id":"m1","content":"r"}`), "messages")
	refused(t, `{"model":"m","max_tokens":9,"system":"s","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{}}]},{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"s","content":[]}]}]}`, "messages")
}

func TestDecodeRequest_Limits(t *testing.T) {
	list := func(element string, n int) string { return strings.TrimSuffix(strings.Repeat(element+",", n), ",") }
	msg := `{"role":"user","content":"x"}`
	block := `{"type":"text","text":"x"}`
	tool := `{"name":"f"}`
	over := map[string]string{
		`{"model":"m","max_tokens":1,"messages":[` + list(msg, ir.MaxMessages+1) + `]}`:                                                    "messages",
		withContent("user", list(block, ir.MaxParts+1)):                                                                                    "messages[0].content",
		minimal(`"system":[` + list(block, ir.MaxParts+1) + `]`):                                                                           "system",
		minimal(`"tools":[` + list(tool, ir.MaxTools+1) + `]`):                                                                             "tools",
		minimal(`"stop_sequences":[` + list(`"s"`, ir.MaxParts+1) + `]`):                                                                   "stop_sequences",
		withContent("user", `{"type":"tool_result","tool_use_id":"t","content":[`+list(block, ir.MaxParts+1)+`]}`):                         "messages[0].content[0].content",
		withContent("assistant", `{"type":"tool_use","id":"t","name":"f","input":{"a":"`+strings.Repeat("x", ir.MaxToolArgsBytes)+`"}}`):   "messages[0].content[0].input",
		minimal(`"x":` + strings.Repeat("[", ir.MaxDepth+1) + strings.Repeat("]", ir.MaxDepth+1)):                                          "body",
		minimal(`"tools":[{"name":"f","input_schema":{"a":` + strings.Repeat("[", ir.MaxDepth) + strings.Repeat("]", ir.MaxDepth) + `}}]`): "body",
	}
	for body, field := range over {
		bad := refused(t, body, field)
		if !errors.Is(bad, ir.ErrLimit) {
			t.Errorf("%s: %v is not ErrLimit", field, bad)
		}
	}
	// At the limits a request is taken.
	decode(t, `{"model":"m","max_tokens":1,"messages":[`+list(msg, ir.MaxMessages)+`]}`)
	decode(t, withContent("user", list(block, ir.MaxParts)))
	decode(t, minimal(`"tools":[`+list(tool, ir.MaxTools)+`]`))
	// A list of hundreds of thousands of empty elements is refused before it is held.
	refused(t, `{"model":"m","max_tokens":1,"messages":[`+list(`{}`, 300_000)+`]}`, "messages")
	refused(t, withContent("user", list(`[]`, 300_000)), "messages[0].content")
}

func TestDecodeRequest_NeverPanics(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `{}`, `{"messages":"x"}`, `{"messages":[{"role":"user","content":[{"type":"tool_use"}]}]}`,
		`{"messages":[null]}`, `{"messages":[{"role":"user","content":[null]}]}`, `{"tools":[null],"messages":[{}]}`,
		`{"max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[null]}]}]}`,
		`{"max_tokens":1,"messages":[{"role":"user","content":` + strings.Repeat("[", 5000) + strings.Repeat("]", 5000) + `}]}`,
		strings.Repeat("[", 100000), strings.Repeat(`{"a":`, 100000), "\x00\xff", `{"max_tokens":1,"messages":[{"role":"user","content":"` + "\xff\xfe" + `"}]}`,
		`{"max_tokens":1,"system":{"type":"text"},"messages":[{"role":"user","content":"x"}]}`,
	} {
		_, _ = DecodeRequest([]byte(body))
	}
	big := strings.Repeat("y", 1<<20)
	req := decode(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"`+big+`"}]}`)
	if req.Messages[0].Parts[0].Text != big {
		t.Fatal("a 1 MiB string was not carried")
	}
}

func TestDecodeRequest_ImagesInToolResultsFollowAsUserContent(t *testing.T) {
	// Claude Code's Read tool returns an image inside a tool_result. A tool result of the neutral
	// form holds text only, and refusing the request would end the session (the history is sent
	// again with every request): the result keeps its text, the images follow in the same user
	// message after a text that names the call. Nothing is lost, so nothing is reported.
	const png = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"},"cache_control":{"type":"ephemeral"}}`
	req := decode(t, `{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"look at both"},
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Read","input":{"file_path":"a.png"}},{"type":"tool_use","id":"b","name":"Read","input":{"file_path":"b.png"}},{"type":"tool_use","id":"c","name":"Read","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"a","content":[`+png+`]},
			{"type":"tool_result","tool_use_id":"b","content":[{"type":"text","text":"b.png, 2 images"},{"type":"image","source":{"type":"url","url":"https://example.org/b.png"}},`+png+`],"is_error":true},
			{"type":"tool_result","tool_use_id":"c","content":"plain"},
			{"type":"text","text":"what do you see?"}]}]}`)
	want := []ir.Part{
		{Kind: ir.ToolResult, ToolID: "a", Text: "[image]"},
		{Kind: ir.ToolResult, ToolID: "b", Text: "b.png, 2 images", IsError: true},
		{Kind: ir.ToolResult, ToolID: "c", Text: "plain"},
		text("Image returned by tool call a:"), {Kind: ir.Image, MediaType: "image/png", Data: "QUJD"},
		text("Image returned by tool call b:"), {Kind: ir.Image, Data: "https://example.org/b.png"}, {Kind: ir.Image, MediaType: "image/png", Data: "QUJD"},
		text("what do you see?"),
	}
	if !reflect.DeepEqual(req.Messages[2].Parts, want) {
		t.Fatalf("parts\n got: %+v\nwant: %+v", req.Messages[2].Parts, want)
	}
	if d := ir.Dropped(req.Dropped); !reflect.DeepEqual(d, []string{"cache_control"}) {
		t.Fatalf("dropped %v", d)
	}
	body, _, err := chat.EncodeRequest(req, "gpt-x")
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if err := chat.CheckRequest(body); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	// The tool messages first, then ONE user message with the images in order.
	assertJSONEqual(t, body, `{"model":"gpt-x","max_tokens":64,"messages":[
		{"role":"user","content":"look at both"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"a","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"a.png\"}"}},
			{"id":"b","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"b.png\"}"}},
			{"id":"c","type":"function","function":{"name":"Read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"a","content":"[image]"},
		{"role":"tool","tool_call_id":"b","content":"Error: b.png, 2 images"},
		{"role":"tool","tool_call_id":"c","content":"plain"},
		{"role":"user","content":[
			{"type":"text","text":"Image returned by tool call a:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
			{"type":"text","text":"Image returned by tool call b:"},{"type":"image_url","image_url":{"url":"https://example.org/b.png"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
			{"type":"text","text":"what do you see?"}]}]}`)
	// Over the limit of parts in one message it is a limit error, not a longer message.
	many := strings.TrimSuffix(strings.Repeat(png+",", ir.MaxParts), ",")
	over := `{"model":"m","max_tokens":5,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[` + many + `]}]}]}`
	if bad := refused(t, over, "messages[1].content"); !bad.Limit {
		t.Fatalf("%+v", bad)
	}
}

func TestDecodeRequest_ToolUseAndToolResultArePaired(t *testing.T) {
	// Claude Code sends its history again with every request, and a strict Chat Completions
	// server answers 400 to a tool call without its result or a result without its call: such a
	// history is repaired and what was repaired is reported.
	use := func(ids ...string) string {
		var b []string
		for _, id := range ids {
			b = append(b, `{"type":"tool_use","id":"`+id+`","name":"f","input":{}}`)
		}
		return `{"role":"assistant","content":[` + strings.Join(b, ",") + `]}`
	}
	res := func(ids ...string) string {
		var b []string
		for _, id := range ids {
			b = append(b, `{"type":"tool_result","tool_use_id":"`+id+`","content":"r-`+id+`"}`)
		}
		return `{"role":"user","content":[` + strings.Join(b, ",") + `]}`
	}
	const hi, said = `{"role":"user","content":"hi"}`, `{"role":"assistant","content":"ok"}`
	for name, c := range map[string]struct {
		messages []string
		dropped  []string
		shape    string // the Chat messages: role, call ids, and for a tool message its content
	}{
		"paired, results in another order than the calls": {
			[]string{hi, use("a", "b"), res("b", "a"), said}, nil,
			"user | assistant a b | tool a r-a | tool b r-b | assistant"},
		"an unanswered tool_use": {
			[]string{hi, use("a"), `{"role":"user","content":"never mind"}`, said}, []string{"input:tool_use.unanswered"},
			"user | assistant a | tool a [no output] | user | assistant"},
		"a turn that is answered in part": {
			[]string{hi, use("a", "b", "c"), `{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"r-b"},{"type":"text","text":"go on"}]}`}, []string{"input:tool_use.unanswered"},
			"user | assistant a b c | tool a [no output] | tool b r-b | tool c [no output] | user"},
		"a trailing tool_use: the conversation ends with the assistant's call": {
			[]string{hi, use("a")}, []string{"input:tool_use.unanswered"},
			"user | assistant a | tool a [no output]"},
		"an unanswered tool_use followed by the assistant": {
			[]string{hi, use("a"), said}, []string{"input:tool_use.unanswered"},
			"user | assistant a | tool a [no output] | assistant"},
		"an orphan tool_result in the first message": {
			[]string{`{"role":"user","content":[{"type":"tool_result","tool_use_id":"gone","content":"x"},{"type":"text","text":"hi"}]}`, said}, []string{"input:tool_result.orphan"},
			"user | assistant"},
		"an orphan tool_result between two assistant messages": {
			[]string{hi, said, res("gone"), said, hi}, []string{"input:tool_result.orphan"},
			"user | assistant | user"},
		"a result answered twice": {
			[]string{hi, use("a"), res("a"), said, res("a"), hi}, []string{"input:tool_result.orphan"},
			"user | assistant a | tool a r-a | assistant | user"},
		"a result that comes a turn late is moved up to its call": {
			[]string{hi, use("a"), `{"role":"user","content":"wait"}`, said, res("a"), said}, nil,
			"user | assistant a | tool a r-a | user | assistant"},
		"a result that stands before its call": {
			[]string{hi, said, res("a"), use("a"), hi}, nil,
			"user | assistant a | tool a r-a | user"},
		"a tool_use id that got its early result already": {
			[]string{res("a"), hi, use("a", "a"), `{"role":"user","content":"x"}`, use("a"), res("a")}, []string{"input:tool_use.duplicate"},
			"user | assistant a | tool a r-a | user | assistant a | tool a r-a"},
		"a tool_use id that waits already": {
			[]string{hi, use("a", "a"), `{"role":"user","content":"x"}`, use("a", "b"), res("a", "b")}, []string{"input:tool_use.duplicate"},
			"user | assistant a | tool a r-a | user | assistant b | tool b r-b"},
	} {
		req, err := DecodeRequest([]byte(`{"model":"m","max_tokens":5,"messages":[` + strings.Join(c.messages, ",") + `]}`))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if d := ir.Dropped(req.Dropped); !reflect.DeepEqual(d, c.dropped) {
			t.Errorf("%s: dropped %v, want %v", name, d, c.dropped)
		}
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
				Content   any
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
			if m.Role == "tool" {
				s += " " + m.CallID + " " + fmt.Sprint(m.Content)
			}
			shape = append(shape, s)
		}
		if got := strings.Join(shape, " | "); got != c.shape {
			t.Errorf("%s: chat messages: %s\nwant: %s", name, got, c.shape)
		}
	}
	// Nothing is left to send.
	refused(t, `{"model":"m","max_tokens":5,"messages":[`+res("gone")+`]}`, "messages")

	// The images of a tool result go where the result goes. An orphan is left out with its image:
	// the report says so, and nothing of it is sent.
	const png = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`
	imageResult := func(id string) string {
		return `{"type":"tool_result","tool_use_id":"` + id + `","content":[{"type":"text","text":"r-` + id + `"},` + png + `]}`
	}
	req, err := DecodeRequest([]byte(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[` + imageResult("gone") + `,{"type":"text","text":"hi"}]},` + said + `]}`))
	if err != nil || !reflect.DeepEqual(ir.Dropped(req.Dropped), []string{"input:tool_result.orphan"}) {
		t.Fatalf("%v, dropped %v", err, req.Dropped)
	}
	body, _, err := chat.EncodeRequest(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`)
	// A result that stands before its call moves to its call, and its image with it.
	req, err = DecodeRequest([]byte(`{"model":"m","max_tokens":5,"messages":[` + hi + `,` + said + `,{"role":"user","content":[` + imageResult("a") + `,{"type":"text","text":"early"}]},` + use("a", "b") + `,{"role":"user","content":[` + imageResult("b") + `,{"type":"text","text":"next"}]}]}`))
	if err != nil || req.Dropped != nil {
		t.Fatalf("%v, dropped %v", err, req.Dropped)
	}
	body, _, err = chat.EncodeRequest(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := chat.CheckRequest(body); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	assertJSONEqual(t, body, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"early"},
	 {"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"f","arguments":"{}"}}]},
	 {"role":"tool","tool_call_id":"a","content":"r-a"},{"role":"tool","tool_call_id":"b","content":"r-b"},
	 {"role":"user","content":[{"type":"text","text":"Image returned by tool call a:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
	  {"type":"text","text":"Image returned by tool call b:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},{"type":"text","text":"next"}]}]}`)
}

func TestStreamEncoder_ToolUseWithoutArgumentsBeforeAFailure(t *testing.T) {
	// A tool call that was cut before its first argument byte: the block is closed (with the "{}"
	// every block gets) and the error event follows. That is right for Anthropic: no message_stop
	// came, and a client discards the turn of a stream that ended with an error.
	s, raw := mustStream(t, []ir.Event{startEv(), toolStart(0, "toolu_1", "f"), ev(ir.PartStop, 0), {Kind: ir.Error, Err: errEarlyEnd}}, true)
	if got := eventNames(raw); got != "message_start content_block_start content_block_delta content_block_stop error" {
		t.Fatalf("events: %s", got)
	}
	if s.Stopped || s.StopReason != "" || s.ErrType != "api_error" || s.ErrMessage != errEarlyEnd {
		t.Fatalf("%+v", s)
	}
}

func TestDecodeRequest_ThroughChat_ToolResultsKeepTheirOrder(t *testing.T) {
	// Review Focus 3: several tool results in one user turn become the right Chat "tool"
	// messages, in order, and the text next to them is not lost.
	req := decode(t, `{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{"n":1}},{"type":"tool_use","id":"b","name":"f","input":{"n":2}},{"type":"tool_use","id":"c","name":"g"}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"a","content":"one"},
			{"type":"text","text":"note"},
			{"type":"tool_result","tool_use_id":"b","content":[{"type":"text","text":"two"}],"is_error":true},
			{"type":"tool_result","tool_use_id":"c"}]}]}`)
	body, dropped, err := chat.EncodeRequest(req, "gpt-x")
	if err != nil || dropped != nil {
		t.Fatalf("EncodeRequest: %v, dropped %v", err, dropped)
	}
	assertJSONEqual(t, body, `{"model":"gpt-x","max_tokens":64,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"a","type":"function","function":{"name":"f","arguments":"{\"n\":1}"}},
			{"id":"b","type":"function","function":{"name":"f","arguments":"{\"n\":2}"}},
			{"id":"c","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"a","content":"one"},
		{"role":"tool","tool_call_id":"b","content":"Error: two"},
		{"role":"tool","tool_call_id":"c","content":""},
		{"role":"user","content":"note"}]}`)
}

// ---------------------------------------------------------------- response

func TestEncodeResponse(t *testing.T) {
	input := `{ "b":1.0, "a":[12345678901234567890, "<&>"] }`
	for name, c := range map[string]struct {
		resp ir.Response
		want string
	}{
		"text": {
			ir.Response{ID: "chatcmpl-1", Model: "m-chat", Parts: []ir.Part{text("Hello there.")}, Stop: ir.StopEnd, Usage: ir.Usage{InputTokens: 12, OutputTokens: 3}},
			`{"id":"msg_chatcmpl-1","type":"message","role":"assistant","model":"m-chat","content":[{"type":"text","text":"Hello there."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":3}}`,
		},
		"two tool calls": {
			ir.Response{ID: "x", Model: "m", Parts: []ir.Part{
				{Kind: ir.ToolUse, ToolID: "call_a", ToolName: "read_file", Input: json.RawMessage(input)},
				{Kind: ir.ToolUse, ToolID: "call_b", ToolName: "list_dir", Input: json.RawMessage(`{}`)},
			}, Stop: ir.StopToolUse, Usage: ir.Usage{InputTokens: 40, OutputTokens: 18}},
			`{"id":"msg_x","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"call_a","name":"read_file","input":` + input + `},{"type":"tool_use","id":"call_b","name":"list_dir","input":{}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":40,"output_tokens":18}}`,
		},
		"thinking and text, no id, no model": {
			ir.Response{Parts: []ir.Part{{Kind: ir.Thinking, Text: "hm"}, text("so")}, Stop: ir.StopMaxTokens},
			`{"id":"msg_burrow","type":"message","role":"assistant","model":"asked-for","content":[{"type":"thinking","thinking":"hm","signature":""},{"type":"text","text":"so"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		"nothing said, an id that is one already": {
			ir.Response{ID: "msg_01", Model: "m", Stop: ir.StopUnknown},
			`{"id":"msg_01","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
	} {
		got, err := EncodeResponse(c.resp, "asked-for")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, c.want)
		}
	}
	for stop, want := range map[ir.StopReason]string{
		ir.StopEnd: "end_turn", ir.StopMaxTokens: "max_tokens", ir.StopToolUse: "tool_use", ir.StopSequence: "stop_sequence",
		ir.StopRefusal: "refusal", ir.StopUnknown: "end_turn", "something new": "end_turn",
	} {
		got, err := EncodeResponse(ir.Response{Stop: stop}, "m")
		if err != nil || !strings.Contains(string(got), `"stop_reason":"`+want+`"`) {
			t.Errorf("stop %q: %v, %s", stop, err, got)
		}
	}
	// What a client could not act on is refused, and the error holds nothing of the answer.
	for name, resp := range map[string]ir.Response{
		"input that is no object":      {Parts: []ir.Part{{Kind: ir.ToolUse, ToolID: "a", ToolName: "f", Input: json.RawMessage(`["SECRET"]`)}}},
		"no id":                        {Parts: []ir.Part{{Kind: ir.ToolUse, ToolName: "f", Input: json.RawMessage(`{}`)}}},
		"no name":                      {Parts: []ir.Part{{Kind: ir.ToolUse, ToolID: "a", Input: json.RawMessage(`{}`)}}},
		"a part an answer cannot hold": {Parts: []ir.Part{{Kind: ir.ToolResult, ToolID: "a", Text: "SECRET"}}},
	} {
		got, err := EncodeResponse(resp, "m")
		if err == nil || got != nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: %s, %v", name, got, err)
		}
	}
}

func TestEncodeError(t *testing.T) {
	for status, want := range map[int]string{
		400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error",
		413: "request_too_large", 422: "invalid_request_error", 429: "rate_limit_error", 500: "api_error", 502: "api_error",
		503: "overloaded_error", 504: "api_error", 529: "overloaded_error", 418: "invalid_request_error",
	} {
		if got := ErrorType(status); got != want {
			t.Errorf("ErrorType(%d) = %q, want %q", status, got, want)
		}
	}
	got := EncodeError(429, "upstream_error", "slow \"down\"\n<now>")
	if string(got) != `{"type":"error","error":{"type":"rate_limit_error","message":"slow \"down\"\n<now>"},"burrow_code":"upstream_error"}` {
		t.Fatalf("body = %s", got)
	}
}

// ---------------------------------------------------------------- stream

func ev(kind ir.EventKind, index int) ir.Event { return ir.Event{Kind: kind, Index: index} }
func startEv() ir.Event                        { return ir.Event{Kind: ir.Start, ID: "chatcmpl-9", Model: "m-up"} }
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

// frozen is a clock that stands still.
func frozen() time.Time { return time.Unix(1_700_000_000, 0) }

// encodeEvents writes events through an encoder and returns the bytes and the first error.
func encodeEvents(events []ir.Event, closeIt bool) ([]byte, error) {
	var buf bytes.Buffer
	e := NewStreamEncoder(&buf, "asked-for")
	e.now = frozen // no pings: the tests that want them move the clock themselves
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
		t.Fatalf("not a well-formed Messages stream: %v\n%s", err, raw)
	}
	return s, raw
}

// eventNames lists the event of every frame.
func eventNames(raw []byte) string {
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, name)
		}
	}
	return strings.Join(names, " ")
}

func TestStreamEncoder_Text(t *testing.T) {
	_, raw := mustStream(t, []ir.Event{startEv(), textStart(0), textDelta(0, "Hel"), textDelta(0, "lo.\n\"x\" <é>"), ev(ir.PartStop, 0), finishEv(ir.StopEnd, 12, 2)}, true)
	want := `event: message_start
data: {"type":"message_start","message":{"id":"msg_chatcmpl-9","type":"message","role":"assistant","model":"m-up","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo.\n\"x\" <é>"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	if string(raw) != want {
		t.Fatalf("stream\n got %s\nwant %s", raw, want)
	}
}

func TestStreamEncoder_StartFallbacksAndUsage(t *testing.T) {
	// No id, no model: made up and the model that was asked for. Input tokens a format tells
	// early stand in message_start, and stay when the finish has none.
	s, _ := mustStream(t, []ir.Event{{Kind: ir.Start, Usage: ir.Usage{InputTokens: 7}}, finishEv(ir.StopEnd, 0, 5)}, false)
	if s.ID != "msg_burrow" || s.Model != "asked-for" || s.StartInputTokens != 7 || s.InputTokens != 7 || s.OutputTokens != 5 || !s.Stopped {
		t.Fatalf("stream = %+v", s)
	}
	// Chat reports usage at the end only: 0 at the start, the real figures in message_delta.
	s, _ = mustStream(t, []ir.Event{startEv(), finishEv(ir.StopEnd, 50, 21)}, false)
	if s.StartInputTokens != 0 || s.InputTokens != 50 || s.OutputTokens != 21 {
		t.Fatalf("stream = %+v", s)
	}
	for stop, want := range map[ir.StopReason]string{
		ir.StopEnd: "end_turn", ir.StopMaxTokens: "max_tokens", ir.StopSequence: "stop_sequence",
		ir.StopRefusal: "refusal", ir.StopUnknown: "end_turn", "new": "end_turn",
	} {
		if s, _ := mustStream(t, []ir.Event{startEv(), finishEv(stop, 1, 1)}, false); s.StopReason != want {
			t.Errorf("stop %q → %q, want %q", stop, s.StopReason, want)
		}
	}
	s, _ = mustStream(t, []ir.Event{startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), finishEv(ir.StopToolUse, 1, 1)}, false)
	if s.StopReason != "tool_use" {
		t.Fatalf("stop = %q", s.StopReason)
	}
}

func TestStreamEncoder_Thinking(t *testing.T) {
	s, raw := mustStream(t, []ir.Event{startEv(),
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Thinking}}, {Kind: ir.ThinkingDelta, Index: 0, Text: "hm"}, ev(ir.PartStop, 0),
		textStart(1), textDelta(1, "so"), ev(ir.PartStop, 1), finishEv(ir.StopEnd, 1, 1)}, false)
	if len(s.Blocks) != 2 || s.Blocks[0].Type != "thinking" || s.Blocks[0].Text != "hm" || s.Blocks[1].Text != "so" {
		t.Fatalf("blocks = %+v", s.Blocks)
	}
	if !strings.Contains(string(raw), `"content_block":{"type":"thinking","thinking":"","signature":""}`) ||
		!strings.Contains(string(raw), `"delta":{"type":"thinking_delta","thinking":"hm"}`) {
		t.Fatalf("stream = %s", raw)
	}
}

// interleaved is what the Chat decoder emits for chat/testdata/stream_tools.sse.
func interleaved() []ir.Event {
	return []ir.Event{
		{Kind: ir.Start, ID: "chatcmpl-4", Model: "m-chat"},
		textStart(0), textDelta(0, "Let me look."), ev(ir.PartStop, 0),
		toolStart(1, "call_a", "read_file"),
		argsDelta(1, `{"pa`),
		toolStart(2, "call_b", "list_dir"), argsDelta(2, `{"dir":`),
		argsDelta(1, `th":"a.txt"}`),
		argsDelta(2, `"."}`),
		ev(ir.PartStop, 1), ev(ir.PartStop, 2),
		finishEv(ir.StopToolUse, 50, 21),
	}
}

func TestStreamEncoder_InterleavedToolCallsAreWrittenOneAfterTheOther(t *testing.T) {
	s, raw := mustStream(t, interleaved(), true)
	if got := eventNames(raw); got != "message_start content_block_start content_block_delta content_block_stop "+
		"content_block_start content_block_delta content_block_delta content_block_stop "+
		"content_block_start content_block_delta content_block_delta content_block_stop message_delta message_stop" {
		t.Fatalf("events = %s", got)
	}
	want := []StreamBlock{
		{Type: "text", Text: "Let me look.", Deltas: []string{"Let me look."}},
		{Type: "tool_use", ToolID: "call_a", ToolName: "read_file", PartialJSON: `{"path":"a.txt"}`, Input: `{"path":"a.txt"}`, Deltas: []string{`{"pa`, `th":"a.txt"}`}},
		{Type: "tool_use", ToolID: "call_b", ToolName: "list_dir", PartialJSON: `{"dir":"."}`, Input: `{"dir":"."}`, Deltas: []string{`{"dir":`, `"."}`}},
	}
	if !reflect.DeepEqual(s.Blocks, want) {
		t.Fatalf("blocks\n got %+v\nwant %+v", s.Blocks, want)
	}
	if s.StopReason != "tool_use" || s.InputTokens != 50 || s.OutputTokens != 21 || !s.Stopped || s.ErrType != "" {
		t.Fatalf("stream = %+v", s)
	}
}

func TestStreamEncoder_LiveBlockIsNotHeldBack(t *testing.T) {
	// The block that is open on the wire gets its deltas at once: nothing waits for the finish.
	var buf bytes.Buffer
	e := NewStreamEncoder(&buf, "m")
	e.now = frozen
	step := func(ev ir.Event, wantNames string) {
		t.Helper()
		before := buf.Len()
		if err := e.Write(ev); err != nil {
			t.Fatal(err)
		}
		if got := eventNames(buf.Bytes()[before:]); got != wantNames {
			t.Fatalf("after %s[%d]: wrote %q, want %q", ev.Kind, ev.Index, got, wantNames)
		}
	}
	step(startEv(), "message_start")
	step(toolStart(0, "a", "f"), "content_block_start")
	step(argsDelta(0, `{"x":`), "content_block_delta")
	step(toolStart(1, "b", "g"), "")  // held: block 0 is open
	step(argsDelta(1, `{"y":2}`), "") // held
	step(textStart(2), "")            // text opening while tool calls are open: held
	step(textDelta(2, "note"), "")    // held
	step(ev(ir.PartStop, 2), "")      // held, stopped
	step(argsDelta(0, `1}`), "content_block_delta")
	step(toolStart(3, "c", "h"), "")                                                       // held
	step(ev(ir.PartStop, 0), "content_block_stop content_block_start content_block_delta") // 1 is replayed and is now live
	step(argsDelta(1, ` `), "content_block_delta")
	step(ev(ir.PartStop, 1), "content_block_stop content_block_start content_block_delta content_block_stop content_block_start")
	step(argsDelta(3, `{}`), "content_block_delta")
	step(ev(ir.PartStop, 3), "content_block_stop")
	step(finishEv(ir.StopToolUse, 3, 4), "message_delta message_stop")
	s, err := CheckStream(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range s.Blocks {
		got = append(got, b.Type+":"+b.ToolID+":"+b.Text+b.PartialJSON)
	}
	if want := []string{`tool_use:a:{"x":1}`, `tool_use:b:{"y":2} `, `text::note`, `tool_use:c:{}`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks = %q", got)
	}
}

func TestStreamEncoder_ToolCallWithoutArguments(t *testing.T) {
	// Review Focus 1. The block starts with "input":{} and gets ONE input_json_delta "{}":
	// a client that parses the joined partial_json gets an object, and the SDK accumulators
	// (input stays the start's {} unless partial_json was non-empty, then it is parsed) get {} too.
	for name, events := range map[string][]ir.Event{
		"live": {startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), finishEv(ir.StopToolUse, 1, 1)},
		"held": {startEv(), toolStart(0, "a", "f"), argsDelta(0, `{"k":1}`), toolStart(1, "b", "g"), ev(ir.PartStop, 0), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1)},
	} {
		s, raw := mustStream(t, events, true)
		last := s.Blocks[len(s.Blocks)-1]
		if !reflect.DeepEqual(last.Deltas, []string{"{}"}) || last.PartialJSON != "{}" || last.Input != "{}" {
			t.Errorf("%s: block = %+v\n%s", name, last, raw)
		}
		if !strings.Contains(string(raw), `"input":{}}}`) {
			t.Errorf("%s: the start has no empty input\n%s", name, raw)
		}
	}
}

func TestStreamEncoder_BadEndings(t *testing.T) {
	// Review Focus 2: every open block is stopped, then one error event; no message_delta, no
	// message_stop, nothing after it.
	open := []ir.Event{startEv(), textStart(0), textDelta(0, "par")}
	tools := interleaved()[:9] // both tool calls open, the second one held back
	cases := map[string]struct {
		events  []ir.Event
		closeIt bool
		names   string
		message string
	}{
		"Close while a block is open": {open, true,
			"message_start content_block_start content_block_delta content_block_stop error", "the provider ended the stream early"},
		"the decoder's failure: stops, then Error": {append(append([]ir.Event{}, open...), ev(ir.PartStop, 0), ir.Event{Kind: ir.Error, Err: "upstream said no"}), true,
			"message_start content_block_start content_block_delta content_block_stop error", "upstream said no"},
		"an Error with a block still open": {append(append([]ir.Event{}, open...), ir.Event{Kind: ir.Error, Err: "boom"}), false,
			"message_start content_block_start content_block_delta content_block_stop error", "boom"},
		"Close with a part held back: it is written, then the error": {tools, true,
			"message_start content_block_start content_block_delta content_block_stop " +
				"content_block_start content_block_delta content_block_delta content_block_stop " +
				"content_block_start content_block_delta content_block_stop error", "the provider ended the stream early"},
		"the decoder's failure with a part held back": {append(append([]ir.Event{}, tools...), ev(ir.PartStop, 1), ev(ir.PartStop, 2), ir.Event{Kind: ir.Error, Err: "the provider's answer is too large"}), true,
			"message_start content_block_start content_block_delta content_block_stop " +
				"content_block_start content_block_delta content_block_delta content_block_stop " +
				"content_block_start content_block_delta content_block_stop error", "the provider's answer is too large"},
		"Close right after the start": {[]ir.Event{startEv()}, true, "message_start error", "the provider ended the stream early"},
		"Close before anything":       {nil, true, "error", "the provider ended the stream early"},
		"an Error before the start":   {[]ir.Event{{Kind: ir.Error, Err: "no"}}, true, "error", "no"},
		"an Error without a message":  {[]ir.Event{startEv(), {Kind: ir.Error}}, true, "message_start error", "the provider reported an error"},
		"events after the Error are ignored": {[]ir.Event{startEv(), {Kind: ir.Error, Err: "no"}, textStart(0), textDelta(0, "x"), ev(ir.PartStop, 0), finishEv(ir.StopEnd, 1, 1)}, true,
			"message_start error", "no"},
	}
	for name, c := range cases {
		s, raw := mustStream(t, c.events, c.closeIt)
		if got := eventNames(raw); got != c.names {
			t.Errorf("%s: events = %s\nwant     %s", name, got, c.names)
		}
		if s.ErrType != "api_error" || s.ErrMessage != c.message || s.Stopped || s.StopReason != "" {
			t.Errorf("%s: stream = %+v", name, s)
		}
		if !bytes.HasSuffix(raw, []byte(`data: {"type":"error","error":{"type":"api_error","message":"`+c.message+`"}}`+"\n\n")) {
			t.Errorf("%s: the error is not the last frame\n%s", name, raw)
		}
	}
}

func TestStreamEncoder_AfterTheFinishNothingIsWritten(t *testing.T) {
	events := []ir.Event{startEv(), textStart(0), textDelta(0, "x"), ev(ir.PartStop, 0), finishEv(ir.StopEnd, 1, 1)}
	want, _ := encodeEvents(events, false)
	got, err := encodeEvents(append(events, textStart(1), textDelta(1, "y"), ir.Event{Kind: ir.Error, Err: "late"}, finishEv(ir.StopEnd, 9, 9), startEv()), true)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("err %v\n got %s\nwant %s", err, got, want)
	}
}

func TestStreamEncoder_MalformedSequencesEndInAnError(t *testing.T) {
	// The decoders keep the rules of ir.Event; an encoder that is handed something else still
	// leaves a well-formed stream that does not report success.
	for name, events := range map[string][]ir.Event{
		"a delta before the start":               {textDelta(0, "x")},
		"two starts":                             {startEv(), startEv()},
		"a part number out of order":             {startEv(), textStart(1)},
		"a delta for a part that is not open":    {startEv(), textDelta(0, "x")},
		"a text delta for a tool call":           {startEv(), toolStart(0, "a", "f"), textDelta(0, "x")},
		"an argument delta for text":             {startEv(), textStart(0), argsDelta(0, "{}")},
		"a stop for a part that is not open":     {startEv(), textStart(0), ev(ir.PartStop, 0), ev(ir.PartStop, 0)},
		"a negative index":                       {startEv(), textStart(0), textDelta(-1, "x")},
		"a tool call without a name":             {startEv(), toolStart(0, "a", "")},
		"a tool call without an id":              {startEv(), toolStart(0, "", "f")},
		"a part of a kind an answer cannot hold": {startEv(), {Kind: ir.PartStart, Part: ir.Part{Kind: ir.Image}}},
		"the finish while a part is open":        {startEv(), textStart(0), finishEv(ir.StopEnd, 1, 1)},
		"the finish while a part is held":        {startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), ev(ir.PartStop, 0), finishEv(ir.StopToolUse, 1, 1)},
		"the finish before the start":            {finishEv(ir.StopEnd, 1, 1)},
		"an unknown kind":                        {startEv(), {Kind: "new"}},
	} {
		raw, err := encodeEvents(append(events, startEv(), finishEv(ir.StopEnd, 1, 1)), true)
		if !errors.Is(err, ir.ErrSequence) {
			t.Errorf("%s: err = %v", name, err)
		}
		s, cerr := CheckStream(raw)
		if cerr != nil || s.ErrType != "api_error" || s.Stopped {
			t.Errorf("%s: %v, %+v\n%s", name, cerr, s, raw)
		}
	}
}

func TestStreamEncoder_HoldBackIsBounded(t *testing.T) {
	// Review Focus 5. What is held back counts against the ir limits; over them the stream
	// ends with an error instead of growing.
	piece := strings.Repeat("x", 64<<10)
	run := func(events func(write func(ir.Event) error) error) (Stream, error) {
		var buf bytes.Buffer
		e := NewStreamEncoder(&buf, "m")
		err := events(e.Write)
		if cerr := e.Close(); cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
		s, cerr := CheckStream(buf.Bytes())
		if cerr != nil {
			t.Fatalf("not well-formed: %v", cerr)
		}
		if buf.Len() > ir.MaxTotalToolArgsBytes+(1<<20) {
			t.Fatalf("%d bytes were written", buf.Len())
		}
		return s, err
	}
	// Text held behind an open tool call.
	s, err := run(func(write func(ir.Event) error) error {
		for _, ev := range []ir.Event{startEv(), toolStart(0, "a", "f"), textStart(1)} {
			if err := write(ev); err != nil {
				return err
			}
		}
		for i := 0; i < 2*ir.MaxTotalToolArgsBytes/len(piece); i++ {
			if err := write(textDelta(1, piece)); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ir.ErrLimit) || s.ErrMessage != "the provider's answer is too large" || s.Stopped {
		t.Fatalf("held text: err = %v, stream ends %q", err, s.ErrMessage)
	}
	// One tool call's arguments over the limit, live.
	s, err = run(func(write func(ir.Event) error) error {
		_ = write(startEv())
		_ = write(toolStart(0, "a", "f"))
		for i := 0; i <= ir.MaxToolArgsBytes/len(piece); i++ {
			if err := write(argsDelta(0, piece)); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ir.ErrLimit) || s.ErrType == "" {
		t.Fatalf("live arguments: err = %v", err)
	}
	// Too many tool calls, too many parts.
	s, err = run(func(write func(ir.Event) error) error {
		_ = write(startEv())
		for i := 0; i <= ir.MaxToolCalls; i++ {
			if err := write(toolStart(i, fmt.Sprint("id", i), "f")); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ir.ErrLimit) || s.ErrType == "" || len(s.Blocks) != ir.MaxToolCalls {
		t.Fatalf("tool calls: err = %v, %d blocks", err, len(s.Blocks))
	}
	s, err = run(func(write func(ir.Event) error) error {
		_ = write(startEv())
		for i := 0; i <= ir.MaxParts; i++ {
			if err := write(textStart(i)); err != nil {
				return err
			}
			if err := write(ev(ir.PartStop, i)); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ir.ErrLimit) || s.ErrType == "" || len(s.Blocks) != ir.MaxParts {
		t.Fatalf("parts: err = %v, %d blocks", err, len(s.Blocks))
	}
	// Many small held pieces are joined instead of kept one by one: the bytes stay the same.
	var want strings.Builder
	events := []ir.Event{startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), argsDelta(1, `{"k":"`)}
	want.WriteString(`{"k":"`)
	for i := 0; i < 5000; i++ {
		events = append(events, argsDelta(1, fmt.Sprint(i%10)))
		want.WriteString(fmt.Sprint(i % 10))
	}
	events = append(events, argsDelta(1, `"}`), ev(ir.PartStop, 0), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1))
	want.WriteString(`"}`)
	st, _ := mustStream(t, events, true)
	if st.Blocks[1].PartialJSON != want.String() || len(st.Blocks[1].Deltas) > 1000 {
		t.Fatalf("held pieces: %d deltas, bytes equal %v", len(st.Blocks[1].Deltas), st.Blocks[1].PartialJSON == want.String())
	}
}

func TestStreamEncoder_PingsWhilePartsAreHeldBack(t *testing.T) {
	// A part that waits for the wire gives the caller nothing; a ping at most once per second
	// on which events arrive shows the stream is alive.
	var buf bytes.Buffer
	clock := frozen()
	e := NewStreamEncoder(&buf, "m")
	e.now = func() time.Time { return clock }
	write := func(ev ir.Event, advance time.Duration, wantNames string) {
		t.Helper()
		clock = clock.Add(advance)
		before := buf.Len()
		if err := e.Write(ev); err != nil {
			t.Fatal(err)
		}
		if got := eventNames(buf.Bytes()[before:]); got != wantNames {
			t.Fatalf("after %s[%d] at +%v: wrote %q, want %q", ev.Kind, ev.Index, advance, got, wantNames)
		}
	}
	write(startEv(), 0, "message_start")
	write(toolStart(0, "a", "f"), 5*time.Second, "content_block_start") // nothing held: no ping, however long it took
	write(argsDelta(0, `{}`), 5*time.Second, "content_block_delta")
	write(toolStart(1, "b", "g"), 0, "")
	write(argsDelta(1, `{"k":"`), 999*time.Millisecond, "")
	write(argsDelta(1, `v`), time.Millisecond, "ping") // a second without a byte
	write(argsDelta(1, `v`), 500*time.Millisecond, "")
	write(argsDelta(1, `v`), 400*time.Millisecond, "")
	write(argsDelta(1, `v`), 100*time.Millisecond, "ping")
	write(argsDelta(0, ` `), 900*time.Millisecond, "content_block_delta") // a live delta is a sign of life
	write(argsDelta(1, `v`), 900*time.Millisecond, "")
	write(argsDelta(1, `"}`), 3*time.Second, "ping") // one, not three
	write(ev(ir.PartStop, 1), 2*time.Second, "ping")
	write(ev(ir.PartStop, 0), 5*time.Second, "content_block_stop content_block_start content_block_delta content_block_delta content_block_delta content_block_delta content_block_delta content_block_delta content_block_delta content_block_stop")
	write(finishEv(ir.StopToolUse, 1, 1), 5*time.Second, "message_delta message_stop")
	if !strings.Contains(buf.String(), "event: ping\ndata: {\"type\":\"ping\"}\n\n") {
		t.Fatalf("no ping frame:\n%s", buf.String())
	}
	s, err := CheckStream(buf.Bytes())
	if err != nil || s.Blocks[1].PartialJSON != `{"k":"vvvvv"}` || s.Blocks[0].PartialJSON != `{} ` {
		t.Fatalf("%v, %+v", err, s.Blocks)
	}
	// Nothing after the end, and no ping before the start.
	clock = clock.Add(time.Hour)
	before := buf.Len()
	_ = e.Write(textDelta(0, "x"))
	if buf.Len() != before {
		t.Fatal("bytes after the end")
	}
}

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("client went away")
	}
	w.after--
	return len(p), nil
}

func TestStreamEncoder_WriteErrorIsReturnedAndSticks(t *testing.T) {
	w := &failingWriter{after: 2}
	e := NewStreamEncoder(w, "m")
	var errs []error
	for _, ev := range []ir.Event{startEv(), textStart(0), textDelta(0, "x"), textDelta(0, "y"), ev(ir.PartStop, 0), finishEv(ir.StopEnd, 1, 1)} {
		errs = append(errs, e.Write(ev))
	}
	if errs[0] != nil || errs[1] != nil || errs[2] == nil || errs[3] == nil || errs[5] == nil || errors.Is(errs[2], ir.ErrSequence) {
		t.Fatalf("errors = %v", errs)
	}
	if err := e.Close(); err == nil {
		t.Fatal("Close after a failed write returned nil")
	}
}

// genPart is one part of a generated answer: what is sent, piece by piece.
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
				events = append(events, ir.Event{Kind: ir.ThinkingDelta, Index: i, Text: piece})
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

// replay sends what a client holds after a stream back as the history of its next request: the
// content blocks of the answer as an assistant turn (a tool_use whose partial JSON no client could
// parse with an empty input), then a tool_result for every tool_use. It also sends the histories
// a client that lost its place sends: results missing, the conversation ending with the
// assistant's calls, results for calls that do not exist or that come first. Whatever the stream
// was, every one of them must go through and be one a strict Chat Completions server takes.
func replay(t testing.TB, s Stream) {
	t.Helper()
	quote := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	var blocks, results []string
	for _, b := range s.Blocks {
		switch b.Type {
		case "tool_use":
			input := b.Input
			if ir.CheckObject([]byte(input)) != nil {
				input = "{}"
			}
			blocks = append(blocks, `{"type":"tool_use","id":`+quote(b.ToolID)+`,"name":`+quote(b.ToolName)+`,"input":`+input+`}`)
			results = append(results, `{"type":"tool_result","tool_use_id":`+quote(b.ToolID)+`,"content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}`)
		case "thinking":
			blocks = append(blocks, `{"type":"thinking","thinking":`+quote(b.Text)+`,"signature":""}`)
		default:
			blocks = append(blocks, `{"type":"text","text":`+quote(b.Text)+`}`)
		}
	}
	const hi, goOn, orphan = `{"role":"user","content":"hi"}`, `{"type":"text","text":"go on"}`, `{"type":"tool_result","tool_use_id":"never-called","content":"x"}`
	user := func(blocks ...string) string { return `{"role":"user","content":[` + strings.Join(blocks, ",") + `]}` }
	assistant := `{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]}`
	histories := map[string][]string{"no answer yet": {hi, user(goOn)}}
	if len(blocks) > 0 {
		histories = map[string][]string{
			"every result":                  {hi, assistant, user(append(append([]string{}, results...), goOn)...)},
			"no result":                     {hi, assistant, user(goOn)},
			"every other result":            {hi, assistant, user(append(everyOther(results), goOn)...)},
			"the assistant's turn last":     {hi, assistant},
			"the assistant twice":           {hi, assistant, assistant, user(append(append([]string{}, results...), goOn)...)},
			"results for calls nobody made": {user(orphan, goOn), assistant, user(append(append([]string{orphan}, results...), orphan, goOn)...)},
			"the results first":             {user(append(append([]string{}, results...), goOn)...), assistant, user(goOn)},
		}
	}
	for name, messages := range histories {
		body := `{"model":"m","max_tokens":9,"messages":[` + strings.Join(messages, ",") + `]}`
		req, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: the history is refused: %v\n%s", name, err, body)
		}
		if name == "every result" && ir.Dropped(req.Dropped) != nil {
			t.Fatalf("%s: the history is repaired: %v\n%s", name, req.Dropped, body)
		}
		out, _, err := chat.EncodeRequest(req, "m")
		if err != nil {
			t.Fatalf("%s: the history cannot be sent: %v\n%s", name, err, body)
		}
		if err := chat.CheckRequest(out); err != nil {
			t.Fatalf("%s: the history is no valid Chat conversation: %v\n%s\n%s", name, err, out, body)
		}
	}
}

func everyOther(in []string) []string {
	out := []string{}
	for i := 1; i < len(in); i += 2 {
		out = append(out, in[i])
	}
	return out
}

func TestProperty_BlocksAreSequentialAndBytesAreKept(t *testing.T) {
	rng := rand.New(rand.NewSource(20261007))
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
		s, err := CheckStream(raw) // sequential blocks, indexes 0,1,2… without gaps: see CheckStream
		if err != nil {
			t.Fatalf("round %d: %v\n%s\n%+v", round, err, raw, events[:n])
		}
		if whole != s.Stopped || whole == (s.ErrType != "") {
			t.Fatalf("round %d: whole %v, stream %+v", round, whole, s)
		}
		replay(t, s)
		// Every part that was started is on the wire, in part order, with exactly the bytes it had got.
		got := map[int]string{}
		started := 0
		for _, ev := range events[:n] {
			switch ev.Kind {
			case ir.PartStart:
				started++
			case ir.TextDelta, ir.ThinkingDelta:
				got[ev.Index] += ev.Text
			case ir.ToolArgsDelta:
				got[ev.Index] += ev.ArgsJSON
			}
		}
		if len(s.Blocks) != started {
			t.Fatalf("round %d: %d blocks for %d parts\n%s", round, len(s.Blocks), started, raw)
		}
		for i, b := range s.Blocks {
			want := got[i]
			switch parts[i].kind {
			case ir.ToolUse:
				if want == "" {
					want = "{}"
				}
				if b.Type != "tool_use" || b.ToolID != fmt.Sprint("call_", i) || b.PartialJSON != want {
					t.Fatalf("round %d block %d: %+v, want arguments %q", round, i, b, want)
				}
				if whole && (b.Input != want || !json.Valid([]byte(b.Input))) {
					t.Fatalf("round %d block %d: input %q, want %q", round, i, b.Input, want)
				}
			default:
				if b.Type != string(parts[i].kind) || b.Text != want {
					t.Fatalf("round %d block %d: %+v, want text %q", round, i, b, want)
				}
			}
		}
	}
}

// ---------------------------------------------------------------- CheckStream itself

func TestCheckStream_RefusesWhatAClientWould(t *testing.T) {
	good, _ := encodeEvents(interleaved(), true)
	if _, err := CheckStream(good); err != nil {
		t.Fatal(err)
	}
	frame := func(event, data string) string { return "event: " + event + "\ndata: " + data + "\n\n" }
	start := frame("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)
	textOpen := frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	toolOpen := func(i int) string {
		return frame("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"t%d","name":"f","input":{}}}`, i, i))
	}
	stop := func(i int) string {
		return frame("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
	}
	args := func(i int, s string) string {
		return frame("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, i, s))
	}
	end := frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`) + frame("message_stop", `{"type":"message_stop"}`)
	fail := frame("error", `{"type":"error","error":{"type":"api_error","message":"x"}}`)
	for name, raw := range map[string]string{
		"nothing":                              "",
		"no end":                               start + textOpen + stop(0),
		"two blocks open at once":              start + toolOpen(0) + toolOpen(1) + stop(0) + stop(1) + end,
		"a gap in the indexes":                 start + toolOpen(1) + stop(1) + end,
		"a delta for a block that is not open": start + toolOpen(0) + stop(0) + args(0, "{}") + end,
		"a delta of the wrong type":            start + textOpen + args(0, "{}") + stop(0) + end,
		"the end while a block is open":        start + textOpen + end,
		"an error while a block is open":       start + textOpen + fail,
		"a frame after message_stop":           start + end + frame("ping", `{"type":"ping"}`),
		"a frame after the error":              start + fail + end,
		"message_stop without message_delta":   start + frame("message_stop", `{"type":"message_stop"}`),
		"arguments that are no object":         start + toolOpen(0) + args(0, `{"a":`) + stop(0) + end,
		"a block before message_start":         textOpen + stop(0) + end,
		"type and event differ":                start + frame("content_block_start", `{"type":"content_block_stop","index":0}`) + end,
		"data over two lines":                  start + "event: ping\ndata: {\"type\":\ndata: \"ping\"}\n\n" + end,
		"no event line":                        start + "data: {\"type\":\"ping\"}\n\n" + end,
		"an unknown event":                     start + frame("surprise", `{"type":"surprise"}`) + end,
		"a stop reason nobody knows":           start + frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop"},"usage":{"output_tokens":1}}`) + frame("message_stop", `{"type":"message_stop"}`),
		"an error without a type":              start + frame("error", `{"type":"error","error":{"message":"x"}}`),
	} {
		if _, err := CheckStream([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Pings are allowed anywhere before the end; broken arguments do not matter once the stream failed.
	ping := frame("ping", `{"type":"ping"}`)
	for name, raw := range map[string]string{
		"pings":                        ping + start + ping + textOpen + ping + stop(0) + ping + end,
		"broken arguments, then error": start + toolOpen(0) + args(0, `{"a":`) + stop(0) + fail,
	} {
		if _, err := CheckStream([]byte(raw)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// ---------------------------------------------------------------- fuzz

func FuzzDecodeRequest(f *testing.F) {
	f.Add(fixture(f, "req_claude_code.json"))
	f.Add([]byte(minimal(`"tools":[{"name":"f","input_schema":{}},{"type":"bash_20250124","name":"bash"}],"tool_choice":{"type":"tool","name":"f","disable_parallel_tool_use":true}`)))
	f.Add([]byte(withContent("user", `{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"a"}],"is_error":true},{"type":"image","source":{"type":"url","url":"u"}}`)))
	f.Add([]byte(withContent("assistant", `{"type":"redacted_thinking","data":"x"},{"type":"server_tool_use","id":"s"},{"type":"tool_use","id":"t","name":"f","input":{"a":[1,{"b":null}]}}`)))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_use"}]}]}`))
	f.Add([]byte(`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"image","source":{"type":"url","url":"u"}}]}]},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f"},{"type":"tool_use","id":"a","name":"f"}]},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f"}]}]}`))
	f.Add([]byte(`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"early"},{"type":"tool_result","tool_use_id":"z"}]},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f"},{"type":"tool_use","id":"a","name":"f"},{"type":"tool_use","id":"b","name":"f"}]},{"role":"assistant","content":"x"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"image","source":{"type":"url","url":"u"}}]}]},{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"f"}]}]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := DecodeRequest(body)
		if err != nil {
			var bad *BadRequestError
			if !errors.As(err, &bad) || bad.Field == "" || bad.Reason == "" {
				t.Fatalf("an error that is no usable *BadRequestError: %#v", err)
			}
			if !reflect.DeepEqual(req, ir.Request{}) {
				t.Fatalf("a request came with the error: %+v", req)
			}
			return
		}
		// What is accepted keeps the limits and the rules of the neutral form.
		if len(req.Messages) == 0 || len(req.Messages) > ir.MaxMessages || len(req.System) > ir.MaxParts || len(req.Tools) > ir.MaxTools || req.MaxTokens <= 0 {
			t.Fatalf("limits: %d messages, %d system parts, %d tools, max_tokens %d", len(req.Messages), len(req.System), len(req.Tools), req.MaxTokens)
		}
		for _, p := range req.System {
			if p.Kind != ir.Text {
				t.Fatalf("system part %+v", p)
			}
		}
		for _, m := range req.Messages {
			if (m.Role != ir.User && m.Role != ir.Assistant) || len(m.Parts) > ir.MaxParts {
				t.Fatalf("message %q with %d parts", m.Role, len(m.Parts))
			}
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.Text:
				case ir.Image:
					if m.Role != ir.User || p.Data == "" {
						t.Fatalf("image %+v in a %s turn", p, m.Role)
					}
				case ir.ToolResult:
					if m.Role != ir.User || p.ToolID == "" || p.Data != "" || p.MediaType != "" {
						t.Fatalf("tool result %+v in a %s turn", p, m.Role)
					}
				case ir.ToolUse:
					if m.Role != ir.Assistant || p.ToolID == "" || p.ToolName == "" || ir.CheckObject(p.Input) != nil || len(p.Input) > ir.MaxToolArgsBytes {
						t.Fatalf("tool call %+v in a %s turn", p, m.Role)
					}
				case ir.Thinking:
					if m.Role != ir.Assistant {
						t.Fatal("thinking in a user turn")
					}
				default:
					t.Fatalf("part of kind %q", p.Kind)
				}
			}
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

func chatFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../chat/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FuzzStreamEncoder drives the encoder with whatever the Chat stream decoder makes of arbitrary
// bytes. Oracle: the bytes written are a well-formed Messages stream (CheckStream) that agrees
// with the events: success exactly when they end in a Finish, and then the same content.
func FuzzStreamEncoder(f *testing.F) {
	f.Add(chatFixture(f, "stream_text.sse"), 7, 1000)
	f.Add(chatFixture(f, "stream_tools.sse"), 64, 1000)
	f.Add(chatFixture(f, "stream_tools.sse"), 3, 9)
	f.Add([]byte("data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\"}},{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"g\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"late text\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"), 5, 1000)
	f.Add([]byte("data: {\"choices\":[{\"delta\":{\"reasoning\":\"r\",\"content\":\"c\",\"refusal\":\"no\"}}]}\n\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n"), 1, 1000)
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
			t.Fatalf("not a well-formed Messages stream: %v\n%s\n%+v", err, out, events)
		}
		replay(t, s)
		finished := len(events) > 0 && events[len(events)-1].Kind == ir.Finish
		if finished != s.Stopped || finished == (s.ErrType != "") {
			t.Fatalf("finished %v, stream stopped %v, error %q\n%+v", finished, s.Stopped, s.ErrType, events)
		}
		if !finished {
			return
		}
		want, err := ir.Collect(events)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if len(s.Blocks) != len(want.Parts) {
			t.Fatalf("%d blocks for %d parts", len(s.Blocks), len(want.Parts))
		}
		for i, p := range want.Parts {
			b := s.Blocks[i]
			switch p.Kind {
			case ir.ToolUse:
				// Review Focus 1: the caller's pieces add up to the upstream's bytes.
				if b.Type != "tool_use" || b.ToolID != p.ToolID || b.ToolName != p.ToolName || b.Input != string(p.Input) {
					t.Fatalf("block %d: %+v, want %+v", i, b, p)
				}
			default:
				if b.Type != string(p.Kind) || b.Text != p.Text {
					t.Fatalf("block %d: %+v, want %+v", i, b, p)
				}
			}
		}
		if s.InputTokens != want.Usage.InputTokens || s.OutputTokens != want.Usage.OutputTokens {
			t.Fatalf("usage %d/%d, want %+v", s.InputTokens, s.OutputTokens, want.Usage)
		}
	})
}

// FuzzStreamEncoderAnyEvents hands the encoder event sequences no decoder would emit. Oracle:
// whatever comes in, what goes out is a well-formed Messages stream that ended, and it reports
// success only when the events were a well-formed answer.
func FuzzStreamEncoderAnyEvents(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2, 0, 3, 0, 5, 0, 6, 0})
	f.Add([]byte{0, 0, 4, 0, 4, 1, 3, 1, 1, 2, 3, 0, 5, 0, 5, 1, 5, 2, 6, 0})
	f.Add([]byte{7, 0})
	f.Fuzz(func(t *testing.T, script []byte) {
		var events []ir.Event
		for i := 0; i+1 < len(script) && len(events) < 4096; i += 2 {
			index := int(script[i+1] % 8)
			piece := strings.Repeat(string(rune('a'+script[i+1]%26)), 1+int(script[i+1]/32))
			switch script[i] % 9 {
			case 0:
				events = append(events, startEv())
			case 1:
				events = append(events, textStart(index))
			case 2:
				events = append(events, ir.Event{Kind: ir.PartStart, Index: index, Part: ir.Part{Kind: ir.Thinking}})
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
			}
		}
		var buf bytes.Buffer
		e := NewStreamEncoder(&buf, "m")
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
		// Arguments are not judged by the encoder (the decoder does that before the finish), so the
		// client's view is checked without them.
		s, err := checkStream(buf.Bytes(), false)
		if err != nil {
			t.Fatalf("not a well-formed Messages stream: %v\n%s\n%+v", err, buf.Bytes(), events)
		}
		if s.Stopped {
			// Success is reported only for a prefix that is a well-formed answer up to its Finish.
			n := 0
			for n < len(events) && events[n].Kind != ir.Finish {
				n++
			}
			if n == len(events) {
				t.Fatalf("success without a Finish: %+v", events)
			}
			if _, err := ir.Collect(events[:n+1]); err != nil && !errors.Is(err, ir.ErrBadJSON) {
				t.Fatalf("success for a sequence that is not well formed (%v): %+v", err, events[:n+1])
			}
		}
	})
}

// An id of a tool call in replayed history that is longer than a tool id may
// be is not refused (no history may end a session): it is cut, the same way
// on the call and on its result, so the two still pair, and reported.
func TestDecodeRequest_LongToolIDInHistoryIsCut(t *testing.T) {
	long := strings.Repeat("x", ir.MaxToolIDBytes+1)
	req := decode(t, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"`+long+`","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+long+`","content":"r"}]}]}`)
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
	if got := strings.Join(ir.Dropped(req.Dropped), ","); got != "input:tool_use.id" {
		t.Fatalf("dropped = %s", got)
	}
}
