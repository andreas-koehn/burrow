package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// MaxFrameBytes is the largest stream frame Feed takes, and the limit to
// give sse.NewParser for a Messages stream. One frame can hold a whole tool
// call's input (ir.MaxToolArgsBytes, escaped once more).
const MaxFrameBytes = 4 << 20

// StreamDecoder turns the events of an Anthropic Messages stream into
// neutral events.
//
// It is driven by its caller: Feed takes one frame and returns the events it
// produces, Close ends the answer. It starts no goroutine and queues
// nothing; the caller writes the events out before it feeds again, so a slow
// client slows the upstream read. A decoder that is dropped half-way needs
// no cleanup. It is not safe for use by several goroutines.
//
// What it holds is bounded: one state per content block (at most
// ir.MaxParts), the arguments of the tool calls that are open, which are
// checked when the call stops (at most ir.MaxToolArgsBytes each and
// ir.MaxTotalToolArgsBytes together), up to three bytes of a character that
// was cut between two deltas, and counters. Text passes through and is not
// kept.
type StreamDecoder struct {
	out []ir.Event // the events of the Feed or Close that is running

	started bool // message_start was read
	done    bool // Finish or Error was emitted

	blocks   map[int]*block // by the stream's "index"; a map, so an absurd index costs nothing
	order    []*block       // in the order they started
	textOpen *block         // the text or thinking block that has not stopped, or nil
	next     int            // the next part's number

	ids       map[string]bool // the ids of the tool calls
	argsBytes int             // argument bytes of all calls

	stop   ir.StopReason
	input  int // input tokens: message_start's, or message_delta's when it tells
	output int
}

// block is one content block of the stream.
type block struct {
	kind   ir.PartKind // "" for a block that is passed over
	part   int         // its part number, or -1 while no part was opened for it
	closed bool
	carry  []byte // the first bytes of a character whose rest has not arrived

	// A tool call.
	args       []byte // the arguments so far, without white space before them
	startInput []byte // the input its start carried, used when no delta brings any
}

// NewStreamDecoder returns a decoder for one streamed answer.
func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{blocks: map[int]*block{}, ids: map[string]bool{}}
}

