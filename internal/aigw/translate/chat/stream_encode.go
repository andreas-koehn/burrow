package chat

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// The "type" and "code" of the error object that ends a failed stream. A
// stream that fails has no HTTP status left to tell more.
const (
	streamErrorType = "server_error"
	streamErrorCode = "upstream_error"
)

// StreamEncoder writes neutral events as a Chat Completions stream: frames
// of one "data:" line each, every one written with a single call to the
// writer.
//
//	data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":N,"model":…,
//	       "choices":[{"index":0,"delta":{…},"logprobs":null,"finish_reason":null}]}
//
// id, created and model are the same in every chunk: the upstream's id (see
// completionID) and model, or the fallback model when it names none.
//
//   - Start gives the first chunk, with the delta {"role":"assistant","content":""}.
//   - A text delta is {"content":…}, a thinking delta {"reasoning_content":…}.
//     Chat Completions has one content string: when a second text part
//     begins, a blank line is put before its first piece (and likewise for
//     thinking), as in a whole answer. A part without text leaves no trace.
//   - A tool call is addressed by its "index", counted from 0 in the order
//     the calls open. Its first chunk carries
//     {"tool_calls":[{"index":k,"id":…,"type":"function","function":{"name":…,"arguments":""}}]},
//     every argument delta {"tool_calls":[{"index":k,"function":{"arguments":…}}]}
//     with the upstream's bytes: joined per index they are the upstream's
//     arguments. Calls that are open side by side are written as their
//     pieces come — nothing is held back, the index tells them apart. A call
//     that stops without arguments gets the piece "{}", so that the
//     arguments of every call are one JSON object.
//   - Finish gives a chunk with an empty delta and the "finish_reason"
//     ("stop", "length", "tool_calls", "content_filter"); then, when the
//     caller asked for it ("stream_options.include_usage"), a chunk with
//     "choices":[] and the upstream's usage (the input figure of the Start
//     stands when the Finish has none); then "data: [DONE]". With
//     include_usage every other chunk says "usage":null, as OpenAI's do.
//
// # Failure
//
// An Error event, Close before the Finish, a sequence that breaks the rules
// of ir.Event, a limit: one object ends the stream,
//
//	data: {"error":{"message":"…","type":"server_error","code":"upstream_error"}}
//
// and nothing follows it: no finish_reason, no usage and no "[DONE]", so no
// client takes the answer for complete (the OpenAI SDKs raise an error on
// this object). A tool call whose arguments had not all come is left as it
// stands. An Error before the Start writes the error object alone (a gateway
// that has not sent its header yet does better to answer with an HTTP error
// then, and does not hand the event in).
//
// An encoder is not safe for use by several goroutines.
type StreamEncoder struct {
	w       io.Writer
	model   string // the model that was asked for
	created int64
	usage   bool // the caller asked for the usage chunk

	started bool
	done    bool  // the stream was ended, well or badly
	werr    error // the writer failed: nothing more is written

	head        []byte // what every chunk begins with, up to "choices"
	inputTokens int    // what the Start told

	parts    []encodedPart // by neutral index
	ids      map[string]bool
	calls    int
	argBytes int // argument bytes of all tool calls
	// A text (a thinking) was written: the next part of that kind is set
	// apart by textGap.
	wroteText, wroteThinking bool

	buf []byte // the frame being built
}

type encodedPart struct {
	kind     ir.PartKind
	open     bool
	call     int  // a tool call's index on the wire
	argBytes int  // a tool call's argument bytes so far
	wrote    bool // a delta of the part was written
}

// NewStreamEncoder returns an encoder that writes one answer to w.
// fallbackModel is named when the upstream names no model, now is "created",
// includeUsage says the caller asked for the usage chunk (see IncludeUsage).
func NewStreamEncoder(w io.Writer, fallbackModel string, now time.Time, includeUsage bool) *StreamEncoder {
	return &StreamEncoder{w: w, model: fallbackModel, created: now.Unix(), usage: includeUsage, ids: map[string]bool{}}
}

