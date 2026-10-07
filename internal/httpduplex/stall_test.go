package httpduplex

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortSettle makes Serve's wait for the client short for one test.
func shortSettle(t *testing.T) time.Duration {
	t.Helper()
	old := settleTimeout
	settleTimeout = 400 * time.Millisecond
	t.Cleanup(func() { settleTimeout = old })
	return settleTimeout
}

// countingBody counts what is read from a request body.
type countingBody struct {
	io.ReadCloser
	n     *atomic.Int64
	reads *atomic.Int64
}

func (c countingBody) Read(p []byte) (int, error) {
	c.reads.Add(1)
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// stallRig is a server whose handler forwards through Serve to an upstream
// that answers "ok" at once and never reads the request body.
type stallRig struct {
	srv      *httptest.Server
	logged   *syncBuffer
	read     atomic.Int64 // bytes read from the client's request bodies
	reads    atomic.Int64 // read calls on them
	atReturn atomic.Int64 // bytes read when the proxy had returned
	returned chan struct{}
}

func newStallRig(t *testing.T, upstream http.Handler, tune func(*httptest.Server, *http.Transport)) *stallRig {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	target, _ := url.Parse(up.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	tr := &http.Transport{ExpectContinueTimeout: 30 * time.Second}
	t.Cleanup(tr.CloseIdleConnections)
	rp.Transport = tr
	rig := &stallRig{logged: &syncBuffer{}, returned: make(chan struct{}, 16)}
	rp.ErrorLog = log.New(io.Discard, "", 0)
	rig.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { rig.returned <- struct{}{} }()
		r.Body = countingBody{r.Body, &rig.read, &rig.reads}
		Serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rp.ServeHTTP(w, r)
			rig.atReturn.Store(rig.read.Load())
		}))
	}))
	rig.srv.Config.ErrorLog = log.New(rig.logged, "server: ", 0)
	if tune != nil {
		tune(rig.srv, tr)
	} else {
		rig.srv.Start()
	}
	t.Cleanup(rig.srv.Close)
	return rig
}

func (r *stallRig) handlerReturned(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-r.returned:
	case <-time.After(within):
		t.Fatalf("the handler has not returned after %v", within)
	}
}

func (r *stallRig) noPanic(t *testing.T) {
	t.Helper()
	if l := r.logged.String(); strings.Contains(l, "panic") {
		t.Fatalf("the server's log has a panic:\n%s", l)
	}
}

// readAnswer reads one response and requires it complete: status 200, body "ok".
func readAnswer(t *testing.T, br *bufio.Reader) {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || string(got) != "ok" {
		t.Fatalf("the response is not complete: status %d body %q err %v", resp.StatusCode, got, err)
	}
}

// closedWithin requires the server to close the connection: the next read
// ends without another byte.
func closedWithin(t *testing.T, conn net.Conn, br *bufio.Reader, within time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	b, err := br.Peek(1)
	if err == nil {
		rest, _ := br.Peek(br.Buffered())
		t.Fatalf("the connection was kept and sent more: %q (first byte %q)", rest, b)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the connection is still open %v after the response", within)
	}
}

func dial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	return conn, bufio.NewReader(conn)
}

// chunkedAnswer answers at once without a Content-Length: the response ends
// with the final chunk, which the server writes when the handler has returned.
var chunkedAnswer = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	// A stand-in that leaves the body unread under plain full duplex must not
	// keep its connection (see the package comment of httpduplex).
	w.Header().Set("Connection", "close")
	_ = http.NewResponseController(w).EnableFullDuplex()
	_, _ = io.WriteString(w, "ok")
	_ = http.NewResponseController(w).Flush()
})

func goroutinesBackTo(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		buf := make([]byte, 1<<16)
		t.Fatalf("%d goroutines before, %d after:\n%s", base, n, buf[:runtime.Stack(buf, true)])
	}
}

// The upstream answers early and the client stops sending: the response is
// complete within the bound, the handler has returned, the connection is
// closed and nothing is left running. Without a bound the handler waited for
// the rest of the body for ever and the response never got its last chunk.
func TestServe_ClientStopsSendingAfterAnEarlyAnswer(t *testing.T) {
	bound := shortSettle(t)
	for _, tc := range []struct{ name, head string }{
		{"known length", "POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n01234"},
		{"chunked", "POST /x HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n5\r\n01234\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStallRig(t, chunkedAnswer, nil)
			// One request to completion first, so the goroutines that stay
			// (the proxy's idle connection) exist before the count.
			warm, wbr := dial(t, rig.srv.Listener.Addr().String())
			_, _ = io.WriteString(warm, "POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\nhi")
			readAnswer(t, wbr)
			rig.handlerReturned(t, 10*time.Second)
			_ = warm.Close()
			time.Sleep(50 * time.Millisecond)
			base := runtime.NumGoroutine()

			conn, br := dial(t, rig.srv.Listener.Addr().String())
			start := time.Now()
			if _, err := io.WriteString(conn, tc.head); err != nil {
				t.Fatal(err)
			}
			// ... and not a byte more.
			_ = conn.SetReadDeadline(time.Now().Add(bound + 5*time.Second))
			readAnswer(t, br)
			if el := time.Since(start); el > bound+4*time.Second {
				t.Fatalf("the response took %v, the bound is %v", el, bound)
			}
			rig.handlerReturned(t, 5*time.Second)
			closedWithin(t, conn, br, 5*time.Second)
			_ = conn.Close()
			goroutinesBackTo(t, base)
			rig.noPanic(t)
		})
	}
}

