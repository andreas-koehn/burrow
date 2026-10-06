package proxy_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proxy"
)

// tcpDialer is a StreamDialer whose "tunnel stream" is a TCP connection to a
// real upstream server. It counts the streams it opened and the ones that
// were closed: a stream that stays open after its request is a leak on the
// relay and, behind it, on the client and at the local app.
type tcpDialer struct {
	addr   string
	res    proxy.Resolved
	opened atomic.Int64
	closed atomic.Int64
}

type countedConn struct {
	net.Conn
	once   sync.Once
	closed *atomic.Int64
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.closed.Add(1) })
	return c.Conn.Close()
}

func (d *tcpDialer) Lookup(context.Context, string) (*proxy.Resolved, error) {
	r := d.res
	return &r, nil
}
func (d *tcpDialer) LookupByServiceID(context.Context, string) (*proxy.Resolved, error) {
	r := d.res
	return &r, nil
}
func (d *tcpDialer) DialTunnelStream(ctx context.Context, _ string) (net.Conn, error) {
	return d.DialTunnelStreamByServiceID(ctx, "")
}
func (d *tcpDialer) DialTunnelStreamByServiceID(ctx context.Context, _ string) (net.Conn, error) {
	var nd net.Dialer
	c, err := nd.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, err
	}
	d.opened.Add(1)
	return &countedConn{Conn: c, closed: &d.closed}, nil
}

// settled waits until every opened stream is closed and returns both counts.
func (d *tcpDialer) settled() (opened, closed int64) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		opened, closed = d.opened.Load(), d.closed.Load()
		if opened == closed || time.Now().After(deadline) {
			return opened, closed
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// leakStack is a proxy in front of a real upstream server. The upstream has no
// idle timeout: it never closes a connection the proxy leaves open.
func leakStack(t *testing.T, upstream http.Handler, pathPrefix string, opts ...proxy.Option) (*tcpDialer, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	d := &tcpDialer{addr: strings.TrimPrefix(up.URL, "http://"),
		res: proxy.Resolved{ServiceID: "svc1", TunnelID: "tun1", AccessMode: "open", LocalHost: "127.0.0.1:3000"}}
	p := proxy.New(d, openChecker{}, authDomain, testLog(), opts...)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pathPrefix != "" {
			r = r.WithContext(proxy.WithPathPrefix(r.Context(), pathPrefix))
		}
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return d, ts
}

// Each answered request closes its tunnel stream. Before, the stream stayed in
// the idle pool of a transport nobody used again, until the local app closed
// it: for 200 requests, 200 open streams and 600 goroutines.
func TestProxy_ClosesTheStreamOfEachAnsweredRequest(t *testing.T) {
	lookup := func(_ context.Context, host string) (string, bool, error) {
		return "svc1", host == "app.example.org", nil
	}
	for _, tc := range []struct {
		name   string
		host   string
		prefix string
		opts   []proxy.Option
	}{
		{name: "host route", host: "abc123." + authDomain},
		{name: "path route", host: "abc123." + authDomain, prefix: "/svc/abc123"},
		{name: "through the AI chain", host: "abc123." + authDomain, opts: []proxy.Option{proxy.WithAIChain(passChain{})}},
		{name: "custom domain", host: "app.example.org", opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup)}},
		{name: "custom domain through the AI chain", host: "app.example.org", opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup), proxy.WithAIChain(passChain{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawClose atomic.Int64
			d, ts := leakStack(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The request the app sees is the one it always saw: the
				// relay does not ask it to close the connection.
				if r.Close || r.Header.Get("Connection") != "" {
					sawClose.Add(1)
				}
				if r.URL.Path == "/missing" {
					http.NotFound(w, r)
					return
				}
				_, _ = io.WriteString(w, "ok")
			}), tc.prefix, tc.opts...)
			hc := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 8}}
			defer hc.CloseIdleConnections()
			warm := func(n int) {
				var wg sync.WaitGroup
				for g := 0; g < 8; g++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := 0; i < n/8; i++ {
							target := "/x"
							if i%5 == 0 {
								target = "/missing"
							}
							req, _ := http.NewRequest("GET", ts.URL+target, nil)
							req.Host = tc.host
							resp, err := hc.Do(req)
							if err != nil {
								t.Error(err)
								return
							}
							_, _ = io.Copy(io.Discard, resp.Body)
							resp.Body.Close()
						}
					}()
				}
				wg.Wait()
			}
			warm(16) // the visitor's own connections exist from here on
			d.settled()
			before := runtime.NumGoroutine()
			const n = 200
			warm(n)
			opened, closed := d.settled()
			if opened != n+16 || closed != opened {
				t.Fatalf("%d streams were opened and %d closed", opened, closed)
			}
			deadline := time.Now().Add(5 * time.Second)
			for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if after := runtime.NumGoroutine(); after > before+2 {
				t.Fatalf("%d goroutines before %d requests, %d after", before, n, after)
			}
			t.Logf("%d requests: %d streams opened, %d closed; goroutines %d before, %d after", n, opened-16, closed-16, before, runtime.NumGoroutine())
			if sawClose.Load() != 0 {
				t.Fatalf("%d requests reached the app with a Connection header", sawClose.Load())
			}
		})
	}
}

