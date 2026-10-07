// Package httpduplex lets a handler forward a request body that is still
// arriving while the response is already being written, as a reverse proxy
// does.
//
// Go's HTTP/1 server is half duplex by default: when the response headers are
// written it consumes what is left of the request body. A proxy still
// forwarding that body then sends a short one, the upstream connection is
// dropped and the client gets a truncated response.
// http.ResponseController.EnableFullDuplex turns that off, but leaves two
// hazards of its own on a connection that is kept alive (seen with Go 1.25):
//
//   - A handler that returns before the body has been read to its end. The
//     server then reads the rest itself, after it has stopped watching the
//     connection, and the watch it starts at the body's end collides with its
//     own read of the next request: the connection's goroutine panics
//     ("invalid concurrent Body.Read call") and the connection is dropped. A
//     proxy is such a handler whenever the upstream answers without reading
//     the body.
//   - A read of the body that is still under way when the handler returns: a
//     proxy's transport reads the body from a goroutine of its own.
//
// Serve enables full duplex and closes both. When the handler returns, no
// read of the body is under way or will start, and the connection is either
// at the end of the body, ready for the next request, or marked to be closed
// after the response. The client's body is never waited for longer than
// settleTimeout nor read further than maxLeftForServer bytes: a complete
// response to the client always wins over reusing the connection.
//
// HTTP/2 has none of this (a stream's body is not the connection's), so its
// requests are passed through untouched.
package httpduplex

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errHandlerReturned is what a read of the body gets once the handler that
// was given it has returned.
var errHandlerReturned = errors.New("httpduplex: request body read after the handler returned")

// settleTimeout is how long, once the handler has returned, the client is
// given to deliver the rest of its request body before the connection is
// given up (closed after the response). It bounds the wait for a read that is
// under way and the reading of what is left, together. A variable for the
// tests only.
var settleTimeout = 3 * time.Second

// maxLeftForServer is how much of a request body is read and dropped after
// the handler has returned, at most. It mirrors net/http's
// maxPostHandlerReadBytes: with more than this left, Go's server does not
// keep the connection either.
const maxLeftForServer = 256 << 10

