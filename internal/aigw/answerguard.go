package aigw

import (
	"bufio"
	"bytes"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
	"github.com/ankoehn/burrow/internal/aigw/translate/sse"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/inspector"
)

// What the app is told when an answer is withheld: by a pattern, or by the
// judge.
const (
	codeBlockedAnswer      = "guardrail_blocked_answer"
	codeJudgeBlockedAnswer = "guardrail_judge_blocked_answer"
	messageBlockedAnswer   = "the answer was withheld by a guardrail"
)

// maxPendingFrame bounds how much of one server-sent event the guard holds
// while it waits for the event's end. An event larger than this is no
// answer text a model streams; the rest of such a stream is delivered
// uninspected.
const maxPendingFrame = 1 << 20

// How the guard treats the response, decided at its header.
type guardMode int

const (
	modeUndecided guardMode = iota
	modePass                // forwarded as it comes, not inspected
	modeTee                 // forwarded as it comes and kept for one check at the end (flag)
	modeHeld                // held until complete, then checked (block)
	modeStream              // an event stream, checked event by event
	modeBlocked             // the stream was stopped: the rest is dropped
)

// answerGuard is the response writer that inspects an answer on its way to
// the app. The chain puts it between the upstream and the meter, so that the
// meter and the inspector see what the app received.
//
// A streamed answer (text/event-stream) is cut into its events. With flag
// every byte is forwarded at once and the events are checked on the side;
// with block an event is forwarded only once it is complete and its text
// passed: the event with a hit and everything after it are dropped, one
// error event in the caller's format is written instead, and the stream
// ends. Text already sent cannot be recalled.
//
// A whole answer (JSON) is, with block, held until complete, checked and then
// either written or replaced by a 502 error in the caller's format; with flag
// it is forwarded at once and checked at the end. An answer larger than max
// is not inspected and is delivered.
//
// Anything else (audio, images, an error status, a compressed body) passes
// uninspected.
//
// With judge set, a whole answer is also held and, once the patterns let it
// pass, shown to the judge; on a block it is replaced by a 502 with code
// guardrail_judge_blocked_answer. A streamed answer is never judged. Without
// answer patterns (inspect nil) only whole answers are held, for the judge,
// and streams pass as they come.
type answerGuard struct {
	w       http.ResponseWriter
	r       *http.Request
	format  string
	action  string
	inspect func(string) (bool, string)
	log     *slog.Logger
	max     int // the most of a whole answer that is held or kept

	// quick and full are the checks of a streamed answer (see
	// guardrails.StreamScanner): quick on the tail of the text with every
	// event, full on the whole window now and then and where the answer may
	// end. newAnswerGuard sets quick to inspect and full to nil; the chain
	// sets the bundled and the custom patterns.
	quick, full func(string) (bool, string)
	// judge is asked about the text of a whole answer the patterns let
	// pass; it reports that the answer is to be withheld. nil = answers are
	// not judged.
	judge func(text string) (withhold bool)
	// onStop is called once a stream was stopped, after its error event was
	// written and flushed: the chain cancels the upstream request with it.
	onStop func()

	mode    guardMode
	status  int
	buf     bytes.Buffer // the held or kept answer, or the bytes of the event being read
	over    bool         // the whole answer was larger than max
	parser  *sse.Parser
	scanner *guardrails.StreamScanner

	hit       bool
	pattern   string
	blocked   bool
	midStream bool
	// judgeBlocked: the judge withheld the answer.
	judgeBlocked bool
}

// newAnswerGuard wraps w for a response whose answers are inspected. For a
// streamed answer it forwards frames until a hit, then (block) writes one
// error event in the caller's format and stops, or (flag) keeps forwarding
// and records. For a non-streamed answer with block it holds the body until
// complete, inspects it, and writes either it or the 502 error. format is a
// guardrails.Format… value, action a guardrails.AnswerAction… value, inspect
// checks a text and names the pattern that matched.
func newAnswerGuard(w http.ResponseWriter, r *http.Request, format, action string, inspect func(string) (bool, string)) *answerGuard {
	return &answerGuard{
		w: w, r: r, format: format, action: action, inspect: inspect,
		log: slog.Default(), max: inspector.MaxRespBodyBytes,
		quick: inspect,
	}
}

