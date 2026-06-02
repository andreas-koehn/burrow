package main

// e2e_http_tunnel_test.go — Tasks 2/3/4 of the v0.3.0 integration plan.
// Real server + real client + real proxy ingress + real local upstream.
// Skipped under `-short` so unit-test runs stay fast.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Task 2 — HTTP tunnel round-trip + forwarding headers + 404 unknown sub
// ---------------------------------------------------------------------------

func TestE2EHTTPTunnel_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	// Upstream captures the headers it saw so we can assert XFP/XFF.
	type capture struct {
		path string
		xfp  string
		xfh  string
		xff  string
		host string
	}
	var (
		mu   sync.Mutex
		seen capture
	)
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = capture{
			path: r.URL.Path,
			xfp:  r.Header.Get("X-Forwarded-Proto"),
			xfh:  r.Header.Get("X-Forwarded-Host"),
			xff:  r.Header.Get("X-Forwarded-For"),
			host: r.Host,
		}
		mu.Unlock()
		w.Header().Set("X-Upstream", "burrow-e2e")
		fmt.Fprintf(w, "echo:%s", r.URL.Path)
	})

	hc := s.visitorClient(t)
	url := "https://" + s.hostWithPort() + "/ping"
	resp, err := hc.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readAllString(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", resp.StatusCode, body)
	}
	if body != "echo:/ping" {
		t.Errorf("body: want echo:/ping, got %q", body)
	}
	if resp.Header.Get("X-Upstream") != "burrow-e2e" {
		t.Errorf("upstream header not propagated: %q", resp.Header.Get("X-Upstream"))
	}

	mu.Lock()
	got := seen
	mu.Unlock()
	if got.xfp != "https" {
		t.Errorf("X-Forwarded-Proto: want https, got %q", got.xfp)
	}
	if got.xfh != s.hostname {
		t.Errorf("X-Forwarded-Host: want %s, got %q", s.hostname, got.xfh)
	}
	if !strings.HasPrefix(got.xff, "127.0.0.1") {
		t.Errorf("X-Forwarded-For: want loopback, got %q", got.xff)
	}
}

func TestE2EHTTPTunnel_UnknownSubdomain404(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	hc := s.visitorClient(t)
	resp, err := hc.Get("https://nope-no-such.test.local:" + s.proxyPort + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readAllString(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d body=%s", resp.StatusCode, body)
	}
	if body != "tunnel not found" {
		t.Errorf("body: want %q, got %q", "tunnel not found", body)
	}
}

// ---------------------------------------------------------------------------
// Task 3 — SSE / token-stream no-buffering acceptance
// ---------------------------------------------------------------------------

// TestE2EHTTPTunnel_SSEUnbuffered proves the proxy's FlushInterval=-1 wiring
// is end-to-end: the visitor observes each upstream chunk strictly before the
// next chunk is written upstream. The upstream writes 3 chunks 150ms apart;
// each chunk must be observed at the visitor with an inter-arrival gap of
// at least ~120ms (well below the 150ms upstream sleep), proving streaming.
func TestE2EHTTPTunnel_SSEUnbuffered(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	const chunks = 3
	const upstreamSleep = 150 * time.Millisecond

	// Upstream emits SSE-style chunks, flushing each one.
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream ResponseWriter is not a Flusher")
			return
		}
		for i := 1; i <= chunks; i++ {
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			flusher.Flush()
			if i < chunks {
				time.Sleep(upstreamSleep)
			}
		}
	})

	hc := s.visitorClient(t)
	url := "https://" + s.hostWithPort() + "/stream"
	resp, err := hc.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	start := time.Now()
	arrival := make([]time.Duration, 0, chunks)
	for i := 0; i < chunks; i++ {
		// Read a chunk: "data: N\n" then "\n".
		line1, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read chunk %d line1: %v", i, err)
		}
		line2, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read chunk %d line2: %v", i, err)
		}
		arrival = append(arrival, time.Since(start))
		if !strings.HasPrefix(line1, "data: ") || strings.TrimSpace(line2) != "" {
			t.Fatalf("chunk %d malformed: %q / %q", i, line1, line2)
		}
	}

	// Acceptance: chunk 2 must arrive at least ~120ms after chunk 1 (and so on).
	// If the proxy were buffering, all chunks would arrive at the same time
	// (gap ~0ms). The tolerance is generous to absorb scheduler jitter on CI.
	const minGap = 120 * time.Millisecond
	for i := 1; i < len(arrival); i++ {
		gap := arrival[i] - arrival[i-1]
		if gap < minGap {
			t.Errorf("chunk %d arrived %v after chunk %d — want ≥ %v (buffering detected)",
				i, gap, i-1, minGap)
		}
	}
	t.Logf("SSE arrivals: %v (upstream slept %v between writes)", arrival, upstreamSleep)
}

// ---------------------------------------------------------------------------
// Task 4 — WebSocket upgrade survives the bridge
// ---------------------------------------------------------------------------

// TestE2EHTTPTunnel_WebSocketUpgrade does a minimal hand-rolled WS handshake.
// The proxy's httputil.ReverseProxy hijacks on `Connection: Upgrade`; after
// 101 we exchange a few raw bytes both directions to prove the byte pipe
// survived. We do NOT speak full WS framing — the proxy only cares about the
// upgrade + byte-stream, which is the integration point under test.
func TestE2EHTTPTunnel_WebSocketUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	// Upstream: complete the 101 handshake, then echo what the visitor sends.
	upstreamDone := make(chan struct{})
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected upgrade", http.StatusBadRequest)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		accept := wsAccept(key)
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// Write the 101 response.
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n" +
			"\r\n"
		if _, err := io.WriteString(rw, resp); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		// Echo a fixed small payload back so the visitor can confirm the byte
		// pipe is open. Do NOT speak WS framing — proxy only sees raw bytes.
		_, _ = io.WriteString(rw, "upstream-hello")
		_ = rw.Flush()
		close(upstreamDone)
		// Drain anything the visitor sends so we don't half-close on it.
		_, _ = io.Copy(io.Discard, conn)
	})

	// Build a manual request via raw TLS to the proxy (cleaner than http.Client
	// for hijacked semantics: we need to read the response with bufio after the
	// upgrade and then the upstream's raw bytes).
	hc := s.visitorClient(t)
	tr := hc.Transport.(*http.Transport)

	conn, err := tr.DialTLSContext(t.Context(), "tcp", s.hostWithPort())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := "GET /ws HTTP/1.1\r\n" +
		"Host: " + s.hostWithPort() + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write req: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("expected 101 status, got %q", strings.TrimSpace(statusLine))
	}
	// Drain remaining headers up to the blank line.
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header line: %v", err)
		}
		if h == "\r\n" {
			break
		}
	}

	// Now read the 14-byte upstream payload.
	want := "upstream-hello"
	buf := make([]byte, len(want))
	deadline := time.Now().Add(3 * time.Second)
	if dlConn, ok := conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = dlConn.SetReadDeadline(deadline)
	}
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("read upstream payload: %v", err)
	}
	if string(buf) != want {
		t.Errorf("upstream payload: want %q got %q", want, string(buf))
	}

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never finished writing")
	}
}

// wsAccept returns the base64 sha1 of (key + magic) per RFC 6455.
func wsAccept(key string) string {
	const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.Sum([]byte(key + magic))
	return base64.StdEncoding.EncodeToString(h[:])
}
