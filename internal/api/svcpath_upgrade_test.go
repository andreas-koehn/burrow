package api

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proxy"
)

// upgradeTunnel resolves one slug and dials a TCP backend in place of the
// tunnel stream.
type upgradeTunnel struct {
	slug string
	addr string
}

func (u upgradeTunnel) res() *proxy.Resolved {
	return &proxy.Resolved{ServiceID: "svc-ws", AccessMode: "open", LocalHost: "127.0.0.1:3000"}
}

func (u upgradeTunnel) Lookup(_ context.Context, slug string) (*proxy.Resolved, error) {
	if slug != u.slug {
		return nil, proxy.ErrNotFound
	}
	return u.res(), nil
}

func (u upgradeTunnel) LookupByServiceID(context.Context, string) (*proxy.Resolved, error) {
	return u.res(), nil
}

func (u upgradeTunnel) DialTunnelStream(ctx context.Context, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", u.addr)
}

func (u upgradeTunnel) DialTunnelStreamByServiceID(ctx context.Context, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", u.addr)
}

type allowAll struct{}

func (allowAll) Allow(context.Context, *proxy.Resolved, *http.Request) (bool, int, string, http.Header) {
	return true, 0, "", nil
}

// syncBuffer is a log sink the test can read while the server still writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A WebSocket upgrade under /svc/<slug>/ must pass through every
// ResponseWriter wrapper between the router and the upstream: the proxy needs
// to hijack the connection to switch protocols.
func TestRouter_SvcPathUpgradeSwitchesProtocols(t *testing.T) {
	// Backend: answers 101 and echoes every byte that follows.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	gotReq := make(chan *http.Request, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		gotReq <- req
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = io.Copy(c, br)
	}()

	const authDomain = "burrow.example.com"
	logs := &syncBuffer{}
	p := proxy.New(upgradeTunnel{slug: "wsapp", addr: ln.Addr().String()}, allowAll{}, authDomain, discardLog())
	h := NewRouter(Deps{TunnelProxy: p, AuthDomain: authDomain, Log: slog.New(slog.NewTextHandler(logs, nil))})
	ts := httptest.NewServer(h)
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(conn, "GET /svc/wsapp/ws HTTP/1.1\r\n"+
		"Host: "+authDomain+"\r\n"+
		"Connection: Upgrade\r\n"+
		"Upgrade: websocket\r\n"+
		"Cookie: burrow_session=secret; app=1; burrow_csrf=tok\r\n"+
		"\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("status = %d, want 101; body %q", resp.StatusCode, body)
	}

	select {
	case req := <-gotReq:
		if req.URL.Path != "/ws" {
			t.Errorf("upstream path = %q, want /ws", req.URL.Path)
		}
		if got := req.Header.Get("Cookie"); got != "app=1" {
			t.Errorf("upstream Cookie = %q, want %q", got, "app=1")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend never saw the upgrade request")
	}

	// Data flows both ways over the switched connection.
	for _, msg := range []string{"ping-one", "ping-two"} {
		if _, err := io.WriteString(conn, msg); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(br, buf); err != nil {
			t.Fatalf("read echo of %q: %v", msg, err)
		}
		if string(buf) != msg {
			t.Fatalf("echo = %q, want %q", buf, msg)
		}
	}

	// The request log line is written when the connection ends.
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "path=/svc/wsapp/ws") {
		if time.Now().After(deadline) {
			t.Fatalf("no request log line for the upgrade; log: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := logs.String(); !strings.Contains(out, "status=101") {
		t.Fatalf("logged status is not 101: %s", out)
	}
}

// Without a hijackable writer underneath, Hijack reports an error instead of
// panicking, and Unwrap exposes the wrapped writer to http.ResponseController.
func TestStatusWriter_HijackAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	if _, _, err := sw.Hijack(); err == nil {
		t.Fatal("Hijack on a non-hijackable writer returned no error")
	}
	if sw.status != http.StatusOK {
		t.Fatalf("status after a failed Hijack = %d, want 200", sw.status)
	}
	if sw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("Unwrap did not return the wrapped writer")
	}
}
