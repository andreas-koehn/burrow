package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// The round-trip property of the codecs that have both halves: what one half of a format writes,
// the other half reads back as the same neutral value. Every case is made from a seed of its
// own, which a failure prints.

const roundTripCases = 1500

// rtWords are texts with more than white space in them (Anthropic takes no other).
var rtWords = []string{"plain", "two words", "é", "日本語", "😀", "tab\there", "line\nbreak", "quote\"d", `back\slash`, "nul\x00byte",
	"<b>&amp;</b>", "x y", " lead", "trail ", "Error: not an error flag", "data: [DONE]", "{\"a\":1}", "a\r\nb", "more", "[image]", "[no output]"}

func rtText(rng *rand.Rand) string {
	var b strings.Builder
	for n := 1 + rng.Intn(3); n > 0; n-- {
		b.WriteString(rtWords[rng.Intn(len(rtWords))])
	}
	return b.String()
}

func rtSpace(rng *rand.Rand) string { return []string{"", "", " ", "\n", "  ", "\t"}[rng.Intn(6)] }

func rtValue(rng *rand.Rand, depth int) string {
	scalars := []string{`1`, `-0`, `12345678901234567890123`, `9007199254740993`, `1.10`, `1E+2`, `true`, `false`, `null`,
		`""`, `"é\u0000"`, `"😀"`, `"<&>"`, `"a\"b\\c\/d"`, `"日本"`, `[]`, `{}`}
	if depth <= 0 || rng.Intn(3) > 0 {
		return scalars[rng.Intn(len(scalars))]
	}
	if rng.Intn(2) == 0 {
		var items []string
		for n := rng.Intn(4); n > 0; n-- {
			items = append(items, rtSpace(rng)+rtValue(rng, depth-1)+rtSpace(rng))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	return rtObject(rng, depth-1, "")
}

// rtObject makes the text of a JSON object whose bytes a codec must not touch: odd spacing, keys
// out of order and repeated, numbers no float can hold. first, when given, is its first member.
func rtObject(rng *rand.Rand, depth int, first string) string {
	keys := []string{"zeta", "alpha", "path", "n", "é", "a b", "", "properties", "zeta"}
	var items []string
	if first != "" {
		items = append(items, first)
	}
	for n := rng.Intn(5); n > 0; n-- {
		key, _ := json.Marshal(keys[rng.Intn(len(keys))])
		items = append(items, rtSpace(rng)+string(key)+rtSpace(rng)+":"+rtSpace(rng)+rtValue(rng, depth)+rtSpace(rng))
	}
	return "{" + strings.Join(items, ",") + rtSpace(rng) + "}"
}

// rtRequest makes a request both formats can carry whole: roles alternate from the user's first
// turn, every tool call is answered in the turn after it, tools have an object schema, texts say
// something. Thinking parts, error flags on tool results and images tools returned are in it:
// what a format does with those is told by forChat and forMessages.
func rtRequest(rng *rand.Rand) ir.Request {
	req := ir.Request{Model: "caller-model", MaxTokens: 1 + rng.Intn(1<<20), Stream: rng.Intn(2) == 0}
	if rng.Intn(2) == 0 {
		req.System = []ir.Part{{Kind: ir.Text, Text: rtText(rng)}}
	}
	for n := rng.Intn(4); n > 0; n-- {
		tool := ir.Tool{Name: fmt.Sprintf("tool_%d", len(req.Tools)), Schema: json.RawMessage(rtObject(rng, 3, `"type":"object"`))}
		if rng.Intn(2) == 0 {
			tool.Description = rtText(rng)
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
		v := []float64{0, 0.2, 1, 0.7000000000000001, 1e-7, 2}[rng.Intn(6)]
		req.Temperature = &v
	}
	if rng.Intn(2) == 0 {
		v := rng.Float64()
		req.TopP = &v
	}
	for n := rng.Intn(4); n > 0; n-- {
		req.Stop = append(req.Stop, rtText(rng))
	}
	var open []string // the calls of the assistant turn before
	calls := 0
	userTurn := func(final bool) {
		var parts, images []ir.Part
		for _, id := range open {
			parts = append(parts, ir.Part{Kind: ir.ToolResult, ToolID: id, Text: rtText(rng), IsError: rng.Intn(5) == 0})
			if rng.Intn(4) == 0 {
				images = append(images, ir.Part{Kind: ir.Text, Text: ir.ToolImageNote(id)}, ir.Part{Kind: ir.Image, MediaType: "image/png", Data: "aGk="})
			}
		}
		parts = append(parts, images...)
		open = nil
		for n := rng.Intn(3); n > 0 || len(parts) == 0; n-- {
			switch rng.Intn(4) {
			case 0:
				parts = append(parts, ir.Part{Kind: ir.Image, MediaType: []string{"image/png", "image/jpeg", "image/gif", "image/webp"}[rng.Intn(4)], Data: "aGk="})
			case 1:
				parts = append(parts, ir.Part{Kind: ir.Image, Data: "https://example.test/" + fmt.Sprint(rng.Intn(100)) + ".png?a=1&b=<2>"})
			default:
				parts = append(parts, ir.Part{Kind: ir.Text, Text: rtText(rng)})
			}
			if final {
				break
			}
		}
		req.Messages = append(req.Messages, ir.Message{Role: ir.User, Parts: parts})
	}
	for turn, turns := 0, 1+rng.Intn(6); turn < turns; turn++ {
		if turn%2 == 0 {
			userTurn(false)
			continue
		}
		var parts []ir.Part
		if rng.Intn(4) == 0 {
			parts = append(parts, ir.Part{Kind: ir.Thinking, Text: rtText(rng)})
		}
		if rng.Intn(3) > 0 {
			parts = append(parts, ir.Part{Kind: ir.Text, Text: rtText(rng)})
		}
		for n := rng.Intn(4); n > 0 || len(parts) == 0 || parts[len(parts)-1].Kind == ir.Thinking; n-- {
			id := fmt.Sprintf("call_%d", calls)
			calls++
			parts = append(parts, ir.Part{Kind: ir.ToolUse, ToolID: id, ToolName: fmt.Sprintf("tool_%d", rng.Intn(4)), Input: json.RawMessage(rtObject(rng, 3, ""))})
			open = append(open, id)
		}
		req.Messages = append(req.Messages, ir.Message{Role: ir.Assistant, Parts: parts})
	}
	if n := len(req.Messages); len(open) > 0 || req.Messages[n-1].Role == ir.Assistant {
		userTurn(true) // every call is answered, and the user speaks last
	}
	return req
}

// forTarget is what a request reads as once a target's format has carried it: the model is the
// target's, and thinking is not sent back.
func forTarget(req ir.Request, model string, part func(p ir.Part) ir.Part) ir.Request {
	out := req
	out.Model = model
	out.Messages = nil
	for _, m := range req.Messages {
		kept := ir.Message{Role: m.Role}
		for _, p := range m.Parts {
			if p.Kind != ir.Thinking {
				kept.Parts = append(kept.Parts, part(p))
			}
		}
		out.Messages = append(out.Messages, kept)
	}
	return out
}

// forChat: Chat Completions has no error flag on a tool message — the text says so.
func forChat(req ir.Request) ir.Request {
	return forTarget(req, "m-target", func(p ir.Part) ir.Part {
		if p.Kind == ir.ToolResult && p.IsError {
			p.Text, p.IsError = "Error: "+p.Text, false
		}
		return p
	})
}

// forMessages: toward Anthropic Messages the sampling settings are not sent and a forced tool
// choice is left to the model (both are told); everything else is carried as it is.
func forMessages(req ir.Request) ir.Request {
	out := forTarget(req, "m-target", func(p ir.Part) ir.Part { return p })
	out.Temperature, out.TopP = nil, nil
	if out.ToolChoice.Mode == ir.ChoiceRequired || out.ToolChoice.Mode == ir.ChoiceTool {
		out.ToolChoice = ir.ToolChoice{Mode: ir.ChoiceAuto}
	}
	return out
}

// messagesDropped is what messages.EncodeRequest reports for req besides its thinking.
func messagesDropped(req ir.Request, more []string) []string {
	names := append([]string(nil), more...)
	if req.Temperature != nil {
		names = append(names, "temperature")
	}
	if req.TopP != nil {
		names = append(names, "top_p")
	}
	if req.ToolChoice.Mode == ir.ChoiceRequired || req.ToolChoice.Mode == ir.ChoiceTool {
		names = append(names, "tool_choice")
	}
	return ir.Dropped(names)
}

func TestRoundTrip_Requests(t *testing.T) {
	for i := 0; i < roundTripCases; i++ {
		seed := int64(20261007 + i)
		req := rtRequest(rand.New(rand.NewSource(seed)))
		hasThinking := false
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				hasThinking = hasThinking || p.Kind == ir.Thinking
			}
		}
		wantDropped := []string(nil)
		if hasThinking {
			wantDropped = []string{"thinking"}
		}

		// Chat Completions.
		body, dropped, err := chat.EncodeRequest(req, "m-target")
		if err != nil || !reflect.DeepEqual(dropped, wantDropped) {
			t.Fatalf("seed %d: chat.EncodeRequest: %v, dropped %v", seed, err, dropped)
		}
		if err := chat.CheckRequest(body); err != nil {
			t.Fatalf("seed %d: %v\n%s", seed, err, body)
		}
		back, err := chat.DecodeRequest(body)
		if err != nil || back.Dropped != nil {
			t.Fatalf("seed %d: chat.DecodeRequest: %v, dropped %v\n%s", seed, err, back.Dropped, body)
		}
		if want := forChat(req); !reflect.DeepEqual(back, want) {
			t.Fatalf("seed %d: Chat Completions changed the request\n sent %+v\n read %+v\n body %s", seed, want, back, body)
		}
		if again, _, err := chat.EncodeRequest(back, "m-target"); err != nil || !bytes.Equal(again, body) {
			t.Fatalf("seed %d: the second Chat encoding differs (%v)\n%s\n%s", seed, err, body, again)
		}

		// Anthropic Messages.
		body, dropped, err = messages.EncodeRequest(req, "m-target")
		if err != nil || !reflect.DeepEqual(dropped, messagesDropped(req, wantDropped)) {
			t.Fatalf("seed %d: messages.EncodeRequest: %v, dropped %v", seed, err, dropped)
		}
		if err := messages.CheckRequest(body); err != nil {
			t.Fatalf("seed %d: %v\n%s", seed, err, body)
		}
		back, err = messages.DecodeRequest(body)
		if err != nil || back.Dropped != nil {
			t.Fatalf("seed %d: messages.DecodeRequest: %v, dropped %v\n%s", seed, err, back.Dropped, body)
		}
		if want := forMessages(req); !reflect.DeepEqual(back, want) {
			t.Fatalf("seed %d: Anthropic Messages changed the request\n sent %+v\n read %+v\n body %s", seed, want, back, body)
		}
		if again, _, err := messages.EncodeRequest(back, "m-target"); err != nil || !bytes.Equal(again, body) {
			t.Fatalf("seed %d: the second Messages encoding differs (%v)\n%s\n%s", seed, err, body, again)
		}
		// Pass-through JSON is in both bodies byte for byte.
		for _, tool := range req.Tools {
			if !bytes.Contains(body, tool.Schema) {
				t.Fatalf("seed %d: schema %s is not in the body", seed, tool.Schema)
			}
		}

		// Through the pairs: a Chat caller's request reaches a Messages target, and a Messages
		// caller's a Chat target, as the same conversation.
		chatBody, _, _ := chat.EncodeRequest(req, "caller-model")
		out, stream, _, err := lookup(t, Chat, Messages).Request(chatBody, nil, "m-target")
		if err != nil || stream != req.Stream {
			t.Fatalf("seed %d: chat-messages: %v", seed, err)
		}
		if back, err = messages.DecodeRequest(out); err != nil || !reflect.DeepEqual(back, forMessages(forChat(req))) {
			t.Fatalf("seed %d: chat-messages changed the request (%v)\n read %+v\n body %s", seed, err, back, out)
		}
		messagesBody, _, _ := messages.EncodeRequest(req, "caller-model")
		out, stream, _, err = lookup(t, Messages, Chat).Request(messagesBody, nil, "m-target")
		if err != nil || stream != req.Stream {
			t.Fatalf("seed %d: messages-chat: %v", seed, err)
		}
		if back, err = chat.DecodeRequest(out); err != nil || !reflect.DeepEqual(back, forChat(forMessages(req))) {
			t.Fatalf("seed %d: messages-chat changed the request (%v)\n read %+v\n body %s", seed, err, back, out)
		}
	}
}

// rtResponse makes an answer. chatShape is the shape a Chat Completions answer has: at most one
// thinking part, then at most one text part, then the tool calls; otherwise the parts come in
// any order, as a Messages answer has them. The id has the format's prefix and the stop reason
// is one the format has a word for.
func rtResponse(rng *rand.Rand, chatShape bool) ir.Response {
	r := ir.Response{Model: rtText(rng), Usage: ir.Usage{InputTokens: rng.Intn(1 << 30), OutputTokens: rng.Intn(1 << 20)}}
	if rng.Intn(8) == 0 {
		r.Usage.InputTokens = 9007199254740993 // no float holds this
	}
	calls := 0
	call := func() ir.Part {
		calls++
		input := "{}"
		if rng.Intn(5) > 0 {
			input = rtObject(rng, 3, "")
		}
		return ir.Part{Kind: ir.ToolUse, ToolID: fmt.Sprintf("call_%d", calls), ToolName: rtText(rng), Input: json.RawMessage(input)}
	}
	if chatShape {
		r.ID = "chatcmpl-" + fmt.Sprint(rng.Intn(1000))
		if rng.Intn(3) == 0 {
			r.Parts = append(r.Parts, ir.Part{Kind: ir.Thinking, Text: rtText(rng)})
		}
		if rng.Intn(3) > 0 {
			r.Parts = append(r.Parts, ir.Part{Kind: ir.Text, Text: rtText(rng)})
		}
		for n := rng.Intn(4); n > 0 && rng.Intn(2) == 0; n-- {
			r.Parts = append(r.Parts, call())
		}
	} else {
		r.ID = "msg_" + fmt.Sprint(rng.Intn(1000))
		for n := rng.Intn(6); n > 0; n-- {
			switch rng.Intn(4) {
			case 0:
				r.Parts = append(r.Parts, ir.Part{Kind: ir.Thinking, Text: rtText(rng)})
			case 1:
				r.Parts = append(r.Parts, call())
			default:
				r.Parts = append(r.Parts, ir.Part{Kind: ir.Text, Text: rtText(rng)})
			}
		}
	}
	stops := []ir.StopReason{ir.StopEnd, ir.StopEnd, ir.StopMaxTokens, ir.StopRefusal}
	if calls > 0 {
		stops = []ir.StopReason{ir.StopToolUse, ir.StopToolUse, ir.StopMaxTokens, ir.StopRefusal}
	}
	if !chatShape {
		stops = append(stops, ir.StopSequence)
	}
	r.Stop = stops[rng.Intn(len(stops))]
	return r
}

// rtCut splits s into pieces at random character boundaries.
func rtCut(rng *rand.Rand, s string) []string {
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

// rtEvents makes a valid event sequence that adds up to r: texts in pieces, and tool calls that
// stay open side by side with their pieces interleaved until something makes them stop.
func rtEvents(rng *rand.Rand, r ir.Response) []ir.Event {
	events := []ir.Event{{Kind: ir.Start, ID: r.ID, Model: r.Model, Usage: ir.Usage{InputTokens: r.Usage.InputTokens}}}
	type open struct {
		index  int
		pieces []string
	}
	var calls []open
	step := func() {
		i := rng.Intn(len(calls))
		c := &calls[i]
		if len(c.pieces) == 0 {
			events = append(events, ir.Event{Kind: ir.PartStop, Index: c.index})
			calls = append(calls[:i], calls[i+1:]...)
			return
		}
		events = append(events, ir.Event{Kind: ir.ToolArgsDelta, Index: c.index, ArgsJSON: c.pieces[0]})
		c.pieces = c.pieces[1:]
	}
	for i, p := range r.Parts {
		if p.Kind == ir.ToolUse {
			events = append(events, ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.ToolUse, ToolID: p.ToolID, ToolName: p.ToolName}})
			input := string(p.Input)
			if input == "{}" && rng.Intn(2) == 0 {
				input = "" // a call without arguments may send none
			}
			calls = append(calls, open{index: i, pieces: rtCut(rng, input)})
		} else {
			delta := ir.TextDelta
			if p.Kind == ir.Thinking {
				delta = ir.ThinkingDelta
			}
			events = append(events, ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: p.Kind}})
			for _, piece := range rtCut(rng, p.Text) {
				events = append(events, ir.Event{Kind: delta, Index: i, Text: piece})
				if len(calls) > 0 && rng.Intn(2) == 0 {
					step()
				}
			}
			events = append(events, ir.Event{Kind: ir.PartStop, Index: i})
		}
		for len(calls) > 0 && rng.Intn(3) > 0 {
			step()
		}
	}
	for len(calls) > 0 {
		step()
	}
	return append(events, ir.Event{Kind: ir.Finish, Stop: r.Stop, Usage: r.Usage})
}