// A response that is still being sent is not cut: the stream is closed when
// the proxy is done with it, not before.
func TestProxy_StreamedResponseArrivesWhole(t *testing.T) {
	d, ts := leakStack(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 40; i++ {
			fmt.Fprintf(w, "chunk %02d %s\n", i, strings.Repeat("x", 4000))
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}), "/svc/abc123")
	req, _ := http.NewRequest("GET", ts.URL+"/stream", nil)
	req.Host = "abc123." + authDomain
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("the response was cut: %v", err)
	}
	if n := strings.Count(string(body), "\n"); n != 40 || !strings.HasPrefix(string(body), "chunk 00 ") || !strings.Contains(string(body), "chunk 39 ") || len(body) != 40*4010 {
		t.Fatalf("%d chunks, %d bytes", n, len(body))
	}
	if opened, closed := d.settled(); opened != 1 || closed != 1 {
		t.Fatalf("%d streams opened, %d closed", opened, closed)
	}
}

// An event stream whose reader is slower than its writer: every event arrives,
// in order, each as soon as it is read, and the stream stays open until the
// app ends it.
func TestProxy_EventStreamWithASlowReader(t *testing.T) {
	const events = 25
	d, ts := leakStack(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < events; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}), "/svc/abc123")
	req, _ := http.NewRequest("GET", ts.URL+"/events", nil)
	req.Host = "abc123." + authDomain
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	for i := 0; i < events; i++ {
		line, err := br.ReadString('\n')
		if err != nil || line != fmt.Sprintf("data: %d\n", i) {
			t.Fatalf("event %d: %q %v", i, line, err)
		}
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if i == 0 {
			// The first event is here while the app still sends: the stream
			// is open and was not closed under it.
			if o, c := d.opened.Load(), d.closed.Load(); o != 1 || c != 0 {
				t.Fatalf("during the stream: %d opened, %d closed", o, c)
			}
		}
		time.Sleep(25 * time.Millisecond) // slower than the app writes
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("after the last event: %v", err)
	}
	if opened, closed := d.settled(); opened != 1 || closed != 1 {
		t.Fatalf("%d streams opened, %d closed", opened, closed)
	}
}

