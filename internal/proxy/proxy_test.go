package proxy_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/ankoehn/burrow/internal/proxy"
)

// testLog returns a discard logger for tests.
func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --------------------------------------------------------------------------
// Fakes / stubs
// --------------------------------------------------------------------------

// openChecker is a permissive AccessChecker that always allows.
type openChecker struct{}

func (openChecker) Allow(_ context.Context, _ *proxy.Resolved, _ *http.Request) (bool, int, string, http.Header) {
	return true, 0, "", nil
}

// denyChecker always denies with 403.
type denyChecker struct{}

func (denyChecker) Allow(_ context.Context, _ *proxy.Resolved, _ *http.Request) (bool, int, string, http.Header) {
	return false, http.StatusForbidden, "access denied", nil
}

// fakeDialer satisfies StreamDialer using a pre-built upstream server.
// For each call to DialTunnelStream it creates a net.Pipe and runs the upstream
// handler on the server side. This lets a real httptest.Server act as the
// tunnel upstream without requiring actual network addresses.
type fakeDialer struct {
	mu            sync.Mutex
	tunnels       map[string]*proxy.Resolved // subdomain → metadata
	upstream      http.Handler               // the "local" server the tunnel wraps
	lastServiceID string                     // last serviceID passed to DialTunnelStreamByServiceID
}

func newFakeDialer(upstream http.Handler) *fakeDialer {
	return &fakeDialer{
		tunnels:  make(map[string]*proxy.Resolved),
		upstream: upstream,
	}
}

func (d *fakeDialer) register(subdomain string, res *proxy.Resolved) {
	d.mu.Lock()
	d.tunnels[subdomain] = res
	d.mu.Unlock()
}

func (d *fakeDialer) Lookup(_ context.Context, subdomain string) (*proxy.Resolved, error) {
	d.mu.Lock()
	res, ok := d.tunnels[subdomain]
	d.mu.Unlock()
	if !ok {
		return nil, proxy.ErrNotFound
	}
	return res, nil
}

func (d *fakeDialer) LookupByServiceID(_ context.Context, serviceID string) (*proxy.Resolved, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, res := range d.tunnels {
		if res.ServiceID == serviceID {
			return res, nil
		}
	}
	return nil, proxy.ErrNotFound
}

func (d *fakeDialer) DialTunnelStream(_ context.Context, subdomain string) (net.Conn, error) {
	d.mu.Lock()
	_, ok := d.tunnels[subdomain]
	d.mu.Unlock()
	if !ok {
		return nil, proxy.ErrNotFound
	}
	// Create a net.Pipe: proxyConn is handed to the proxy's Transport,
	// serverConn is driven by the upstream handler.
	proxyConn, serverConn := net.Pipe()
	go func() {
		// Serve exactly one HTTP/1.1 request on the server side.
		srv := &http.Server{Handler: d.upstream}
		// httptest.Server wraps a listener; we instead serve a single conn.
		// Use http.Serve with a one-shot listener.
		ln := &singleConnListener{conn: serverConn}
		_ = srv.Serve(ln)
	}()
	return proxyConn, nil
}

func (d *fakeDialer) DialTunnelStreamByServiceID(_ context.Context, serviceID string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Record which serviceID was dialed (for custom-domain routing assertions).
	d.lastServiceID = serviceID
	// Find a subdomain with matching serviceID and delegate to the normal pipe path.
	for sub, res := range d.tunnels {
		if res.ServiceID == serviceID {
			_ = sub // found it; use the upstream handler
			proxyConn, serverConn := net.Pipe()
			go func() {
				srv := &http.Server{Handler: d.upstream}
				ln := &singleConnListener{conn: serverConn}
				_ = srv.Serve(ln)
			}()
			return proxyConn, nil
		}
	}
	return nil, proxy.ErrNotFound
}

// deadDialer is a StreamDialer that always returns ErrNotFound.
type deadDialer struct{}

func (deadDialer) Lookup(_ context.Context, _ string) (*proxy.Resolved, error) {
	return nil, proxy.ErrNotFound
}
func (deadDialer) LookupByServiceID(_ context.Context, _ string) (*proxy.Resolved, error) {
	return nil, proxy.ErrNotFound
}
func (deadDialer) DialTunnelStream(_ context.Context, _ string) (net.Conn, error) {
	return nil, proxy.ErrNotFound
}
func (deadDialer) DialTunnelStreamByServiceID(_ context.Context, _ string) (net.Conn, error) {
	return nil, proxy.ErrNotFound
}

