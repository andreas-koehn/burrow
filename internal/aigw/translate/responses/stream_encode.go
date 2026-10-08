package responses

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// The texts of the failures an encoder makes up itself. They are fixed:
// nothing of the answer is in them.
const (
	errEarlyEnd   = "the provider ended the stream early"
	errProvider   = "the provider reported an error"
	errUnreadable = "the provider sent an answer that cannot be read"
	errTooLarge   = "the provider's answer is too large"
)

// Limits of a streamed answer besides those of package ir. The done events
// and the final response repeat what the deltas brought, so the encoder
// keeps it; these bound what it keeps. Both count the bytes as they are
// written — the text or the arguments as a JSON string, without the quotes
// — so text that JSON has to escape cannot grow past them on the wire.
const (
	// MaxItemBytes is the largest text of one output item (a message's
	// text, a reasoning summary). A call's arguments are bounded more
	// tightly by ir.MaxToolArgsBytes.
	MaxItemBytes = 4 << 20
	// MaxOutputBytes is the largest sum of the texts and arguments of all
	// output items of one answer: what the final event repeats.
	MaxOutputBytes = 16 << 20
)

// maxHeldPieces is how many deltas of one held part are kept apart, so that
// they are replayed as they came. Further ones are joined into one last
// delta: a part held back in one-byte pieces must not cost a slice header
// for every byte.
const maxHeldPieces = 256

// pingAfter is how long the wire may stay silent, while events arrive and
// nothing of them can be written, before a keep-alive is written.
const pingAfter = time.Second

// keepAliveFrame is an SSE comment: a line no event-stream parser hands to
// its client. The Responses stream has no ping event of its own.
const keepAliveFrame = ": keep-alive\n\n"