// Result says what the inspection found: whether a pattern matched and
// which, whether the answer was withheld, and whether it was stopped in the
// middle of a stream (some of it reached the app).
func (g *answerGuard) Result() (hit bool, patternID string, blocked bool, midStream bool) {
	return g.hit, g.pattern, g.blocked, g.midStream
}

func (g *answerGuard) Header() http.Header { return g.w.Header() }

func (g *answerGuard) WriteHeader(code int) {
	if g.mode != modeUndecided {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		g.w.WriteHeader(code) // an informational response; the real one follows
		return
	}
	g.status = code
	h := g.w.Header()
	ct := strings.ToLower(strings.TrimSpace(h.Get("Content-Type")))
	enc := strings.TrimSpace(h.Get("Content-Encoding"))
	switch {
	case code < 200 || code >= 300:
		g.mode = modePass // an error is no answer
	case enc != "" && !strings.EqualFold(enc, "identity"):
		g.mode = modePass
		g.log.Warn("aigw: answer not inspected: the body is compressed", slog.String("content_encoding", enc))
	case strings.HasPrefix(ct, "text/event-stream") && g.inspect == nil:
		g.mode = modePass // streams are not judged, and there are no patterns
	case strings.HasPrefix(ct, "text/event-stream"):
		g.mode = modeStream
		g.parser = sse.NewParser(maxPendingFrame + 1024)
		g.scanner = guardrails.NewStreamScanner(g.quick, g.full)
	case ct == "" || isJSONType(ct):
		if g.action == guardrails.AnswerActionBlock || g.judge != nil {
			g.mode = modeHeld
			return // the status goes out with the answer, or the error does
		}
		if g.inspect == nil {
			g.mode = modePass
			break
		}
		g.mode = modeTee
	default:
		g.mode = modePass // audio, images, anything that is not text
	}
	g.w.WriteHeader(code)
}