// singleConnListener is a net.Listener that yields exactly one conn then blocks.
type singleConnListener struct {
	once sync.Once
	conn net.Conn
	done chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() {
		c = l.conn
	})
	if c != nil {
		return c, nil
	}
	// Block until Close is called (signals via done channel).
	if l.done == nil {
		l.done = make(chan struct{})
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	if l.done != nil {
		select {
		case <-l.done:
		default:
			close(l.done)
		}
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return &net.TCPAddr{} }

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

const authDomain = "tunnels.example.com"

// TestProxyUnknownSubdomain404 verifies that an unknown subdomain returns 404
// with plain-text "tunnel not found".
func TestProxyUnknownSubdomain404(t *testing.T) {
	p := proxy.New(deadDialer{}, openChecker{}, authDomain, testLog())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://nope.tunnels.example.com/", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	body := rec.Body.String()
	if body != "tunnel not found" {
		t.Errorf("want body 'tunnel not found', got %q", body)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("want text/plain content-type, got %q", ct)
	}
}

// TestProxyHostSuffixMismatch verifies that a host not matching authDomain
// returns 404.
func TestProxyHostSuffixMismatch(t *testing.T) {
	p := proxy.New(deadDialer{}, openChecker{}, authDomain, testLog())
	tests := []struct {
		host string
	}{
		{"foo.other.example.com"},
		{"tunnels.example.com.evil.com"},
		{"notunnels.example.com"},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://"+tc.host+"/", nil)
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("host %q: want 404, got %d", tc.host, rec.Code)
		}
		if rec.Body.String() != "tunnel not found" {
			t.Errorf("host %q: wrong body %q", tc.host, rec.Body.String())
		}
	}
}

// TestProxyRoundTrip verifies that a request is proxied to the upstream and the
// response body is returned correctly.
func TestProxyRoundTrip(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from upstream: " + r.URL.Path))
	})
	d := newFakeDialer(upstream)
	d.register("abc123", &proxy.Resolved{ServiceID: "svc1", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	p := proxy.New(d, openChecker{}, authDomain, testLog())

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/hello", nil)
	req.Host = "abc123." + authDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "hello from upstream") {
		t.Errorf("unexpected body: %q", body)
	}
}