// Serve calls h with full duplex enabled for the request: h may read r.Body
// while it writes the response. See the package comment for what happens
// when h returns.
//
// A request without a body, an HTTP/2 request, and a writer that is not a
// server connection's (a test recorder) are passed to h as they are.
//
// A handler that hijacks the connection owns it from then on, also after it
// has returned: Serve then leaves the connection alone.
func Serve(w http.ResponseWriter, r *http.Request, h http.Handler) {
	if r.ProtoMajor != 1 || r.Body == nil || r.Body == http.NoBody {
		h.ServeHTTP(w, r)
		return
	}
	if _, guarded := r.Body.(*guard); guarded {
		// A caller further out has done it for this request.
		h.ServeHTTP(w, r)
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.EnableFullDuplex(); err != nil {
		// Not an HTTP/1 server connection: nothing consumes the body early
		// and nothing reads a next request from it.
		h.ServeHTTP(w, r)
		return
	}
	g := &guard{rc: r.Body}
	// A shallow copy: the caller's request keeps its own Body field.
	r2 := new(http.Request)
	*r2 = *r
	r2.Body = g
	hw := &hijackNoter{ResponseWriter: w}
	returned := false
	defer func() {
		if hw.hijacked.Load() {
			// The connection is the handler's now, and may be in use by a
			// goroutine it left behind: a read deadline or a read of ours
			// would break it. The server is done with it as well.
			return
		}
		if !returned {
			// h panicked (http.ErrAbortHandler ends a broken stream that
			// way) and the server drops the connection. A read under way is
			// ended at once; the panic goes on unchanged.
			_ = rc.SetReadDeadline(time.Now())
			g.stop()
			return
		}
		settle(w, rc, r, g)
	}()
	h.ServeHTTP(hw, r2)
	returned = true
}

// hijackNoter is the writer the handler gets: the caller's, with a note of
// whether the connection was hijacked. Everything else reaches the writer
// underneath: Unwrap serves http.ResponseController (deadlines, full duplex),
// Flush and Hijack are passed on, and so is NoteUpstreamTimeout, which a
// provider's upstream handler looks for with a type assertion
// (aiprovider.TimeoutNoter) that would not see through Unwrap.
type hijackNoter struct {
	http.ResponseWriter
	hijacked atomic.Bool
}

func (h *hijackNoter) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// Flush implements http.Flusher.
func (h *hijackNoter) Flush() { _ = h.FlushError() }

// FlushError is the form of Flush http.ResponseController asks for.
func (h *hijackNoter) FlushError() error {
	return http.NewResponseController(h.ResponseWriter).Flush()
}

// Hijack implements http.Hijacker.
func (h *hijackNoter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(h.ResponseWriter).Hijack()
	if err == nil {
		h.hijacked.Store(true)
	}
	return conn, brw, err
}

// NoteUpstreamTimeout passes the note on to a writer that wants it.
func (h *hijackNoter) NoteUpstreamTimeout() {
	if tn, ok := h.ResponseWriter.(interface{ NoteUpstreamTimeout() }); ok {
		tn.NoteUpstreamTimeout()
	}
}

// settle leaves the connection, after the handler has returned, either at
// the end of the request body or marked to be closed after the response.
func settle(w http.ResponseWriter, rc *http.ResponseController, r *http.Request, g *guard) {
	if !g.touched() && expectsContinue(r) {
		// The client waits for "100 Continue" before it sends the body, and
		// the handler answered without asking for it. Reading now would ask
		// for it. The connection cannot be used again: the client may or may
		// not send the body. No read is under way; the deadline is for one
		// that starts just now and would otherwise hold up stop.
		_ = rc.SetReadDeadline(time.Now())
		g.stop()
		giveUp(w, rc)
		return
	}
	// One deadline for the read that may be under way and for what is read
	// after it. It also ends a read the proxy's transport is blocked in.
	_ = rc.SetReadDeadline(time.Now().Add(settleTimeout))
	g.stop()
	if g.drain(r.ContentLength) {
		// At the end of the body: the connection takes the next request.
		_ = rc.SetReadDeadline(time.Time{})
		return
	}
	giveUp(w, rc)
}

// expectsContinue reports whether the client announced its body with
// "Expect: 100-continue".
func expectsContinue(r *http.Request) bool {
	for _, v := range r.Header.Values("Expect") {
		if strings.EqualFold(strings.TrimSpace(v), "100-continue") {
			return true
		}
	}
	return false
}

// giveUp has the server close the connection once the response is complete,
// and keeps it from reading more of the request body on the way.
//
// The response has usually started, so a "Connection: close" header is too
// late. net/http closes a connection after the reply when a body passes the
// limit of an http.MaxBytesReader: a one-byte reader with a limit of zero
// does that. The reader is given the server's own writer, found under the
// wrappers that have Unwrap.
//
// This leans on a detail of net/http, not on a documented promise. If the
// hook ever stopped working the connection would be kept, and the part of the
// request body that was never read would be parsed as the next request on
// it. The stall tests (stall_test.go, and TestProxy_UploadAnsweredEarly in
// internal/proxy) require the connection to be closed and turn red then.
func giveUp(w http.ResponseWriter, rc *http.ResponseController) {
	for {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = u.Unwrap()
	}
	var one [1]byte
	_, _ = http.MaxBytesReader(w, io.NopCloser(strings.NewReader("x")), 0).Read(one[:])
	// The server closes the request body after the handler and would read
	// on, looking for its end. With the deadline in the past that read ends
	// at once.
	_ = rc.SetReadDeadline(time.Now())
}

// guard is a request body that can be stopped: after stop no read of the
// handler's reaches the body underneath.
type guard struct {
	rc io.ReadCloser

	mu      sync.Mutex // held for the length of a Read
	stopped bool
	n       int64 // bytes read so far
	eof     bool  // the body has been read to its end
	failed  bool  // reading it failed

	askMu sync.Mutex
	asked bool // a read has been started
}

func (g *guard) Read(p []byte) (int, error) {
	g.askMu.Lock()
	g.asked = true
	g.askMu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return 0, errHandlerReturned
	}
	n, err := g.rc.Read(p)
	g.n += int64(n)
	switch {
	case err == io.EOF:
		g.eof = true
	case err != nil:
		g.failed = true
	}
	return n, err
}

// touched reports whether a read of the body was ever started.
func (g *guard) touched() bool {
	g.askMu.Lock()
	defer g.askMu.Unlock()
	return g.asked
}

// Close closes nothing: the body underneath is the server's, which closes it
// when the handler has returned. A transport that is done with the body
// early must not make the server read the rest while the handler still runs.
func (g *guard) Close() error { return nil }

// stop waits for a read that is under way and refuses every later one. The
// caller bounds the wait with a read deadline on the connection.
func (g *guard) stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
}

// drain reads what is left of the body, after stop, and reports whether the
// body is at its end. It gives up, without reading, on a body of known
// length with more than maxLeftForServer left, and after maxLeftForServer
// bytes of a body of unknown length; the connection's read deadline ends it
// in time.
func (g *guard) drain(contentLength int64) (atEnd bool) {
	if g.eof {
		return true
	}
	if g.failed {
		return false
	}
	if contentLength > 0 && contentLength-g.n > maxLeftForServer {
		return false
	}
	// One byte more than the cap tells "ended within it" from "goes on".
	n, err := io.Copy(io.Discard, io.LimitReader(g.rc, maxLeftForServer+1))
	return err == nil && n <= maxLeftForServer
}
