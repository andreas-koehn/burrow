package chat

import (
	"bytes"
	"encoding/json"
	"sort"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

const (
	// MaxFrameBytes is the largest stream frame Feed takes, and the limit to
	// give sse.NewParser for a Chat Completions stream. One frame can hold a
	// whole tool call (ir.MaxToolArgsBytes of arguments, escaped once more).
	MaxFrameBytes = 4 << 20
	// maxPendingArgsBytes is how much of a tool call's arguments is kept
	// while the call cannot be opened yet because its id or name has not
	// arrived. Providers send both with the first chunk; the buffer is for
	// the few that do not, and it is small so that 64 such calls stay cheap.
	maxPendingArgsBytes = 64 << 10
	// maxErrorBytes is the longest provider error message that is shown.
	maxErrorBytes = 300

	errEarlyEnd = "the provider ended the stream early"
	errProvider = "the provider reported an error"
)

// StreamDecoder turns chat.completion.chunk frames into neutral events.
//
// It is driven by its caller: Feed takes the data of one frame and returns
// the events it produces, Close ends the answer. It starts no goroutine and
// queues nothing; the caller writes the events out before it feeds again,
// so a slow client slows the upstream read. A decoder that is dropped
// half-way needs no cleanup. It is not safe for use by several goroutines.
//
// What it holds is bounded: one state per tool call (at most
// ir.MaxToolCalls), the arguments of a call that cannot be opened yet (at
// most maxPendingArgsBytes each), and counters. Text and argument fragments
// pass through and are not kept.
type StreamDecoder struct {
	started   bool // Start was emitted
	done      bool // Finish or Error was emitted, or Feed failed
	sawFinish bool // a finish_reason arrived
	refused   bool // a refusal text arrived

	open     int // the open Text or Thinking part, or -1
	openKind ir.PartKind
	next     int // the next part's number

	calls   []*toolCall          // in the order they first appeared
	byIndex map[int]*toolCall    // by the chunk's "index"; a map, so an absurd index costs nothing
	byID    map[string]*toolCall // by id
	opened  bool                 // a tool call part was opened

	stop  ir.StopReason
	usage ir.Usage
}

type toolCall struct {
	id, name string
	part     int    // its part number, or -1 while it is not open
	args     int    // bytes of arguments seen so far
	pending  []byte // arguments that arrived before the part could be opened
	closed   bool
}

// NewStreamDecoder returns a decoder for one streamed answer.
func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{open: -1, byIndex: map[int]*toolCall{}, byID: map[string]*toolCall{}}
}

// Feed takes the data of one SSE frame ("[DONE]" included) and returns the
// events it produces; empty data produces none.
//
//   - The first chunk with an id, a model or a choice gives Start.
//   - Reasoning text ("reasoning_content" or "reasoning") and content text
//     each open a part when none of their kind is open, closing the other
//     kind first; empty text opens nothing. A refusal text is content.
//   - A tool call is addressed by its "index" (or, with providers that send
//     none or number every call 0, by its id). Its part opens as soon as id
//     and name are known, closing an open text part first; arguments that
//     came earlier follow as one delta. Calls may interleave.
//   - A finish_reason closes every open part, in part order. A tool call
//     that still has no name or no id by then is an error.
//   - "[DONE]" gives Finish with the stop reason and the usage of the last
//     chunk that had one. Without a finish_reason before it the model never
//     said it was done: the stream ends with Error instead.
//   - An {"error": …} object gives Error with its message, cut to 300
//     bytes; everything after it is ignored.
//
// An error wraps ErrMalformed (a frame that is not JSON, a field of the
// wrong type, a broken tool call) or ir.ErrLimit (a frame over
// MaxFrameBytes or ir.MaxDepth, more than ir.MaxToolCalls tool calls or
// ir.MaxParts parts, a call's arguments over ir.MaxToolArgsBytes). No events
// come with an error, and the decoder is finished: later calls to Feed and
// Close return nothing. The caller ends its own stream with an Error event.
func (d *StreamDecoder) Feed(data []byte) ([]ir.Event, error) {
	if d.done {
		return nil, nil
	}
	evs, err := d.feed(data)
	if err != nil {
		d.end()
		return nil, err
	}
	return evs, nil
}