// Write takes the next event. It returns nil for an event that was written
// or ignored (anything after the end); an error wrapping ir.ErrSequence or
// ir.ErrLimit when the event broke the rules or a limit, in which case the
// stream has been ended with the error object; or the writer's error, after
// which nothing more is written and every call returns it.
func (e *StreamEncoder) Write(ev ir.Event) error {
	if e.werr != nil {
		return e.werr
	}
	if e.done {
		return nil
	}
	if err := e.write(ev); err != nil {
		return err
	}
	return e.werr
}

// Close ends the stream when the events stopped coming: nothing after a
// Finish or an Error; otherwise the error object with the text "the provider
// ended the stream early". It returns the writer's error, if there was one.
func (e *StreamEncoder) Close() error {
	if e.werr == nil && !e.done {
		e.abort(errEarlyEnd)
	}
	return e.werr
}

func (e *StreamEncoder) write(ev ir.Event) error {
	if ev.Kind == ir.Error {
		msg := ev.Err
		if msg == "" {
			msg = errProvider
		}
		e.abort(msg)
		return nil
	}
	if e.started == (ev.Kind == ir.Start) {
		return e.sequence("the stream must begin with exactly one start")
	}
	switch ev.Kind {
	case ir.Start:
		e.started = true
		e.inputTokens = max(ev.Usage.InputTokens, 0)
		model := ev.Model
		if model == "" {
			model = e.model
		}
		e.head = append(e.head[:0], `{"id":`...)
		e.head = ir.AppendString(e.head, completionID(ev.ID))
		e.head = append(e.head, `,"object":"chat.completion.chunk","created":`...)
		e.head = strconv.AppendInt(e.head, e.created, 10)
		e.head = append(e.head, `,"model":`...)
		e.head = ir.AppendString(e.head, model)
		e.head = append(e.head, ',')
		e.chunk(`{"role":"assistant","content":""}`, "")
	case ir.PartStart:
		return e.partStart(ev)
	case ir.TextDelta, ir.ThinkingDelta:
		kind, key, wrote := ir.Text, "content", &e.wroteText
		if ev.Kind == ir.ThinkingDelta {
			kind, key, wrote = ir.Thinking, "reasoning_content", &e.wroteThinking
		}
		if !e.isOpen(ev.Index, kind) {
			return e.sequence("a text delta for a part that is not open")
		}
		if ev.Text == "" {
			return nil
		}
		p := &e.parts[ev.Index]
		text := ev.Text
		if !p.wrote && *wrote {
			text = textGap + text
		}
		p.wrote, *wrote = true, true
		delta := append(make([]byte, 0, len(text)+32), `{"`+key+`":`...)
		delta = append(ir.AppendString(delta, text), '}')
		e.chunk(string(delta), "")
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
		if ev.ArgsJSON != "" {
			e.arguments(p, ev.ArgsJSON)
		}
	case ir.PartStop:
		if !e.isOpen(ev.Index, "") {
			return e.sequence("a stop for a part that is not open")
		}
		p := &e.parts[ev.Index]
		p.open = false
		if p.kind == ir.ToolUse && !p.wrote {
			e.arguments(p, "{}")
		}
	case ir.Finish:
		for i := range e.parts {
			if e.parts[i].open {
				return e.sequence("the finish came while a part was open")
			}
		}
		e.chunk(`{}`, finishReason(ev.Stop, e.calls > 0))
		if e.usage {
			usage := ev.Usage
			if usage.InputTokens <= 0 {
				usage.InputTokens = e.inputTokens
			}
			e.buf = append(e.buf[:0], e.head...)
			e.buf = append(e.buf, `"choices":[],"usage":`...)
			e.buf = append(appendUsage(e.buf, usage), '}')
			e.frame()
		}
		e.buf = append(e.buf[:0], "[DONE]"...)
		e.frame()
		e.end()
	default:
		return e.sequence("an event of an unknown kind")
	}
	return nil
}

