package translate

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
)

// maxErrorBody is how much of an upstream error body is kept to read its
// message from.
const maxErrorBody = 64 << 10

// The texts of the errors a writer makes up itself. They are fixed: nothing
// of the answer is in them.
const (
	msgUnreadable = "the provider sent an answer that cannot be read"
	msgTooLarge   = "the provider's answer is too large"
	msgWrongForm  = "the provider did not answer in the form that was asked for"
	msgNoAnswer   = "the provider did not answer"
	msgStatus     = "the provider answered " // + the status
)

type streamDecoder interface {
	Feed(data []byte) ([]ir.Event, error)
	Close() []ir.Event
}

type streamEncoder interface {
	Write(ev ir.Event) error
	Close() error
}

// targetCodec reads what a target of one format answers.
type targetCodec struct {
	maxBody          int // the largest buffered answer
	maxFrame         int // the largest stream frame
	decodeResponse   func(body []byte) (ir.Response, error)
	decodeError      func(body []byte) string // the message of an error body, "" when it has none
	newStreamDecoder func() streamDecoder
}

// callerCodec writes answers for a caller of one format.
type callerCodec struct {
	encodeResponse   func(resp ir.Response, fallbackModel string) ([]byte, error)
	encodeError      func(status int, message string) []byte
	newStreamEncoder func(w io.Writer, fallbackModel string) streamEncoder
}

// codec is what a response writer translates with: the target's half of
// one format and the caller's half of another.
type codec struct {
	targetCodec
	callerCodec
}

var chatTarget = targetCodec{
	maxBody:          chat.MaxResponseBytes,
	maxFrame:         chat.MaxFrameBytes,
	decodeResponse:   chat.DecodeResponse,
	decodeError:      chat.DecodeError,
	newStreamDecoder: func() streamDecoder { return chat.NewStreamDecoder() },
}

var messagesCaller = callerCodec{
	encodeResponse:   messages.EncodeResponse,
	encodeError:      messages.EncodeError,
	newStreamEncoder: func(w io.Writer, model string) streamEncoder { return messages.NewStreamEncoder(w, model) },
}

type mode int

const (
	modeNone   mode = iota // the upstream has not answered yet
	modeStream             // a success that streams: translated as it passes
	modeBody               // a success in one piece: buffered, translated by Finish
	modeError              // a status that is no success: buffered, re-shaped by Finish
	modeFailed             // an answer that cannot be used: discarded, an error by Finish
)

// writer is the ResponseWriter of every pair.
type writer struct {
	w http.ResponseWriter
	o ResponseOptions
	c codec

	header http.Header // the upstream's
	status int         // the upstream's
	mode   mode

	buf  []byte // modeBody, modeError: the upstream's body so far
	over bool   // modeBody: the body went over the limit and was let go

	parser *sse.Parser
	dec    streamDecoder
	enc    streamEncoder
	ended  bool // modeStream: the answer ended (well or badly); the rest is discarded

	committed bool  // the caller's header was written
	finished  bool  // Finish ran
	clientErr error // the caller's writer failed

	// What Finish answers in modeFailed, and what Failure reports.
	failStatus  int
	failCode    string
	failMessage string
	midStream   bool
}

func newWriter(w http.ResponseWriter, o ResponseOptions, c codec) *writer {
	return &writer{w: w, o: o, c: c, header: http.Header{}}
}

// Header returns the upstream's header: a map of its own. Nothing of it
// reaches the caller before the answer is committed.
func (t *writer) Header() http.Header { return t.header }

// Unwrap gives http.ResponseController the caller's writer (deadlines,
// full duplex). Flush does not go that way: see Flush.
func (t *writer) Unwrap() http.ResponseWriter { return t.w }

// NoteUpstreamTimeout passes the note on to a writer that wants it.
func (t *writer) NoteUpstreamTimeout() {
	if tn, ok := t.w.(interface{ NoteUpstreamTimeout() }); ok {
		tn.NoteUpstreamTimeout()
	}
}

// Flush flushes the caller's writer once a stream is committed. Before
// that, and on the buffered paths, there is nothing to flush and the
// caller's header must not go out yet.
func (t *writer) Flush() { _ = t.FlushError() }

// FlushError is Flush for http.ResponseController.
func (t *writer) FlushError() error {
	if t.mode != modeStream || !t.committed || t.clientErr != nil {
		return nil
	}
	return http.NewResponseController(t.w).Flush()
}

func (t *writer) Failure() (code string, midStream bool) { return t.failCode, t.midStream }