type wireEvent struct {
	Type    string          `json:"type"`
	Index   count           `json:"index"`
	Message *wireAnswer     `json:"message"`
	Block   *wireStartBlock `json:"content_block"`
	Delta   *wireDelta      `json:"delta"`
	Usage   *wireUsage      `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

// wireStartBlock is the block of a content_block_start; its texts are kept
// as bytes (see ir.StringBytes).
type wireStartBlock struct {
	Type     string          `json:"type"`
	Text     json.RawMessage `json:"text"`
	Thinking json.RawMessage `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type wireDelta struct {
	Type        string          `json:"type"`
	Text        json.RawMessage `json:"text"`
	Thinking    json.RawMessage `json:"thinking"`
	PartialJSON json.RawMessage `json:"partial_json"`
	StopReason  *string         `json:"stop_reason"` // message_delta
}

// Feed takes one SSE frame — its event name and its data — and returns the
// events it produces. The kind of a frame is the "type" of its data; the
// event name is read only when the data has none. The events follow the
// rules written down at ir.Event.
//
//   - message_start gives Start: id, model and the input tokens (cached ones
//     included, see wireUsage).
//   - content_block_start opens a block. A tool_use block is a ToolUse part
//     at once and needs an id of its own and a name. A text or thinking
//     block becomes a part with its first text that is not empty, so a block
//     without text is no part. Any other block type (redacted_thinking,
//     server_tool_use, …) is passed over with its deltas and its stop.
//   - content_block_delta: text_delta, thinking_delta and input_json_delta
//     give the deltas of their block; signature_delta, citations_delta and
//     delta types nobody here knows are passed over. A character cut in two
//     between deltas (as bytes, or as the two halves of an escaped surrogate
//     pair) is put together again. White space before a call's arguments is
//     not passed on.
//   - content_block_stop stops the block's part. Before that a tool call is
//     checked: its arguments are one JSON object or empty. A call that got
//     no input_json_delta takes the input its start carried, as the SDKs do.
//   - message_delta is remembered: the stop reason and the usage.
//   - message_stop stops what is still open, checked the same way, and gives
//     Finish.
//   - ping, and event types nobody here knows, give nothing.
//
// The answer fails — a PartStop for every open part, in part order, then
// one Error, and the decoder is finished — when
//   - an error event comes (overloaded_error, …): the Error carries its
//     message, cut to 300 bytes;
//   - the frame or the answer is malformed (not JSON, a field of the wrong
//     type, a second message_start, a content event before message_start, a
//     delta or a stop for an index that never started or has stopped, a
//     second start of an index, a block that starts while a text block is
//     open, a delta of a kind its block cannot hold, a tool_use block
//     without id or name or with an id that was used, arguments that are no
//     JSON object or hold bytes that are not UTF-8 (text is repaired with
//     U+FFFD, arguments never are), a character whose second half never
//     came) or over a limit (MaxFrameBytes, ir.MaxDepth, ir.MaxParts,
//     ir.MaxToolCalls, ir.MaxToolArgsBytes, ir.MaxTotalToolArgsBytes): the
//     Error carries a fixed text.
//
// Only in the last case Feed returns an error as well, wrapping ErrMalformed
// or ir.ErrLimit, so that the caller can tell what happened. The events
// returned with it already end the answer: the caller writes them and adds
// no Error of its own. After the answer has ended, Feed and Close return
// nothing.
func (d *StreamDecoder) Feed(event string, data []byte) ([]ir.Event, error) {
	if d.done {
		return nil, nil
	}
	err := d.feed(event, data)
	if err != nil {
		text := errUnreadable
		if errors.Is(err, ir.ErrLimit) {
			text = errTooLarge
		}
		d.fail(text)
	}
	out := d.out
	d.out = nil
	return out, err
}

// Close ends the answer when the upstream stopped: nothing after a Finish or
// an Error; otherwise a PartStop for every open part followed by Error "the
// provider ended the stream early" — just that Error when nothing was ever
// read. A stream without message_stop is never an answer, whatever it held:
// the model did not say it was done.
func (d *StreamDecoder) Close() []ir.Event {
	if d.done {
		return nil
	}
	d.fail(errEarlyEnd)
	out := d.out
	d.out = nil
	return out
}

func (d *StreamDecoder) emit(ev ir.Event) { d.out = append(d.out, ev) }

// end marks the answer ended and lets go of what the decoder held.
func (d *StreamDecoder) end() {
	d.done = true
	d.blocks, d.order, d.textOpen, d.ids = nil, nil, nil, nil
}

// fail ends the answer with an error: a PartStop for every open part, in
// part order (the order the blocks started in), then the one Error.
func (d *StreamDecoder) fail(message string) {
	for _, b := range d.order {
		if b.part >= 0 && !b.closed {
			d.emit(ir.Event{Kind: ir.PartStop, Index: b.part})
		}
	}
	d.emit(ir.Event{Kind: ir.Error, Err: message})
	d.end()
}

func (d *StreamDecoder) feed(event string, data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if len(data) > MaxFrameBytes {
		return answerOver("stream frame bytes", MaxFrameBytes)
	}
	if ir.Depth(data) > ir.MaxDepth {
		return answerOver("stream frame nesting", ir.MaxDepth)
	}
	var ev wireEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return jsonError(err)
	}
	kind := ev.Type
	if kind == "" {
		kind = event
	}
	switch kind {
	case "error":
		d.fail(errorMessage(ev.Error))
		return nil
	case "message_start":
		if d.started {
			return malformed("a second message_start")
		}
		d.started = true
		start := ir.Event{Kind: ir.Start}
		if m := ev.Message; m != nil {
			start.ID, start.Model = m.ID, m.Model
			if m.Usage != nil {
				d.input, _ = m.Usage.input()
			}
		}
		start.Usage.InputTokens = d.input
		d.emit(start)
		return nil
	case "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop":
		if !d.started {
			return malformed(kind + " before message_start")
		}
	default:
		return nil // ping, and what nobody here knows
	}
	switch kind {
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != nil {
			d.stop = decodeStop(*ev.Delta.StopReason)
		}
		if u := ev.Usage; u != nil {
			if n, told := u.input(); told && n > 0 {
				d.input = n
			}
			if u.Output.set {
				d.output = u.Output.n
			}
		}
		return nil
	case "message_stop":
		for _, b := range d.order {
			if err := d.closeBlock(b); err != nil {
				return err
			}
		}
		d.emit(ir.Event{Kind: ir.Finish, Stop: settleStop(d.stop, len(d.ids) > 0), Usage: ir.Usage{InputTokens: d.input, OutputTokens: d.output}})
		d.end()
		return nil
	}
	if !ev.Index.set {
		return malformed(kind + " without a usable index")
	}
	b := d.blocks[ev.Index.n]
	if kind == "content_block_start" {
		if b != nil {
			return malformed("a second start of one block")
		}
		return d.startBlock(ev.Index.n, ev.Block)
	}
	if b == nil {
		return malformed(kind + " for a block that never started")
	}
	if b.closed {
		return malformed(kind + " for a block that has stopped")
	}
	if kind == "content_block_stop" {
		return d.closeBlock(b)
	}
	if ev.Delta == nil || b.kind == "" {
		return nil
	}
	switch ev.Delta.Type {
	case "text_delta":
		return d.text(b, ir.Text, ev.Delta.Text)
	case "thinking_delta":
		return d.text(b, ir.Thinking, ev.Delta.Thinking)
	case "input_json_delta":
		return d.arguments(b, ev.Delta.PartialJSON)
	}
	return nil // signature_delta, citations_delta, and what nobody here knows
}

