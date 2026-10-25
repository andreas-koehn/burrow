package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// The grpc-message of a refusal is cut on a rune boundary.
func TestDenyMessage_CutIsUTF8Safe(t *testing.T) {
	long := strings.Repeat("a", maxDenyMessage-1) + "é and more"
	for _, body := range []string{`{"error":"` + long + `"}`, long} {
		msg := denyMessage(503, []byte(body))
		if !utf8.ValidString(msg) || len(msg) > maxDenyMessage {
			t.Fatalf("message %q (%d bytes) is not a valid cut", msg, len(msg))
		}
		if msg != strings.Repeat("a", maxDenyMessage-1) {
			t.Fatalf("cut at %d bytes, want before the é", len(msg))
		}
	}
}

// A trailers-only reply of the service (grpc-status in the headers, no
// body) must leave the relay as one HEADERS frame with END_STREAM, which the
// HTTP/2 server sends only when the headers were not flushed before the
// handler returned. gRPC clients read headers followed by an empty DATA
// frame as a stream without trailers (INTERNAL, "server closed the stream
// without sending trailers"). ReverseProxy with FlushInterval -1 flushes the
// headers on a timer goroutine (time.AfterFunc(-1, …)), which raced the end
// of the handler and lost about one call in ten end to end.
func TestGRPCWriter_TrailersOnlyIsNotFlushed(t *testing.T) {
	newReq := func() *http.Request {
		r := httptest.NewRequest("POST", "/pkg.S/M", strings.NewReader(""))
		r.Header.Set("Content-Type", "application/grpc")
		return r
	}

	t.Run("trailers-only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		g := newGRPCWriter(rec, newReq(), nil)
		g.Header().Set("Grpc-Status", "5")
		g.WriteHeader(http.StatusOK)
		g.Flush()
		if err := g.FlushError(); err != nil {
			t.Fatal(err)
		}
		if rec.Flushed {
			t.Fatal("a trailers-only reply was flushed before the handler ended")
		}
	})

	t.Run("a response with a body is flushed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		g := newGRPCWriter(rec, newReq(), nil)
		g.Header().Set("Content-Type", "application/grpc")
		g.WriteHeader(http.StatusOK)
		g.Flush() // headers of a stream go out before its first message
		if !rec.Flushed {
			t.Fatal("the headers of a streaming response were held")
		}
	})

	t.Run("grpc-status in the headers, then a body", func(t *testing.T) {
		// Not valid gRPC, but nothing is held once bytes passed.
		rec := httptest.NewRecorder()
		g := newGRPCWriter(rec, newReq(), nil)
		g.Header().Set("Grpc-Status", "0")
		g.WriteHeader(http.StatusOK)
		_, _ = g.Write([]byte("x"))
		g.Flush()
		if !rec.Flushed {
			t.Fatal("a body was held")
		}
	})
}
