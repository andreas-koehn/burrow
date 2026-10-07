package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// This file holds the property and fuzz tests. The package is the target
// half of the Chat codec (write a request, read an answer); the caller half
// (read a request, write an answer) comes with task X05. Until then the two
// inverse functions the round trips need live here, as plain test code:
// readChatRequest and writeChatResponse / writeChatStream.

// ------------------------------------------------- inverse codecs (tests)

type wireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	Stop          []string        `json:"stop"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// readChatRequest reads a Chat Completions request body into the neutral
// form. strict refuses every key EncodeRequest is not supposed to write.
func readChatRequest(body []byte, strict bool) (ir.Request, error) {
	var w wireRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(&w); err != nil {
		return ir.Request{}, err
	}
	if w.Stream != (w.StreamOptions != nil && w.StreamOptions.IncludeUsage) && strict {
		return ir.Request{}, errors.New("stream and stream_options.include_usage disagree")
	}
	req := ir.Request{Model: w.Model, MaxTokens: w.MaxTokens, Temperature: w.Temperature, TopP: w.TopP, Stop: w.Stop, Stream: w.Stream}
	var pending *ir.Message // tool results waiting for the rest of their user message
	flush := func() {
		if pending != nil {
			req.Messages = append(req.Messages, *pending)
			pending = nil
		}
	}
	for _, m := range w.Messages {
		switch m.Role {
		case "system":
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				return ir.Request{}, err
			}
			req.System = append(req.System, ir.Part{Kind: ir.Text, Text: s})
		case "tool":
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				return ir.Request{}, err
			}
			if pending == nil {
				pending = &ir.Message{Role: ir.User}
			}
			pending.Parts = append(pending.Parts, ir.Part{Kind: ir.ToolResult, ToolID: m.ToolCallID, Text: s})
		case "user":
			parts, err := readUserContent(m.Content)
			if err != nil {
				return ir.Request{}, err
			}
			if pending != nil {
				pending.Parts = append(pending.Parts, parts...)
				flush()
				continue
			}
			req.Messages = append(req.Messages, ir.Message{Role: ir.User, Parts: parts})
		case "assistant":
			flush()
			msg := ir.Message{Role: ir.Assistant}
			var s *string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				return ir.Request{}, err
			}
			if s != nil && *s != "" {
				msg.Parts = append(msg.Parts, ir.Part{Kind: ir.Text, Text: *s})
			}
			for _, tc := range m.ToolCalls {
				if tc.Type != "function" && strict {
					return ir.Request{}, errors.New("tool call type")
				}
				msg.Parts = append(msg.Parts, ir.Part{Kind: ir.ToolUse, ToolID: tc.ID, ToolName: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments)})
			}
			req.Messages = append(req.Messages, msg)
		default:
			return ir.Request{}, errors.New("role")
		}
	}
	flush()
	for _, t := range w.Tools {
		if t.Type != "function" && strict {
			return ir.Request{}, errors.New("tool type")
		}
		req.Tools = append(req.Tools, ir.Tool{Name: t.Function.Name, Description: t.Function.Description, Schema: t.Function.Parameters})
	}
	if len(w.ToolChoice) > 0 {
		var mode string
		var named struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		switch {
		case json.Unmarshal(w.ToolChoice, &mode) == nil:
			req.ToolChoice.Mode = ir.ToolChoiceMode(mode)
		case json.Unmarshal(w.ToolChoice, &named) == nil && named.Type == "function":
			req.ToolChoice = ir.ToolChoice{Mode: ir.ChoiceTool, Name: named.Function.Name}
		default:
			return ir.Request{}, errors.New("tool_choice")
		}
	}
	return req, nil
}

func readUserContent(raw json.RawMessage) ([]ir.Part, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []ir.Part{{Kind: ir.Text, Text: s}}, nil
	}
	var arr []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, err
	}
	var parts []ir.Part
	for _, p := range arr {
		switch p.Type {
		case "text":
			parts = append(parts, ir.Part{Kind: ir.Text, Text: p.Text})
		case "image_url":
			media, data, isData := strings.Cut(strings.TrimPrefix(p.ImageURL.URL, "data:"), ";base64,")
			if strings.HasPrefix(p.ImageURL.URL, "data:") && isData && media != "" {
				parts = append(parts, ir.Part{Kind: ir.Image, MediaType: media, Data: data})
			} else {
				parts = append(parts, ir.Part{Kind: ir.Image, Data: p.ImageURL.URL})
			}
		default:
			return nil, errors.New("content part type")
		}
	}
	return parts, nil
}

var wireStop = map[ir.StopReason]string{
	ir.StopEnd: "stop", ir.StopMaxTokens: "length", ir.StopToolUse: "tool_calls", ir.StopRefusal: "content_filter",
	ir.StopSequence: "stop", ir.StopUnknown: "something_new",
}

type wireCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string  `json:"name,omitempty"`
		Arguments *string `json:"arguments,omitempty"`
	} `json:"function"`
}

type wireMsg struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content"`
	Reasoning string     `json:"reasoning_content,omitempty"`
	ToolCalls []wireCall `json:"tool_calls,omitempty"`
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func joinKind(r ir.Response, kind ir.PartKind) string {
	var b strings.Builder
	for _, p := range r.Parts {
		if p.Kind == kind {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// writeChatResponse writes a neutral answer as a chat.completion body.
func writeChatResponse(r ir.Response) []byte {
	msg := wireMsg{Role: "assistant", Reasoning: joinKind(r, ir.Thinking)}
	if s := joinKind(r, ir.Text); s != "" {
		msg.Content = &s
	}
	for _, p := range r.Parts {
		if p.Kind == ir.ToolUse {
			args := string(p.Input)
			c := wireCall{ID: p.ToolID, Type: "function"}
			c.Function.Name, c.Function.Arguments = p.ToolName, &args
			msg.ToolCalls = append(msg.ToolCalls, c)
		}
	}
	return mustJSON(map[string]any{
		"id": r.ID, "object": "chat.completion", "model": r.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": wireStop[r.Stop]}},
		"usage":   map[string]any{"prompt_tokens": r.Usage.InputTokens, "completion_tokens": r.Usage.OutputTokens},
	})
}

// cutRunes splits s into pieces at random character boundaries.
func cutRunes(rng *rand.Rand, s string) []string {
	var out []string
	for len(s) > 0 {
		n := 0
		for want := 1 + rng.Intn(6); want > 0 && n < len(s); want-- {
			_, size := utf8.DecodeRuneInString(s[n:])
			n += size
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}

// writeChatStream writes a neutral answer as chat.completion.chunk frames:
// text and arguments cut into random pieces, the pieces of several tool
// calls interleaved.
func writeChatStream(rng *rand.Rand, r ir.Response) []byte {
	var buf bytes.Buffer
	frame := func(delta any, finish any, usage any) {
		m := map[string]any{"id": r.ID, "object": "chat.completion.chunk", "model": r.Model}
		if delta != nil || finish != nil {
			m["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
		} else {
			m["choices"] = []any{}
		}
		if usage != nil {
			m["usage"] = usage
		}
		if err := sse.Write(&buf, "", mustJSON(m)); err != nil {
			panic(err)
		}
	}
	frame(map[string]any{"role": "assistant", "content": ""}, nil, nil)
	for _, piece := range cutRunes(rng, joinKind(r, ir.Thinking)) {
		frame(map[string]any{"reasoning_content": piece}, nil, nil)
	}
	for _, piece := range cutRunes(rng, joinKind(r, ir.Text)) {
		frame(map[string]any{"content": piece}, nil, nil)
	}
	var pieces [][]string
	n := 0
	for _, p := range r.Parts {
		if p.Kind != ir.ToolUse {
			continue
		}
		i := n
		n++
		empty := ""
		c := wireCall{Index: &i, ID: p.ToolID, Type: "function"}
		c.Function.Name, c.Function.Arguments = p.ToolName, &empty
		frame(map[string]any{"tool_calls": []wireCall{c}}, nil, nil)
		pieces = append(pieces, cutRunes(rng, string(p.Input)))
	}
	for left := true; left; {
		left = false
		for i := range pieces {
			if len(pieces[i]) == 0 || rng.Intn(3) == 0 && len(pieces) > 1 {
				left = left || len(pieces[i]) > 0
				continue
			}
			idx := i
			c := wireCall{Index: &idx}
			c.Function.Arguments = &pieces[i][0]
			pieces[i] = pieces[i][1:]
			frame(map[string]any{"tool_calls": []wireCall{c}}, nil, nil)
			left = left || len(pieces[i]) > 0
		}
	}
	frame(map[string]any{}, wireStop[r.Stop], nil)
	frame(nil, nil, map[string]any{"prompt_tokens": r.Usage.InputTokens, "completion_tokens": r.Usage.OutputTokens})
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

// --------------------------------------------------------------- generators

var genStrings = []string{
	"plain", "two words", "é", "日本語", "😀", "tab\there", "line\nbreak", "quote\"d", `back\slash`, "nul\x00byte",
	"<b>&amp;</b>", "\u2028", " lead", "trail ", "Error: not an error flag", "data: [DONE]", "{\"a\":1}", "\r\n", "more",
}

func genText(rng *rand.Rand) string {
	var b strings.Builder
	for n := 1 + rng.Intn(4); n > 0; n-- {
		b.WriteString(genStrings[rng.Intn(len(genStrings))])
	}
	return b.String()
}

func genSpace(rng *rand.Rand) string { return []string{"", "", " ", "\n", "  ", "\t"}[rng.Intn(6)] }

func genValue(rng *rand.Rand, depth int) string {
	scalars := []string{`1`, `-0`, `12345678901234567890123`, `9007199254740993`, `1.10`, `1E+2`, `0.1e-7`, `true`, `false`, `null`,
		`""`, `"\u00e9\u0000"`, `"\ud83d\ude00"`, `"<&>"`, `"a\"b\\c\/d"`, `"日本"`, `[]`, `{}`}
	if depth <= 0 || rng.Intn(3) > 0 {
		return scalars[rng.Intn(len(scalars))]
	}
	if rng.Intn(2) == 0 {
		var items []string
		for n := rng.Intn(4); n > 0; n-- {
			items = append(items, genSpace(rng)+genValue(rng, depth-1)+genSpace(rng))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	return genObject(rng, depth-1)
}

// genObject makes the text of a JSON object whose bytes a codec must not touch:
// odd spacing, keys out of order and repeated, numbers no float can hold.
func genObject(rng *rand.Rand, depth int) string {
	keys := []string{"zeta", "alpha", "path", "n", "é", "a b", "", "type", "properties", "zeta"}
	var items []string
	for n := rng.Intn(5); n > 0; n-- {
		items = append(items, genSpace(rng)+string(mustJSON(keys[rng.Intn(len(keys))]))+genSpace(rng)+":"+genSpace(rng)+genValue(rng, depth)+genSpace(rng))
	}
	return "{" + strings.Join(items, ",") + genSpace(rng) + "}"
}

// genRequest makes a request in the shape Chat Completions can carry
// without loss: roles alternate, one system part, no thinking, no error
// flag on a tool result, no empty text.
func genRequest(rng *rand.Rand) ir.Request {
	req := ir.Request{Model: "caller-model", Stream: rng.Intn(2) == 0}
	if rng.Intn(2) == 0 {
		req.System = []ir.Part{{Kind: ir.Text, Text: genText(rng)}}
	}
	for n := rng.Intn(4); n > 0; n-- {
		tool := ir.Tool{Name: fmt.Sprintf("tool_%d", len(req.Tools))}
		if rng.Intn(2) == 0 {
			tool.Description = genText(rng)
		}
		if rng.Intn(4) > 0 {
			tool.Schema = json.RawMessage(genObject(rng, 3))
		}
		req.Tools = append(req.Tools, tool)
	}
	if len(req.Tools) > 0 {
		switch rng.Intn(5) {
		case 0:
			req.ToolChoice.Mode = ir.ChoiceAuto
		case 1:
			req.ToolChoice.Mode = ir.ChoiceNone
		case 2:
			req.ToolChoice.Mode = ir.ChoiceRequired
		case 3:
			req.ToolChoice = ir.ToolChoice{Mode: ir.ChoiceTool, Name: req.Tools[rng.Intn(len(req.Tools))].Name}
		}
	}
	if rng.Intn(2) == 0 {
		req.MaxTokens = 1 + rng.Intn(1<<20)
	}
	if rng.Intn(2) == 0 {
		v := []float64{0, 0.2, 1, 0.7000000000000001, 1e-7, 2}[rng.Intn(6)]
		req.Temperature = &v
	}
	if rng.Intn(2) == 0 {
		v := rng.Float64()
		req.TopP = &v
	}
	for n := rng.Intn(maxStop + 1); n > 0; n-- {
		req.Stop = append(req.Stop, genText(rng))
	}

	var open []string // tool calls of the last assistant turn that still want a result
	calls := 0
	turns := 1 + rng.Intn(6)
	for i := 0; i < turns; i++ {
		if i%2 == 1 {
			var parts []ir.Part
			if rng.Intn(3) > 0 {
				parts = append(parts, ir.Part{Kind: ir.Text, Text: genText(rng)})
			}
			open = nil
			for n := rng.Intn(4); n > 0 || len(parts) == 0; n-- {
				id := fmt.Sprintf("call_%d", calls)
				calls++
				parts = append(parts, ir.Part{Kind: ir.ToolUse, ToolID: id, ToolName: genText(rng), Input: json.RawMessage(genObject(rng, 3))})
				open = append(open, id)
			}
			req.Messages = append(req.Messages, ir.Message{Role: ir.Assistant, Parts: parts})
			continue
		}
		var parts []ir.Part
		for _, id := range open {
			parts = append(parts, ir.Part{Kind: ir.ToolResult, ToolID: id, Text: genText(rng)})
		}
		open = nil
		for n := rng.Intn(4); n > 0 || len(parts) == 0; n-- {
			switch rng.Intn(4) {
			case 0:
				parts = append(parts, ir.Part{Kind: ir.Image, MediaType: []string{"image/png", "image/jpeg"}[rng.Intn(2)], Data: "aGk="})
			case 1:
				parts = append(parts, ir.Part{Kind: ir.Image, Data: "https://example.test/" + fmt.Sprint(rng.Intn(100)) + ".png?a=1&b=<2>"})
			default:
				parts = append(parts, ir.Part{Kind: ir.Text, Text: genText(rng)})
			}
		}
		req.Messages = append(req.Messages, ir.Message{Role: ir.User, Parts: parts})
	}
	return req
}

// genResponse makes an answer in the shape a Chat Completions answer has:
// at most one thinking part, then at most one text part, then tool calls.
func genResponse(rng *rand.Rand) ir.Response {
	r := ir.Response{ID: "chatcmpl-" + fmt.Sprint(rng.Intn(1000)), Model: genText(rng),
		Usage: ir.Usage{InputTokens: rng.Intn(1 << 30), OutputTokens: rng.Intn(1 << 20)}}
	if rng.Intn(8) == 0 {
		r.Usage.InputTokens = 9007199254740993 // no float holds this
	}
	if rng.Intn(3) == 0 {
		r.Parts = append(r.Parts, ir.Part{Kind: ir.Thinking, Text: genText(rng)})
	}
	if rng.Intn(3) > 0 {
		r.Parts = append(r.Parts, ir.Part{Kind: ir.Text, Text: genText(rng)})
	}
	tools := 0
	if rng.Intn(2) == 0 {
		tools = 1 + rng.Intn(4)
	}
	for i := 0; i < tools; i++ {
		input := genObject(rng, 3)
		r.Parts = append(r.Parts, ir.Part{Kind: ir.ToolUse, ToolID: fmt.Sprintf("call_%d", i), ToolName: genText(rng), Input: json.RawMessage(input)})
	}
	if tools > 0 {
		r.Stop = []ir.StopReason{ir.StopToolUse, ir.StopToolUse, ir.StopMaxTokens, ir.StopRefusal}[rng.Intn(4)]
	} else {
		r.Stop = []ir.StopReason{ir.StopEnd, ir.StopEnd, ir.StopMaxTokens, ir.StopRefusal, ir.StopUnknown}[rng.Intn(5)]
	}
	return r
}

// ---------------------------------------------------------------- properties

func TestProperty_RequestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20261007))
	for i := 0; i < 2000; i++ {
		req := genRequest(rng)
		body, dropped, err := EncodeRequest(req, "m-target")
		if err != nil {
			t.Fatalf("case %d: %v\n%+v", i, err, req)
		}
		if dropped != nil {
			t.Fatalf("case %d: dropped %v", i, dropped)
		}
		if !json.Valid(body) || !utf8.Valid(body) {
			t.Fatalf("case %d: the body is not JSON:\n%s", i, body)
		}
		back, err := readChatRequest(body, true)
		if err != nil {
			t.Fatalf("case %d: %v\n%s", i, err, body)
		}
		if back.Model != "m-target" {
			t.Fatalf("case %d: model %q", i, back.Model)
		}
		back.Model = req.Model
		if !reflect.DeepEqual(back, req) {
			t.Fatalf("case %d: the request changed\n sent %+v\n read %+v\n body %s", i, req, back, body)
		}
		// decode -> encode -> decode: what was read encodes to the same bytes again.
		again, _, err := EncodeRequest(back, "m-target")
		if err != nil || !bytes.Equal(again, body) {
			t.Fatalf("case %d: the second encoding differs (%v)\n%s\n%s", i, err, body, again)
		}
		// Pass-through JSON is in the body byte for byte.
		for _, tool := range req.Tools {
			if tool.Schema != nil && !bytes.Contains(body, tool.Schema) {
				t.Fatalf("case %d: schema %s is not in the body", i, tool.Schema)
			}
		}
	}
}

func TestProperty_ResponseRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20261007))
	for i := 0; i < 2000; i++ {
		want := genResponse(rng)

		// Buffered: write -> decode gives the value; decode -> write -> decode gives it again.
		body := writeChatResponse(want)
		got, err := DecodeResponse(body)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: buffered: %v\nwant %+v\n got %+v\n%s", i, err, want, got, body)
		}
		if again, err := DecodeResponse(writeChatResponse(got)); err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("case %d: second decoding differs: %v", i, err)
		}

		// Streamed, in pieces of a random size: the events are well formed and fold to the same value.
		raw := writeChatStream(rng, want)
		events, err := decodeStream(t, raw, 1+rng.Intn(300))
		if err != nil {
			t.Fatalf("case %d: stream: %v\n%s", i, err, raw)
		}
		if err := checkEvents(events); err != nil {
			t.Fatalf("case %d: %v\n%+v", i, err, events)
		}
		streamed, err := ir.Collect(events)
		if err != nil || !reflect.DeepEqual(streamed, want) {
			t.Fatalf("case %d: stream: %v\nwant %+v\n got %+v\n%s", i, err, want, streamed, raw)
		}
	}
}

// checkEvents reports whether events is a sequence a caller-side stream
// encoder can rely on (the rules are those of ir.Event).
func checkEvents(events []ir.Event) error {
	const closed = ir.PartKind("closed")
	var parts []ir.PartKind
	var args [][]byte // per part: the argument pieces so far
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
			if !openPart(ir.ToolUse) || ev.ArgsJSON == "" {
				return fail("delta")
			}
			args[ev.Index] = append(args[ev.Index], ev.ArgsJSON...)
		case ir.PartStop:
			if ev.Index < 0 || ev.Index >= len(parts) || parts[ev.Index] == closed {
				return fail("not open")
			}
			parts[ev.Index] = closed
		case ir.Finish:
			for _, k := range parts {
				if k != closed {
					return fail("a part is open")
				}
			}
			if ev.Usage.InputTokens < 0 || ev.Usage.OutputTokens < 0 {
				return fail("usage")
			}
			// The pieces of a tool call's arguments are one JSON object, or there are none.
			total := 0
			for n, a := range args {
				total += len(a)
				if len(a) > 0 && (ir.CheckObject(a) != nil || a[0] != '{' || len(a) > ir.MaxToolArgsBytes) {
					return fail(fmt.Sprintf("the arguments of part %d are no JSON object", n))
				}
			}
			if total > ir.MaxTotalToolArgsBytes {
				return fail("arguments over the total limit")
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

// ---------------------------------------------------------------- fuzz

func FuzzDecodeResponse(f *testing.F) {
	f.Add(fixture(f, "resp_text.json"))
	f.Add(fixture(f, "resp_tools.json"))
	f.Add([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"a"}],"reasoning":"r","refusal":"no","tool_calls":[{"id":"a","function":{"name":"f","arguments":{"a":[1e400]}}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":"7","completion_tokens":1e2}}`))
	f.Add([]byte(`{"choices":[{"index":null,"message":null}],"error":{"message":"x"}}`))
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
				if p.ToolID == "" || p.ToolName == "" || ids[p.ToolID] || ir.CheckObject(p.Input) != nil || len(p.Input) > ir.MaxToolArgsBytes {
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
		// decode -> encode -> decode gives the same neutral value.
		again, err := DecodeResponse(writeChatResponse(got))
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("round trip: %v\n first %+v\nsecond %+v", err, got, again)
		}
	})
}

func FuzzStreamDecoder(f *testing.F) {
	f.Add(fixture(f, "stream_text.sse"), 7)
	f.Add(fixture(f, "stream_tools.sse"), 64)
	f.Add(bytes.ReplaceAll(fixture(f, "stream_tools.sse"), []byte("\n"), []byte("\r\n")), 3)
	f.Add([]byte("data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1e9,\"function\":{\"arguments\":\"{\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"a\",\"function\":{\"name\":\"f\"}}]},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), 5)
	f.Add([]byte("data: {\"choices\":[{\"delta\":{\"reasoning\":\"r\",\"content\":\"c\",\"refusal\":\"no\"}}]}\n\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n"), 1)
	f.Add([]byte("data: "+`{"id":"c","choices":[{"delta":{"content":"x`+"\xf0\x9f"+`"}}]}`+"\n\n"+
		"data: "+`{"choices":[{"delta":{"content":"`+"\x98\x80"+`","tool_calls":[{"function":{"name":"f","arguments":" {\"a\":\"`+"\xc3"+`"}}]}}]}`+"\n\n"+
		"data: "+`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"arguments":"`+"\xa9"+`\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n"), 4)
	f.Add([]byte(": ping\n\nnot a field\ndata: null\n\ndata: {\"usage\":{\"prompt_tokens\":\"3\"},\"choices\":[]}\n\ndata:[DONE]"), 2)
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
				evs, err := d.Feed(fr.Data)
				if err != nil {
					if failed {
						t.Fatal("a second error from a finished decoder")
					}
					if !errors.Is(err, ErrMalformed) && !errors.Is(err, ir.ErrLimit) {
						t.Fatalf("an error of no known kind: %v", err)
					}
					// The decoder ends the answer itself: its events close with the one Error.
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
		// The answer was ended one way or the other, exactly once (checkEvents: nothing follows an
		// Error, every part that started was stopped, a Finish comes with sound arguments).
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
		// What ended in a Finish folds into an answer; what ended in an Error says so.
		_, err := ir.Collect(events)
		if finished := events[n-1].Kind == ir.Finish; finished && err != nil || !finished && !errors.Is(err, ir.ErrStream) {
			t.Fatalf("Collect: %v: %+v", err, events)
		}
	})
}

func FuzzEncodeRequest(f *testing.F) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 8; i++ {
		body, _, err := EncodeRequest(genRequest(rng), "m")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body)
	}
	f.Add([]byte(`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"tool","tool_call_id":"a","content":"r"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]},{"role":"assistant","content":null,"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{\"a\":1}"}}]}],"tools":[{"function":{"name":"f","parameters":{"a":"\u0000"}}}],"tool_choice":{"type":"function","function":{"name":"f"}},"stop":["a","b","c","d","e"],"temperature":1e-9}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		req, err := readChatRequest(in, false)
		if err != nil {
			return
		}
		body, dropped, err := EncodeRequest(req, "m")
		if err != nil {
			if !errors.Is(err, ErrUnsupported) || body != nil || dropped != nil {
				t.Fatalf("err = %v, body %s", err, body)
			}
			return
		}
		// Whatever the request held, the body written by hand is JSON a strict reader accepts...
		if !json.Valid(body) || !utf8.Valid(body) || ir.Depth(body) > ir.MaxDepth+8 {
			t.Fatalf("the body is not sound JSON:\n%s", body)
		}
		back, err := readChatRequest(body, true)
		if err != nil {
			t.Fatalf("the body cannot be read back: %v\n%s", err, body)
		}
		// ...and reading it and writing it again settles: the test's reader folds "tool" messages
		// into the user message that follows, which an arbitrary input need not have meant (the
		// generated requests of TestProperty_RequestRoundTrip come back byte for byte at once).
		again, _, err := EncodeRequest(back, "m")
		if err != nil || !json.Valid(again) {
			t.Fatalf("the second encoding failed (%v)\n%s\n%s", err, body, again)
		}
		back, err = readChatRequest(again, true)
		if err != nil {
			t.Fatalf("the second body cannot be read back: %v\n%s", err, again)
		}
		third, _, err := EncodeRequest(back, "m")
		if err != nil || !bytes.Equal(third, again) {
			t.Fatalf("the encoding does not settle (%v)\n%s\n%s\n%s", err, body, again, third)
		}
		for _, d := range dropped {
			if d != "thinking" && d != "stop.extra" && d != "tool_choice" {
				t.Fatalf("dropped %q", d)
			}
		}
	})
}
