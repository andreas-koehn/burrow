package httpduplex

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer collects a server's error log.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// front serves h through a reverse proxy to upstream and returns the server
// and its error log.
func front(t *testing.T, upstream http.Handler, wrap func(rp http.Handler) http.Handler) (*httptest.Server, *syncBuffer) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	target, _ := url.Parse(up.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	logged := &syncBuffer{}
	rp.ErrorLog = log.New(logged, "proxy: ", 0)
	srv := httptest.NewUnstartedServer(wrap(rp))
	srv.Config.ErrorLog = log.New(logged, "server: ", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, logged
}

func duplex(rp http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Serve(w, r, rp) })
}

// An upstream that answers before it has read the request body still gets all
// of it, and the client the whole answer.
func TestServe_UpstreamAnswersBeforeTheBodyIsRead(t *testing.T) {
	srv, _ := front(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex()
		_, _ = io.WriteString(w, "early\n")
		_ = rc.Flush()
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = fmt.Fprintf(w, "got=%d\n", n)
	}), duplex)

	half := strings.Repeat("x", 4096)
	pr, pw := io.Pipe()
	sawEarly := make(chan struct{})
	go func() {
		_, _ = io.WriteString(pw, half)
		select {
		case <-sawEarly:
			_, _ = io.WriteString(pw, half)
			_ = pw.Close()
		case <-time.After(20 * time.Second):
			_ = pw.CloseWithError(fmt.Errorf("the response never started"))
		}
	}()
	resp, err := http.Post(srv.URL+"/upload", "application/octet-stream", pr)
	if err != nil {
		t.Fatalf("the response did not start while the request body was still being sent: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); line != "early\n" {
		t.Fatalf("first line = %q err = %v", line, err)
	}
	close(sawEarly)
	rest, err := io.ReadAll(br)
	if want := fmt.Sprintf("got=%d\n", 2*len(half)); err != nil || string(rest) != want {
		t.Fatalf("rest = %q err = %v, want %q", rest, err, want)
	}
}

// lateTail sends a request whose last bytes arrive only after the whole
// response has been read, then a second request on the same connection, and
// returns the second response's status line (or the error).
//
// This is the hazard of full duplex alone: the proxy's transport is still
// waiting for the end of the request body when the handler returns. When that
// read ends afterwards, it collides with the server reading the next request
// on the connection: the connection's goroutine panics ("invalid concurrent
// Body.Read call") and the connection is dropped.
func lateTail(t *testing.T, addr string) (string, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n01234"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("first response: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(got) != "ok" {
		t.Fatalf("first response: %d %q", resp.StatusCode, got)
	}
	// The response is complete; now the rest of the body, and the next request.
	if _, err := io.WriteString(conn, "56789"); err != nil {
		return "", err
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := io.WriteString(conn, "GET /y HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		return "", err
	}
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		return "", err
	}
	_, _ = io.ReadAll(resp.Body)
	return resp.Status, nil
}

// answersAtOnce never reads the request body.
var answersAtOnce = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	// Itself a Go server: without this it would wait for the body's end
	// before it sends the answer.
	// A stand-in that leaves the body unread under plain full duplex must not
	// keep its connection (see the package comment of httpduplex).
	w.Header().Set("Connection", "close")
	_ = http.NewResponseController(w).EnableFullDuplex()
	w.Header().Set("Content-Length", "2")
	_, _ = io.WriteString(w, "ok")
})

// With Serve no read of the body outlives the handler: the connection serves
// the next request and the server's log has no panic.
func TestServe_NoBodyReadOutlivesTheHandler(t *testing.T) {
	srv, logged := front(t, answersAtOnce, duplex)
	for i := 0; i < 20; i++ {
		status, err := lateTail(t, srv.Listener.Addr().String())
		if err != nil || status != "200 OK" {
			t.Fatalf("round %d: second request on the connection: %q %v\nserver log:\n%s", i, status, err, logged.String())
		}
	}
	if log := logged.String(); strings.Contains(log, "panic") || strings.Contains(log, "concurrent Body.Read") {
		t.Fatalf("the server's log has a panic:\n%s", log)
	}
}