// The client trickles an endless chunked body against an early answer: the
// response is complete, the relay stops reading at the cap, and the
// connection is closed.
func TestServe_EndlessChunkedBodyIsReadUpToTheCap(t *testing.T) {
	shortSettle(t)
	settleTimeout = 30 * time.Second // the cap must end it, not the clock
	rig := newStallRig(t, chunkedAnswer, nil)
	conn, br := dial(t, rig.srv.Listener.Addr().String())
	if _, err := io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		chunk := fmt.Sprintf("%x\r\n%s\r\n", 8192, strings.Repeat("c", 8192))
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := io.WriteString(conn, chunk); err != nil {
				return // the relay closed the connection
			}
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	readAnswer(t, br)
	rig.handlerReturned(t, 10*time.Second)
	closedWithin(t, conn, br, 10*time.Second)
	// After the proxy had returned, at most the cap was read (and the one
	// byte that tells the body goes on), plus what a read of the proxy's
	// transport that was under way at that moment still took: a buffer or two.
	const underWay = 64 << 10
	if after := rig.read.Load() - rig.atReturn.Load(); after > maxLeftForServer+1+underWay {
		t.Fatalf("%d bytes of the body were read after the answer; the cap is %d", after, maxLeftForServer)
	} else {
		t.Logf("read after the answer: %d bytes (cap %d)", after, maxLeftForServer)
	}
	rig.noPanic(t)
}

// An upload announced with "Expect: 100-continue" that the upstream refuses
// at once: the client gets the complete refusal, is never asked for the body
// (no "100 Continue"), and the body is never read.
func TestServe_ExpectContinueRefusedEarly(t *testing.T) {
	bound := shortSettle(t)
	refuse := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "no")
	})
	rig := newStallRig(t, refuse, nil)
	conn, br := dial(t, rig.srv.Listener.Addr().String())
	start := time.Now()
	if _, err := io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 100000\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(bound + 10*time.Second))
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 401") {
		t.Fatalf("first line %q (%v): the client was asked for the body or got no answer", line, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(io.MultiReader(strings.NewReader(line), br)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(resp.Body); err != nil || string(got) != "no" {
		t.Fatalf("the refusal is not complete: %q %v", got, err)
	}
	if el := time.Since(start); el > bound+4*time.Second {
		t.Fatalf("the refusal took %v", el)
	}
	rig.handlerReturned(t, 5*time.Second)
	closedWithin(t, conn, br, 5*time.Second)
	if n, calls := rig.read.Load(), rig.reads.Load(); n != 0 || calls != 0 {
		t.Fatalf("the body was read: %d bytes in %d calls", n, calls)
	}
	rig.noPanic(t)
}

// On HTTP/2 nothing of this applies: Serve passes the request through, and an
// early answer ends the response although the client never ends its body.
func TestServe_HTTP2_EarlyAnswerWhileTheClientStopsSending(t *testing.T) {
	rig := newStallRig(t, chunkedAnswer, func(srv *httptest.Server, _ *http.Transport) {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	})
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _, _ = io.WriteString(pw, "01234") }() // and never more, never closed
	req, _ := http.NewRequest("POST", rig.srv.URL+"/x", pr)
	type result struct {
		proto string
		body  string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := rig.srv.Client().Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		done <- result{resp.Proto, string(b), err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.proto != "HTTP/2.0" || r.body != "ok" {
			t.Fatalf("proto %q body %q err %v", r.proto, r.body, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the response did not end: the handler waits for a body the client has stopped sending")
	}
	rig.handlerReturned(t, 5*time.Second)
}

// A handler that aborts (http.ErrAbortHandler) while a read of the body is
// under way: the abort goes through unchanged and at once, it does not wait
// for the client.
func TestServe_AbortDoesNotWaitForTheClient(t *testing.T) {
	shortSettle(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := make(chan struct{})
		go func() { close(started); _, _ = io.Copy(io.Discard, r.Body) }()
		<-started
		time.Sleep(50 * time.Millisecond) // the read is now waiting for the client
		panic(http.ErrAbortHandler)
	})
	recovered := make(chan any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			recovered <- rec
			panic(rec)
		}()
		Serve(w, r, inner)
	}))
	defer srv.Close()
	conn, br := dial(t, srv.Listener.Addr().String())
	if _, err := io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n01234"); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-recovered:
		if rec != http.ErrAbortHandler {
			t.Fatalf("the panic that came out is %v", rec)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the abort is waiting for a read of the body")
	}
	closedWithin(t, conn, br, 5*time.Second)
}