// StreamEncoder writes neutral events as an OpenAI Responses stream:
//
//	response.created, response.in_progress
//	per output item, one after the other:
//	  text:      response.output_item.added, response.content_part.added,
//	             response.output_text.delta…, response.output_text.done,
//	             response.content_part.done, response.output_item.done
//	  tool call: response.output_item.added,
//	             response.function_call_arguments.delta…,
//	             response.function_call_arguments.done, response.output_item.done
//	  thinking:  response.output_item.added, response.output_item.done
//	response.completed | response.incomplete | response.failed
//
// Every frame is "event: <type>" and one "data:" line of JSON whose "type"
// is the event's name and whose "sequence_number" counts the frames from 0,
// written with a single call to the writer. The first and the last event
// carry the response object; the last one's "output" holds every item in
// full and its "usage" the upstream's figures.
//
// Items are numbered from 0 by "output_index", and their ids say so:
// "msg_0", "fc_1", "rs_2". The ids are made up here, are the same in every
// event of an item, and mean nothing beyond the response: no response is
// stored, so nothing can refer to them later. A tool call's "call_id" is
// the upstream's id of the call.
//
// # One item at a time
//
// Clients (the OpenAI SDKs, Codex) are written against streams whose output
// items come strictly one after the other. The neutral events allow more
// (see ir.Event): tool calls open side by side with their argument pieces
// interleaved, and a text opening while they are open. The encoder
// therefore keeps one item open on the wire. The first part that opens is
// written live, delta by delta. A part that opens while an item is open is
// held back, with the deltas it gets; when the open item is done, the held
// parts are written in the order they opened, each with its deltas as they
// came, until one is reached that has not stopped yet: that one is the live
// item from then on. The bytes of a part are never reordered, joined with
// another part's or changed.
//
// What is held is bounded: at most ir.MaxParts parts and
// ir.MaxTotalToolArgsBytes of text and arguments together, and at most
// maxHeldPieces separate deltas per part.
//
// # Tool calls
//
// A function_call item is added with "arguments":"" and gets its arguments
// as response.function_call_arguments.delta events whose "delta" pieces are
// the upstream's bytes. The joined deltas, the "arguments" of the done
// event, of the done item and of the item in the final response are the
// same string. A call the upstream gave no arguments (or only white space)
// gets one delta "{}" before it is done: Codex parses "arguments" as JSON
// and the SDKs add the deltas to the "" of the added item, so every one of
// them reads the empty object, as with OpenAI itself.
//
// A call that stops without a byte of arguments is not done at once. A
// stream decoder stops every open part before it reports a failure, so
// such a call may be one that was cut before its first byte, and "{}" would
// be an invention a client then runs. Its "{}" and its done events are held
// until the next event shows that the stream goes on — a part that starts,
// a delta, the Finish; the stop of another part does not show it — and
// when the stream fails instead, the call is dropped like any cut call.
//
// A call is done only with arguments that are one JSON object. Codex has no
// status on a function call: it records and runs every call that is done,
// and sends it back as history with every later request. So when a call
// ends — it stopped, or the stream is being ended badly — its arguments are
// checked, and a call that fails the check gets no
// response.function_call_arguments.done and no response.output_item.done
// and is left out of the final "output": the item was added and never
// done, and clients drop it (its output_index is not used again). A held
// call that fails the check is not written at all. After that the answer
// cannot complete any more — a Finish fails the stream. A stream decoder
// checks the same before it finishes; the encoder does not rely on it.
//
// # Thinking
//
// A thinking part is kept until it stops and then written as one reasoning
// item whose summary is its text, added and done at once: nothing is
// written while it streams.
//
// # What is kept, and how large the final event is
//
// The done events of an item and the final response repeat its text or
// arguments, so they are kept: at most MaxItemBytes per item
// (ir.MaxToolArgsBytes for a call) and MaxOutputBytes for the answer,
// counted as JSON string bytes. The final event is therefore at most
// MaxOutputBytes plus a few hundred bytes for each of at most ir.MaxParts
// items. Each piece is copied a fixed number of times, whatever the number
// of deltas. An answer over a limit fails.
//
// # Signs of life
//
// A held part and a thinking part give the caller nothing for a while, and
// with a Chat Completions target a held tool call waits long: such a stream
// closes its calls only at the finish_reason. So that the client and what
// stands between see the stream is alive, the encoder writes an SSE comment
// (": keep-alive") when an event arrived, nothing of it could be written,
// and the wire was silent for a second: at most one per second, and none
// when no event arrives.
//
// # Usage and status
//
// The final event is response.completed, or response.incomplete with
// "incomplete_details" when the answer was cut by the token cap
// ("max_output_tokens") or withheld ("content_filter"). Its usage is the
// Finish event's; input tokens fall back to the Start event's.
//
// What Codex does with response.incomplete: it takes both reasons for a
// stream error it may retry, up to its own retry count. An answer that the
// upstream ended with finish_reason "length" can therefore be requested —
// and billed — several times before Codex gives up. That is what the
// protocol says about such an answer, and it is kept: calling a cut answer
// completed would hide the cut from every client.
//
// # Failure
//
// An Error event, Close before the Finish, a sequence that breaks the rules
// of ir.Event, a limit: the open item is ended, held parts are written and
// ended, and then one event ends the stream. A text that had not stopped is
// done with status "incomplete"; a call is done, as "completed", when its
// arguments are one JSON object, and otherwise not done at all (see Tool
// calls):
//
//	event: response.failed
//	data: {"type":"response.failed","sequence_number":N,"response":{…,"status":"failed",
//	       "error":{"code":"server_error","message":"…"},…,"output":[the items so far],"usage":null}}
//
// response.completed is never written then, and nothing is written after
// the end, whatever is handed in. A failure before the Start writes
// response.created first, because the SDKs refuse a stream that begins with
// anything else (a gateway that has not sent its header yet does better to
// answer with an HTTP error then, and does not hand the event in).
//
// An encoder is not safe for use by several goroutines.
type StreamEncoder struct {
	w    io.Writer
	head head // id, model (the one that was asked for until the Start names one), created_at

	started bool
	done    bool  // the stream was ended, well or badly
	werr    error // the writer failed: nothing more is written
	broken  bool  // a call was left without its done events: the answer cannot complete
	waiting bool  // the open item is a call that stopped without arguments: its end is held back

	seq         int // the next sequence_number
	inputTokens int // what the Start told

	parts     []part // by neutral index
	ids       map[string]bool
	calls     int
	argBytes  int         // argument bytes of all tool calls
	total     int         // JSON string bytes of all texts and arguments
	cur       int         // the neutral index of the item open on the wire, or -1
	wire      int         // items written so far: the next output_index
	acc       []byte      // the text or arguments of the open item so far
	held      []*heldPart // parts that wait for the wire, in the order they opened
	heldBytes int
	output    []byte // the items that are done, comma-joined: the final "output"

	buf    []byte // the frame being built
	quoted []byte // scratch: a text as a JSON string

	now     func() time.Time // the clock; a test's own in tests
	lastOut time.Time        // when the last frame was written
}