func (d *StreamDecoder) startBlock(index int, wire *wireStartBlock) error {
	if wire == nil {
		return malformed("content_block_start without a block")
	}
	if d.textOpen != nil {
		return malformed("a block starts while a text block is open")
	}
	if len(d.order) >= ir.MaxParts {
		return answerOver("content blocks", ir.MaxParts)
	}
	b := &block{part: -1}
	switch wire.Type {
	case "text", "thinking":
		b.kind, d.textOpen = ir.Text, b
		first := wire.Text
		if wire.Type == "thinking" {
			b.kind, first = ir.Thinking, wire.Thinking
		}
		d.blocks[index], d.order = b, append(d.order, b)
		return d.text(b, b.kind, first)
	case "tool_use":
		if wire.ID == "" || wire.Name == "" {
			return malformed("a tool call without a name or an id")
		}
		// Before anything of the call is kept: its id stays for the whole
		// answer, and both are handed on.
		if len(wire.ID) > ir.MaxToolIDBytes {
			return answerOver("tool call id bytes", ir.MaxToolIDBytes)
		}
		if len(wire.Name) > ir.MaxToolNameBytes {
			return answerOver("tool call name bytes", ir.MaxToolNameBytes)
		}
		if d.ids[wire.ID] {
			return malformed("two tool calls with one id")
		}
		if len(d.ids) >= ir.MaxToolCalls {
			return answerOver("tool calls", ir.MaxToolCalls)
		}
		input, err := blockInput(wire.Input)
		if err != nil {
			return err
		}
		if string(input) != "{}" {
			// Held until the block stops; it counts as arguments from now on.
			if d.argsBytes += len(input); d.argsBytes > ir.MaxTotalToolArgsBytes {
				return answerOver("tool call argument bytes in all", ir.MaxTotalToolArgsBytes)
			}
			b.startInput = bytes.Clone(input)
		}
		b.kind, b.part = ir.ToolUse, d.next
		d.next++
		d.ids[wire.ID] = true
		d.emit(ir.Event{Kind: ir.PartStart, Index: b.part, Part: ir.Part{Kind: ir.ToolUse, ToolID: wire.ID, ToolName: wire.Name}})
	}
	d.blocks[index], d.order = b, append(d.order, b)
	return nil
}

