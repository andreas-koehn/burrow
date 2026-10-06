package aigateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommitWriter_CommitsNonRetryable(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Burrow-Request-Id", "req-1")
	committed := 0
	cw := newCommitWriter(rec, func(int) bool { return false }, func(status int) { committed = status })
	cw.Header().Set("Content-Type", "application/json")
	cw.Header().Set("Burrow-Request-Id", "from-upstream")
	cw.WriteHeader(201)
	_, _ = cw.Write([]byte("body"))
	if rec.Code != 201 || rec.Body.String() != "body" || committed != 201 || !cw.committed {
		t.Fatalf("code %d body %q committed %d", rec.Code, rec.Body.String(), committed)
	}
	// An upstream cannot replace the relay's request id.
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Burrow-Request-Id") != "req-1" {
		t.Fatalf("headers: %v", rec.Header())
	}
}

func TestCommitWriter_SwallowsRetryable(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCommitWriter(rec, func(s int) bool { return s >= 500 }, func(int) { t.Fatal("must not commit") })
	cw.Header().Set("X-Upstream", "leak")
	cw.WriteHeader(502)
	n, err := cw.Write([]byte("upstream error page"))
	cw.Flush()
	if n != 19 || err != nil {
		t.Fatalf("Write on a discarded response must pretend success: %d %v", n, err)
	}
	if cw.committed || !cw.discarded || cw.status != 502 {
		t.Fatalf("state: %+v", cw)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("X-Upstream") != "" || rec.Flushed {
		t.Fatal("a discarded attempt leaked to the client")
	}
}

func TestCommitWriter_ImplicitOKAndFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCommitWriter(rec, func(int) bool { return false }, func(int) {})
	_, _ = cw.Write([]byte("data: one\n\n")) // no explicit WriteHeader
	cw.Flush()
	if rec.Code != 200 || !rec.Flushed || !cw.committed {
		t.Fatalf("code %d flushed %v", rec.Code, rec.Flushed)
	}
}

// Nothing is held back once the decision is made: the status line and every
// write reach the client at once, and a flush before the decision does not.
func TestCommitWriter_NothingIsBufferedAfterTheDecision(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCommitWriter(rec, func(int) bool { return false }, func(int) {})
	cw.Header().Set("Content-Type", "text/event-stream")
	cw.Flush()
	if rec.Flushed {
		t.Fatal("a flush before the decision reached the client")
	}
	cw.WriteHeader(200)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status line not passed on at the decision: %d %v", rec.Code, rec.Header())
	}
	_, _ = cw.Write([]byte("data: one\n\n"))
	if rec.Body.String() != "data: one\n\n" {
		t.Fatalf("first event held back: %q", rec.Body.String())
	}
	if err := http.NewResponseController(cw).Flush(); err != nil || !rec.Flushed {
		t.Fatalf("flush through ResponseController: err %v flushed %v", err, rec.Flushed)
	}
	// Headers set after the commit (trailers) go to the client's header map.
	cw.Header().Set("X-Late", "1")
	if rec.Header().Get("X-Late") != "1" {
		t.Fatal("Header() after the commit is not the client's")
	}
}

func TestCommitWriter_FirstStatusWins(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCommitWriter(rec, func(s int) bool { return s >= 500 }, func(int) {})
	cw.WriteHeader(503)
	cw.WriteHeader(200) // a handler that tries again on the same writer must not un-discard it
	_, _ = cw.Write([]byte("x"))
	if cw.committed || rec.Body.Len() != 0 {
		t.Fatal("a discarded writer came back to life")
	}
}

func TestCommitWriter_InformationalIsNoDecision(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCommitWriter(rec, func(s int) bool { return s >= 500 }, func(int) {})
	cw.WriteHeader(103)
	if cw.committed || cw.discarded || cw.status != 0 {
		t.Fatalf("state after 103: %+v", cw)
	}
	cw.WriteHeader(200)
	if !cw.committed || rec.Code != 200 {
		t.Fatal("the final status was not committed")
	}
}

// ReverseProxy flushes through http.ResponseController. The wrapper must offer
// Flush itself and must NOT offer Unwrap, or a flush would reach the client
// while the attempt is still undecided.
func TestCommitWriter_NoUnwrap(t *testing.T) {
	var w any = newCommitWriter(httptest.NewRecorder(), func(int) bool { return false }, func(int) {})
	if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
		t.Fatal("commitWriter must not expose Unwrap")
	}
	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("commitWriter must implement http.Flusher")
	}
}