type part struct {
	kind     ir.PartKind
	open     bool
	id, name string // of a tool call
	argBytes int    // a tool call's argument bytes so far
	size     int    // JSON string bytes so far
}

type heldPart struct {
	index   int
	pieces  []string
	tail    []byte // the deltas after maxHeldPieces, joined
	stopped bool
}

// NewStreamEncoder returns an encoder that writes one answer to w.
// fallbackModel is named in the response when the upstream names none; now
// is the response's "created_at".
func NewStreamEncoder(w io.Writer, fallbackModel string, now time.Time) *StreamEncoder {
	return &StreamEncoder{w: w, head: head{id: fallbackID, model: fallbackModel, created: now.Unix()},
		cur: -1, ids: map[string]bool{}, now: time.Now}
}

// Write takes the next event. It returns nil for an event that was written,
// held back or ignored (anything after the end); an error wrapping
// ir.ErrSequence or ir.ErrLimit when the event broke the rules or a limit,
// in which case the stream has been ended with response.failed; or the
// writer's error — also when it was one of the events of that end the writer
// did not take — after which nothing more is written and every call returns
// it.
func (e *StreamEncoder) Write(ev ir.Event) error {
	if e.werr != nil {
		return e.werr
	}
	if e.done {
		return nil
	}
	err := e.write(ev)
	e.keepAlive()
	if err == nil {
		err = e.werr
	}
	return err
}

// keepAlive writes a comment when nothing can be written for what arrives
// and nothing was written for pingAfter (see Signs of life at
// StreamEncoder). It runs after every event, so a comment is written only
// when upstream data arrived.
func (e *StreamEncoder) keepAlive() {
	if !e.started || e.done || e.werr != nil || e.now().Sub(e.lastOut) < pingAfter {
		return
	}
	if len(e.held) == 0 && !(e.cur >= 0 && e.parts[e.cur].kind == ir.Thinking) {
		return
	}
	_, e.werr = io.WriteString(e.w, keepAliveFrame)
	e.lastOut = e.now()
}