// A connection that was upgraded (a WebSocket) through /svc/<slug> stays open
// for as long as both ends use it, and is closed when they are done.
func TestProxy_UpgradedConnectionStaysOpen(t *testing.T) {
	d, ts := leakStack(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected upgrade", http.StatusBadRequest)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		// Echo line by line until the visitor hangs up.
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = io.WriteString(rw, "echo "+line)
			_ = rw.Flush()
		}
	}), "/svc/abc123")
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: abc123."+authDomain+"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	br := bufio.NewReader(conn)
	if status, err := br.ReadString('\n'); err != nil || !strings.Contains(status, "101") {
		t.Fatalf("status %q %v", status, err)
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if h == "\r\n" {
			break
		}
	}
	for i := 0; i < 5; i++ {
		time.Sleep(60 * time.Millisecond) // idle in between, as a WebSocket is
		fmt.Fprintf(conn, "message %d\n", i)
		if got, err := br.ReadString('\n'); err != nil || got != fmt.Sprintf("echo message %d\n", i) {
			t.Fatalf("message %d: %q %v (streams: %d opened, %d closed)", i, got, err, d.opened.Load(), d.closed.Load())
		}
	}
	if o, c := d.opened.Load(), d.closed.Load(); o != 1 || c != 0 {
		t.Fatalf("while the connection is in use: %d opened, %d closed", o, c)
	}
	_ = conn.Close()
	if opened, closed := d.settled(); opened != 1 || closed != 1 {
		t.Fatalf("after the visitor left: %d streams opened, %d closed", opened, closed)
	}
}

// A request sent with "Expect: 100-continue": the app's go-ahead reaches the
// visitor, the body follows, and the stream is closed after the answer.
func TestProxy_ExpectContinueLeavesNothingOpen(t *testing.T) {
	for _, prefix := range []string{"", "/svc/abc123"} {
		var got atomic.Int64
		d, ts := leakStack(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body) // reading sends the 100 Continue
			got.Store(n)
			_, _ = io.WriteString(w, "stored")
		}), prefix)
		conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		const body = "twenty bytes of body"
		fmt.Fprintf(conn, "POST /upload HTTP/1.1\r\nHost: abc123.%s\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", authDomain, len(body))
		br := bufio.NewReader(conn)
		status, err := br.ReadString('\n')
		if err != nil || !strings.HasPrefix(status, "HTTP/1.1 100") {
			t.Fatalf("prefix %q: before the body was sent: %q %v", prefix, status, err)
		}
		if blank, err := br.ReadString('\n'); err != nil || blank != "\r\n" {
			t.Fatalf("prefix %q: after the 100 line: %q %v", prefix, blank, err)
		}
		// The stream is in use while the app waits for the body.
		if o, c := d.opened.Load(), d.closed.Load(); o != 1 || c != 0 {
			t.Fatalf("prefix %q: while the app waits for the body: %d opened, %d closed", prefix, o, c)
		}
		_, _ = io.WriteString(conn, body)
		// The app's own go-ahead is passed on as well; a client takes any
		// number of interim answers before the final one.
		var resp *http.Response
		for {
			resp, err = http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("prefix %q: %v", prefix, err)
			}
			if resp.StatusCode != http.StatusContinue {
				break
			}
		}
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(answer) != "stored" || got.Load() != int64(len(body)) {
			t.Fatalf("prefix %q: status %d, answer %q, the app read %d bytes", prefix, resp.StatusCode, answer, got.Load())
		}
		// The visitor's connection stays open (keep-alive); the stream does not.
		if opened, closed := d.settled(); opened != 1 || closed != 1 {
			t.Fatalf("prefix %q: %d streams opened, %d closed", prefix, opened, closed)
		}
		_ = conn.Close()
	}
}

