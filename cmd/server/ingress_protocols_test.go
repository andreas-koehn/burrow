package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/acme"
	"github.com/ankoehn/burrow/internal/proxy"
)

func protoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	})
}

// startIngress serves h on a loopback listener the way main.go does.
func startIngress(t *testing.T, h http.Handler, tlsCfg *tls.Config, h2c bool) (addr string) {
	t.Helper()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, TLSConfig: tlsCfg}
	applyIngressProtocols(srv, tlsCfg != nil, h2c)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg != nil {
		go func() { _ = srv.ServeTLS(ln, "", "") }()
	} else {
		go func() { _ = srv.Serve(ln) }()
	}
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func getProto(t *testing.T, c *http.Client, url string) (string, error) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.ProtoMajor == 2 && string(b) != "HTTP/2.0" {
		t.Fatalf("client sees %s but handler saw %s", resp.Proto, b)
	}
	return resp.Proto, nil
}

func protoClient(setup func(*http.Protocols), tlsCfg *tls.Config) *http.Client {
	var p http.Protocols
	setup(&p)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Protocols: &p, TLSClientConfig: tlsCfg}}
}

func TestIngressPlainH2C(t *testing.T) {
	addr := startIngress(t, protoHandler(), nil, true)
	h2c := protoClient(func(p *http.Protocols) { p.SetUnencryptedHTTP2(true) }, nil)
	if got, err := getProto(t, h2c, "http://"+addr+"/"); err != nil || got != "HTTP/2.0" {
		t.Fatalf("h2c client: proto=%q err=%v, want HTTP/2.0", got, err)
	}
	h1 := protoClient(func(p *http.Protocols) { p.SetHTTP1(true) }, nil)
	if got, err := getProto(t, h1, "http://"+addr+"/"); err != nil || got != "HTTP/1.1" {
		t.Fatalf("h1 client: proto=%q err=%v, want HTTP/1.1", got, err)
	}
}

func TestIngressPlainH2COff(t *testing.T) {
	addr := startIngress(t, protoHandler(), nil, false)
	h2c := protoClient(func(p *http.Protocols) { p.SetUnencryptedHTTP2(true) }, nil)
	if got, err := getProto(t, h2c, "http://"+addr+"/"); err == nil {
		t.Fatalf("prior-knowledge h2c must be refused with h2c off, got %q", got)
	}
	h1 := protoClient(func(p *http.Protocols) { p.SetHTTP1(true) }, nil)
	if got, err := getProto(t, h1, "http://"+addr+"/"); err != nil || got != "HTTP/1.1" {
		t.Fatalf("h1 client: proto=%q err=%v, want HTTP/1.1", got, err)
	}
}

func selfSignedCfg(t *testing.T) *tls.Config {
	t.Helper()
	certPath, keyPath := generateWildcardCert(t, t.TempDir(), "*.example.test", "example.test")
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

func TestIngressTLSNegotiatesH2AndKeepsHTTP1(t *testing.T) {
	cfg := selfSignedCfg(t)
	ensureIngressNextProtos(cfg)
	addr := startIngress(t, protoHandler(), cfg, true)
	skip := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test cert
	h2 := protoClient(func(p *http.Protocols) { p.SetHTTP1(true); p.SetHTTP2(true) }, skip)
	if got, err := getProto(t, h2, "https://"+addr+"/"); err != nil || got != "HTTP/2.0" {
		t.Fatalf("tls client: proto=%q err=%v, want HTTP/2.0", got, err)
	}
	h1 := protoClient(func(p *http.Protocols) { p.SetHTTP1(true) },
		&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) //nolint:gosec // test cert
	if got, err := getProto(t, h1, "https://"+addr+"/"); err != nil || got != "HTTP/1.1" {
		t.Fatalf("tls http1 client: proto=%q err=%v, want HTTP/1.1", got, err)
	}
}

func TestEnsureIngressNextProtosKeepsACME(t *testing.T) {
	cfg := &tls.Config{NextProtos: []string{acme.ACMETLSProto}}
	ensureIngressNextProtos(cfg)
	want := []string{"h2", "http/1.1", acme.ACMETLSProto}
	if !slices.Equal(cfg.NextProtos, want) {
		t.Fatalf("NextProtos = %v, want %v", cfg.NextProtos, want)
	}
	ensureIngressNextProtos(cfg) // idempotent
	if !slices.Equal(cfg.NextProtos, want) {
		t.Fatalf("not idempotent: %v", cfg.NextProtos)
	}
	empty := &tls.Config{}
	ensureIngressNextProtos(empty)
	if !slices.Equal(empty.NextProtos, []string{"h2", "http/1.1"}) {
		t.Fatalf("empty NextProtos = %v", empty.NextProtos)
	}
}

func TestIngressProtocolSettings(t *testing.T) {
	srv := &http.Server{}
	applyIngressProtocols(srv, false, true)
	if !srv.Protocols.HTTP1() || srv.Protocols.HTTP2() || !srv.Protocols.UnencryptedHTTP2() {
		t.Fatalf("plain h2c: %v", srv.Protocols)
	}
	applyIngressProtocols(srv, false, false)
	if !srv.Protocols.HTTP1() || srv.Protocols.UnencryptedHTTP2() {
		t.Fatalf("plain no h2c: %v", srv.Protocols)
	}
	applyIngressProtocols(srv, true, false)
	if !srv.Protocols.HTTP1() || !srv.Protocols.HTTP2() || srv.Protocols.UnencryptedHTTP2() {
		t.Fatalf("tls: %v", srv.Protocols)
	}
	if srv.IdleTimeout != 120*time.Second || srv.HTTP2.MaxConcurrentStreams != 250 ||
		srv.HTTP2.SendPingTimeout != 60*time.Second || srv.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("timeouts: idle=%v h2=%+v", srv.IdleTimeout, srv.HTTP2)
	}
}