func (e *StreamEncoder) write(ev ir.Event) error {
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
	// The stream goes on: a call that stopped without arguments was a call
	// without arguments, not one that was cut. (The stop of another part
	// does not say so: a failing decoder stops every part first.)
	for e.waiting && ev.Kind != ir.PartStop {
		e.waiting = false
		e.stopItem(false)
		e.drain(false)
	}
	switch ev.Kind {
	case ir.Start:
		e.start(ev.ID, ev.Model)
		e.inputTokens = max(ev.Usage.InputTokens, 0)
		e.response("response.in_progress", statusInProgress, nil, "", "")
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
			if e.parts[e.cur].kind == ir.ToolUse && len(bytes.TrimSpace(e.acc)) == 0 {
				e.waiting = true // see Tool calls at StreamEncoder
				break
			}
			e.stopItem(false)
			e.drain(false)
		} else if h := e.heldAt(ev.Index); h != nil {
			h.stopped = true
		}
	case ir.Finish:
		// Nothing is held when nothing is open: the last stop drained it.
		if e.cur >= 0 || len(e.held) > 0 || e.waiting {
			return e.sequence("the finish came while a part was open")
		}
		if e.broken {
			return e.sequence("a tool call's arguments are not a JSON object")
		}
		usage := ev.Usage
		if usage.InputTokens <= 0 {
			usage.InputTokens = e.inputTokens
		}
		status, reason := stopStatus(ev.Stop)
		e.response("response."+status, status, &usage, "", reason)
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

// start writes response.created.
func (e *StreamEncoder) start(id, model string) {
	e.started = true
	e.head.id = responseID(id)
	if model != "" {
		e.head.model = model
	}
	e.response("response.created", statusInProgress, nil, "", "")
}

func (e *StreamEncoder) partStart(ev ir.Event) error {
	if ev.Index != len(e.parts) {
		return e.sequence("a part's number is out of order")
	}
	if len(e.parts) >= ir.MaxParts {
		return e.limit(fmt.Sprintf("more than %d parts", ir.MaxParts))
	}
	p := part{kind: ev.Part.Kind, open: true}
	switch p.kind {
	case ir.Text, ir.Thinking:
	case ir.ToolUse:
		p.id, p.name = ev.Part.ToolID, ev.Part.ToolName
		if p.id == "" || p.name == "" || e.ids[p.id] {
			return e.sequence("a tool call needs a name and an id of its own")
		}
		if e.calls >= ir.MaxToolCalls {
			return e.limit(fmt.Sprintf("more than %d tool calls", ir.MaxToolCalls))
		}
		e.calls++
		e.ids[p.id] = true
	default:
		return e.sequence("a part of a kind an answer cannot hold")
	}
	e.parts = append(e.parts, p)
	if e.cur < 0 && len(e.held) == 0 {
		e.startItem(ev.Index)
		return e.werr
	}
	e.held = append(e.held, &heldPart{index: ev.Index})
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
// part is the live item or held. nil when there is none.
func (e *StreamEncoder) heldAt(index int) *heldPart {
	for _, h := range e.held {
		if h.index == index {
			return h
		}
	}
	return nil
}

// delta counts a piece against the limits, then writes it when it belongs
// to the live item, or keeps it when its part is held.
func (e *StreamEncoder) delta(index int, s string) error {
	if s == "" {
		return nil
	}
	e.quoted = ir.AppendString(e.quoted[:0], s)
	n := len(e.quoted) - 2
	if e.parts[index].size+n > MaxItemBytes {
		return e.limit(fmt.Sprintf("an output item over %d bytes", MaxItemBytes))
	}
	e.parts[index].size += n
	if e.total += n; e.total > MaxOutputBytes {
		return e.limit(fmt.Sprintf("an answer over %d bytes", MaxOutputBytes))
	}
	if index == e.cur {
		e.writeDelta(s)
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
// that has not stopped: that one becomes the live item. When the stream is
// being aborted all of them are written and ended. A held call that ends
// here with arguments that are not one JSON object is not written at all.
func (e *StreamEncoder) drain(aborting bool) {
	for len(e.held) > 0 && e.werr == nil {
		h := e.held[0]
		e.held[0] = nil
		e.held = e.held[1:]
		ends := h.stopped || aborting
		call := e.parts[h.index].kind == ir.ToolUse
		empty, object := false, false
		if call && ends {
			empty, object = h.arguments()
		}
		// A call that ends here and is not whole — its arguments are not one
		// JSON object, or it has none and the stream is failing — is not
		// written.
		if call && ends && ((empty && aborting) || (!empty && !object)) {
			for _, s := range h.pieces {
				e.heldBytes -= len(s)
			}
			e.heldBytes -= len(h.tail)
			e.broken = true
			continue
		}
		e.startItem(h.index)
		for _, s := range h.pieces {
			e.heldBytes -= len(s)
			e.writeDelta(s)
		}
		if len(h.tail) > 0 {
			e.heldBytes -= len(h.tail)
			e.writeDelta(string(h.tail))
		}
		if !ends {
			return
		}
		if call && empty {
			e.waiting = true // stopped without arguments: what follows decides
			return
		}
		e.stopItem(!h.stopped)
	}
}

// arguments reports what a held call got: nothing but white space, or one
// JSON object.
func (h *heldPart) arguments() (empty, object bool) {
	n := len(h.tail)
	for _, s := range h.pieces {
		n += len(s)
	}
	args := make([]byte, 0, n)
	for _, s := range h.pieces {
		args = append(args, s...)
	}
	args = append(args, h.tail...)
	return len(bytes.TrimSpace(args)) == 0, ir.CheckObject(args) == nil
}

// open begins a frame: {"type":"<event>","sequence_number":N
func (e *StreamEncoder) open(event string) {
	e.buf = append(e.buf[:0], `{"type":"`...)
	e.buf = append(e.buf, event...)
	e.buf = append(e.buf, `","sequence_number":`...)
	e.buf = strconv.AppendInt(e.buf, int64(e.seq), 10)
}

// ref appends the open item's place: ,"item_id":"…","output_index":N
func (e *StreamEncoder) ref() {
	e.buf = append(e.buf, `,"item_id":"`...)
	e.buf = appendItemID(e.buf, e.parts[e.cur].kind, e.wire)
	e.buf = append(e.buf, `","output_index":`...)
	e.buf = strconv.AppendInt(e.buf, int64(e.wire), 10)
}

// item appends ,"output_index":N,"item":
func (e *StreamEncoder) item() {
	e.buf = append(e.buf, `,"output_index":`...)
	e.buf = strconv.AppendInt(e.buf, int64(e.wire), 10)
	e.buf = append(e.buf, `,"item":`...)
}

// send writes e.buf as one event, unless the writer has failed.
func (e *StreamEncoder) send(event string) {
	if e.werr != nil {
		return
	}
	e.werr = sse.Write(e.w, event, e.buf)
	e.seq++
	e.lastOut = e.now()
}

// response writes an event that carries the response object.
func (e *StreamEncoder) response(event, status string, usage *ir.Usage, failure, reason string) {
	e.open(event)
	e.buf = append(e.buf, `,"response":`...)
	e.buf = appendResponse(e.buf, e.head, status, e.output, usage, failure, reason)
	e.buf = append(e.buf, '}')
	e.send(event)
}

// startItem makes the part the item that is open on the wire.
func (e *StreamEncoder) startItem(index int) {
	e.cur = index
	e.acc = e.acc[:0]
	p := &e.parts[index]
	switch p.kind {
	case ir.Text:
		e.open("response.output_item.added")
		e.item()
		e.buf = appendMessageItem(e.buf, e.wire, statusInProgress, nil)
		e.buf = append(e.buf, '}')
		e.send("response.output_item.added")
		e.open("response.content_part.added")
		e.ref()
		e.buf = append(e.buf, `,"content_index":0,"part":`...)
		e.buf = appendTextPart(e.buf, []byte(`""`))
		e.buf = append(e.buf, '}')
		e.send("response.content_part.added")
	case ir.ToolUse:
		e.open("response.output_item.added")
		e.item()
		e.buf = appendCallItem(e.buf, e.wire, p.id, p.name, "", statusInProgress)
		e.buf = append(e.buf, '}')
		e.send("response.output_item.added")
	}
	// Thinking: nothing until it stops.
}

// writeDelta adds a piece to the open item and writes its delta event.
func (e *StreamEncoder) writeDelta(s string) {
	e.acc = append(e.acc, s...)
	switch e.parts[e.cur].kind {
	case ir.Text:
		e.open("response.output_text.delta")
		e.ref()
		e.buf = append(e.buf, `,"content_index":0,"delta":`...)
		e.buf = ir.AppendString(e.buf, s)
		e.buf = append(e.buf, `,"logprobs":[]}`...)
		e.send("response.output_text.delta")
	case ir.ToolUse:
		e.open("response.function_call_arguments.delta")
		e.ref()
		e.buf = append(e.buf, `,"delta":`...)
		e.buf = ir.AppendString(e.buf, s)
		e.buf = append(e.buf, '}')
		e.send("response.function_call_arguments.delta")
	}
}

// stopItem ends the item that is open on the wire: it writes its done
// events and adds it to the final output. aborted says the part did not
// stop by itself: the stream is being ended badly. A call whose arguments
// are not one JSON object gets no done event and stays out of the output
// (see Tool calls at StreamEncoder); its output_index is spent.
func (e *StreamEncoder) stopItem(aborted bool) {
	p := &e.parts[e.cur]
	status := statusCompleted
	switch {
	case p.kind == ir.ToolUse:
		if !aborted && len(bytes.TrimSpace(e.acc)) == 0 {
			e.writeDelta("{}")
		}
		if ir.CheckObject(e.acc) != nil {
			e.broken = true
			e.wire++
			e.cur = -1
			return
		}
	case aborted:
		status = statusIncomplete
	}
	if len(e.output) > 0 {
		e.output = append(e.output, ',')
	}
	from := len(e.output) // where this item begins in the output
	text := string(e.acc)
	switch p.kind {
	case ir.Text:
		e.quoted = ir.AppendString(e.quoted[:0], text)
		e.open("response.output_text.done")
		e.ref()
		e.buf = append(e.buf, `,"content_index":0,"text":`...)
		e.buf = append(e.buf, e.quoted...)
		e.buf = append(e.buf, `,"logprobs":[]}`...)
		e.send("response.output_text.done")
		e.open("response.content_part.done")
		e.ref()
		e.buf = append(e.buf, `,"content_index":0,"part":`...)
		at := len(e.buf)
		e.buf = appendTextPart(e.buf, e.quoted)
		e.output = appendMessageItem(e.output, e.wire, status, e.buf[at:])
		e.buf = append(e.buf, '}')
		e.send("response.content_part.done")
	case ir.ToolUse:
		e.open("response.function_call_arguments.done")
		e.ref()
		e.buf = append(e.buf, `,"name":`...)
		e.buf = ir.AppendString(e.buf, p.name)
		e.buf = append(e.buf, `,"arguments":`...)
		e.buf = ir.AppendString(e.buf, text)
		e.buf = append(e.buf, '}')
		e.send("response.function_call_arguments.done")
		e.output = appendCallItem(e.output, e.wire, p.id, p.name, text, status)
	case ir.Thinking:
		e.output = appendReasoningItem(e.output, e.wire, text)
		e.open("response.output_item.added")
		e.item()
		e.buf = append(e.buf, e.output[from:]...)
		e.buf = append(e.buf, '}')
		e.send("response.output_item.added")
	}
	e.open("response.output_item.done")
	e.item()
	e.buf = append(e.buf, e.output[from:]...)
	e.buf = append(e.buf, '}')
	e.send("response.output_item.done")
	e.wire++
	e.cur = -1
}

// abort ends the stream badly: the open item is done, held parts are
// written and done, then the one response.failed.
func (e *StreamEncoder) abort(message string) {
	if !e.started {
		e.start("", "")
	}
	e.waiting = false
	if e.cur >= 0 {
		e.stopItem(true) // a call that waited has no arguments: it gets no done event
	}
	e.drain(true)
	e.response("response.failed", statusFailed, nil, message, "")
	e.end()
}

// sequence and limit end the stream with response.failed. When the writer
// did not take it, its error is what they return: the client is gone, and
// that — not the answer's fault — is what the caller has to act on.
func (e *StreamEncoder) sequence(what string) error {
	if e.abort(errUnreadable); e.werr != nil {
		return e.werr
	}
	return fmt.Errorf("%w: %s", ir.ErrSequence, what)
}

func (e *StreamEncoder) limit(what string) error {
	if e.abort(errTooLarge); e.werr != nil {
		return e.werr
	}
	return fmt.Errorf("%w: %s", ir.ErrLimit, what)
}

// end marks the stream ended and lets go of what the encoder held.
func (e *StreamEncoder) end() {
	e.done = true
	e.parts, e.ids, e.held, e.buf, e.acc, e.output, e.quoted = nil, nil, nil, nil, nil, nil, nil
}