// isJSONType reports whether a lower-case Content-Type is JSON.
func isJSONType(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	mt = strings.TrimSpace(mt)
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func (g *answerGuard) Write(p []byte) (int, error) {
	if g.mode == modeUndecided {
		g.WriteHeader(http.StatusOK)
	}
	switch g.mode {
	case modeTee:
		n, err := g.w.Write(p)
		g.keep(p[:n])
		return n, err
	case modeHeld:
		if g.buf.Len()+len(p) <= g.max {
			g.buf.Write(p)
			return len(p), nil
		}
		g.overCap()
		g.w.WriteHeader(g.status)
		if _, err := g.w.Write(g.buf.Bytes()); err != nil {
			g.buf.Reset()
			return 0, err
		}
		g.buf.Reset()
		return g.w.Write(p)
	case modeStream:
		return g.writeStream(p)
	case modeBlocked:
		return len(p), nil // the upstream goes on; the app gets nothing more
	}
	return g.w.Write(p)
}

// keep holds a copy of what was forwarded for the check at the end (flag).
func (g *answerGuard) keep(p []byte) {
	if g.buf.Len()+len(p) > g.max {
		g.overCap()
		g.buf.Reset()
		g.mode = modePass
		return
	}
	g.buf.Write(p)
}

func (g *answerGuard) overCap() {
	g.over = true
	g.mode = modePass
	g.log.Info("aigw: answer not inspected: larger than the inspection limit", slog.Int("limit_bytes", g.max))
}

// writeStream takes the next bytes of an event stream. With flag they are
// forwarded first and checked after; with block only the complete events
// that passed are forwarded.
func (g *answerGuard) writeStream(p []byte) (int, error) {
	if g.action != guardrails.AnswerActionBlock {
		n, err := g.w.Write(p)
		g.buf.Write(p[:n])
		for {
			end := frameEnd(g.buf.Bytes())
			if end < 0 {
				break
			}
			if g.checkFrame(g.buf.Next(end)) {
				g.mode, g.scanner = modePass, nil // recorded; nothing more to look for
				g.buf.Reset()
				return n, err
			}
		}
		g.boundPending(false)
		return n, err
	}
	g.buf.Write(p)
	for {
		end := frameEnd(g.buf.Bytes())
		if end < 0 {
			break
		}
		frame := g.buf.Next(end)
		if g.checkFrame(frame) {
			g.stopStream()
			return len(p), nil
		}
		if _, err := g.w.Write(frame); err != nil {
			return len(p), err
		}
	}
	if err := g.boundPending(true); err != nil {
		return len(p), err
	}
	return len(p), nil
}

// boundPending gives up on an event that grew past maxPendingFrame without
// ending: the rest of the stream passes uninspected (with block, the bytes
// held so far are forwarded).
func (g *answerGuard) boundPending(forward bool) error {
	if g.buf.Len() <= maxPendingFrame {
		return nil
	}
	g.log.Info("aigw: answer stream no longer inspected: an event is larger than the limit", slog.Int("limit_bytes", maxPendingFrame))
	g.mode = modePass
	var err error
	if forward {
		_, err = g.w.Write(g.buf.Bytes())
	}
	g.buf.Reset()
	return err
}

// checkFrame parses one complete event and checks the text it adds. It
// reports a hit (and records it).
func (g *answerGuard) checkFrame(raw []byte) bool {
	frames, err := g.parser.Feed(raw)
	if err != nil {
		g.parser = sse.NewParser(maxPendingFrame + 1024) // the old one is spent
	}
	return g.checkFrames(frames)
}

func (g *answerGuard) checkFrames(frames []sse.Frame) bool {
	for _, f := range frames {
		var hit bool
		var id string
		if text, ok := guardrails.DeltaText(g.format, f.Data); ok {
			hit, id = g.scanner.Add(text)
		} else {
			// No text: the event itself passes unchecked, but the answer
			// may end here (finish reason, usage, [DONE]), so the text not
			// yet checked against the custom patterns is checked now.
			hit, id = g.scanner.Flush()
		}
		if hit {
			g.hit, g.pattern = true, id
			return true
		}
	}
	return false
}

// stopped reports whether the guard stopped a stream: the response is then
// complete as far as the app is concerned, whatever the upstream does.
func (g *answerGuard) stopped() bool { return g.blocked && g.midStream }

// flushScanner runs the scanner's last full check at the end of a stream
// and records a hit.
func (g *answerGuard) flushScanner() bool {
	if g.scanner == nil {
		return false
	}
	if hit, id := g.scanner.Flush(); hit {
		g.hit, g.pattern = true, id
		return true
	}
	return false
}

// stopStream ends a stream after a hit: one error event in the caller's
// format, and nothing more.
func (g *answerGuard) stopStream() {
	g.blocked, g.midStream = true, true
	g.mode = modeBlocked
	g.buf.Reset()
	_, _ = g.w.Write(blockedStreamEvent(g.format))
	g.Flush()
	if g.onStop != nil {
		g.onStop()
	}
}

// finish ends the response once the upstream handler returned. aborted says
// the response was cut off (the handler panicked): a held answer is then
// dropped, never delivered in part.
func (g *answerGuard) finish(aborted bool) {
	switch g.mode {
	case modeHeld:
		body := g.buf.Bytes()
		if aborted {
			g.buf.Reset()
			return
		}
		if text, ok := guardrails.AnswerText(g.format, body); ok {
			if g.inspect != nil {
				if hit, id := g.inspect(text); hit {
					g.hit, g.pattern = true, id
					if g.action == guardrails.AnswerActionBlock {
						g.blocked = true
						g.buf.Reset()
						g.writeBlocked(codeBlockedAnswer)
						return
					}
				}
			}
			if g.judge != nil && g.judge(text) {
				g.judgeBlocked = true
				g.buf.Reset()
				g.writeBlocked(codeJudgeBlockedAnswer)
				return
			}
		}
		g.w.WriteHeader(g.status)
		_, _ = g.w.Write(body)
		g.buf.Reset()
	case modeTee:
		if text, ok := guardrails.AnswerText(g.format, g.buf.Bytes()); ok {
			if hit, id := g.inspect(text); hit {
				g.hit, g.pattern = true, id
			}
		}
		g.buf.Reset()
	case modeStream:
		// The last event may have come without the blank line that ends it.
		pending := append([]byte(nil), g.buf.Bytes()...)
		g.buf.Reset()
		frames, err := g.parser.Feed(pending)
		if err == nil {
			frames = append(frames, g.parser.Flush()...)
		}
		if g.checkFrames(frames) || g.flushScanner() {
			if g.action == guardrails.AnswerActionBlock && !aborted {
				g.stopStream()
			}
			return
		}
		if g.action == guardrails.AnswerActionBlock && len(pending) > 0 {
			_, _ = g.w.Write(pending)
		}
	}
}

// writeBlocked replaces a held answer with the 502 error in the caller's
// format, with code. Nothing of the answer goes out; the upstream's length and
// encoding do not describe the error.
func (g *answerGuard) writeBlocked(code string) {
	body := blockedAnswerBody(g.format, code)
	h := g.w.Header()
	h.Del("Content-Encoding")
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	g.w.WriteHeader(http.StatusBadGateway)
	_, _ = g.w.Write(body)
}

// blockedAnswerBody is the body of the 502 that replaces a whole answer: the
// error shape of the caller's format, as the gateway writes its own errors.
func blockedAnswerBody(format, code string) []byte {
	if format == guardrails.FormatMessages {
		return messages.EncodeError(http.StatusBadGateway, code, messageBlockedAnswer)
	}
	return chat.EncodeError(http.StatusBadGateway, code, messageBlockedAnswer)
}

// blockedStreamEvent is the one event that ends a stopped stream: for Chat
// a data frame with an "error" object and then "[DONE]", for Responses an
// "error" event, for Messages an "event: error" frame.
func blockedStreamEvent(format string) []byte {
	var b bytes.Buffer
	switch format {
	case guardrails.FormatResponses:
		data := []byte(`{"type":"error","code":`)
		data = ir.AppendString(data, codeBlockedAnswer)
		data = append(data, `,"message":`...)
		data = ir.AppendString(data, messageBlockedAnswer)
		data = append(data, `,"param":null}`...)
		_ = sse.Write(&b, "error", data)
	case guardrails.FormatMessages:
		_ = sse.Write(&b, "error", messages.EncodeError(http.StatusBadGateway, codeBlockedAnswer, messageBlockedAnswer))
	default:
		_ = sse.Write(&b, "", chat.EncodeError(http.StatusBadGateway, codeBlockedAnswer, messageBlockedAnswer))
		_ = sse.Write(&b, "", []byte("[DONE]"))
	}
	return b.Bytes()
}

// frameEnd returns the length of the first complete event in b, up to and
// including the blank line that ends it, or -1 when b holds none yet. Lines
// end with "\n", "\r\n" or "\r"; a "\r" at the end of b waits for the next
// byte, which may be its "\n".
func frameEnd(b []byte) int {
	i := 0
	for i < len(b) {
		j := bytes.IndexAny(b[i:], "\r\n")
		if j < 0 {
			return -1
		}
		end := i + j
		next := end + 1
		if b[end] == '\r' {
			if next >= len(b) {
				return -1
			}
			if b[next] == '\n' {
				next++
			}
		}
		if end == i {
			return next // a blank line
		}
		i = next
	}
	return -1
}

// Flush forwards a flush, except while an answer is held: a flush then would
// send the status before it is decided.
func (g *answerGuard) Flush() {
	if g.mode == modeHeld || g.mode == modeUndecided {
		return
	}
	if f, ok := g.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the wrapped writer when it supports it.
func (g *answerGuard) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := g.w.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("aigw: underlying ResponseWriter does not implement Hijacker")
}