// twoOnOneConnection sends a request with a body of n bytes, reads the answer,
// and sends a second request on the same connection. It returns the status of
// the second answer, or the error of reading it.
func twoOnOneConnection(t *testing.T, addr string, n int) (string, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(conn)
	go func() {
		_, _ = fmt.Fprintf(conn, "POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s", n, strings.Repeat("z", n))
	}()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("first response: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(got) != "ok" {
		t.Fatalf("first response: %d %q", resp.StatusCode, got)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := io.WriteString(conn, "GET /y HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		return "", err
	}
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		return "", err
	}
	_, _ = io.ReadAll(resp.Body)
	return resp.Status, nil
}

// neverReads answers and returns without touching the request body: what a
// proxy does when the upstream answers before the body has been forwarded.
var neverReads = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "2")
	_, _ = io.WriteString(w, "ok")
})

// A handler that returns with the body unread leaves the connection usable:
// the next request on it is served, and the server's log has no panic.
func TestServe_HandlerReturnsWithTheBodyUnread(t *testing.T) {
	logged := &syncBuffer{}
	srv := httptest.NewUnstartedServer(duplex(neverReads))
	srv.Config.ErrorLog = log.New(logged, "server: ", 0)
	srv.Start()
	defer srv.Close()
	for _, n := range []int{10, 4096, 200 << 10} {
		status, err := twoOnOneConnection(t, srv.Listener.Addr().String(), n)
		if err != nil || status != "200 OK" {
			t.Fatalf("body of %d bytes: second request on the connection: %q %v\nserver log:\n%s", n, status, err, logged.String())
		}
	}
	// More left than the server reads: it closes the connection, without a panic.
	_, _ = twoOnOneConnection(t, srv.Listener.Addr().String(), 1<<20)
	if log := logged.String(); strings.Contains(log, "panic") || strings.Contains(log, "concurrent Body.Read") {
		t.Fatalf("the server's log has a panic:\n%s", log)
	}
}

// The control: full duplex alone panics in that situation (Go 1.25). Should a
// later Go make it safe by itself, this test says so and Serve's drain can go.
func TestPlainFullDuplex_PanicsWhenTheBodyIsLeftUnread(t *testing.T) {
	logged := &syncBuffer{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		neverReads(w, r)
	}))
	srv.Config.ErrorLog = log.New(logged, "server: ", 0)
	srv.Start()
	defer srv.Close()
	status, err := twoOnOneConnection(t, srv.Listener.Addr().String(), 10)
	if strings.Contains(logged.String(), "concurrent Body.Read") {
		return // the hazard is real: the second request got no answer
	}
	t.Skipf("plain full duplex did not panic here (second request: %q %v): Serve's drain may no longer be needed with this Go version", status, err)
}

// A read that starts after the handler has returned does not reach the body.
func TestGuard_StopsReads(t *testing.T) {
	g := &guard{rc: io.NopCloser(strings.NewReader("abcdef"))}
	buf := make([]byte, 3)
	if n, err := g.Read(buf); n != 3 || err != nil {
		t.Fatalf("read before stop: %d %v", n, err)
	}
	g.stop()
	if n, err := g.Read(buf); n != 0 || err != errHandlerReturned {
		t.Fatalf("read after stop: %d %v", n, err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A request without a body, a recorder and an already guarded request are
// passed through untouched.
func TestServe_PassesThroughWhereThereIsNothingToGuard(t *testing.T) {
	var seen io.ReadCloser
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.Body })

	req := httptest.NewRequest("GET", "/", nil)
	Serve(httptest.NewRecorder(), req, h)
	if seen != req.Body {
		t.Fatal("a request without a body was given another body")
	}
	// A recorder is not a server connection: full duplex is not available.
	req = httptest.NewRequest("POST", "/", strings.NewReader("x"))
	Serve(httptest.NewRecorder(), req, h)
	if seen != req.Body {
		t.Fatal("the body was wrapped although the writer does not support full duplex")
	}
	g := &guard{rc: io.NopCloser(strings.NewReader("x"))}
	req = httptest.NewRequest("POST", "/", nil)
	req.Body = g
	Serve(httptest.NewRecorder(), req, h)
	if seen != io.ReadCloser(g) {
		t.Fatal("a guarded body was wrapped again")
	}
}
