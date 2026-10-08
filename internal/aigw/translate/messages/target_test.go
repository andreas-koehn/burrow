package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// The tests of the half a target needs: EncodeRequest, DecodeResponse,
// StreamDecoder, DecodeError and CheckRequest.

// ---------------------------------------------------------------- helpers

func userMsg(parts ...ir.Part) ir.Message      { return ir.Message{Role: ir.User, Parts: parts} }
func assistantMsg(parts ...ir.Part) ir.Message { return ir.Message{Role: ir.Assistant, Parts: parts} }
func toolUse(id, name, input string) ir.Part {
	return ir.Part{Kind: ir.ToolUse, ToolID: id, ToolName: name, Input: json.RawMessage(input)}
}
func result(id, out string) ir.Part { return ir.Part{Kind: ir.ToolResult, ToolID: id, Text: out} }
func png(data string) ir.Part       { return ir.Part{Kind: ir.Image, MediaType: "image/png", Data: data} }

// encodeTo writes req for "claude-x", checks that a strict server takes the
// body, and returns its top-level fields.
func encodeTo(t *testing.T, req ir.Request) (map[string]json.RawMessage, []string) {
	t.Helper()
	body, dropped, err := EncodeRequest(req, "claude-x")
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if err := CheckRequest(body); err != nil {
		t.Fatalf("a strict server refuses the body: %v\n%s", err, body)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, body)
	}
	return top, dropped
}

func talk(messages ...ir.Message) ir.Request {
	return ir.Request{Messages: messages, MaxTokens: 64}
}

func hello() ir.Request { return talk(userMsg(text("hi"))) }

// ---------------------------------------------------------------- request