// WriteHeader takes the upstream's status and decides how its body is read.
// Informational statuses are not an answer and are not passed on.
func (t *writer) WriteHeader(status int) {
	if t.mode != modeNone || t.finished || (status >= 100 && status < 200) {
		return
	}
	t.status = status
	contentType, _, _ := strings.Cut(t.header.Get("Content-Type"), ";")
	isStream := strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream")
	encoding := strings.TrimSpace(t.header.Get("Content-Encoding"))
	switch {
	case status >= 400:
		t.mode = modeError
	case status < 200 || status >= 300:
		// A redirect the proxy did not follow is no answer a client of
		// another format could use.
		t.fail(http.StatusBadGateway, CodeUpstreamError, msgStatus+strconv.Itoa(status))
	case encoding != "" && !strings.EqualFold(encoding, "identity"):
		// A compressed body cannot be read here; the gateway must not ask
		// for one on a translated call.
		t.fail(http.StatusBadGateway, CodeUpstreamInvalid, msgUnreadable)
	case t.o.Stream && isStream:
		t.mode = modeStream
		t.parser = sse.NewParser(t.c.maxFrame)
		t.dec = t.c.newStreamDecoder()
	case isStream:
		t.fail(http.StatusBadGateway, CodeUpstreamInvalid, msgWrongForm)
	default:
		// One piece. When a stream was asked for this is at best the
		// provider's error object: Finish looks.
		t.mode = modeBody
	}
}

// fail makes the answer an error response that Finish writes; what the
// upstream still sends is discarded.
func (t *writer) fail(status int, code, message string) {
	t.mode = modeFailed
	t.buf, t.parser, t.dec, t.enc = nil, nil, nil, nil
	t.failStatus, t.failCode, t.failMessage = status, code, message
}

// Write takes the next bytes of the upstream's body. It reports them all
// taken unless the caller's writer failed: that error is returned, so that
// the upstream is not read to its end for a caller that is gone.
func (t *writer) Write(p []byte) (int, error) {
	if t.finished {
		return len(p), nil
	}
	if t.mode == modeNone {
		t.WriteHeader(http.StatusOK)
	}
	switch t.mode {
	case modeStream:
		if t.clientErr != nil {
			return 0, t.clientErr
		}
		if !t.ended {
			t.feed(p)
			_ = t.FlushError()
		}
		if t.clientErr != nil {
			return 0, t.clientErr
		}
	case modeBody:
		if t.over {
			break
		}
		if len(t.buf)+len(p) > t.c.maxBody {
			t.over, t.buf = true, nil
			break
		}
		t.buf = append(t.buf, p...)
	case modeError:
		if room := maxErrorBody - len(t.buf); room > 0 {
			t.buf = append(t.buf, p[:min(room, len(p))]...)
		}
	}
	return len(p), nil
}

// feed runs upstream bytes through the parser and the decoder and writes
// the events.
func (t *writer) feed(p []byte) {
	frames, perr := t.parser.Feed(p)
	for _, f := range frames {
		if t.ended {
			return
		}
		events, err := t.dec.Feed(f.Data)
		code := CodeUpstreamError // an Error event without an error: the provider's own words, or its early "[DONE]"
		if err != nil {
			code = CodeUpstreamInvalid
		}
		t.events(events, code)
	}
	if perr != nil && !t.ended {
		// A frame over the limit. The decoder ends the answer; its Error
		// event says "ended early", which is not what happened.
		events := t.dec.Close()
		for i := range events {
			if events[i].Kind == ir.Error {
				events[i].Err = msgTooLarge
			}
		}
		t.events(events, CodeUpstreamInvalid)
		t.ended = true
	}
}

// events writes decoded events to the caller. code is what a failure among
// them is reported as. The caller's header goes out with the first event
// that is not an error; an Error before that makes the answer an error
// response instead.
func (t *writer) events(events []ir.Event, code string) {
	for _, ev := range events {
		if t.ended {
			return
		}
		if ev.Kind == ir.Error && !t.committed {
			message := ev.Err
			if message == "" {
				message = msgUnreadable
			}
			t.fail(http.StatusBadGateway, code, message)
			t.ended = true
			return
		}
		if !t.committed {
			t.commitStream()
		}
		err := t.enc.Write(ev)
		switch {
		case err == nil:
			if ev.Kind == ir.Error {
				t.endedBadly(code)
			} else if ev.Kind == ir.Finish {
				t.ended = true
			}
		case errors.Is(err, ir.ErrSequence), errors.Is(err, ir.ErrLimit):
			// The encoder refused the event and ended the stream itself.
			t.endedBadly(CodeUpstreamInvalid)
		default:
			t.clientErr, t.ended = err, true
		}
	}
}