// Upgrade (websocket) needs HTTP/1.1: it must keep working on a plain h2c
// listener and on a TLS listener that also offers h2.
func TestIngressUpgradeStaysHTTP1(t *testing.T) {
	upgrade := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nhello")
		_ = rw.Flush()
	})
	cfg := selfSignedCfg(t)
	ensureIngressNextProtos(cfg)
	for name, tc := range map[string]struct {
		addr string
		tls  bool
	}{
		"plain": {startIngress(t, upgrade, nil, true), false},
		"tls":   {startIngress(t, upgrade, cfg, true), true},
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("tcp", tc.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			c := net.Conn(conn)
			if tc.tls {
				tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) //nolint:gosec // test cert
				if err := tc.Handshake(); err != nil {
					t.Fatal(err)
				}
				c = tc
			}
			_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, nil)
			if err != nil || resp.StatusCode != 101 {
				t.Fatalf("resp=%v err=%v, want 101", resp, err)
			}
		})
	}
}

// The real proxy behind a plain listener: prior-knowledge h2c reaches the
// tunnelled service, and the Upgrade: h2c flavour is still refused with 505.
func TestIngressProxyH2CAndUpgradeRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "up-ok") })
	checker := proxy.NewAccessCheckerWithSessionsAndLogger(s.store, s.store, e2eAuthDomain, s.log)
	p := proxy.New(proxyDialerAdapter{st: s.store, srv: s.server}, checker, e2eAuthDomain, s.log, proxy.WithGate(s.gate))
	addr := startIngress(t, p, nil, true)

	h2c := protoClient(func(p *http.Protocols) { p.SetUnencryptedHTTP2(true) }, nil)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/", nil)
	req.Host = s.hostname
	resp, err := h2c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != 200 || string(body) != "up-ok" {
		t.Fatalf("h2c via proxy: %s %d %q", resp.Proto, resp.StatusCode, body)
	}

	h1 := protoClient(func(p *http.Protocols) { p.SetHTTP1(true) }, nil)
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/", nil)
	req2.Host = s.hostname
	req2.Header.Set("Upgrade", "h2c")
	req2.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	req2.Header.Set("HTTP2-Settings", "AAMAAABkAARAAAAAAAIAAAAA")
	resp2, err := h1.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusHTTPVersionNotSupported {
		t.Fatalf("Upgrade: h2c = %d, want 505", resp2.StatusCode)
	}
}

// The per-vhost mTLS clone of the base config negotiates h2 too.
func TestIngressMTLSVhostNegotiatesH2(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "mtls-ok") })
	ca := newMTLSCA(t, "h2-mtls-ca")
	good := ca.issueClient(t, "alice@example.com")
	must(t, s.store.SetServiceAccessMode(context.Background(), s.userID, "admin", s.serviceID, "mtls", "", ca.pemBytes), "set mtls")

	base := selfSignedCfg(t)
	ensureIngressNextProtos(base)
	checker := proxy.NewAccessCheckerWithSessionsAndLogger(s.store, s.store, e2eAuthDomain, s.log)
	p := proxy.New(proxyDialerAdapter{st: s.store, srv: s.server}, checker, e2eAuthDomain, s.log,
		proxy.WithGate(s.gate), proxy.WithTLSBase(base))
	base.GetConfigForClient = p.GetConfigForClient

	vh, err := p.GetConfigForClient(&tls.ClientHelloInfo{ServerName: s.hostname})
	if err != nil || vh == nil {
		t.Fatalf("vhost config: %v %v", vh, err)
	}
	if !slices.Equal(vh.NextProtos, []string{"h2", "http/1.1"}) {
		t.Fatalf("mtls clone NextProtos = %v", vh.NextProtos)
	}

	addr := startIngress(t, p, base, true)
	c := protoClient(func(p *http.Protocols) { p.SetHTTP1(true); p.SetHTTP2(true) },
		&tls.Config{InsecureSkipVerify: true, ServerName: s.hostname, Certificates: []tls.Certificate{good}}) //nolint:gosec // test cert
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+addr+"/", nil)
	req.Host = s.hostname
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != 200 || string(b) != "mtls-ok" {
		t.Fatalf("mtls h2: %s %d %q", resp.Proto, resp.StatusCode, b)
	}
}