// A large upload arrives whole, and its stream is closed after the answer.
func TestProxy_LargeUploadLeavesNothingOpen(t *testing.T) {
	const size = 24 << 20
	var got atomic.Int64
	d, ts := leakStack(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got.Store(n)
		fmt.Fprintf(w, "%d", n)
	}), "/svc/abc123")
	hc := &http.Client{Transport: &http.Transport{}}
	defer hc.CloseIdleConnections()
	before := runtime.NumGoroutine()
	for i := 0; i < 3; i++ {
		// Once with a length, then chunked (a reader of unknown length).
		var body io.Reader = strings.NewReader(strings.Repeat("u", size))
		if i > 0 {
			body = io.LimitReader(endless('u'), size)
		}
		req, _ := http.NewRequest("POST", ts.URL+"/upload", body)
		req.Host = "abc123." + authDomain
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(answer) != fmt.Sprint(size) || got.Load() != size {
			t.Fatalf("upload %d: status %d, answer %q, the app read %d of %d bytes", i, resp.StatusCode, answer, got.Load(), size)
		}
	}
	if opened, closed := d.settled(); opened != 3 || closed != 3 {
		t.Fatalf("%d streams opened, %d closed", opened, closed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Fatalf("%d goroutines before the uploads, %d after", before, after)
	}
}

type endless byte

func (e endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(e)
	}
	return len(p), nil
}

// A visitor who hangs up in the middle of an upload: the app sees the body
// end early, and the stream is closed although no answer was ever sent.
func TestProxy_AbortedUploadLeavesNothingOpen(t *testing.T) {
	const announced, sent = 4 << 20, 1 << 20
	type seen struct {
		n   int64
		err error
	}
	ended := make(chan seen, 8)
	started := make(chan struct{}, 8)
	d, ts := leakStack(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		n, err := io.Copy(io.Discard, r.Body)
		ended <- seen{n, err}
		_, _ = io.WriteString(w, "never read")
	}), "/svc/abc123")
	before := runtime.NumGoroutine()
	const rounds = 5
	for i := 0; i < rounds; i++ {
		conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprintf(conn, "POST /upload HTTP/1.1\r\nHost: abc123.%s\r\nContent-Length: %d\r\n\r\n", authDomain, announced)
		if _, err := io.WriteString(conn, strings.Repeat("u", sent)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the request did not reach the app")
		}
		_ = conn.Close() // in the middle of the body
		select {
		case s := <-ended:
			if s.err == nil || s.n >= announced {
				t.Fatalf("round %d: the app read %d bytes without an error (announced %d)", i, s.n, announced)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the app still waits for the rest of the body: the stream was not closed", i)
		}
	}
	if opened, closed := d.settled(); opened != rounds || closed != rounds {
		t.Fatalf("%d streams opened, %d closed", opened, closed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("%d goroutines before %d aborted uploads, %d after", before, rounds, after)
	}
}

// wantSink says for which tunnels it wants summaries and counts what it is
// asked and handed.
type wantSink struct {
	summarySink
	want  bool
	asked atomic.Int64
	last  atomic.Value
}

func (w *wantSink) SummaryWanted(tunnelID string) bool {
	w.asked.Add(1)
	w.last.Store(tunnelID)
	return w.want
}

// The proxy asks whether a summary is wanted for the tunnel before it builds
// one: a service whose client did not ask costs a request nothing but that.
func TestSummary_NotBuiltWhenNotWanted(t *testing.T) {
	for _, want := range []bool{false, true} {
		d := newFakeDialer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
		d.register("abc123", &proxy.Resolved{ServiceID: "svc1", TunnelID: "tun1", AccessMode: "open"})
		sink := &wantSink{want: want}
		ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithSummarySink(sink)))
		if code := get(t, ts.URL, "abc123."+authDomain, "/x"); code != 200 {
			t.Fatalf("status %d", code)
		}
		deadline := time.Now().Add(2 * time.Second)
		for sink.asked.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(30 * time.Millisecond)
		ts.Close()
		if sink.asked.Load() != 1 || sink.last.Load() != "tun1" {
			t.Fatalf("want %v: asked %d times, last for %v", want, sink.asked.Load(), sink.last.Load())
		}
		if got := len(sink.all()); got != map[bool]int{false: 0, true: 1}[want] {
			t.Fatalf("want %v: %d summaries were built", want, got)
		}
	}
}