// decodeAs reads a stream of the given format in pieces of the given size.
func decodeAs(format Format, raw []byte, piece int) ([]ir.Event, error) {
	target := chatTarget
	if format == Messages {
		target = messagesTarget
	}
	p := sse.NewParser(target.maxFrame)
	d := target.newStreamDecoder()
	var events []ir.Event
	var first error
	feed := func(frames []sse.Frame) {
		for _, f := range frames {
			evs, err := d.Feed(f.Event, f.Data)
			events = append(events, evs...)
			if err != nil && first == nil {
				first = err
			}
		}
	}
	for i := 0; i < len(raw); i += piece {
		frames, err := p.Feed(raw[i:min(i+piece, len(raw))])
		feed(frames)
		if err != nil {
			return events, err
		}
	}
	feed(p.Flush())
	return append(events, d.Close()...), first
}

func TestRoundTrip_Responses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < roundTripCases; i++ {
		seed := int64(20261007 + i)
		rng := rand.New(rand.NewSource(seed))

		// Chat Completions, whole and streamed.
		want := rtResponse(rng, true)
		body, err := chat.EncodeResponse(want, "fallback", now)
		if err != nil {
			t.Fatalf("seed %d: chat.EncodeResponse: %v", seed, err)
		}
		got, err := chat.DecodeResponse(body)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: Chat Completions changed the answer (%v)\nwant %+v\n got %+v\n%s", seed, err, want, got, body)
		}
		events := rtEvents(rng, want)
		collected, err := ir.Collect(events)
		if err != nil || !reflect.DeepEqual(collected, want) {
			t.Fatalf("seed %d: the generated events do not add up to the answer: %v", seed, err)
		}
		var buf bytes.Buffer
		enc := chat.NewStreamEncoder(&buf, "fallback", now, true)
		for _, e := range events {
			if err := enc.Write(e); err != nil {
				t.Fatalf("seed %d: chat.StreamEncoder: %v", seed, err)
			}
		}
		if s, err := chat.CheckStream(buf.Bytes()); err != nil || !s.Done {
			t.Fatalf("seed %d: a client refuses the Chat stream: %v\n%s", seed, err, buf.Bytes())
		}
		decoded, err := decodeAs(Chat, buf.Bytes(), 1+rng.Intn(300))
		if err != nil {
			t.Fatalf("seed %d: the Chat stream cannot be read: %v\n%s", seed, err, buf.Bytes())
		}
		if got, err = ir.Collect(decoded); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: the Chat stream changed the answer (%v)\nwant %+v\n got %+v\n%s", seed, err, want, got, buf.Bytes())
		}

		// Anthropic Messages, whole and streamed.
		want = rtResponse(rng, false)
		if body, err = messages.EncodeResponse(want, "fallback"); err != nil {
			t.Fatalf("seed %d: messages.EncodeResponse: %v", seed, err)
		}
		got, err = messages.DecodeResponse(body)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: Anthropic Messages changed the answer (%v)\nwant %+v\n got %+v\n%s", seed, err, want, got, body)
		}
		events = rtEvents(rng, want)
		if collected, err = ir.Collect(events); err != nil || !reflect.DeepEqual(collected, want) {
			t.Fatalf("seed %d: the generated events do not add up to the answer: %v", seed, err)
		}
		buf.Reset()
		menc := messages.NewStreamEncoder(&buf, "fallback")
		for _, e := range events {
			if err := menc.Write(e); err != nil {
				t.Fatalf("seed %d: messages.StreamEncoder: %v", seed, err)
			}
		}
		if s, err := messages.CheckStream(buf.Bytes()); err != nil || !s.Stopped {
			t.Fatalf("seed %d: a client refuses the Messages stream: %v\n%s", seed, err, buf.Bytes())
		}
		decoded, err = decodeAs(Messages, buf.Bytes(), 1+rng.Intn(300))
		if err != nil {
			t.Fatalf("seed %d: the Messages stream cannot be read: %v\n%s", seed, err, buf.Bytes())
		}
		if got, err = ir.Collect(decoded); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: the Messages stream changed the answer (%v)\nwant %+v\n got %+v\n%s", seed, err, want, got, buf.Bytes())
		}
	}
}