func (e *StreamEncoder) partStart(ev ir.Event) error {
	if ev.Index != len(e.parts) {
		return e.sequence("a part's number is out of order")
	}
	if len(e.parts) >= ir.MaxParts {
		return e.limit(fmt.Sprintf("more than %d parts", ir.MaxParts))
	}
	p := encodedPart{kind: ev.Part.Kind, open: true}
	switch p.kind {
	case ir.Text, ir.Thinking:
		e.parts = append(e.parts, p)
	case ir.ToolUse:
		id, name := ev.Part.ToolID, ev.Part.ToolName
		if id == "" || name == "" || e.ids[id] {
			return e.sequence("a tool call needs a name and an id of its own")
		}
		if e.calls >= ir.MaxToolCalls {
			return e.limit(fmt.Sprintf("more than %d tool calls", ir.MaxToolCalls))
		}
		p.call = e.calls
		e.calls++
		e.ids[id] = true
		e.parts = append(e.parts, p)
		delta := append(make([]byte, 0, len(id)+len(name)+96), `{"tool_calls":[{"index":`...)
		delta = strconv.AppendInt(delta, int64(p.call), 10)
		delta = append(delta, `,"id":`...)
		delta = ir.AppendString(delta, id)
		delta = append(delta, `,"type":"function","function":{"name":`...)
		delta = ir.AppendString(delta, name)
		delta = append(delta, `,"arguments":""}}]}`...)
		e.chunk(string(delta), "")
	default:
		return e.sequence("a part of a kind an answer cannot hold")
	}
	return nil
}

// arguments writes a piece of a tool call's arguments.
func (e *StreamEncoder) arguments(p *encodedPart, piece string) {
	p.wrote = true
	delta := append(make([]byte, 0, len(piece)+64), `{"tool_calls":[{"index":`...)
	delta = strconv.AppendInt(delta, int64(p.call), 10)
	delta = append(delta, `,"function":{"arguments":`...)
	delta = ir.AppendString(delta, piece)
	delta = append(delta, `}}]}`...)
	e.chunk(string(delta), "")
}

// isOpen reports whether index is a part that started and has not stopped,
// of the given kind ("" for any).
func (e *StreamEncoder) isOpen(index int, kind ir.PartKind) bool {
	if index < 0 || index >= len(e.parts) || !e.parts[index].open {
		return false
	}
	return kind == "" || e.parts[index].kind == kind
}

// chunk writes one chat.completion.chunk with the given delta; finish is the
// finish_reason, "" for null.
func (e *StreamEncoder) chunk(delta, finish string) {
	e.buf = append(e.buf[:0], e.head...)
	e.buf = append(e.buf, `"choices":[{"index":0,"delta":`...)
	e.buf = append(e.buf, delta...)
	e.buf = append(e.buf, `,"logprobs":null,"finish_reason":`...)
	if finish == "" {
		e.buf = append(e.buf, `null`...)
	} else {
		e.buf = append(e.buf, '"')
		e.buf = append(e.buf, finish...)
		e.buf = append(e.buf, '"')
	}
	e.buf = append(e.buf, `}]`...)
	if e.usage {
		e.buf = append(e.buf, `,"usage":null`...)
	}
	e.buf = append(e.buf, '}')
	e.frame()
}

// abort ends the stream badly: the one error object, and nothing after it.
func (e *StreamEncoder) abort(message string) {
	e.buf = append(e.buf[:0], `{"error":{"message":`...)
	e.buf = ir.AppendString(e.buf, message)
	e.buf = append(e.buf, `,"type":"`+streamErrorType+`","code":"`+streamErrorCode+`"}}`...)
	e.frame()
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
	e.parts, e.ids, e.buf, e.head = nil, nil, nil, nil
}

// frame writes e.buf as one frame, unless the writer has failed.
func (e *StreamEncoder) frame() {
	if e.werr == nil {
		e.werr = sse.Write(e.w, "", e.buf)
	}
}
