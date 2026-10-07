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
// Serve enables full duplex and closes both: when the handler returns, no
// read of the body is under way or will start, and the body has been read to
// its end unless so much is left that the server will close the connection.
package httpduplex

import (
	"errors"
	"io"
	"net/http"
	"sync"
)

// errHandlerReturned is what a read of the body gets once the handler that
// was given it has returned.
var errHandlerReturned = errors.New("httpduplex: request body read after the handler returned")

// maxLeftForServer mirrors net/http's maxPostHandlerReadBytes: with more than
// this left of a body of known length, the server does not read the rest and
// closes the connection after the response.
const maxLeftForServer = 256 << 10

// Serve calls h with full duplex enabled for the request, if the connection
// supports it: h may read r.Body while it writes the response. When h
// returns, a read of the body that is under way is waited for, every later
// one fails, and what is left of the body is read and dropped (see the
// package comment), so nothing of this request touches the connection
// afterwards.
//
// A request without a body, and a writer that is not a server connection's
// (a test recorder), are passed to h as they are.
func Serve(w http.ResponseWriter, r *http.Request, h http.Handler) {
	if r.Body == nil || r.Body == http.NoBody {
		h.ServeHTTP(w, r)
		return
	}
	if _, guarded := r.Body.(*guard); guarded {
		// A caller further out has done it for this request.
		h.ServeHTTP(w, r)
		return
	}
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
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
	returned := false
	defer func() {
		g.stop()
		// When h panicked (http.ErrAbortHandler ends a broken stream that
		// way) the server drops the connection: nothing is left to settle.
		if returned {
			g.drain(r.ContentLength)
		}
	}()
	h.ServeHTTP(w, r2)
	returned = true
}

// guard is a request body that can be stopped: after stop no read of the
// handler's reaches the body underneath.
type guard struct {
	rc io.ReadCloser

	mu      sync.Mutex // held for the length of a Read
	stopped bool
	n       int64 // bytes read so far
	done    bool  // the body has ended, or reading it failed
}

func (g *guard) Read(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return 0, errHandlerReturned
	}
	n, err := g.rc.Read(p)
	g.n += int64(n)
	if err != nil {
		g.done = true
	}
	return n, err
}

// Close closes nothing: the body underneath is the server's, which closes it
// when the handler has returned. A transport that is done with the body
// early must not make the server read the rest while the handler still runs.
func (g *guard) Close() error { return nil }

// stop waits for a read that is under way and refuses every later one.
func (g *guard) stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
}

// drain reads what is left of the body, after stop. A body of known length
// with more left than the server would read is left alone: the server closes
// that connection. A body of unknown length is read to its end, as the server
// would have to before the next request.
func (g *guard) drain(contentLength int64) {
	if g.done {
		return
	}
	if contentLength > 0 && contentLength-g.n > maxLeftForServer {
		return
	}
	_, _ = io.Copy(io.Discard, g.rc)
}
