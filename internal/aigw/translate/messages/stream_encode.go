package messages

import (
	"fmt"
	"io"
	"strconv"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// The texts of the error events an encoder makes up itself. They are fixed:
// nothing of the answer is in them.
const (
	errEarlyEnd   = "the provider ended the stream early"
	errProvider   = "the provider reported an error"
	errUnreadable = "the provider sent an answer that cannot be read"
	errTooLarge   = "the provider's answer is too large"
)

// streamErrorType is the Anthropic error type of an error event. A stream
// that fails has no HTTP status left to tell more.
const streamErrorType = "api_error"

// maxHeldPieces is how many deltas of one held part are kept apart, so that
// they are replayed as they came. Further ones are joined into one last
// delta: a part held back in one-byte pieces must not cost a slice header
// for every byte.
const maxHeldPieces = 256

// StreamEncoder writes neutral events as an Anthropic Messages stream:
//
//	message_start
//	( content_block_start, content_block_delta…, content_block_stop )…
//	message_delta, message_stop
//
// Every frame is "event: <type>" and one "data:" line of JSON whose "type"
// is the event's name, written with a single call to the writer.
//
// # One block at a time
//
// A Messages client takes content blocks strictly one after the other,
// numbered 0, 1, 2… The neutral events allow more (see ir.Event): tool
// calls open side by side with their argument pieces interleaved, and a
// text opening while they are open. The encoder therefore keeps one block
// open on the wire. The first part that opens is written live, delta by
// delta. A part that opens while a block is open is held back, with the
// deltas it gets; when the open block stops, the held parts are written in
// the order they opened, each with its deltas as they came, until one is
// reached that has not stopped yet: that one is the live block from then
// on. The bytes of a part are never reordered, joined with another part's
// or changed.
//
// What is held is bounded: at most ir.MaxParts parts and
// ir.MaxTotalToolArgsBytes of text and arguments together, and at most
// maxHeldPieces separate deltas per part. An answer over a limit ends with
// an error event.
//
// # Tool calls
//
// A tool_use block starts with "input":{} and gets its arguments as
// input_json_delta events whose "partial_json" pieces are the upstream's
// bytes: joined, they are the upstream's arguments. A call without
// arguments gets one input_json_delta "{}" before it stops, so the pieces of
// every block add up to one JSON object, for a client that parses the
// joined text as well as for the SDK accumulators (which keep the start's
// {} unless the joined text is not empty).
//
// # Usage and stop reason
//
// message_start carries the input tokens the Start event has; a target that
// reports usage only at the end (Chat Completions) has none, so 0 stands
// there. message_delta carries the stop reason and the upstream's final
// figures in "usage": "output_tokens" and "input_tokens", both of which
// Anthropic allows there.
//
// # Failure
//
// An Error event, Close before the Finish, a sequence that breaks the rules
// of ir.Event, a limit: the open block is stopped, held parts are written
// and stopped, and then one error event ends the stream:
//
//	event: error
//	data: {"type":"error","error":{"type":"api_error","message":"…"}}
//
// No message_delta and no message_stop are written, so no client takes the
// answer for complete. Nothing is written after the end, whatever is
// handed in. An Error before the Start writes the error event alone (a
// gateway that has not sent its header yet does better to answer with an
// HTTP error then, and does not hand the event in).
//
// An encoder is not safe for use by several goroutines.
type StreamEncoder struct {
	w     io.Writer
	model string // the model that was asked for

	started bool
	done    bool  // the stream was ended, well or badly
	werr    error // the writer failed: nothing more is written

	inputTokens int // what message_start told

	parts     []part // by neutral index
	ids       map[string]bool
	calls     int
	argBytes  int         // argument bytes of all tool calls
	cur       int         // the neutral index of the block open on the wire, or -1
	wire      int         // blocks written so far: the next wire index
	held      []*heldPart // parts that wait for the wire, in the order they opened
	heldBytes int

	buf []byte // the frame being built
}

type part struct {
	kind     ir.PartKind
	open     bool
	argBytes int  // a tool call's argument bytes so far
	wroteArg bool // an input_json_delta was written for it
}

type heldPart struct {
	index   int
	start   ir.Part
	pieces  []string
	tail    []byte // the deltas after maxHeldPieces, joined
	stopped bool
}

// NewStreamEncoder returns an encoder that writes one answer to w.
// fallbackModel is named in message_start when the upstream names none.
func NewStreamEncoder(w io.Writer, fallbackModel string) *StreamEncoder {
	return &StreamEncoder{w: w, model: fallbackModel, cur: -1, ids: map[string]bool{}}
}

// Write takes the next event. It returns nil for an event that was written,
// held back or ignored (anything after the end); an error wrapping
// ir.ErrSequence or ir.ErrLimit when the event broke the rules or a limit,
// in which case the stream has been ended with an error event; or the
// writer's error, after which nothing more is written and every call
// returns it.
func (e *StreamEncoder) Write(ev ir.Event) error {
	if e.werr != nil {
		return e.werr
	}
	if e.done {
		return nil
	}
	if ev.Kind == ir.Error {
		msg := ev.Err
		if msg == "" {
			msg = errProvider
		}
		e.abort(msg)
		return e.werr
	}
	if e.started == (ev.Kind == ir.Start) {
		return e.sequence("the stream must begin with exactly one start")
	}
	switch ev.Kind {
	case ir.Start:
		e.started = true
		e.inputTokens = max(ev.Usage.InputTokens, 0)
		e.buf = append(e.buf[:0], `{"type":"message_start","message":`...)
		e.buf = appendMessageHead(e.buf, ev.ID, ev.Model, e.model)
		e.buf = append(e.buf, `"content":[],"stop_reason":null,"stop_sequence":null,`...)
		e.buf = appendUsage(e.buf, ir.Usage{InputTokens: e.inputTokens})
		e.buf = append(e.buf, `}}`...)
		e.frame("message_start")
	case ir.PartStart:
		return e.partStart(ev)
	case ir.TextDelta, ir.ThinkingDelta:
		kind := ir.Text
		if ev.Kind == ir.ThinkingDelta {
			kind = ir.Thinking
		}
		if !e.isOpen(ev.Index, kind) {
			return e.sequence("a text delta for a part that is not open")
		}
		return e.delta(ev.Index, ev.Text)
	case ir.ToolArgsDelta:
		if !e.isOpen(ev.Index, ir.ToolUse) {
			return e.sequence("an argument delta for a tool call that is not open")
		}
		p := &e.parts[ev.Index]
		if p.argBytes += len(ev.ArgsJSON); p.argBytes > ir.MaxToolArgsBytes {
			return e.limit(fmt.Sprintf("tool arguments over %d bytes", ir.MaxToolArgsBytes))
		}
		if e.argBytes += len(ev.ArgsJSON); e.argBytes > ir.MaxTotalToolArgsBytes {
			return e.limit(fmt.Sprintf("tool arguments over %d bytes in all", ir.MaxTotalToolArgsBytes))
		}
		return e.delta(ev.Index, ev.ArgsJSON)
	case ir.PartStop:
		if !e.isOpen(ev.Index, "") {
			return e.sequence("a stop for a part that is not open")
		}
		e.parts[ev.Index].open = false
		if ev.Index == e.cur {
			e.stopBlock()
			e.drain()
		} else if h := e.heldAt(ev.Index); h != nil {
			h.stopped = true
		}
	case ir.Finish:
		// Nothing is held when nothing is open: the last stop drained it.
		if e.cur >= 0 || len(e.held) > 0 {
			return e.sequence("the finish came while a part was open")
		}
		usage := ev.Usage
		if usage.InputTokens <= 0 {
			usage.InputTokens = e.inputTokens
		}
		e.buf = append(e.buf[:0], `{"type":"message_delta","delta":{"stop_reason":"`...)
		e.buf = append(e.buf, stopReason(ev.Stop)...)
		e.buf = append(e.buf, `","stop_sequence":null},`...)
		e.buf = appendUsage(e.buf, usage)
		e.buf = append(e.buf, '}')
		e.frame("message_delta")
		e.buf = append(e.buf[:0], `{"type":"message_stop"}`...)
		e.frame("message_stop")
		e.end()
	default:
		return e.sequence("an event of an unknown kind")
	}
	return e.werr
}

// Close ends the stream when the events stopped coming: nothing after a
// Finish or an Error; otherwise what Failure describes at StreamEncoder,
// with the text "the provider ended the stream early". It returns the
// writer's error, if there was one.
func (e *StreamEncoder) Close() error {
	if e.werr == nil && !e.done {
		e.abort(errEarlyEnd)
	}
	return e.werr
}

func (e *StreamEncoder) partStart(ev ir.Event) error {
	if ev.Index != len(e.parts) {
		return e.sequence("a part's number is out of order")
	}
	if len(e.parts) >= ir.MaxParts {
		return e.limit(fmt.Sprintf("more than %d parts", ir.MaxParts))
	}
	p := ir.Part{Kind: ev.Part.Kind}
	switch p.Kind {
	case ir.Text, ir.Thinking:
	case ir.ToolUse:
		p.ToolID, p.ToolName = ev.Part.ToolID, ev.Part.ToolName
		if p.ToolID == "" || p.ToolName == "" || e.ids[p.ToolID] {
			return e.sequence("a tool call needs a name and an id of its own")
		}
		if e.calls >= ir.MaxToolCalls {
			return e.limit(fmt.Sprintf("more than %d tool calls", ir.MaxToolCalls))
		}
		e.calls++
		e.ids[p.ToolID] = true
	default:
		return e.sequence("a part of a kind an answer cannot hold")
	}
	e.parts = append(e.parts, part{kind: p.Kind, open: true})
	if e.cur < 0 && len(e.held) == 0 {
		e.cur = ev.Index
		e.startBlock(p)
		return e.werr
	}
	e.held = append(e.held, &heldPart{index: ev.Index, start: p})
	return nil
}

// isOpen reports whether index is a part that started and has not stopped,
// of the given kind ("" for any).
func (e *StreamEncoder) isOpen(index int, kind ir.PartKind) bool {
	if index < 0 || index >= len(e.parts) || !e.parts[index].open {
		return false
	}
	return kind == "" || e.parts[index].kind == kind
}

// heldAt returns the held part with the given neutral index: every open
// part is the live block or held. nil when there is none.
func (e *StreamEncoder) heldAt(index int) *heldPart {
	for _, h := range e.held {
		if h.index == index {
			return h
		}
	}
	return nil
}

// delta writes a piece of the live block, or keeps a piece of a held part.
func (e *StreamEncoder) delta(index int, s string) error {
	if s == "" {
		return nil
	}
	if index == e.cur {
		e.writeDelta(e.parts[index].kind, s)
		return e.werr
	}
	if e.heldBytes += len(s); e.heldBytes > ir.MaxTotalToolArgsBytes {
		return e.limit(fmt.Sprintf("more than %d bytes held back", ir.MaxTotalToolArgsBytes))
	}
	h := e.heldAt(index)
	if h == nil {
		return e.sequence("a delta for a part that is not open")
	}
	if len(h.pieces) < maxHeldPieces {
		h.pieces = append(h.pieces, s)
	} else {
		h.tail = append(h.tail, s...)
	}
	return nil
}

// drain writes the held parts, in order, up to and including the first one
// that has not stopped: that one becomes the live block.
func (e *StreamEncoder) drain() {
	for len(e.held) > 0 && e.werr == nil {
		h := e.held[0]
		e.held[0] = nil
		e.held = e.held[1:]
		e.cur = h.index
		e.startBlock(h.start)
		for _, s := range h.pieces {
			e.heldBytes -= len(s)
			e.writeDelta(h.start.Kind, s)
		}
		if len(h.tail) > 0 {
			e.heldBytes -= len(h.tail)
			e.writeDelta(h.start.Kind, string(h.tail))
		}
		if !h.stopped {
			return
		}
		e.stopBlock()
	}
}

func (e *StreamEncoder) startBlock(p ir.Part) {
	e.buf = append(e.buf[:0], `{"type":"content_block_start","index":`...)
	e.buf = strconv.AppendInt(e.buf, int64(e.wire), 10)
	switch p.Kind {
	case ir.ToolUse:
		e.buf = append(e.buf, `,"content_block":{"type":"tool_use","id":`...)
		e.buf = ir.AppendString(e.buf, p.ToolID)
		e.buf = append(e.buf, `,"name":`...)
		e.buf = ir.AppendString(e.buf, p.ToolName)
		e.buf = append(e.buf, `,"input":{}}}`...)
	case ir.Thinking:
		e.buf = append(e.buf, `,"content_block":{"type":"thinking","thinking":"","signature":""}}`...)
	default:
		e.buf = append(e.buf, `,"content_block":{"type":"text","text":""}}`...)
	}
	e.frame("content_block_start")
}

func (e *StreamEncoder) writeDelta(kind ir.PartKind, s string) {
	e.buf = append(e.buf[:0], `{"type":"content_block_delta","index":`...)
	e.buf = strconv.AppendInt(e.buf, int64(e.wire), 10)
	switch kind {
	case ir.ToolUse:
		e.parts[e.cur].wroteArg = true
		e.buf = append(e.buf, `,"delta":{"type":"input_json_delta","partial_json":`...)
	case ir.Thinking:
		e.buf = append(e.buf, `,"delta":{"type":"thinking_delta","thinking":`...)
	default:
		e.buf = append(e.buf, `,"delta":{"type":"text_delta","text":`...)
	}
	e.buf = ir.AppendString(e.buf, s)
	e.buf = append(e.buf, `}}`...)
	e.frame("content_block_delta")
}

// stopBlock stops the block that is open on the wire. A tool call that got
// no arguments gets "{}" first (see Tool calls at StreamEncoder).
func (e *StreamEncoder) stopBlock() {
	if p := e.parts[e.cur]; p.kind == ir.ToolUse && !p.wroteArg {
		e.writeDelta(ir.ToolUse, "{}")
	}
	e.buf = append(e.buf[:0], `{"type":"content_block_stop","index":`...)
	e.buf = strconv.AppendInt(e.buf, int64(e.wire), 10)
	e.buf = append(e.buf, '}')
	e.frame("content_block_stop")
	e.wire++
	e.cur = -1
}

// abort ends the stream badly: the open block is stopped, held parts are
// written and stopped, then the one error event.
func (e *StreamEncoder) abort(message string) {
	if e.cur >= 0 {
		e.stopBlock()
	}
	for _, h := range e.held {
		h.stopped = true
	}
	e.drain()
	e.buf = appendError(e.buf[:0], streamErrorType, message)
	e.frame("error")
	e.end()
}

func (e *StreamEncoder) sequence(what string) error {
	e.abort(errUnreadable)
	return fmt.Errorf("%w: %s", ir.ErrSequence, what)
}

func (e *StreamEncoder) limit(what string) error {
	e.abort(errTooLarge)
	return fmt.Errorf("%w: %s", ir.ErrLimit, what)
}

// end marks the stream ended and lets go of what the encoder held.
func (e *StreamEncoder) end() {
	e.done = true
	e.parts, e.ids, e.held, e.buf = nil, nil, nil, nil
}

// frame writes e.buf as one event, unless the writer has failed.
func (e *StreamEncoder) frame(event string) {
	if e.werr != nil {
		return
	}
	e.werr = sse.Write(e.w, event, e.buf)
}