func TestEncodeRequest_FullConversation(t *testing.T) {
	temp := 0.5
	req := ir.Request{
		Model:  "what-the-caller-said",
		System: []ir.Part{text("Be brief."), text("Use tools.")},
		Messages: []ir.Message{
			userMsg(text("Weather in Oslo and Rome?")),
			assistantMsg(text("Checking."), toolUse("toolu_1", "get_weather", `{"city":"Oslo"}`), toolUse("toolu_2", "get_weather", `{ "city" : "Rome" }`)),
			userMsg(result("toolu_1", "4°C"), ir.Part{Kind: ir.ToolResult, ToolID: "toolu_2", Text: "no such city", IsError: true}, text("And Paris?")),
		},
		Tools:       []ir.Tool{{Name: "get_weather", Description: "Weather for a city", Schema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
		ToolChoice:  ir.ToolChoice{Mode: ir.ChoiceAuto},
		MaxTokens:   256,
		Temperature: &temp,
		Stop:        []string{"END"},
		Stream:      true,
	}
	body, dropped, err := EncodeRequest(req, "claude-x")
	// The sampling settings are not sent to a Messages target; that is told.
	if err != nil || !reflect.DeepEqual(dropped, []string{"temperature"}) {
		t.Fatalf("err %v, dropped %v", err, dropped)
	}
	assertJSONEqual(t, body, `{"model":"claude-x","max_tokens":256,"system":"Be brief.\n\nUse tools.",
"messages":[
 {"role":"user","content":[{"type":"text","text":"Weather in Oslo and Rome?"}]},
 {"role":"assistant","content":[{"type":"text","text":"Checking."},
  {"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Oslo"}},
  {"type":"tool_use","id":"toolu_2","name":"get_weather","input":{"city":"Rome"}}]},
 {"role":"user","content":[
  {"type":"tool_result","tool_use_id":"toolu_1","content":"4°C"},
  {"type":"tool_result","tool_use_id":"toolu_2","content":"no such city","is_error":true},
  {"type":"text","text":"And Paris?"}]}],
"tools":[{"name":"get_weather","description":"Weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
"tool_choice":{"type":"auto"},"stop_sequences":["END"],"stream":true}`)
	// A tool call's input is the caller's bytes.
	if !bytes.Contains(body, []byte(`"input":{ "city" : "Rome" }`)) {
		t.Fatalf("the input's bytes changed: %s", body)
	}
	if err := CheckRequest(body); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeRequest_MaxTokensIsRequired(t *testing.T) {
	req := hello()
	req.MaxTokens = 0
	top, dropped := encodeTo(t, req)
	if string(top["max_tokens"]) != "32000" || DefaultMaxTokens != 32000 || !reflect.DeepEqual(dropped, []string{"max_tokens.default"}) {
		t.Fatalf("max_tokens %s, dropped %v", top["max_tokens"], dropped)
	}
	top, dropped = encodeTo(t, hello())
	if string(top["max_tokens"]) != "64" || dropped != nil {
		t.Fatalf("max_tokens %s, dropped %v", top["max_tokens"], dropped)
	}
	req.MaxTokens = -1
	if _, _, err := EncodeRequest(req, "m"); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeRequest_MinimalHasNoOptionalKeys(t *testing.T) {
	top, dropped := encodeTo(t, hello())
	if len(top) != 3 || top["model"] == nil || top["max_tokens"] == nil || top["messages"] == nil || dropped != nil {
		t.Fatalf("top %v, dropped %v", top, dropped)
	}
}

func TestEncodeRequest_RolesAlternate(t *testing.T) {
	// Messages of one role that follow each other are one message.
	top, dropped := encodeTo(t, talk(
		userMsg(text("a")), userMsg(text("b")),
		assistantMsg(text("c")), assistantMsg(text("d"), toolUse("t1", "f", `{}`)),
		userMsg(result("t1", "r")), userMsg(text("e"))))
	assertJSONEqual(t, top["messages"], `[
 {"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},
 {"role":"assistant","content":[{"type":"text","text":"c"},{"type":"text","text":"d"},{"type":"tool_use","id":"t1","name":"f","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"r"},{"type":"text","text":"e"}]}]`)
	if dropped != nil {
		t.Fatalf("dropped %v", dropped)
	}
}

func TestEncodeRequest_ConversationThatStartsWithTheAssistant(t *testing.T) {
	// Anthropic wants the user to speak first. A Chat history may begin with the assistant's
	// greeting; it is not refused: a user turn with a fixed text stands before it, and that is told.
	top, dropped := encodeTo(t, talk(assistantMsg(text("Hello, how can I help?")), userMsg(text("hi"))))
	assertJSONEqual(t, top["messages"], `[
 {"role":"user","content":[{"type":"text","text":"[no message]"}]},
 {"role":"assistant","content":[{"type":"text","text":"Hello, how can I help?"}]},
 {"role":"user","content":[{"type":"text","text":"hi"}]}]`)
	if !reflect.DeepEqual(dropped, []string{"messages.start"}) {
		t.Fatalf("dropped %v", dropped)
	}
}

func TestEncodeRequest_ToolResultsStandFirst(t *testing.T) {
	// Whatever the order in the neutral message, and when two user messages are joined.
	top, _ := encodeTo(t, talk(
		userMsg(text("go")),
		assistantMsg(toolUse("a", "f", `{}`), toolUse("b", "f", `{}`)),
		userMsg(text("early"), result("a", "ra")), userMsg(result("b", "rb"), text("late"))))
	assertJSONEqual(t, top["messages"], `[
 {"role":"user","content":[{"type":"text","text":"go"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}},{"type":"tool_use","id":"b","name":"f","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ra"},{"type":"tool_result","tool_use_id":"b","content":"rb"},
  {"type":"text","text":"early"},{"type":"text","text":"late"}]}]`)
}

func TestEncodeRequest_ToolImagesGoBackIntoTheirResult(t *testing.T) {
	calls := assistantMsg(toolUse("toolu_1", "read", `{}`), toolUse("toolu_2", "read", `{}`))
	top, dropped := encodeTo(t, talk(userMsg(text("go")), calls, userMsg(
		result("toolu_1", ir.ToolImageText), result("toolu_2", "a chart"),
		text(ir.ToolImageNote("toolu_1")), png("AAAA"),
		text(ir.ToolImageNote("toolu_2")), png("BBBB"), ir.Part{Kind: ir.Image, Data: "https://example.test/c.png"},
		text("what do you see?"))))
	var msgs []json.RawMessage
	if err := json.Unmarshal(top["messages"], &msgs); err != nil || len(msgs) != 3 {
		t.Fatalf("%v: %s", err, top["messages"])
	}
	assertJSONEqual(t, msgs[2], `{"role":"user","content":[
 {"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
 {"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"a chart"},
   {"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBBB"}},
   {"type":"image","source":{"type":"url","url":"https://example.test/c.png"}}]},
 {"type":"text","text":"what do you see?"}]}`)
	if dropped != nil {
		t.Fatalf("dropped %v", dropped)
	}

	// When the note does not tell one result of the message (two ids that read the same, or an
	// id the message has no result for), note and image stay what they are: content after the results.
	calls = assistantMsg(toolUse("a b", "read", `{}`), toolUse("ab", "read", `{}`))
	top, _ = encodeTo(t, talk(userMsg(text("go")), calls, userMsg(
		result("a b", "one"), result("ab", "two"), text(ir.ToolImageNote("ab")), png("AAAA"),
		text(ir.ToolImageNote("zz")), png("BBBB"))))
	json.Unmarshal(top["messages"], &msgs)
	assertJSONEqual(t, msgs[2], `{"role":"user","content":[
 {"type":"tool_result","tool_use_id":"a_b","content":"one"},{"type":"tool_result","tool_use_id":"ab","content":"two"},
 {"type":"text","text":"Image returned by tool call ab:"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
 {"type":"text","text":"Image returned by tool call zz:"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBBB"}}]}`)

	// A note that no image follows is text like any other.
	top, _ = encodeTo(t, talk(userMsg(text("go")), assistantMsg(toolUse("t", "read", `{}`)), userMsg(result("t", "r"), text(ir.ToolImageNote("t")))))
	json.Unmarshal(top["messages"], &msgs)
	assertJSONEqual(t, msgs[2], `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"r"},{"type":"text","text":"Image returned by tool call t:"}]}`)
}

func TestEncodeRequest_ToolCallIDsAnthropicWouldRefuse(t *testing.T) {
	// An id from another provider (a history that began on a Chat target) may hold characters
	// Anthropic refuses in a tool_use id. It is rewritten the same way in the call and in its
	// result; two ids that would read the same stay apart.
	top, dropped := encodeTo(t, talk(userMsg(text("go")),
		assistantMsg(toolUse("functions.read:0", "read", `{}`), toolUse("functions_read_0", "read", `{}`), toolUse("functions/read/0", "read", `{}`)),
		userMsg(result("functions.read:0", "a"), result("functions_read_0", "b"), result("functions/read/0", "c"))))
	assertJSONEqual(t, top["messages"], `[
 {"role":"user","content":[{"type":"text","text":"go"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"functions_read_0","name":"read","input":{}},
   {"type":"tool_use","id":"functions_read_0_2","name":"read","input":{}},{"type":"tool_use","id":"functions_read_0_3","name":"read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"functions_read_0","content":"a"},
   {"type":"tool_result","tool_use_id":"functions_read_0_2","content":"b"},{"type":"tool_result","tool_use_id":"functions_read_0_3","content":"c"}]}]`)
	if dropped != nil {
		t.Fatalf("dropped %v", dropped)
	}
}

func TestEncodeRequest_ToolCallIDsAreUniqueInTheWholeConversation(t *testing.T) {
	// Providers that number their calls anew in every answer (call_0, call_0, …) leave a history
	// in which one id stands in every turn. Anthropic wants each tool_use id once: the first keeps
	// it, every later call gets a numbered one, and its result the same.
	msgs := []ir.Message{userMsg(text("go"))}
	for turn := 0; turn < 5; turn++ {
		msgs = append(msgs, assistantMsg(toolUse("call_0", "f", `{}`), toolUse("call.1", "f", `{}`)),
			userMsg(result("call_0", fmt.Sprint("zero ", turn)), result("call.1", fmt.Sprint("one ", turn))))
	}
	top, dropped := encodeTo(t, talk(msgs...))
	var got []struct {
		Content []struct {
			Type, ID, Content string
			ToolUseID         string `json:"tool_use_id"`
		}
	}
	if err := json.Unmarshal(top["messages"], &got); err != nil || len(got) != 11 || dropped != nil {
		t.Fatalf("%v, dropped %v: %s", err, dropped, top["messages"])
	}
	seen := map[string]bool{}
	for turn := 0; turn < 5; turn++ {
		calls, results := got[1+2*turn].Content, got[2+2*turn].Content
		for i := range calls {
			if seen[calls[i].ID] || results[i].ToolUseID != calls[i].ID || results[i].Content != fmt.Sprint([]string{"zero ", "one "}[i], turn) {
				t.Fatalf("turn %d: call %+v, result %+v", turn, calls[i], results[i])
			}
			seen[calls[i].ID] = true
		}
	}
	if !seen["call_0"] || !seen["call_1"] || !seen["call_0_2"] || !seen["call_1_2"] || !seen["call_0_5"] || len(seen) != 10 {
		t.Fatalf("ids %v", seen)
	}
}

func TestEncodeRequest_ConversationThatEndsWithTheAssistant(t *testing.T) {
	// Current models take no prefill: the user is given a turn with a fixed text, and that is told.
	top, dropped := encodeTo(t, talk(userMsg(text("hi")), assistantMsg(text("Let me"))))
	assertJSONEqual(t, top["messages"], `[
 {"role":"user","content":[{"type":"text","text":"hi"}]},
 {"role":"assistant","content":[{"type":"text","text":"Let me"}]},
 {"role":"user","content":[{"type":"text","text":"[continue]"}]}]`)
	if !reflect.DeepEqual(dropped, []string{"messages.end"}) || ir.ContinueText != "[continue]" {
		t.Fatalf("dropped %v", dropped)
	}
}

func TestEncodeRequest_ToolNames(t *testing.T) {
	// A tool that is declared under a name Anthropic refuses is a client error that names it.
	for _, name := range []string{"", "get.weather", "a b", "é", strings.Repeat("x", 129)} {
		req := hello()
		req.Tools = []ir.Tool{{Name: "ok_Tool-1"}, {Name: name}}
		_, _, err := EncodeRequest(req, "m")
		var bad *ir.BadRequestError
		if !errors.As(err, &bad) || bad.Field != "tools[1].name" || bad.Format != "messages" || strings.Contains(err.Error(), "weather") {
			t.Errorf("%q: err = %v", name, err)
		}
	}
	req := hello()
	req.Tools = []ir.Tool{{Name: strings.Repeat("x", 128)}}
	if _, _, err := EncodeRequest(req, "m"); err != nil {
		t.Fatal(err)
	}
	// A call in the history under such a name (it cannot be one of the declared tools) does not
	// end the session: the name is written with the characters Anthropic takes, and that is told.
	top, dropped := encodeTo(t, talk(userMsg(text("go")),
		assistantMsg(toolUse("a", "functions.read:0", `{}`), toolUse("b", strings.Repeat("é", 200), `{}`), toolUse("c", "fine", `{}`)),
		userMsg(result("a", "ra"), result("b", "rb"), result("c", "rc"))))
	if !bytes.Contains(top["messages"], []byte(`"name":"functions_read_0"`)) || !bytes.Contains(top["messages"], []byte(`"name":"`+strings.Repeat("_", 128)+`"`)) ||
		!bytes.Contains(top["messages"], []byte(`"name":"fine"`)) || !reflect.DeepEqual(dropped, []string{"input:tool_use.name"}) {
		t.Fatalf("dropped %v: %s", dropped, top["messages"])
	}
}

func TestEncodeRequest_ThinkingAndEmptyTextAreNotSent(t *testing.T) {
	thinking := ir.Part{Kind: ir.Thinking, Text: "hm"}
	top, dropped := encodeTo(t, talk(
		userMsg(text("a")), assistantMsg(thinking), userMsg(text(" \n")), assistantMsg(text(""), text("b")), userMsg(text("c"), text(""))))
	// The turn that held only thinking is gone and reported; text that says nothing is no block
	// (Anthropic refuses one); the user messages left next to each other are one.
	assertJSONEqual(t, top["messages"], `[
 {"role":"user","content":[{"type":"text","text":"a"}]},
 {"role":"assistant","content":[{"type":"text","text":"b"}]},
 {"role":"user","content":[{"type":"text","text":"c"}]}]`)
	if !reflect.DeepEqual(dropped, []string{"thinking"}) {
		t.Fatalf("dropped %v", dropped)
	}
	// A tool result may be empty.
	top, _ = encodeTo(t, talk(userMsg(text("a")), assistantMsg(toolUse("t", "f", "")), userMsg(result("t", ""))))
	if !bytes.Contains(top["messages"], []byte(`{"type":"tool_result","tool_use_id":"t","content":""}`)) || !bytes.Contains(top["messages"], []byte(`"input":{}`)) {
		t.Fatalf("%s", top["messages"])
	}
	// Nothing left to send is a client error.
	if _, _, err := EncodeRequest(talk(assistantMsg(thinking), userMsg(text(""))), "m"); !errors.Is(err, ErrUnsupported) || !strings.HasSuffix(err.Error(), ": messages") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeRequest_Tools(t *testing.T) {
	odd := `{ "type" : "object", "properties":{"n":{"const":12345678901234567890123,"description":"<&>"}} }`
	req := hello()
	req.Tools = []ir.Tool{
		{Name: "odd", Schema: json.RawMessage(odd)},
		{Name: "none"},
		{Name: "untyped", Description: "d", Schema: json.RawMessage(` {"properties":{}}`)},
		{Name: "empty", Schema: json.RawMessage(`{}`)},
	}
	body, dropped, err := EncodeRequest(req, "claude-x")
	if err != nil || dropped != nil {
		t.Fatalf("err %v, dropped %v", err, dropped)
	}
	// A schema travels byte for byte; one without a "type" gets the one Anthropic insists on,
	// and a tool without a schema gets the empty object schema.
	for _, want := range []string{
		`{"name":"odd","input_schema":` + odd + `}`,
		`{"name":"none","input_schema":{"type":"object","properties":{}}}`,
		`{"name":"untyped","description":"d","input_schema":{"type":"object","properties":{}}}`,
		`{"name":"empty","input_schema":{"type":"object"}}`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("missing %s in\n%s", want, body)
		}
	}
	if err := CheckRequest(body); err != nil {
		t.Fatal(err)
	}
	// A schema of another type cannot be a tool's input: a client error that names the tool.
	for _, schema := range []string{`{"type":"string"}`, `{"type":["object","null"]}`, `{"type":null}`, `[]`, `{"type":"object"`, `"x"`} {
		req.Tools = []ir.Tool{{Name: "ok", Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "bad", Schema: json.RawMessage(schema)}}
		if _, _, err := EncodeRequest(req, "m"); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "tools[1].input_schema") {
			t.Errorf("schema %s: err = %v", schema, err)
		}
	}
	req.Tools = []ir.Tool{{Schema: json.RawMessage(`{"type":"object"}`)}}
	if _, _, err := EncodeRequest(req, "m"); err == nil || !strings.Contains(err.Error(), "tools[0].name") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeRequest_ToolChoice(t *testing.T) {
	tools := []ir.Tool{{Name: "f"}}
	for _, c := range []struct {
		choice  ir.ToolChoice
		tools   []ir.Tool
		want    string
		dropped []string
	}{
		{ir.ToolChoice{}, tools, ``, nil},
		{ir.ToolChoice{Mode: ir.ChoiceAuto}, tools, `{"type":"auto"}`, nil},
		{ir.ToolChoice{Mode: ir.ChoiceNone}, tools, `{"type":"none"}`, nil},
		// Current models answer 400 to a forced choice: the model decides, and that is told.
		{ir.ToolChoice{Mode: ir.ChoiceRequired}, tools, `{"type":"auto"}`, []string{"tool_choice"}},
		{ir.ToolChoice{Mode: ir.ChoiceTool, Name: "f"}, tools, `{"type":"auto"}`, []string{"tool_choice"}},
		// Anthropic refuses a tool_choice without tools: "auto" and "none" mean nothing then, the others are told.
		{ir.ToolChoice{Mode: ir.ChoiceAuto}, nil, ``, nil},
		{ir.ToolChoice{Mode: ir.ChoiceNone}, nil, ``, nil},
		{ir.ToolChoice{Mode: ir.ChoiceRequired}, nil, ``, []string{"tool_choice"}},
		{ir.ToolChoice{Mode: ir.ChoiceTool, Name: "g"}, tools, `{"type":"auto"}`, []string{"tool_choice"}},
		{ir.ToolChoice{Mode: ir.ChoiceTool, Name: "g"}, nil, ``, []string{"tool_choice"}},
	} {
		req := hello()
		req.ToolChoice, req.Tools = c.choice, c.tools
		top, dropped := encodeTo(t, req)
		if string(top["tool_choice"]) != c.want || !reflect.DeepEqual(dropped, c.dropped) {
			t.Errorf("%+v: tool_choice %s, dropped %v", c.choice, top["tool_choice"], dropped)
		}
	}
	req := hello()
	req.Tools = tools
	for _, bad := range []ir.ToolChoice{{Mode: ir.ChoiceTool}, {Mode: "sometimes"}} {
		req.ToolChoice = bad
		if _, _, err := EncodeRequest(req, "m"); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "tool_choice") {
			t.Errorf("%+v: err = %v", bad, err)
		}
	}
}

func TestEncodeRequest_Parameters(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		name        string
		temp, topP  *float64
		stop        []string
		want        string // temperature, top_p, stop_sequences as they are written
		wantDropped []string
	}{
		// Neither setting is sent: current models refuse any value but their default.
		{"temperature", f(0.7000000000000001), nil, nil, `||`, []string{"temperature"}},
		{"top_p", nil, f(0.9), nil, `||`, []string{"top_p"}},
		{"both", f(1.5), f(0.5), nil, `||`, []string{"temperature", "top_p"}},
		{"stop", nil, nil, []string{"END", "\n\nHuman:"}, `||["END","\n\nHuman:"]`, nil},
		// Anthropic refuses a stop sequence that is only white space.
		{"blank stops", nil, nil, []string{"\n", "END", " ", ""}, `||["END"]`, []string{"stop.blank"}},
		{"only blank stops", nil, nil, []string{"\n"}, `||`, []string{"stop.blank"}},
	} {
		req := hello()
		req.Temperature, req.TopP, req.Stop = c.temp, c.topP, c.stop
		top, dropped := encodeTo(t, req)
		got := string(top["temperature"]) + "|" + string(top["top_p"]) + "|" + string(top["stop_sequences"])
		if got != strings.ReplaceAll(c.want, "\n", `\n`) || !reflect.DeepEqual(dropped, c.wantDropped) {
			t.Errorf("%s: got %s, dropped %v", c.name, got, dropped)
		}
	}
	top, _ := encodeTo(t, hello())
	if top["stream"] != nil {
		t.Fatal("stream written though not asked")
	}
}

func TestEncodeRequest_Images(t *testing.T) {
	for _, media := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		top, _ := encodeTo(t, talk(userMsg(text("look"), ir.Part{Kind: ir.Image, MediaType: media, Data: "aGk="})))
		want := `[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"` + media + `","data":"aGk="}}]}]`
		assertJSONEqual(t, top["messages"], want)
	}
	top, _ := encodeTo(t, talk(userMsg(ir.Part{Kind: ir.Image, Data: "https://example.test/a.png?x=1&y=<2>"})))
	assertJSONEqual(t, top["messages"], `[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/a.png?x=1&y=<2>"}}]}]`)

	// An image the target cannot take is a client error that names it, never a missing image.
	for field, m := range map[string]ir.Message{
		"messages[0].image.media_type": userMsg(ir.Part{Kind: ir.Image, MediaType: "image/tiff", Data: "aGk="}),
		"messages[0].image.data":       userMsg(ir.Part{Kind: ir.Image, MediaType: "image/png"}),
		"messages[0].image.url":        userMsg(ir.Part{Kind: ir.Image, Data: "data:image/png,raw"}),
		"messages[0].assistant.image":  assistantMsg(png("aGk=")),
		"messages[0].tool_result.image": userMsg(ir.Part{Kind: ir.ToolResult, ToolID: "t", MediaType: "image/png",
			Data: "aGk="}),
	} {
		if _, _, err := EncodeRequest(talk(m, userMsg(text("x"))), "m"); !errors.Is(err, ErrUnsupported) || !strings.HasSuffix(err.Error(), ": "+field) {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	req := hello()
	req.System = []ir.Part{png("aGk=")}
	if _, _, err := EncodeRequest(req, "m"); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "system.image") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeRequest_Refused(t *testing.T) {
	for field, req := range map[string]ir.Request{
		"messages":                   {MaxTokens: 1},
		"messages[0].role":           talk(ir.Message{Role: "system", Parts: []ir.Part{text("x")}}),
		"messages[0].tool_use":       talk(userMsg(toolUse("a", "f", `{}`))),
		"messages[0].thinking":       talk(userMsg(ir.Part{Kind: ir.Thinking, Text: "x"})),
		"messages[0].part":           talk(userMsg(ir.Part{Kind: "video"})),
		"messages[0].tool_result.id": talk(userMsg(result("", "x"))),
		"messages[1].tool_use.id":    talk(userMsg(text("x")), assistantMsg(toolUse("", "f", `{}`))),
		"messages[1].tool_use.name":  talk(userMsg(text("x")), assistantMsg(toolUse("a", "", `{}`))),
		"messages[1].tool_use.input": talk(userMsg(text("x")), assistantMsg(toolUse("a", "f", `[1]`))),
		"messages[1].tool_result":    talk(userMsg(text("x")), assistantMsg(result("a", "x"))),
		"system.part":                {MaxTokens: 1, System: []ir.Part{result("a", "x")}, Messages: []ir.Message{userMsg(text("x"))}},
	} {
		body, dropped, err := EncodeRequest(req, "m")
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), field) || body != nil || dropped != nil {
			t.Errorf("%s: err = %v, body %s", field, err, body)
		}
	}
	// A second tool call with an id the turn holds already.
	if _, _, err := EncodeRequest(talk(userMsg(text("x")), assistantMsg(toolUse("a", "f", `{}`), toolUse("a", "f", `{}`))), "m"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := EncodeRequest(hello(), ""); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "model") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeRequest_Limits(t *testing.T) {
	many := func(n int) []ir.Part {
		parts := make([]ir.Part, n)
		for i := range parts {
			parts[i] = text("x")
		}
		return parts
	}
	calls := make([]ir.Part, ir.MaxToolCalls+1)
	for i := range calls {
		calls[i] = toolUse(fmt.Sprint("t", i), "f", `{}`)
	}
	deep := strings.Repeat(`{"a":`, ir.MaxDepth+1) + `1` + strings.Repeat(`}`, ir.MaxDepth+1)
	for name, req := range map[string]ir.Request{
		"messages": {MaxTokens: 1, Messages: make([]ir.Message, ir.MaxMessages+1)},
		"parts":    talk(userMsg(many(ir.MaxParts + 1)...)),
		"merged":   talk(userMsg(many(ir.MaxParts)...), userMsg(text("one more"))),
		"system":   {MaxTokens: 1, System: many(ir.MaxParts + 1), Messages: []ir.Message{userMsg(text("x"))}},
		"tools":    {MaxTokens: 1, Tools: make([]ir.Tool, ir.MaxTools+1), Messages: []ir.Message{userMsg(text("x"))}},
		"calls":    talk(userMsg(text("x")), assistantMsg(calls...)),
		"input":    talk(userMsg(text("x")), assistantMsg(toolUse("a", "f", deep))),
		"schema":   {MaxTokens: 1, Tools: []ir.Tool{{Name: "f", Schema: json.RawMessage(deep)}}, Messages: []ir.Message{userMsg(text("x"))}},
	} {
		if _, _, err := EncodeRequest(req, "m"); !errors.Is(err, ErrUnsupported) || !errors.Is(err, ir.ErrLimit) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestEncodeRequest_StringsAreKeptAndInputIsNotChanged(t *testing.T) {
	odd := "tab\t quote\" back\\ nul\x00 <b>&amp;</b>   é 日本 😀 \r\n"
	req := talk(userMsg(text(odd)), assistantMsg(text(odd), toolUse("t", "f", `{"a":"<&>"}`)), userMsg(result("t", odd)))
	req.System = []ir.Part{text(odd)}
	req.Tools = []ir.Tool{{Name: "f", Description: odd}}
	req.Dropped = []string{"zeta", "alpha", "zeta"}
	before := fmt.Sprintf("%#v", req)
	body, dropped, err := EncodeRequest(req, "claude-x")
	if err != nil || !utf8.Valid(body) || !json.Valid(body) {
		t.Fatalf("err %v: %s", err, body)
	}
	if fmt.Sprintf("%#v", req) != before {
		t.Fatal("EncodeRequest changed its input")
	}
	if !reflect.DeepEqual(dropped, []string{"alpha", "zeta"}) {
		t.Fatalf("dropped %v", dropped)
	}
	var got struct {
		System   string
		Messages []struct {
			Content []struct{ Text, Content string }
		}
		Tools []struct{ Description string }
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.System != odd || got.Messages[0].Content[0].Text != odd || got.Messages[1].Content[0].Text != odd || got.Messages[2].Content[0].Content != odd || got.Tools[0].Description != odd {
		t.Fatalf("a string changed: %s", body)
	}
	if !bytes.Contains(body, []byte(`"input":{"a":"<&>"}`)) {
		t.Fatalf("pass-through JSON was escaped: %s", body)
	}
}

func TestCheckRequest_RefusesWhatAnthropicWould(t *testing.T) {
	ok := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`
	if err := CheckRequest([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	with := func(messages string) string { return `{"model":"m","max_tokens":5,"messages":` + messages + `}` }
	use := `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]}`
	u := `{"role":"user","content":[{"type":"text","text":"x"}]}`
	for name, body := range map[string]string{
		"not JSON":                     `{`,
		"no model":                     `{"max_tokens":5,"messages":[` + u + `]}`,
		"no max_tokens":                `{"model":"m","messages":[` + u + `]}`,
		"no messages":                  with(`[]`),
		"assistant first":              with(`[{"role":"assistant","content":[{"type":"text","text":"x"}]},` + u + `]`),
		"two user messages":            with(`[` + u + `,` + u + `]`),
		"a system role":                with(`[{"role":"system","content":"x"}]`),
		"empty content":                with(`[{"role":"user","content":[]}]`),
		"blank text":                   with(`[{"role":"user","content":[{"type":"text","text":" \n"}]}]`),
		"unanswered tool_use":          with(`[` + u + `,` + use + `,` + u + `]`),
		"unanswered tool_use at end":   with(`[` + u + `,` + use + `]`),
		"tool_result for nothing":      with(`[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"r"}]}]`),
		"tool_result after text":       with(`[` + u + `,` + use + `,{"role":"user","content":[{"type":"text","text":"x"},{"type":"tool_result","tool_use_id":"a","content":"r"}]}]`),
		"tool_result twice":            with(`[` + u + `,` + use + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"r"},{"type":"tool_result","tool_use_id":"a","content":"r"}]}]`),
		"a tool_use id twice":          with(`[` + u + `,{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}},{"type":"tool_use","id":"a","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"r"}]}]`),
		"a tool_use id with a dot":     with(`[` + u + `,{"role":"assistant","content":[{"type":"tool_use","id":"a.b","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a.b","content":"r"}]}]`),
		"a tool_use input as a list":   with(`[` + u + `,{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":[]}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"r"}]}]`),
		"an image of an unknown type":  with(`[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/tiff","data":"x"}}]}]`),
		"a thinking block":             with(`[` + u + `,{"role":"assistant","content":[{"type":"thinking","thinking":"x"}]}]`),
		"a schema that is no object":   `{"model":"m","max_tokens":5,"tools":[{"name":"f","input_schema":{"type":"string"}}],"messages":[` + u + `]}`,
		"a tool without a schema":      `{"model":"m","max_tokens":5,"tools":[{"name":"f"}],"messages":[` + u + `]}`,
		"tool_choice without tools":    `{"model":"m","max_tokens":5,"tool_choice":{"type":"auto"},"messages":[` + u + `]}`,
		"a temperature":                `{"model":"m","max_tokens":5,"temperature":0.5,"messages":[` + u + `]}`,
		"a top_p":                      `{"model":"m","max_tokens":5,"top_p":1,"messages":[` + u + `]}`,
		"a forced tool_choice":         `{"model":"m","max_tokens":5,"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"},"messages":[` + u + `]}`,
		"a named tool_choice":          `{"model":"m","max_tokens":5,"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"f"},"messages":[` + u + `]}`,
		"a tool name with a dot":       `{"model":"m","max_tokens":5,"tools":[{"name":"a.b","input_schema":{"type":"object"}}],"messages":[` + u + `]}`,
		"a tool_use name with a dot":   with(`[` + u + `,{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"a.b","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"r"}]}]`),
		"an assistant turn at the end": with(`[` + u + `,{"role":"assistant","content":[{"type":"text","text":"x"}]}]`),
		"a tool_use id in two turns":   with(`[` + u + `,` + use + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"r"}]},` + use + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"r"}]}]`),
		"a blank stop sequence":        `{"model":"m","max_tokens":5,"stop_sequences":["\n"],"messages":[` + u + `]}`,
		"an unknown field":             `{"model":"m","max_tokens":5,"n":2,"messages":[` + u + `]}`,
	} {
		if err := CheckRequest([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ---------------------------------------------------------------- answer

func TestDecodeResponse_Fixtures(t *testing.T) {
	got, err := DecodeResponse(fixture(t, "resp_text.json"))
	want := ir.Response{ID: "msg_01A", Model: "claude-x", Parts: []ir.Part{text("Hello there.")}, Stop: ir.StopEnd,
		Usage: ir.Usage{InputTokens: 20, OutputTokens: 4}} // 12 + 3 written to the cache + 5 read from it
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("err %v\n got %+v\nwant %+v", err, got, want)
	}
	got, err = DecodeResponse(fixture(t, "resp_tools.json"))
	want = ir.Response{ID: "msg_01B", Model: "claude-x", Stop: ir.StopToolUse, Usage: ir.Usage{InputTokens: 131, OutputTokens: 17},
		Parts: []ir.Part{text("Checking both."),
			toolUse("toolu_1", "get_weather", `{"city":"Oslo","days":[1,2,3]}`),
			toolUse("toolu_2", "get_weather", `{ "city": "Rome", "unit": "°C" }`)}} // the input's bytes, as they came
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("err %v\n got %+v\nwant %+v", err, got, want)
	}
}

func message(content, stop string) []byte {
	return []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":` + content + `,"stop_reason":` + stop + `,"usage":{"input_tokens":1,"output_tokens":2}}`)
}

func TestDecodeResponse_Mapping(t *testing.T) {
	thinking := ir.Part{Kind: ir.Thinking, Text: "hm"}
	for _, c := range []struct {
		name, content, stop string
		parts               []ir.Part
		want                ir.StopReason
	}{
		{"text", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, `"end_turn"`, []ir.Part{text("a"), text("b")}, ir.StopEnd},
		{"thinking", `[{"type":"thinking","thinking":"hm","signature":"sig"},{"type":"text","text":"a"}]`, `"end_turn"`, []ir.Part{thinking, text("a")}, ir.StopEnd},
		{"redacted thinking and unknown blocks are skipped", `[{"type":"redacted_thinking","data":"x"},{"type":"server_tool_use","id":"s","name":"web_search","input":{}},{"type":"text","text":"a"}]`, `"end_turn"`, []ir.Part{text("a")}, ir.StopEnd},
		{"empty texts are no part", `[{"type":"text","text":""},{"type":"thinking","thinking":""}]`, `"end_turn"`, nil, ir.StopEnd},
		{"no content", `[]`, `"end_turn"`, nil, ir.StopEnd},
		{"max_tokens", `[{"type":"text","text":"a"}]`, `"max_tokens"`, []ir.Part{text("a")}, ir.StopMaxTokens},
		{"the context window", `[{"type":"text","text":"a"}]`, `"model_context_window_exceeded"`, []ir.Part{text("a")}, ir.StopMaxTokens},
		{"stop_sequence", `[{"type":"text","text":"a"}]`, `"stop_sequence"`, []ir.Part{text("a")}, ir.StopSequence},
		{"refusal", `[{"type":"text","text":"a"}]`, `"refusal"`, []ir.Part{text("a")}, ir.StopRefusal},
		{"pause_turn", `[{"type":"text","text":"a"}]`, `"pause_turn"`, []ir.Part{text("a")}, ir.StopEnd},
		{"an unknown reason", `[{"type":"text","text":"a"}]`, `"something_new"`, []ir.Part{text("a")}, ir.StopUnknown},
		{"no reason", `[{"type":"text","text":"a"}]`, `null`, []ir.Part{text("a")}, ir.StopUnknown},
		{"a tool call without input", `[{"type":"tool_use","id":"t","name":"f"}]`, `"tool_use"`, []ir.Part{toolUse("t", "f", `{}`)}, ir.StopToolUse},
		{"a tool call with a null input", `[{"type":"tool_use","id":"t","name":"f","input":null}]`, `"tool_use"`, []ir.Part{toolUse("t", "f", `{}`)}, ir.StopToolUse},
		// A caller acts on the stop reason: it follows what the answer holds.
		{"tool_use without a tool call", `[{"type":"text","text":"a"}]`, `"tool_use"`, []ir.Part{text("a")}, ir.StopEnd},
		{"a tool call that ends in end_turn", `[{"type":"tool_use","id":"t","name":"f","input":{"a":1}}]`, `"end_turn"`, []ir.Part{toolUse("t", "f", `{"a":1}`)}, ir.StopToolUse},
		{"a tool call cut by the token cap", `[{"type":"tool_use","id":"t","name":"f","input":{}}]`, `"max_tokens"`, []ir.Part{toolUse("t", "f", `{}`)}, ir.StopMaxTokens},
	} {
		got, err := DecodeResponse(message(c.content, c.stop))
		if err != nil || !reflect.DeepEqual(got.Parts, c.parts) || got.Stop != c.want {
			t.Errorf("%s: err %v, parts %+v, stop %q", c.name, err, got.Parts, got.Stop)
		}
	}
}

func TestDecodeResponse_Usage(t *testing.T) {
	for usage, want := range map[string]ir.Usage{
		`{"input_tokens":10,"output_tokens":3}`: {InputTokens: 10, OutputTokens: 3},
		`{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":300,"output_tokens":3}`:      {InputTokens: 330, OutputTokens: 3},
		`{"input_tokens":"7","output_tokens":1e2,"cache_read_input_tokens":null}`:                                   {InputTokens: 7, OutputTokens: 100},
		`{"input_tokens":-4,"output_tokens":1.5}`:                                                                   {},
		`{"input_tokens":9007199254740993,"output_tokens":0}`:                                                       {InputTokens: 9007199254740993},
		`{"input_tokens":9223372036854775807,"cache_read_input_tokens":9223372036854775807,"output_tokens":"many"}`: {InputTokens: 9223372036854775807},
		`null`: {},
	} {
		body := []byte(`{"type":"message","content":[],"stop_reason":"end_turn","usage":` + usage + `}`)
		got, err := DecodeResponse(body)
		if err != nil || got.Usage != want {
			t.Errorf("%s: err %v, usage %+v", usage, err, got.Usage)
		}
	}
}

func TestDecodeResponse_Refused(t *testing.T) {
	big := `{"a":"` + strings.Repeat("x", ir.MaxToolArgsBytes) + `"}`
	var calls, parts []string
	for i := 0; i <= ir.MaxToolCalls; i++ {
		calls = append(calls, fmt.Sprintf(`{"type":"tool_use","id":"t%d","name":"f","input":{}}`, i))
	}
	for i := 0; i <= ir.MaxParts; i++ {
		parts = append(parts, `{"type":"text","text":"x"}`)
	}
	for name, c := range map[string]struct {
		body  []byte
		limit bool
	}{
		"not JSON":                      {[]byte(`{"type":"message","content":[`), false},
		"a list":                        {[]byte(`[]`), false},
		"an empty object":               {[]byte(`{}`), false},
		"no content":                    {[]byte(`{"type":"message","stop_reason":"end_turn"}`), false},
		"another type":                  {[]byte(`{"type":"completion","content":[]}`), false},
		"a chat completion":             {[]byte(`{"id":"c","object":"chat.completion","choices":[{"message":{"content":"x"}}]}`), false},
		"content of another type":       {message(`"text"`, `"end_turn"`), false},
		"a block that is a string":      {message(`["x"]`, `"end_turn"`), false},
		"text of another type":          {message(`[{"type":"text","text":5}]`, `"end_turn"`), false},
		"a tool call without id":        {message(`[{"type":"tool_use","name":"f","input":{}}]`, `"tool_use"`), false},
		"a tool call without name":      {message(`[{"type":"tool_use","id":"t","input":{}}]`, `"tool_use"`), false},
		"one id twice":                  {message(`[{"type":"tool_use","id":"t","name":"f","input":{}},{"type":"tool_use","id":"t","name":"f","input":{}}]`, `"tool_use"`), false},
		"an input that is a list":       {message(`[{"type":"tool_use","id":"t","name":"f","input":[1]}]`, `"tool_use"`), false},
		"an input that is a string":     {message(`[{"type":"tool_use","id":"t","name":"f","input":"{}"}]`, `"tool_use"`), false},
		"an input that is no UTF-8":     {message(`[{"type":"tool_use","id":"t","name":"f","input":{"a":"`+"\xff"+`"}}]`, `"tool_use"`), false},
		"a stop reason of another type": {message(`[]`, `5`), false},
		"too many tool calls":           {message(`[`+strings.Join(calls, ",")+`]`, `"tool_use"`), true},
		"too many parts":                {message(`[`+strings.Join(parts, ",")+`]`, `"end_turn"`), true},
		"an input over the limit":       {message(`[{"type":"tool_use","id":"t","name":"f","input":`+big+`}]`, `"tool_use"`), true},
		"nested too deep":               {[]byte(strings.Repeat(`{"a":`, ir.MaxDepth+1) + `1` + strings.Repeat(`}`, ir.MaxDepth+1)), true},
		"a body over the limit":         {bytes.Repeat([]byte(" "), MaxResponseBytes+1), true},
	} {
		got, err := DecodeResponse(c.body)
		if err == nil || !reflect.DeepEqual(got, ir.Response{}) {
			t.Errorf("%s: accepted (%v)", name, err)
			continue
		}
		if errors.Is(err, ir.ErrLimit) != c.limit || (!c.limit && !errors.Is(err, ErrMalformed)) {
			t.Errorf("%s: err = %v", name, err)
		}
		if strings.Contains(err.Error(), "xxxx") {
			t.Errorf("%s: the error holds content", name)
		}
	}
	// The inputs of all calls together.
	half := `{"a":"` + strings.Repeat("x", ir.MaxToolArgsBytes-16) + `"}`
	var blocks []string
	for i := 0; i < 5; i++ {
		blocks = append(blocks, fmt.Sprintf(`{"type":"tool_use","id":"t%d","name":"f","input":%s}`, i, half))
	}
	if _, err := DecodeResponse(message(`[`+strings.Join(blocks, ",")+`]`, `"tool_use"`)); !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeResponse_ErrorObject(t *testing.T) {
	long := strings.Repeat("é", 400)
	for body, want := range map[string]string{
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`: "Overloaded",
		`{"type":"error","error":{"type":"api_error","message":"` + long + `"}}`:      strings.Repeat("é", 150),
		`{"type":"error","error":{"type":"api_error"}}`:                               errProvider,
		`{"type":"error"}`:  errProvider,
		`{"error":"plain"}`: "plain",
	} {
		_, err := DecodeResponse([]byte(body))
		var se *ir.StreamError
		if !errors.As(err, &se) || !errors.Is(err, ir.ErrStream) || se.Message != want {
			t.Errorf("%s: err = %v (%+v)", body, err, se)
			continue
		}
		if strings.Contains(err.Error(), "Overloaded") || strings.Contains(err.Error(), "plain") {
			t.Errorf("the error text holds the provider's words: %v", err)
		}
	}
}

func TestDecodeError(t *testing.T) {
	for body, want := range map[string]string{
		`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`: "slow down",
		`{"error":{"message":"openai shape"}}`:                                       "openai shape",
		`{"error":"a string"}`:                                                       "a string",
		`{"message":"top level"}`:                                                    "top level",
		`{"type":"error","error":{"message":5}}`:                                     "",
		`<html>`:                                                                     "",
		``:                                                                           "",
		strings.Repeat(`[`, ir.MaxDepth+1):                                           "",
		`{"error":{"message":"` + strings.Repeat("x", 400) + `"}}`: strings.Repeat("x", 300),
	} {
		if got := DecodeError([]byte(body)); got != want {
			t.Errorf("%.40s: %q", body, got)
		}
	}
}

// ---------------------------------------------------------------- stream

// decodeSSE runs a Messages stream through the parser and a decoder in pieces of the given size.
func decodeSSE(raw []byte, piece int) (events []ir.Event, err error) {
	p := sse.NewParser(MaxFrameBytes)
	d := NewStreamDecoder()
	feed := func(frames []sse.Frame) {
		for _, f := range frames {
			evs, ferr := d.Feed(f.Event, f.Data)
			events = append(events, evs...)
			if ferr != nil && err == nil {
				err = ferr
			}
		}
	}
	for i := 0; i < len(raw); i += piece {
		frames, perr := p.Feed(raw[i:min(i+piece, len(raw))])
		feed(frames)
		if perr != nil {
			return events, perr
		}
	}
	feed(p.Flush())
	return append(events, d.Close()...), err
}

// frames writes Messages stream frames from their data; the event name is the data's type.
func frames(datas ...string) []byte {
	var b bytes.Buffer
	for _, data := range datas {
		var head struct{ Type string }
		json.Unmarshal([]byte(data), &head)
		if head.Type != "" {
			b.WriteString("event: " + head.Type + "\n")
		}
		b.WriteString("data: " + data + "\n\n")
	}
	return b.Bytes()
}

const (
	mStart     = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":7,"output_tokens":1}}}`
	mTextStart = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	mStop0     = `{"type":"content_block_stop","index":0}`
	mEnd       = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`
	mToolEnd   = `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`
	mDone      = `{"type":"message_stop"}`
)

func mText(i int, s string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, i, quote(s))
}
func mTool(i int, id, name string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, i, id, name)
}
func mArgs(i int, s string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, i, quote(s))
}
func mStop(i int) string { return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i) }

// quote writes s as a JSON string and keeps bytes that are not UTF-8 as they are.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func kindsOf(events []ir.Event) string {
	var names []string
	for _, e := range events {
		names = append(names, string(e.Kind))
	}
	return strings.Join(names, " ")
}

func TestStreamDecoder_Fixtures(t *testing.T) {
	wantText := []ir.Event{
		{Kind: ir.Start, ID: "msg_01A", Model: "claude-x", Usage: ir.Usage{InputTokens: 20}},
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Text}},
		{Kind: ir.TextDelta, Index: 0, Text: "Hello"},
		{Kind: ir.TextDelta, Index: 0, Text: " there."},
		{Kind: ir.PartStop, Index: 0},
		{Kind: ir.Finish, Stop: ir.StopEnd, Usage: ir.Usage{InputTokens: 20, OutputTokens: 4}},
	}
	got, err := decodeSSE(fixture(t, "stream_text.sse"), 1<<20)
	if err != nil || !reflect.DeepEqual(got, wantText) {
		t.Fatalf("err %v\n got %+v\nwant %+v", err, got, wantText)
	}
	// The stream and the whole answer are one answer, however the bytes arrive.
	whole, err := DecodeResponse(fixture(t, "resp_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range []int{1, 2, 7, 13, 64, 1 << 20} {
		for _, crlf := range []bool{false, true} {
			raw := fixture(t, "stream_tools.sse")
			if crlf {
				raw = bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
			}
			events, err := decodeSSE(raw, piece)
			if err != nil {
				t.Fatalf("piece %d: %v", piece, err)
			}
			got, err := ir.Collect(events)
			if err != nil || !reflect.DeepEqual(got, whole) {
				t.Fatalf("piece %d: %v\n got %+v\nwant %+v", piece, err, got, whole)
			}
		}
	}
	// The pieces of a call's arguments are the upstream's, one event per piece that is not empty.
	events, _ := decodeSSE(fixture(t, "stream_tools.sse"), 1<<20)
	var pieces []string
	for _, e := range events {
		if e.Kind == ir.ToolArgsDelta && e.Index == 1 {
			pieces = append(pieces, e.ArgsJSON)
		}
	}
	if !reflect.DeepEqual(pieces, []string{`{"ci`, `ty":"Os`, `lo","days":[1,2`, `,3]}`}) {
		t.Fatalf("pieces %q", pieces)
	}
}

func TestStreamDecoder_Blocks(t *testing.T) {
	thinkingStart := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`
	for _, c := range []struct {
		name   string
		frames []string
		want   ir.Response
	}{
		{"thinking, its signature and text",
			[]string{mStart, thinkingStart,
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2ln"}}`, mStop0,
				`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`, mText(1, "a"), mStop(1), mEnd, mDone},
			ir.Response{Parts: []ir.Part{{Kind: ir.Thinking, Text: "hm"}, text("a")}, Stop: ir.StopEnd}},
		{"blocks nobody knows, their deltas and stops, and events nobody knows are passed over",
			[]string{mStart, `{"type":"new_event","x":1}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"x"}}`, mStop0,
				`{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"s","name":"web_search","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":1}"}}`, mStop(1),
				`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`, mText(2, "a"),
				`{"type":"content_block_delta","index":2,"delta":{"type":"citations_delta","citation":{}}}`, mStop(2), mEnd, mDone},
			ir.Response{Parts: []ir.Part{text("a")}, Stop: ir.StopEnd}},
		{"a block that starts with text, and one that never gets any",
			[]string{mStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"He"}}`, mText(0, "llo"), mStop0,
				`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`, mText(1, ""), mStop(1), mEnd, mDone},
			ir.Response{Parts: []ir.Part{text("Hello")}, Stop: ir.StopEnd}},
		{"a tool call without arguments",
			[]string{mStart, mTool(0, "t", "f"), mStop0, mToolEnd, mDone},
			ir.Response{Parts: []ir.Part{toolUse("t", "f", `{}`)}, Stop: ir.StopToolUse}},
		{"a tool call whose input came with its start",
			[]string{mStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{"a": 1}}}`, mStop0, mToolEnd, mDone},
			ir.Response{Parts: []ir.Part{toolUse("t", "f", `{"a": 1}`)}, Stop: ir.StopToolUse}},
		{"the deltas count when the start had an input as well (as the SDKs read it)",
			[]string{mStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{"a":1}}}`, mArgs(0, `{"b":2}`), mStop0, mToolEnd, mDone},
			ir.Response{Parts: []ir.Part{toolUse("t", "f", `{"b":2}`)}, Stop: ir.StopToolUse}},
		{"white space before the arguments is not passed on",
			[]string{mStart, mTool(0, "t", "f"), mArgs(0, " \n"), mArgs(0, ` {"a":1}`), mStop0, mToolEnd, mDone},
			ir.Response{Parts: []ir.Part{toolUse("t", "f", `{"a":1}`)}, Stop: ir.StopToolUse}},
		{"tool calls open side by side",
			[]string{mStart, mTool(0, "a", "f"), mTool(1, "b", "g"), mArgs(1, `{"b"`), mArgs(0, `{"a":`), mArgs(1, `:2}`), mArgs(0, `1}`), mStop(1), mStop0, mToolEnd, mDone},
			ir.Response{Parts: []ir.Part{toolUse("a", "f", `{"a":1}`), toolUse("b", "g", `{"b":2}`)}, Stop: ir.StopToolUse}},
		{"wire indexes need not count from 0",
			[]string{mStart, `{"type":"content_block_start","index":7,"content_block":{"type":"text","text":""}}`, mText(7, "a"), mStop(7), mEnd, mDone},
			ir.Response{Parts: []ir.Part{text("a")}, Stop: ir.StopEnd}},
		{"blocks left open are stopped by message_stop",
			[]string{mStart, mTextStart, mText(0, "a"), mEnd, mDone},
			ir.Response{Parts: []ir.Part{text("a")}, Stop: ir.StopEnd}},
		{"no message_delta: the stop reason follows the answer",
			[]string{mStart, mTool(0, "t", "f"), mStop0, mDone},
			ir.Response{Parts: []ir.Part{toolUse("t", "f", `{}`)}, Stop: ir.StopToolUse}},
		{"the event name is read when the data has no type",
			[]string{mStart, mTextStart, mText(0, "a"), mStop0, mEnd},
			ir.Response{Parts: []ir.Part{text("a")}, Stop: ir.StopEnd}},
	} {
		raw := frames(c.frames...)
		if c.name == "the event name is read when the data has no type" {
			raw = append(raw, "event: message_stop\ndata: {}\n\n"...)
		}
		events, err := decodeSSE(raw, 1<<20)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got, err := ir.Collect(events)
		c.want.ID, c.want.Model, c.want.Usage = "msg_1", "m", ir.Usage{InputTokens: 7, OutputTokens: 3}
		if strings.HasPrefix(c.name, "no message_delta") {
			c.want.Usage.OutputTokens = 0
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v\n got %+v\nwant %+v\n%s", c.name, err, got, c.want, kindsOf(events))
		}
	}
}

func TestStreamDecoder_Usage(t *testing.T) {
	// message_delta may correct the input figure (Anthropic allows it there); cache tokens are input.
	events, err := decodeSSE(frames(mStart, mTextStart, mText(0, "a"), mStop0,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":5,"output_tokens":"12"}}`, mDone), 5)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if events[0].Usage != (ir.Usage{InputTokens: 7}) || last.Kind != ir.Finish || last.Stop != ir.StopMaxTokens || last.Usage != (ir.Usage{InputTokens: 105, OutputTokens: 12}) {
		t.Fatalf("start %+v, finish %+v", events[0], last)
	}
}

func TestStreamDecoder_BadEndings(t *testing.T) {
	big := strings.Repeat("x", 1<<19-16)
	var manyTools, manyParts []string
	for i := 0; i <= ir.MaxToolCalls; i++ {
		manyTools = append(manyTools, mTool(i, fmt.Sprint("t", i), "f"), mStop(i))
	}
	for i := 0; i <= ir.MaxParts; i++ {
		manyParts = append(manyParts, fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":"x"}}`, i), mStop(i))
	}
	for _, c := range []struct {
		name    string
		frames  []string
		kinds   string // the events, in order
		message string // of the Error
		feedErr error  // what Feed returned: nil for the provider's own error and an early end
	}{
		{"nothing at all", nil, "error", errEarlyEnd, nil},
		{"cut after message_start", []string{mStart}, "start error", errEarlyEnd, nil},
		{"cut inside a text", []string{mStart, mTextStart, mText(0, "a")}, "start part_start text_delta part_stop error", errEarlyEnd, nil},
		{"cut inside the arguments", []string{mStart, mTool(0, "t", "f"), mArgs(0, `{"a":`)}, "start part_start tool_args_delta part_stop error", errEarlyEnd, nil},
		{"cut between two calls", []string{mStart, mTool(0, "a", "f"), mTool(1, "b", "f"), mArgs(1, `{}`)}, "start part_start part_start tool_args_delta part_stop part_stop error", errEarlyEnd, nil},
		{"no message_stop after message_delta", []string{mStart, mTextStart, mText(0, "a"), mStop0, mEnd}, "start part_start text_delta part_stop error", errEarlyEnd, nil},
		{"an error event mid-stream", []string{mStart, mTextStart, mText(0, "a"), `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, mText(0, "b"), mDone},
			"start part_start text_delta part_stop error", "Overloaded", nil},
		{"an error event first", []string{`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}, "error", "Overloaded", nil},
		{"an error event that says nothing", []string{mStart, `{"type":"error"}`}, "start error", errProvider, nil},
		{"a long error message", []string{mStart, `{"type":"error","error":{"message":"` + strings.Repeat("é", 400) + `"}}`}, "start error", strings.Repeat("é", 150), nil},
		{"a delta for an index that never started", []string{mStart, mText(3, "a")}, "start error", errUnreadable, ErrMalformed},
		{"a stop for an index that never started", []string{mStart, mStop(3)}, "start error", errUnreadable, ErrMalformed},
		{"a delta after the block's stop", []string{mStart, mTextStart, mText(0, "a"), mStop0, mText(0, "b")}, "start part_start text_delta part_stop error", errUnreadable, ErrMalformed},
		{"a second stop", []string{mStart, mTextStart, mText(0, "a"), mStop0, mStop0}, "start part_start text_delta part_stop error", errUnreadable, ErrMalformed},
		{"a second start of one index", []string{mStart, mTextStart, mText(0, "a"), mStop0, mTextStart}, "start part_start text_delta part_stop error", errUnreadable, ErrMalformed},
		{"a block that starts while a text is open", []string{mStart, mTextStart, mText(0, "a"), mTool(1, "t", "f")}, "start part_start text_delta part_stop error", errUnreadable, ErrMalformed},
		{"a second message_start", []string{mStart, mTextStart, mText(0, "a"), mStart}, "start part_start text_delta part_stop error", errUnreadable, ErrMalformed},
		{"a message_start after the end is passed over", []string{mStart, mEnd, mDone, mStart}, "start finish", "", nil},
		{"content before message_start", []string{mTextStart}, "error", errUnreadable, ErrMalformed},
		{"message_stop before message_start", []string{mDone}, "error", errUnreadable, ErrMalformed},
		{"a tool_use block without an id", []string{mStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"f","input":{}}}`}, "start error", errUnreadable, ErrMalformed},
		{"a tool_use block without a name", []string{mStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"","input":{}}}`}, "start error", errUnreadable, ErrMalformed},
		{"two tool_use blocks with one id", []string{mStart, mTool(0, "t", "f"), mStop0, mTool(1, "t", "f")}, "start part_start part_stop error", errUnreadable, ErrMalformed},
		{"arguments that are no object", []string{mStart, mTool(0, "t", "f"), mArgs(0, `[1]`), mStop0, mToolEnd, mDone}, "start part_start tool_args_delta part_stop error", errUnreadable, ErrMalformed},
		{"arguments that are cut, then message_stop", []string{mStart, mTool(0, "t", "f"), mArgs(0, `{"a":`), mToolEnd, mDone}, "start part_start tool_args_delta part_stop error", errUnreadable, ErrMalformed},
		{"arguments that are the text null", []string{mStart, mTool(0, "t", "f"), mArgs(0, `null`), mStop0}, "start part_start tool_args_delta part_stop error", errUnreadable, ErrMalformed},
		{"arguments that are no UTF-8", []string{mStart, mTool(0, "t", "f"), mArgs(0, "{\"a\":\"\xff\"}"), mStop0}, "start part_start part_stop error", errUnreadable, ErrMalformed},
		{"a start input that is no object", []string{mStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":"x"}}`}, "start error", errUnreadable, ErrMalformed},
		{"a text delta for a tool call", []string{mStart, mTool(0, "t", "f"), mText(0, "a")}, "start part_start part_stop error", errUnreadable, ErrMalformed},
		{"an argument delta for a text", []string{mStart, mTextStart, mArgs(0, "{}")}, "start error", errUnreadable, ErrMalformed},
		{"a frame that is no JSON", []string{mStart, mTextStart, mText(0, "a"), `{"type":"content_block_delta",`}, "start part_start text_delta part_stop error", errUnreadable, ErrMalformed},
		{"a field of another type", []string{mStart, `{"type":"content_block_start","index":"zero","content_block":{"type":"text"}}`}, "start error", errUnreadable, ErrMalformed},
		{"a text of another type", []string{mStart, mTextStart, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":5}}`}, "start error", errUnreadable, ErrMalformed},
		{"a text that ends inside a character", []string{mStart, mTextStart, mText(0, "a\xc3"), mStop0}, "start part_start text_delta part_stop error", errUnreadable, ErrMalformed},
		{"a frame nested too deep", []string{mStart, strings.Repeat(`{"a":`, ir.MaxDepth+1) + `1` + strings.Repeat(`}`, ir.MaxDepth+1)}, "start error", errTooLarge, ir.ErrLimit},
		{"arguments over the limit", []string{mStart, mTool(0, "t", "f"), mArgs(0, `{"a":"`+big), mArgs(0, big), mArgs(0, big)}, "start part_start tool_args_delta tool_args_delta part_stop error", errTooLarge, ir.ErrLimit},
		{"a start input over the limit", []string{mStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f","input":{"a":"` + big + big + big + `"}}}`}, "start error", errTooLarge, ir.ErrLimit},
		{"too many tool calls", append([]string{mStart}, manyTools...), "start" + strings.Repeat(" part_start part_stop", ir.MaxToolCalls) + " error", errTooLarge, ir.ErrLimit},
		{"too many parts", append([]string{mStart}, manyParts...), "start" + strings.Repeat(" part_start text_delta part_stop", ir.MaxParts) + " error", errTooLarge, ir.ErrLimit},
	} {
		for _, piece := range []int{1 << 30, 17} {
			if piece == 17 && len(c.frames) > 100 {
				continue
			}
			events, err := decodeSSE(frames(c.frames...), piece)
			if got := kindsOf(events); got != c.kinds {
				t.Errorf("%s: events %s\n want %s", c.name, got, c.kinds)
				continue
			}
			if c.feedErr == nil && err != nil || c.feedErr != nil && !errors.Is(err, c.feedErr) {
				t.Errorf("%s: Feed returned %v, want %v", c.name, err, c.feedErr)
			}
			if last := events[len(events)-1]; last.Kind == ir.Error && last.Err != c.message {
				t.Errorf("%s: message %q", c.name, last.Err)
			}
			if _, err := ir.Collect(events); c.message != "" && !errors.Is(err, ir.ErrStream) {
				t.Errorf("%s: Collect = %v: a failed stream read as an answer", c.name, err)
			}
			if err != nil && (strings.Contains(err.Error(), "xxxx") || strings.Contains(err.Error(), "Overloaded")) {
				t.Errorf("%s: the error holds content", c.name)
			}
		}
	}
}

func TestStreamDecoder_FeedReturnsTheClosingEventsWithItsError(t *testing.T) {
	d := NewStreamDecoder()
	feed := func(data string) ([]ir.Event, error) { return d.Feed("", []byte(data)) }
	feed(mStart)
	feed(mTool(0, "a", "f"))
	feed(mTool(1, "b", "f"))
	feed(mArgs(1, `{"x":`))
	events, err := feed(`{"type":"content_block_delta","index":0,"delta":`)
	if !errors.Is(err, ErrMalformed) || kindsOf(events) != "part_stop part_stop error" || events[0].Index != 0 || events[1].Index != 1 {
		t.Fatalf("err %v, events %+v", err, events)
	}
	// Nothing after the end, whatever comes.
	if evs, err := feed(mDone); evs != nil || err != nil {
		t.Fatalf("after the end: %v, %v", evs, err)
	}
	if evs, err := feed(`{`); evs != nil || err != nil {
		t.Fatalf("after the end: %v, %v", evs, err)
	}
	if d.Close() != nil || d.Close() != nil {
		t.Fatal("Close after the end returned events")
	}
	// An oversized frame is refused before it is read.
	d = NewStreamDecoder()
	if evs, err := d.Feed("message_start", bytes.Repeat([]byte("x"), MaxFrameBytes+1)); !errors.Is(err, ir.ErrLimit) || kindsOf(evs) != "error" {
		t.Fatalf("err %v, events %+v", err, evs)
	}
	// Empty data and a ping are nothing.
	d = NewStreamDecoder()
	for _, data := range []string{"", "  ", `{"type":"ping"}`} {
		if evs, err := d.Feed("ping", []byte(data)); evs != nil || err != nil {
			t.Fatalf("%q: %v, %v", data, evs, err)
		}
	}
}

func TestStreamDecoder_CharacterSplitAcrossDeltas(t *testing.T) {
	// A character cut in two between deltas — as bytes, or as the halves of an escaped surrogate
	// pair — is put together again, in text and in arguments.
	for _, c := range []struct{ name, one, two, want string }{
		{"two bytes", `"a` + "\xc3" + `"`, `"` + "\xa9" + `b"`, "aéb"},
		{"four bytes, cut after one", `"` + "\xf0" + `"`, `"` + "\x9f\x98\x80" + `"`, "😀"},
		{"four bytes, cut after three", `"x` + "\xf0\x9f\x98" + `"`, `"` + "\x80" + `y"`, "x😀y"},
		{"an escaped surrogate pair", `"a\ud83d"`, `"\ude00b"`, "a😀b"},
	} {
		for _, kind := range []string{"text", "thinking", "arguments"} {
			var raw []byte
			var want ir.Part
			delta := func(typ, key, value string) string {
				return `{"type":"content_block_delta","index":0,"delta":{"type":"` + typ + `","` + key + `":` + value + `}}`
			}
			switch kind {
			case "text":
				raw = frames(mStart, mTextStart, delta("text_delta", "text", c.one), delta("text_delta", "text", c.two), mStop0, mEnd, mDone)
				want = text(c.want)
			case "thinking":
				raw = frames(mStart, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
					delta("thinking_delta", "thinking", c.one), delta("thinking_delta", "thinking", c.two), mStop0, mEnd, mDone)
				want = ir.Part{Kind: ir.Thinking, Text: c.want}
			default:
				open, shut := `"{\"k\":\"`+c.one[1:], c.two[:len(c.two)-1]+`\"}"`
				raw = frames(mStart, mTool(0, "t", "f"), delta("input_json_delta", "partial_json", open), delta("input_json_delta", "partial_json", shut), mStop0, mToolEnd, mDone)
				want = toolUse("t", "f", `{"k":"`+c.want+`"}`)
			}
			events, err := decodeSSE(raw, 1<<20)
			if err != nil {
				t.Errorf("%s in %s: %v", c.name, kind, err)
				continue
			}
			for _, e := range events {
				if !utf8.ValidString(e.Text) || !utf8.ValidString(e.ArgsJSON) || (e.Kind == ir.TextDelta || e.Kind == ir.ToolArgsDelta) && e.Text+e.ArgsJSON == "" {
					t.Errorf("%s in %s: a delta that is empty or no UTF-8: %+v", c.name, kind, e)
				}
			}
			got, err := ir.Collect(events)
			if err != nil || len(got.Parts) != 1 || !reflect.DeepEqual(got.Parts[0], want) {
				t.Errorf("%s in %s: %v, %+v", c.name, kind, err, got.Parts)
			}
		}
	}
	// Text that is no UTF-8 at all is repaired as in a whole answer; arguments never are.
	events, err := decodeSSE(frames(mStart, mTextStart, mText(0, "a\xffb"), mStop0, mEnd, mDone), 1<<20)
	if got, cerr := ir.Collect(events); err != nil || cerr != nil || got.Parts[0].Text != "a�b" {
		t.Fatalf("%v %v %+v", err, cerr, got)
	}
}

func TestStreamDecoder_TotalArgumentsLimit(t *testing.T) {
	d := NewStreamDecoder()
	d.Feed("", []byte(mStart))
	chunk := `{"a":"` + strings.Repeat("x", ir.MaxToolArgsBytes-64) + `"}`
	var err error
	var events []ir.Event
	for i := 0; i < 6 && err == nil; i++ {
		d.Feed("", []byte(mTool(i, fmt.Sprint("t", i), "f")))
		events, err = d.Feed("", []byte(mArgs(i, chunk)))
		if err == nil {
			d.Feed("", []byte(mStop(i)))
		}
	}
	if !errors.Is(err, ir.ErrLimit) || events[len(events)-1].Kind != ir.Error || events[len(events)-1].Err != errTooLarge {
		t.Fatalf("err %v, events %s", err, kindsOf(events))
	}
}

// ---------------------------------------------------------------- properties and fuzz

// checkEvents reports whether events is a sequence a caller-side stream encoder can rely on
// (the rules are those of ir.Event).
func checkEvents(events []ir.Event) error {
	const closed = ir.PartKind("closed")
	var parts []ir.PartKind
	var args [][]byte
	ids := map[string]bool{}
	started, ended := false, false
	for i, ev := range events {
		fail := func(why string) error { return fmt.Errorf("event %d (%s): %s", i, ev.Kind, why) }
		if ended {
			return fail("after the end")
		}
		if ev.Kind == ir.Error {
			if ev.Err == "" || len(ev.Err) > 300 || !utf8.ValidString(ev.Err) {
				return fail("message")
			}
			for _, k := range parts {
				if k != closed {
					return fail("a part was not stopped before the error")
				}
			}
			ended = true
			continue
		}
		if started == (ev.Kind == ir.Start) {
			return fail("start")
		}
		openPart := func(kind ir.PartKind) bool {
			return ev.Index >= 0 && ev.Index < len(parts) && parts[ev.Index] == kind
		}
		switch ev.Kind {
		case ir.Start:
			started = true
			if ev.Usage.InputTokens < 0 || ev.Usage.OutputTokens != 0 {
				return fail("usage")
			}
		case ir.PartStart:
			if ev.Index != len(parts) || len(parts) >= ir.MaxParts {
				return fail("index")
			}
			for _, k := range parts {
				if k == ir.Text || k == ir.Thinking {
					return fail("a text part is still open")
				}
			}
			switch ev.Part.Kind {
			case ir.Text, ir.Thinking:
				if !reflect.DeepEqual(ev.Part, ir.Part{Kind: ev.Part.Kind}) {
					return fail("a text part that is not empty")
				}
			case ir.ToolUse:
				if ev.Part.ToolID == "" || ev.Part.ToolName == "" || ids[ev.Part.ToolID] || ev.Part.Input != nil {
					return fail("tool call")
				}
				ids[ev.Part.ToolID] = true
				if len(ids) > ir.MaxToolCalls {
					return fail("too many tool calls")
				}
			default:
				return fail("kind")
			}
			parts = append(parts, ev.Part.Kind)
			args = append(args, nil)
		case ir.TextDelta:
			if !openPart(ir.Text) || ev.Text == "" || !utf8.ValidString(ev.Text) {
				return fail("delta")
			}
		case ir.ThinkingDelta:
			if !openPart(ir.Thinking) || ev.Text == "" || !utf8.ValidString(ev.Text) {
				return fail("delta")
			}
		case ir.ToolArgsDelta:
			if !openPart(ir.ToolUse) || ev.ArgsJSON == "" || !utf8.ValidString(ev.ArgsJSON) {
				return fail("delta")
			}
			args[ev.Index] = append(args[ev.Index], ev.ArgsJSON...)
		case ir.PartStop:
			if ev.Index < 0 || ev.Index >= len(parts) || parts[ev.Index] == closed {
				return fail("not open")
			}
			parts[ev.Index] = closed
		case ir.Finish:
			total := 0
			for n, k := range parts {
				if k != closed {
					return fail("a part is open")
				}
				total += len(args[n])
				if a := args[n]; len(a) > 0 && (ir.CheckObject(a) != nil || a[0] != '{' || len(a) > ir.MaxToolArgsBytes) {
					return fail(fmt.Sprintf("the arguments of part %d are no JSON object", n))
				}
			}
			if total > ir.MaxTotalToolArgsBytes || ev.Usage.InputTokens < 0 || ev.Usage.OutputTokens < 0 {
				return fail("over a limit")
			}
			if ev.Stop == ir.StopToolUse && len(ids) == 0 {
				return fail("tool_use without a tool call")
			}
			ended = true
		default:
			return fail("unknown kind")
		}
	}
	return nil
}

func FuzzDecodeResponse(f *testing.F) {
	f.Add(fixture(f, "resp_text.json"))
	f.Add(fixture(f, "resp_tools.json"))
	f.Add([]byte(`{"type":"message","content":[{"type":"thinking","thinking":"t"},{"type":"tool_use","id":"a","name":"f","input":{"a":[1e400]}}],"stop_reason":"end_turn","usage":{"input_tokens":"7","output_tokens":1e2}}`))
	f.Add([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"x"}}`))
	f.Add([]byte(`{"type":"message","content":[{"type":"tool_use","id":"` + strings.Repeat("i", ir.MaxToolIDBytes+1) + `","name":"` + strings.Repeat("n", ir.MaxToolNameBytes+1) + `","input":{}}],"stop_reason":"tool_use"}`))
	f.Add([]byte(`[[[[[[[[`))
	f.Fuzz(func(t *testing.T, body []byte) {
		got, err := DecodeResponse(body)
		if err != nil {
			if !errors.Is(err, ErrMalformed) && !errors.Is(err, ir.ErrLimit) && !errors.Is(err, ir.ErrStream) {
				t.Fatalf("an error of no known kind: %v", err)
			}
			if !reflect.DeepEqual(got, ir.Response{}) {
				t.Fatal("a value came with the error")
			}
			return
		}
		ids := map[string]bool{}
		for _, p := range got.Parts {
			switch p.Kind {
			case ir.Text, ir.Thinking:
				if p.Text == "" {
					t.Fatal("an empty text part")
				}
			case ir.ToolUse:
				if p.ToolID == "" || p.ToolName == "" || ids[p.ToolID] || len(p.ToolID) > ir.MaxToolIDBytes || len(p.ToolName) > ir.MaxToolNameBytes || ir.CheckObject(p.Input) != nil || len(p.Input) > ir.MaxToolArgsBytes {
					t.Fatalf("tool call %+v", p)
				}
				ids[p.ToolID] = true
			default:
				t.Fatalf("part kind %q", p.Kind)
			}
		}
		if got.Stop == ir.StopToolUse && len(ids) == 0 {
			t.Fatal("tool_use without a tool call")
		}
		if len(ids) > ir.MaxToolCalls || len(got.Parts) > ir.MaxParts || got.Usage.InputTokens < 0 || got.Usage.OutputTokens < 0 {
			t.Fatalf("over a limit: %d calls, %d parts, usage %+v", len(ids), len(got.Parts), got.Usage)
		}
		// What was read can be written for a Messages caller and read again as the same answer
		// (a stop reason no format knows is written as end_turn).
		out, err := EncodeResponse(got, "m")
		if err != nil {
			t.Fatalf("EncodeResponse: %v", err)
		}
		again, err := DecodeResponse(out)
		got.ID, again.ID = "", ""
		if got.Model == "" {
			again.Model = ""
		}
		if got.Stop == ir.StopUnknown {
			again.Stop = ir.StopUnknown
		}
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("round trip: %v\n first %+v\nsecond %+v", err, got, again)
		}
	})
}

func FuzzStreamDecoder(f *testing.F) {
	f.Add(fixture(f, "stream_text.sse"), 7)
	f.Add(fixture(f, "stream_tools.sse"), 64)
	f.Add(bytes.ReplaceAll(fixture(f, "stream_tools.sse"), []byte("\n"), []byte("\r\n")), 3)
	f.Add(frames(mStart, mTool(0, "a", "f"), mTool(1, "b", "g"), mArgs(1, `{"b"`), mArgs(0, `{"a":`), mArgs(1, `:2}`), mArgs(0, `1}`), mToolEnd, mDone), 5)
	f.Add(frames(mStart, mTool(0, strings.Repeat("i", ir.MaxToolIDBytes+1), "f"), mStop(0), mTool(1, "t", strings.Repeat("n", ir.MaxToolNameBytes+1)), mToolEnd, mDone), 4096)
	f.Add(frames(mStart, mTextStart, mText(0, "a\xf0\x9f"), mText(0, "\x98\x80"), `{"type":"error","error":{"message":"overloaded"}}`), 1)
	f.Add(frames(mStart, `{"type":"content_block_start","index":1e9,"content_block":{"type":"tool_use","id":"t","name":"f","input":{"a":1}}}`, mStart), 4)
	f.Add([]byte(": ping\n\nnot a field\ndata: null\n\nevent: message_stop\ndata: {}\n\ndata:{\"type\":\"message_delta\",\"usage\":{\"output_tokens\":\"3\"}}"), 2)
	f.Fuzz(func(t *testing.T, raw []byte, piece int) {
		if piece < 1 {
			piece = 1
		}
		p := sse.NewParser(MaxFrameBytes)
		d := NewStreamDecoder()
		var events []ir.Event
		failed := false
		feed := func(frames []sse.Frame) {
			for _, fr := range frames {
				evs, err := d.Feed(fr.Event, fr.Data)
				if err != nil {
					if failed {
						t.Fatal("a second error from a finished decoder")
					}
					if !errors.Is(err, ErrMalformed) && !errors.Is(err, ir.ErrLimit) {
						t.Fatalf("an error of no known kind: %v", err)
					}
					if n := len(evs); n == 0 || evs[n-1].Kind != ir.Error {
						t.Fatalf("no Error event came with the error %v: %+v", err, evs)
					}
					events = append(events, evs...)
					failed = true
					continue
				}
				if failed && evs != nil {
					t.Fatal("events after an error")
				}
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
		tail := d.Close()
		if failed && tail != nil {
			t.Fatal("Close returned events after an error")
		}
		events = append(events, tail...)
		if d.Close() != nil {
			t.Fatal("a second Close returned events")
		}
		if err := checkEvents(events); err != nil {
			t.Fatalf("%v\n%+v", err, events)
		}
		n := len(events)
		if n == 0 || (events[n-1].Kind != ir.Finish && events[n-1].Kind != ir.Error) {
			t.Fatalf("the stream was not ended: %+v", events)
		}
		if failed && events[n-1].Kind != ir.Error {
			t.Fatalf("a Finish after an error: %+v", events)
		}
		starts, stops := 0, 0
		for _, ev := range events {
			switch ev.Kind {
			case ir.PartStart:
				starts++
			case ir.PartStop:
				stops++
			}
		}
		if starts != stops {
			t.Fatalf("%d parts started, %d stopped: %+v", starts, stops, events)
		}
		_, err := ir.Collect(events)
		if finished := events[n-1].Kind == ir.Finish; finished && err != nil || !finished && !errors.Is(err, ir.ErrStream) {
			t.Fatalf("Collect: %v: %+v", err, events)
		}
	})
}

// FuzzEncodeRequest: whatever a caller's request decodes to, the body written for Anthropic is
// one a strict server takes, or the request is refused with a named error.
func FuzzEncodeRequest(f *testing.F) {
	// Short seeds: the fuzzer minimises every input that reaches new code, and a long one costs it minutes.
	f.Add([]byte(`{"model":"m","max_tokens":5,"system":"s","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{"a":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"r","is_error":true}]}]}`))
	f.Add([]byte(`{"model":"m","max_tokens":5,"temperature":1.5,"top_p":0.2,"stop_sequences":["\n","x"],"tool_choice":{"type":"any"},"tools":[{"name":"f","input_schema":{}},{"name":"g"}],
"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t"},{"type":"tool_use","id":"a.b","name":"f","input":{}},{"type":"tool_use","id":"a_b","name":"f","input":{}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"a.b","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]},{"type":"text","text":""}]}]}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		req, err := DecodeRequest(in)
		if err != nil {
			return
		}
		body, dropped, err := EncodeRequest(req, "claude-x")
		if err != nil {
			var bad *ir.BadRequestError
			if !errors.Is(err, ErrUnsupported) && !errors.As(err, &bad) || body != nil || dropped != nil {
				t.Fatalf("err = %v, body %s", err, body)
			}
			return
		}
		if !json.Valid(body) || !utf8.Valid(body) {
			t.Fatalf("the body is not sound JSON:\n%s", body)
		}
		if err := CheckRequest(body); err != nil {
			t.Fatalf("a strict server refuses the body: %v\n%s", err, body)
		}
		// What was written is read again without anything left to repair or report, and written
		// again it is the same body.
		back, err := DecodeRequest(body)
		if err != nil || back.Dropped != nil {
			t.Fatalf("the body cannot be read back: %v, dropped %v\n%s", err, back.Dropped, body)
		}
		again, moreDropped, err := EncodeRequest(back, "claude-x")
		if err != nil || moreDropped != nil || !bytes.Equal(again, body) {
			t.Fatalf("the encoding does not settle (%v, dropped %v)\n%s\n%s", err, moreDropped, body, again)
		}
	})
}

// A tool call's id and name come from the upstream and are kept for the whole
// answer: each has a limit of its own, checked before anything is kept.
func TestToolCallIDAndNameLimits(t *testing.T) {
	okID, okName := strings.Repeat("i", ir.MaxToolIDBytes), strings.Repeat("n", ir.MaxToolNameBytes)
	body := func(id, name string) []byte {
		return []byte(fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`, id, name))
	}
	stream := func(id, name string) ([]ir.Event, error) {
		d := NewStreamDecoder()
		var out []ir.Event
		for _, f := range []string{mStart, mTool(0, id, name), mStop(0), mToolEnd, mDone} {
			evs, err := d.Feed("", []byte(f))
			out = append(out, evs...)
			if err != nil {
				return out, err
			}
		}
		return out, nil
	}
	if resp, err := DecodeResponse(body(okID, okName)); err != nil || len(resp.Parts) != 1 || resp.Parts[0].ToolID != okID || resp.Parts[0].ToolName != okName {
		t.Fatalf("at the limit, buffered: %v", err)
	}
	if evs, err := stream(okID, okName); err != nil || len(evs) == 0 || evs[len(evs)-1].Kind != ir.Finish {
		t.Fatalf("at the limit, streamed: %v, %s", err, kindsOf(evs))
	}
	for name, c := range map[string][2]string{"id": {okID + "i", "f"}, "name": {"toolu_1", okName + "n"}} {
		if _, err := DecodeResponse(body(c[0], c[1])); !errors.Is(err, ir.ErrLimit) {
			t.Fatalf("%s over the limit, buffered: %v", name, err)
		}
		evs, err := stream(c[0], c[1])
		if !errors.Is(err, ir.ErrLimit) {
			t.Fatalf("%s over the limit, streamed: %v", name, err)
		}
		for i, e := range evs {
			if e.Kind == ir.PartStart || (e.Kind == ir.Error) != (i == len(evs)-1) {
				t.Fatalf("%s over the limit, streamed: %s", name, kindsOf(evs))
			}
		}
		if last := evs[len(evs)-1]; last.Err != errTooLarge {
			t.Fatalf("%s over the limit: the caller is told %q", name, last.Err)
		}
	}
}