// TestProxyForwardingHeaders verifies that:
//  1. Inbound X-Forwarded-* and Forwarded headers from the visitor are stripped.
//  2. Burrow sets authoritative X-Forwarded-Proto, X-Forwarded-Host, X-Forwarded-For.
func TestProxyForwardingHeaders(t *testing.T) {
	var (
		gotXFP   string
		gotXFH   string
		gotXFF   string
		gotFwd   string
		gotProto string
	)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFP = r.Header.Get("X-Forwarded-Port")
		gotXFH = r.Header.Get("X-Forwarded-Host")
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotFwd = r.Header.Get("Forwarded")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("hdr", &proxy.Resolved{ServiceID: "svc2", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	p := proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithIngressPort("443"))

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "hdr." + authDomain
	// Inject spoofed forwarding headers from the visitor — these MUST be stripped.
	req.Header.Set("X-Forwarded-For", "evil.attacker.com")
	req.Header.Set("X-Forwarded-Proto", "evil")
	req.Header.Set("X-Forwarded-Host", "evil.host")
	req.Header.Set("X-Forwarded-Port", "9999")
	req.Header.Set("Forwarded", "for=evil.attacker.com;host=evil.host")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if gotProto != "https" {
		t.Errorf("X-Forwarded-Proto: want 'https', got %q", gotProto)
	}
	if gotXFH != "hdr."+authDomain {
		t.Errorf("X-Forwarded-Host: want 'hdr.%s', got %q", authDomain, gotXFH)
	}
	if gotXFF == "evil.attacker.com" {
		t.Errorf("visitor's spoofed XFF was not stripped: %q", gotXFF)
	}
	if gotFwd != "" {
		t.Errorf("Forwarded header was not stripped: %q", gotFwd)
	}
	if gotXFP != "443" {
		t.Errorf("X-Forwarded-Port: want '443', got %q", gotXFP)
	}
}

// TestProxyStripsXForwardedPortWhenIngressEmpty verifies that a visitor-supplied
// X-Forwarded-Port header is stripped even when no ingressPort is configured
// (i.e. WithIngressPort is not used). This is the trust-boundary requirement:
// Burrow must unconditionally delete inbound X-Forwarded-Port so an attacker
// cannot inject an arbitrary port value that propagates to upstream.
func TestProxyStripsXForwardedPortWhenIngressEmpty(t *testing.T) {
	var gotXFP string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFP = r.Header.Get("X-Forwarded-Port")
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("noportlabel", &proxy.Resolved{ServiceID: "svc-noport", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	// Intentionally NO WithIngressPort — default empty.
	p := proxy.New(d, openChecker{}, authDomain, testLog())

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "noportlabel." + authDomain
	// Inject a spoofed X-Forwarded-Port — must NOT reach upstream.
	req.Header.Set("X-Forwarded-Port", "9999")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if gotXFP != "" {
		t.Errorf("X-Forwarded-Port should be empty (stripped), but upstream saw %q", gotXFP)
	}
}

// TestProxySSEFlush is the core streaming / FlushInterval=-1 test.
//
// The upstream writes 3 SSE chunks 50 ms apart. The test captures the
// receive timestamps on the client side and asserts that each chunk arrives
// within a short window of when it was written (not batched at the end).
//
// This test FAILS if FlushInterval is not -1 (or if the proxy buffers the
// response body before forwarding).
func TestProxySSEFlush(t *testing.T) {
	const chunks = 3
	const chunkDelay = 50 * time.Millisecond

	// upstream writes chunks one at a time, with a delay between each.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			if i > 0 {
				time.Sleep(chunkDelay)
			}
			fmt.Fprintf(w, "data: chunk%d\n\n", i)
			flusher.Flush()
		}
	})

	d := newFakeDialer(upstream)
	d.register("sse", &proxy.Resolved{ServiceID: "svc3", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	p := proxy.New(d, openChecker{}, authDomain, testLog())

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/events", nil)
	req.Host = "sse." + authDomain

	// Use a custom transport with no response buffering.
	client := &http.Client{Transport: &http.Transport{}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	// Read chunks and record timestamps.
	type chunkResult struct {
		data string
		at   time.Time
	}
	var results []chunkResult
	start := time.Now()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			results = append(results, chunkResult{data: line, at: time.Now()})
			if len(results) == chunks {
				break
			}
		}
	}

	if len(results) != chunks {
		t.Fatalf("want %d chunks, got %d", chunks, len(results))
	}

	// Assert that each chunk arrived within a reasonable time window — not all
	// at the end. With FlushInterval=-1 and 50ms chunk delays:
	//   chunk 0: ~0ms from start
	//   chunk 1: ~50ms from start
	//   chunk 2: ~100ms from start
	//
	// If the proxy buffers (FlushInterval=0 or positive), all chunks arrive
	// together at the end. We detect this by verifying that the first chunk
	// arrives before chunk 1's expected write time (100ms), confirming
	// incremental delivery.
	_ = start
	elapsed0 := results[0].at.Sub(start)
	elapsed2 := results[2].at.Sub(start)

	// chunk 0 must arrive well before all chunks are written (< 90ms).
	if elapsed0 > 90*time.Millisecond {
		t.Errorf("SSE flush: chunk 0 arrived too late (%v); proxy may be buffering", elapsed0)
	}

	// chunk 2 must arrive after the delays (> 80ms from start).
	if elapsed2 < 80*time.Millisecond {
		t.Errorf("SSE flush: chunk 2 arrived too early (%v); timing seems wrong", elapsed2)
	}

	// The spread between chunk 0 and chunk 2 must be at least 2*chunkDelay-tolerance.
	spread := results[2].at.Sub(results[0].at)
	if spread < 70*time.Millisecond {
		t.Errorf("SSE flush: spread between chunk 0 and chunk 2 is %v (want ≥70ms); chunks may not be incremental", spread)
	}
}

// TestProxyH2CUpgrade verifies that h2c upgrade requests are rejected with 505.
func TestProxyH2CUpgrade(t *testing.T) {
	p := proxy.New(deadDialer{}, openChecker{}, authDomain, testLog())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://foo."+authDomain+"/", nil)
	req.Header.Set("Upgrade", "h2c")
	req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusHTTPVersionNotSupported {
		t.Fatalf("want 505, got %d", rec.Code)
	}
}

// TestProxyGatePath verifies that /__burrow/* on the auth domain is dispatched
// to the gate handler when set, and returns 404 when gate is nil.
func TestProxyGatePath(t *testing.T) {
	// Without gate: 404.
	p := proxy.New(deadDialer{}, openChecker{}, authDomain, testLog())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://"+authDomain+"/__burrow/login", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no gate: want 404, got %d", rec.Code)
	}

	// With gate: routed to gate handler.
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("gate ok"))
	})
	p2 := proxy.New(deadDialer{}, openChecker{}, authDomain, testLog(), proxy.WithGate(gate))
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "http://"+authDomain+"/__burrow/login", nil)
	p2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("with gate: want 200, got %d", rec2.Code)
	}
	if rec2.Body.String() != "gate ok" {
		t.Errorf("with gate: unexpected body %q", rec2.Body.String())
	}
}