// endedBadly records that the caller's stream was ended with its error
// event.
func (t *writer) endedBadly(code string) {
	t.ended, t.midStream, t.failCode = true, true, code
	if t.o.OnError != nil {
		t.o.OnError(t.status, code)
	}
}

// passHeaders copies the upstream's header to the caller's, without the
// fields that describe the upstream's body.
func (t *writer) passHeaders() {
	dst := t.w.Header()
	for k, v := range t.header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Content-Type", "Content-Encoding", "Transfer-Encoding":
			continue
		}
		dst[k] = append([]string(nil), v...)
	}
}

func (t *writer) commitStream() {
	t.committed = true
	t.passHeaders()
	h := t.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	t.w.WriteHeader(t.status)
	t.enc = t.c.newStreamEncoder(t.w, t.o.RequestedModel)
}

// respond writes a whole response to the caller.
func (t *writer) respond(status int, body []byte) {
	t.committed = true
	t.passHeaders()
	h := t.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	t.w.WriteHeader(status)
	_, _ = t.w.Write(body)
}

// respondError writes an error response in the caller's shape.
func (t *writer) respondError(status int, code, message string) {
	t.failCode = code
	if t.o.OnError != nil {
		t.o.OnError(status, code)
	}
	t.respond(status, t.c.encodeError(status, message))
}

func (t *writer) Finish() {
	if t.finished {
		return
	}
	t.finished = true
	switch t.mode {
	case modeNone:
		t.respondError(http.StatusBadGateway, CodeUpstreamInvalid, msgNoAnswer)
	case modeStream:
		t.finishStream()
	case modeBody:
		t.finishBody()
	case modeError:
		message := t.c.decodeError(t.buf)
		if message == "" {
			message = msgStatus + strconv.Itoa(t.status)
		}
		t.respondError(t.status, CodeUpstreamError, message)
	}
	if t.mode == modeFailed && !t.committed {
		t.respondError(t.failStatus, t.failCode, t.failMessage)
	}
	t.buf, t.parser, t.dec, t.enc = nil, nil, nil, nil
}

func (t *writer) finishStream() {
	if !t.ended {
		for _, f := range t.parser.Flush() {
			if t.ended {
				break
			}
			events, err := t.dec.Feed(f.Data)
			code := CodeUpstreamError
			if err != nil {
				code = CodeUpstreamInvalid
			}
			t.events(events, code)
		}
	}
	if !t.ended {
		// The upstream stopped. The decoder says how the answer ends: with
		// a Finish when only "[DONE]" was missing, else with an Error.
		t.events(t.dec.Close(), CodeUpstreamInvalid)
	}
	if t.mode != modeStream || t.clientErr != nil {
		return
	}
	if !t.committed {
		// A decoder that ended without an event: no answer.
		t.fail(http.StatusBadGateway, CodeUpstreamInvalid, msgNoAnswer)
		return
	}
	if !t.ended {
		t.endedBadly(CodeUpstreamInvalid)
	}
	if err := t.enc.Close(); err != nil {
		t.clientErr = err
		return
	}
	_ = t.FlushError()
}

func (t *writer) finishBody() {
	if t.over {
		t.respondError(http.StatusBadGateway, CodeUpstreamInvalid, msgTooLarge)
		return
	}
	resp, err := t.c.decodeResponse(t.buf)
	var provider *ir.StreamError
	switch {
	case errors.As(err, &provider):
		message := provider.Message
		if message == "" {
			message = msgUnreadable
		}
		t.respondError(http.StatusBadGateway, CodeUpstreamError, message)
		return
	case errors.Is(err, ir.ErrLimit):
		t.respondError(http.StatusBadGateway, CodeUpstreamInvalid, msgTooLarge)
		return
	case err != nil:
		t.respondError(http.StatusBadGateway, CodeUpstreamInvalid, msgUnreadable)
		return
	case t.o.Stream:
		// A whole answer where a stream was asked for.
		t.respondError(http.StatusBadGateway, CodeUpstreamInvalid, msgWrongForm)
		return
	}
	body, err := t.c.encodeResponse(resp, t.o.RequestedModel)
	if err != nil {
		t.respondError(http.StatusBadGateway, CodeUpstreamInvalid, msgUnreadable)
		return
	}
	t.respond(t.status, body)
}