// Close ends the answer when the upstream stopped: nothing after a Finish or
// an Error; Finish when a finish_reason had arrived and only "[DONE]" was
// missing (several servers leave it out); otherwise a PartStop for every
// open part followed by Error "the provider ended the stream early" — just
// that Error when no chunk was ever seen.
func (d *StreamDecoder) Close() []ir.Event {
	if d.done {
		return nil
	}
	if !d.started {
		d.end()
		return []ir.Event{{Kind: ir.Error, Err: errEarlyEnd}}
	}
	out, _ := d.closeParts(nil, false)
	if d.sawFinish {
		out = append(out, d.finish())
	} else {
		out = append(out, ir.Event{Kind: ir.Error, Err: errEarlyEnd})
	}
	d.end()
	return out
}

// end marks the decoder finished and lets go of what it held.
func (d *StreamDecoder) end() {
	d.done = true
	d.calls, d.byIndex, d.byID = nil, nil, nil
}

func (d *StreamDecoder) finish() ir.Event {
	return ir.Event{Kind: ir.Finish, Stop: settleStop(d.stop, d.refused, d.opened), Usage: d.usage}
}

func (d *StreamDecoder) feed(data []byte) ([]ir.Event, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) > MaxFrameBytes {
		return nil, overLimit("stream frame bytes", MaxFrameBytes)
	}
	if string(data) == "[DONE]" {
		return d.doneFrame()
	}
	if ir.Depth(data) > ir.MaxDepth {
		return nil, overLimit("stream frame nesting", ir.MaxDepth)
	}
	var c wireAnswer
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, jsonError(err)
	}
	if !isNull(c.Error) {
		d.end()
		return []ir.Event{{Kind: ir.Error, Err: errorMessage(c.Error)}}, nil
	}
	choice, err := firstChoice(c.Choices)
	if err != nil {
		return nil, err
	}
	var out []ir.Event
	if !d.started && (c.ID != "" || c.Model != "" || choice != nil) {
		d.started = true
		out = append(out, ir.Event{Kind: ir.Start, ID: c.ID, Model: c.Model})
	}
	if c.Usage != nil {
		d.usage = c.Usage.usage()
	}
	if choice == nil {
		return out, nil
	}
	if delta := choice.Delta; delta != nil {
		if out, err = d.text(out, ir.Thinking, delta.reasoningText()); err != nil {
			return nil, err
		}
		content, ok := rawString(delta.Content)
		if !ok {
			return nil, malformed("a delta's content is not a string")
		}
		if out, err = d.text(out, ir.Text, content); err != nil {
			return nil, err
		}
		if delta.Refusal != "" {
			d.refused = true
			if out, err = d.text(out, ir.Text, delta.Refusal); err != nil {
				return nil, err
			}
		}
		for i := range delta.ToolCalls {
			if out, err = d.toolCall(out, &delta.ToolCalls[i]); err != nil {
				return nil, err
			}
		}
	}
	if choice.FinishReason != "" {
		d.sawFinish = true
		d.stop = stopReason(choice.FinishReason)
		if out, err = d.closeParts(out, true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *StreamDecoder) doneFrame() ([]ir.Event, error) {
	if !d.started {
		d.end()
		return []ir.Event{{Kind: ir.Error, Err: errEarlyEnd}}, nil
	}
	out, err := d.closeParts(nil, true)
	if err != nil {
		return nil, err
	}
	if d.sawFinish {
		out = append(out, d.finish())
	} else {
		out = append(out, ir.Event{Kind: ir.Error, Err: errEarlyEnd})
	}
	d.end()
	return out, nil
}

// newPart numbers the next part.
func (d *StreamDecoder) newPart() (int, error) {
	if d.next >= ir.MaxParts {
		return 0, overLimit("parts", ir.MaxParts)
	}
	d.next++
	return d.next - 1, nil
}

func (d *StreamDecoder) closeText(out []ir.Event) []ir.Event {
	if d.open >= 0 {
		out = append(out, ir.Event{Kind: ir.PartStop, Index: d.open})
		d.open = -1
	}
	return out
}

// text emits a text or thinking delta, opening its part when needed.
func (d *StreamDecoder) text(out []ir.Event, kind ir.PartKind, s string) ([]ir.Event, error) {
	if s == "" {
		return out, nil
	}
	if d.open >= 0 && d.openKind != kind {
		out = d.closeText(out)
	}
	if d.open < 0 {
		n, err := d.newPart()
		if err != nil {
			return nil, err
		}
		d.open, d.openKind = n, kind
		out = append(out, ir.Event{Kind: ir.PartStart, Index: n, Part: ir.Part{Kind: kind}})
	}
	delta := ir.TextDelta
	if kind == ir.Thinking {
		delta = ir.ThinkingDelta
	}
	return append(out, ir.Event{Kind: delta, Index: d.open, Text: s}), nil
}

// find returns the call a tool-call delta belongs to, making a new one when
// it is the first delta of its call.
func (d *StreamDecoder) find(index count, id string) (*toolCall, error) {
	var c *toolCall
	switch {
	case index.bad:
		return nil, malformed("a tool call's index is not a number")
	case index.set:
		// A known index with another id is a new call: some providers number
		// every call 0.
		if c = d.byIndex[index.n]; c != nil && id != "" && c.id != "" && c.id != id {
			c = nil
		}
	case id != "":
		c = d.byID[id]
	case len(d.calls) > 0:
		c = d.calls[len(d.calls)-1] // no index, no id: more of the latest call
	}
	if c == nil {
		if len(d.calls) >= ir.MaxToolCalls {
			return nil, overLimit("tool calls", ir.MaxToolCalls)
		}
		c = &toolCall{part: -1}
		d.calls = append(d.calls, c)
		if index.set {
			d.byIndex[index.n] = c
		}
	}
	return c, nil
}

func (d *StreamDecoder) toolCall(out []ir.Event, tc *wireToolCall) ([]ir.Event, error) {
	c, err := d.find(tc.Index, tc.ID)
	if err != nil {
		return nil, err
	}
	if c.closed {
		return nil, malformed("a tool call delta after the finish")
	}
	if tc.ID != "" && c.id == "" {
		if d.byID[tc.ID] != nil {
			return nil, malformed("two tool calls with one id")
		}
		c.id = tc.ID
		d.byID[tc.ID] = c
	}
	fragment := ""
	if tc.Function != nil {
		if c.name == "" {
			c.name = tc.Function.Name
		}
		if fragment, err = argumentsText(tc.Function.Arguments); err != nil {
			return nil, err
		}
	}
	if c.args += len(fragment); c.args > ir.MaxToolArgsBytes {
		return nil, overLimit("tool call argument bytes", ir.MaxToolArgsBytes)
	}
	if c.part >= 0 {
		if fragment != "" {
			out = append(out, ir.Event{Kind: ir.ToolArgsDelta, Index: c.part, ArgsJSON: fragment})
		}
		return out, nil
	}
	if c.id == "" || c.name == "" {
		if len(c.pending)+len(fragment) > maxPendingArgsBytes {
			return nil, overLimit("tool call argument bytes before the call's name and id", maxPendingArgsBytes)
		}
		c.pending = append(c.pending, fragment...)
		return out, nil
	}
	out = d.closeText(out)
	if c.part, err = d.newPart(); err != nil {
		return nil, err
	}
	d.opened = true
	out = append(out, ir.Event{Kind: ir.PartStart, Index: c.part, Part: ir.Part{Kind: ir.ToolUse, ToolID: c.id, ToolName: c.name}})
	if args := string(c.pending) + fragment; args != "" {
		out = append(out, ir.Event{Kind: ir.ToolArgsDelta, Index: c.part, ArgsJSON: args})
	}
	c.pending = nil
	return out, nil
}

// closeParts emits a PartStop for every open part, in part order. With
// strict, a tool call that never got a name and an id is an error; without
// (the upstream is gone anyway) it is passed over.
func (d *StreamDecoder) closeParts(out []ir.Event, strict bool) ([]ir.Event, error) {
	var open []int
	if d.open >= 0 {
		open = append(open, d.open)
		d.open = -1
	}
	for _, c := range d.calls {
		if c.closed {
			continue
		}
		c.closed = true
		c.pending = nil
		if c.part < 0 {
			if strict {
				return nil, malformed("a tool call without a name or an id")
			}
			continue
		}
		open = append(open, c.part)
	}
	sort.Ints(open)
	for _, n := range open {
		out = append(out, ir.Event{Kind: ir.PartStop, Index: n})
	}
	return out, nil
}

// errorMessage returns what a provider's error object says: its "message",
// or the error itself when it is a string, cut to maxErrorBytes at a
// character boundary; a fixed text when it says nothing.
func errorMessage(raw json.RawMessage) string {
	s, ok := rawString(raw)
	if !ok {
		var obj struct {
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			s, _ = rawString(obj.Message)
		}
	}
	if len(s) > maxErrorBytes {
		n := maxErrorBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	if s == "" {
		return errProvider
	}
	return s
}