// text emits a text or thinking delta of block b, opening its part with the
// first text that is not empty.
func (d *StreamDecoder) text(b *block, kind ir.PartKind, raw json.RawMessage) error {
	if b.kind != kind {
		return malformed("a text delta for a block of another kind")
	}
	piece, ok := ir.StringBytes(raw)
	if !ok {
		return malformed("a delta's text is not a string")
	}
	if len(piece) == 0 {
		return nil
	}
	s := ir.Whole(&b.carry, piece)
	if s == "" {
		return nil
	}
	if b.part < 0 {
		b.part = d.next
		d.next++
		d.emit(ir.Event{Kind: ir.PartStart, Index: b.part, Part: ir.Part{Kind: kind}})
	}
	delta := ir.TextDelta
	if kind == ir.Thinking {
		delta = ir.ThinkingDelta
	}
	d.emit(ir.Event{Kind: delta, Index: b.part, Text: s})
	return nil
}

// arguments emits the next piece of a tool call's arguments.
func (d *StreamDecoder) arguments(b *block, raw json.RawMessage) error {
	if b.kind != ir.ToolUse {
		return malformed("an argument delta for a block that is no tool call")
	}
	piece, ok := ir.StringBytes(raw)
	if !ok {
		return malformed("a delta's partial_json is not a string")
	}
	// Arguments are passed on or refused, never repaired (text is).
	piece = ir.Complete(&b.carry, piece)
	if !utf8.Valid(piece) {
		return malformed("tool call arguments are not valid UTF-8")
	}
	if len(b.args) == 0 {
		piece = bytes.TrimLeft(piece, " \t\r\n")
	}
	if len(piece) == 0 {
		return nil
	}
	return d.addArguments(b, piece)
}

func (d *StreamDecoder) addArguments(b *block, piece []byte) error {
	if b.startInput != nil {
		// The deltas are the input: what the start carried is let go.
		d.argsBytes -= len(b.startInput)
		b.startInput = nil
	}
	if len(b.args)+len(piece) > ir.MaxToolArgsBytes {
		return answerOver("tool call argument bytes", ir.MaxToolArgsBytes)
	}
	if d.argsBytes += len(piece); d.argsBytes > ir.MaxTotalToolArgsBytes {
		return answerOver("tool call argument bytes in all", ir.MaxTotalToolArgsBytes)
	}
	b.args = append(b.args, piece...)
	d.emit(ir.Event{Kind: ir.ToolArgsDelta, Index: b.part, ArgsJSON: string(piece)})
	return nil
}

// closeBlock stops a block that is open: a tool call is checked first, and
// a block whose check fails is left for fail to stop.
func (d *StreamDecoder) closeBlock(b *block) error {
	if b.closed {
		return nil
	}
	if len(b.carry) > 0 {
		b.carry = nil
		return malformed("a block ends inside a character")
	}
	if b.kind == ir.ToolUse {
		if input := b.startInput; input != nil && len(b.args) == 0 {
			b.startInput = nil
			d.argsBytes -= len(input)
			if err := d.addArguments(b, input); err != nil {
				return err
			}
		}
		if len(b.args) > 0 {
			// The pieces were sent on as they came; the caller is told now
			// that they are no object, before it sees a Finish.
			if err := ir.CheckObject(b.args); errors.Is(err, ir.ErrLimit) {
				return err
			} else if err != nil {
				return malformed("tool call arguments are not a JSON object")
			}
		}
		b.args = nil
	}
	b.closed = true
	if d.textOpen == b {
		d.textOpen = nil
	}
	if b.part >= 0 {
		d.emit(ir.Event{Kind: ir.PartStop, Index: b.part})
	}
	return nil
}