// TestProxyAccessDenied verifies that when AccessChecker denies, the proxy
// does not open a stream and writes the denial response.
func TestProxyAccessDenied(t *testing.T) {
	// Use a dialer that would panic if DialTunnelStream is called — confirming
	// no stream is opened on denial.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream was called despite access denial")
		w.WriteHeader(500)
	})
	d := newFakeDialer(upstream)
	d.register("secret", &proxy.Resolved{ServiceID: "svc4", AccessMode: "api_key"})

	p := proxy.New(d, denyChecker{}, authDomain, testLog())

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "secret." + authDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "access denied" {
		t.Errorf("want 'access denied', got %q", string(body))
	}
}

// TestProxyWebSocketUpgradeHeaders verifies that WebSocket Upgrade/Connection
// headers pass through to the upstream (httputil.ReverseProxy handles WS
// transparently in Go ≥ 1.20).
func TestProxyWebSocketUpgradeHeaders(t *testing.T) {
	var (
		gotUpgrade    string
		gotConnection string
	)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUpgrade = r.Header.Get("Upgrade")
		gotConnection = r.Header.Get("Connection")
		// Simulate a minimal 101 switching response — enough for the test.
		// (A real WS handshake needs Sec-WebSocket-* headers too; this test
		// only verifies header propagation, not a full WS handshake.)
		w.WriteHeader(http.StatusSwitchingProtocols)
	})
	d := newFakeDialer(upstream)
	d.register("ws", &proxy.Resolved{ServiceID: "svc5", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	p := proxy.New(d, openChecker{}, authDomain, testLog())

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/ws", nil)
	req.Host = "ws." + authDomain
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")

	// Use a transport that does not follow the upgrade to avoid timeout.
	client := &http.Client{
		Transport: &http.Transport{},
		// Don't follow 101 as an error.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		// net.Pipe doesn't fully support bidirectional HTTP upgrade semantics,
		// so an error here is acceptable — what matters is that the headers
		// reached the upstream handler (checked below).
		t.Logf("WS request error (expected with net.Pipe): %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}

	if gotUpgrade != "websocket" {
		t.Errorf("Upgrade header not propagated to upstream: got %q", gotUpgrade)
	}
	_ = gotConnection // Connection header may be transformed by httputil; Upgrade is the key check
}

// TestProxyXFFClientIP verifies that when trusted proxies are configured,
// the authoritative X-Forwarded-For sent upstream reflects the real client IP.
func TestProxyXFFClientIP(t *testing.T) {
	var gotXFF string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("xff", &proxy.Resolved{ServiceID: "svc6", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	// Configure the test server's loopback as trusted.
	_, loopCIDR, _ := net.ParseCIDR("127.0.0.0/8")
	p := proxy.New(d, openChecker{}, authDomain, testLog(),
		proxy.WithTrustedProxies([]*net.IPNet{loopCIDR}))

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "xff." + authDomain
	// Simulate a client IP forwarded by a trusted LB.
	req.Header.Set("X-Forwarded-For", "5.6.7.8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Because the test-server peer is 127.0.0.1 (trusted), the leftmost
	// XFF entry "5.6.7.8" should be used as the client IP.
	if gotXFF != "5.6.7.8" {
		t.Errorf("X-Forwarded-For sent upstream: want '5.6.7.8', got %q", gotXFF)
	}
}

// TestProxyCustomDomainRouting verifies that WithCustomDomainLookup routes a
// request with a non-authDomain Host header to the correct backend service.
// This covers the ~90-line serveCustomDomain path that was previously uncovered.
func TestProxyCustomDomainRouting(t *testing.T) {
	const customHost = "foo.example.com"
	const targetServiceID = "svc-custom-1"

	// Upstream records the Host header it receives.
	var gotHost string
	var gotPath string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("custom-domain-ok"))
	})

	d := newFakeDialer(upstream)
	// Register the service under a subdomain key so LookupByServiceID can find it.
	d.register("custom", &proxy.Resolved{ServiceID: targetServiceID, AccessMode: "open", LocalHost: "127.0.0.1:9000"})

	// customDomainLookup returns targetServiceID for foo.example.com only.
	lookup := func(_ context.Context, host string) (string, bool, error) {
		if host == customHost {
			return targetServiceID, true, nil
		}
		return "", false, nil
	}

	p := proxy.New(d, openChecker{}, authDomain, testLog(),
		proxy.WithCustomDomainLookup(lookup))

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/hello/world", nil)
	req.Host = customHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", resp.StatusCode, body)
	}
	if string(body) != "custom-domain-ok" {
		t.Errorf("unexpected body: %q", body)
	}
	if gotPath != "/hello/world" {
		t.Errorf("upstream path: want /hello/world, got %q", gotPath)
	}
	_ = gotHost // upstreamHost is set to res.LocalHost, not the custom domain — that's expected

	// Verify that DialTunnelStreamByServiceID was called with targetServiceID.
	d.mu.Lock()
	dialedID := d.lastServiceID
	d.mu.Unlock()
	if dialedID != targetServiceID {
		t.Errorf("DialTunnelStreamByServiceID called with %q; want %q", dialedID, targetServiceID)
	}
}

// TestProxyCustomDomainNotFound verifies that a host not matching any custom
// domain (and not matching the authDomain subdomain pattern) returns 404.
func TestProxyCustomDomainNotFound(t *testing.T) {
	lookup := func(_ context.Context, host string) (string, bool, error) {
		return "", false, nil // nothing registered
	}
	p := proxy.New(deadDialer{}, openChecker{}, authDomain, testLog(),
		proxy.WithCustomDomainLookup(lookup))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://unknown.example.com/", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	if rec.Body.String() != "tunnel not found" {
		t.Errorf("want body 'tunnel not found', got %q", rec.Body.String())
	}
}

func TestRewriteLocationHeader(t *testing.T) {
	cases := []struct{ loc, prefix, want string }{
		{"/login", "/t/abc", "/t/abc/login"},
		{"/", "/t/abc", "/t/abc/"},
		{"https://other.example/x", "/t/abc", "https://other.example/x"}, // absolute: untouched
		{"//evil.example/x", "/t/abc", "//evil.example/x"},               // protocol-relative: untouched
		{"", "/t/abc", ""},
	}
	for _, c := range cases {
		if got := proxy.RewriteLocationPrefix(c.loc, c.prefix); got != c.want {
			t.Errorf("rewriteLocationPrefix(%q,%q)=%q want %q", c.loc, c.prefix, got, c.want)
		}
	}
}

// TestProxyForwardedHostOnPathRoute verifies that a path-routed request
// (X-Burrow-Path-Prefix set by the /svc/{slug} adapter) reports the public
// auth domain upstream in X-Forwarded-Host, not the internal routing label.
func TestProxyForwardedHostOnPathRoute(t *testing.T) {
	var gotXFH string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFH = r.Header.Get("X-Forwarded-Host")
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("hdr", &proxy.Resolved{ServiceID: "svc2", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	p := proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithIngressPort("443"))

	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "hdr." + authDomain
	req.Header.Set("X-Burrow-Path-Prefix", "/svc/hdr")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if gotXFH != authDomain {
		t.Errorf("X-Forwarded-Host on a path-routed request: want %q, got %q", authDomain, gotXFH)
	}
}

// Tunnelled apps share the dashboard's origin, so the browser attaches the
// dashboard session to every /svc/ request. It must never reach the upstream.
func TestProxyStripsBurrowCookies(t *testing.T) {
	var gotCookie string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("ck", &proxy.Resolved{ServiceID: "svc-ck", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	p := proxy.New(d, openChecker{}, authDomain, testLog())
	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "ck." + authDomain
	req.Header.Set("Cookie", "burrow_session=secret; app=1; burrow_csrf=tok; theme=dark")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if gotCookie != "app=1; theme=dark" {
		t.Fatalf("upstream Cookie = %q, want %q", gotCookie, "app=1; theme=dark")
	}
}

func TestProxyStripsBurrowCookies_OnlyBurrowCookies(t *testing.T) {
	var gotHeader http.Header
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("ck2", &proxy.Resolved{ServiceID: "svc-ck2", AccessMode: "open", LocalHost: "127.0.0.1:3000"})

	p := proxy.New(d, openChecker{}, authDomain, testLog())
	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "ck2." + authDomain
	req.Header.Set("Cookie", "burrow_session=secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if gotHeader == nil {
		t.Fatal("upstream was not reached")
	}
	if _, ok := gotHeader["Cookie"]; ok {
		t.Fatalf("upstream got a Cookie header: %q", gotHeader["Cookie"])
	}
}
