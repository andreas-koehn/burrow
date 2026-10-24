package server

// TLS passthrough on the HTTPS port (H11): SplitPassthrough peeks at each
// ClientHello on the listener of the relay's own HTTPS server. A name of a
// tcp service goes there raw; everything else reaches the TLS server, which
// still gets *tls.Conn connections with the visitor's address.

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxyproto"
	"github.com/ankoehn/burrow/internal/testutil"
)

const muxAuthDomain = "relay.example.com"

// muxRelay is an accessRelay whose HTTPS server (a stand-in for the
// dashboard: it answers "relay <proto> <remote addr>") is served over
// SplitPassthrough at addr.
type muxRelay struct {
	*accessRelay
	addr string
	// tlsConns counts the connections the HTTPS server was handed (each a
	// *tls.Conn).
	tlsConns atomic.Int32
}

// newMuxRelay starts the relay with BURROW_TCP_DOMAIN and an auth domain;
// tweak changes the server before the mux runs.
func newMuxRelay(t *testing.T, min, max int, mut func(*Options), tweak func(*Server)) *muxRelay {
	t.Helper()
	a := newAccessRelay(t, min, max, func(o *Options) {
		o.TCPDomain = testTCPDomain
		o.AuthDomain = muxAuthDomain
		if mut != nil {
			mut(o)
		}
	})
	if tweak != nil {
		tweak(a.srv)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &muxRelay{accessRelay: a, addr: ln.Addr().String()}
	hs := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := ""
			if r.TLS != nil {
				name = r.TLS.ServerName
			}
			_, _ = fmt.Fprintf(w, "relay %s %s %s", r.Proto, r.RemoteAddr, name)
		}),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{namedCert(t, "relay")},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"h2", "http/1.1", acmeTLSALPN},
		},
		ConnState: func(c net.Conn, st http.ConnState) {
			if _, ok := c.(*tls.Conn); ok && st == http.StateNew {
				m.tlsConns.Add(1)
			}
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	hs.Protocols = new(http.Protocols)
	hs.Protocols.SetHTTP1(true)
	hs.Protocols.SetHTTP2(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = hs.ServeTLS(a.srv.SplitPassthrough(ctx, ln), "", ""); close(done) }()
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = hs.Shutdown(sctx)
		cancel()
		<-done
	})
	return m
}

// namedCert is a self-signed certificate with common name cn for any name
// under example.com.
func namedCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"*.example.com", "*.tcp.example.com", muxAuthDomain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tlsHome is a home client whose target terminates TLS with a certificate
// named "echo" (as passRelay's).
func (m *muxRelay) tlsHome(t *testing.T, name string, told *atomic.Int32) *handClient {
	return (&passRelay{accessRelay: m.accessRelay, addr: m.addr}).tlsHome(t, name, told)
}

// muxHandshake dials addr (writing pre first, a PROXY header) and runs a TLS
// handshake with server name sni offering alpn; it returns the connection
// and the common name of the certificate it saw.
func muxHandshake(addr, sni string, alpn []string, pre []byte) (*tls.Conn, string, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, "", err
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if len(pre) > 0 {
		if _, err := conn.Write(pre); err != nil {
			_ = conn.Close()
			return nil, "", err
		}
	}
	tc := tls.Client(conn, &tls.Config{ServerName: sni, NextProtos: alpn, InsecureSkipVerify: true}) //nolint:gosec // the test certificates are self-signed
	if err := tc.Handshake(); err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	return tc, tc.ConnectionState().PeerCertificates[0].Subject.CommonName, nil
}

// relayGet is an HTTPS GET of / with server name sni through the mux,
// speaking h2 when the relay offers it; pre is written before the hello.
func relayGet(t *testing.T, addr, sni string, pre []byte) (*http.Response, string) {
	t.Helper()
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: sni, InsecureSkipVerify: true}, //nolint:gosec // self-signed
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err == nil && len(pre) > 0 {
				_, err = c.Write(pre)
			}
			return c, err
		},
		ForceAttemptHTTP2: true,
	}
	defer tr.CloseIdleConnections()
	host := sni
	if host == "" {
		host = "127.0.0.1"
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://" + host + "/")
	if err != nil {
		t.Fatalf("GET %s: %v", sni, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// A tcp service's name goes raw to the service (the visitor sees the
// service's certificate); the relay's own names, unknown names, no name, a
// hello that offers acme-tls/1 and a private service's names are
// terminated by the relay (its certificate, h2 by ALPN). The home client is
// told of the raw ones only.
func TestMux_RoutesByServerName(t *testing.T) {
	r := newMuxRelay(t, 19700, 19700, nil, nil)
	r.res.mu.Lock()
	r.res.hosts["svc-web"] = "rdp.example.com"
	r.res.hosts["svc-vault"] = "vault.example.com"
	r.res.mu.Unlock()
	var toldWeb, toldVault atomic.Int32
	web, vault := r.tlsHome(t, "web", &toldWeb), r.tlsHome(t, "vault", &toldVault)
	if rr := web.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register web: %+v", rr)
	}
	if rr := vault.register(t, proto.TunnelRegister{Name: "vault", Type: "tcp", LocalAddr: "x", Access: "private"}); !rr.OK {
		t.Fatalf("register vault: %+v", rr)
	}

	// Raw to the service, by either name, through a real TLS client.
	for _, sni := range []string{"rdp.example.com", "s-web.tcp.example.com"} {
		if got, err := tlsVisit(r.addr, sni); err != nil || got != "web" {
			t.Fatalf("%s: %q %v", sni, got, err)
		}
		tc, cn, err := muxHandshake(r.addr, sni, []string{"h2", "http/1.1"}, nil)
		if err != nil || cn != "echo" {
			t.Fatalf("%s: certificate %q %v", sni, cn, err)
		}
		_ = tc.Close()
	}
	row := r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.SNI == "rdp.example.com" })
	if row.ServiceID != "svc-web" || row.Protocol != "tls" || row.SourceIP != "127.0.0.1" {
		t.Fatalf("row %+v", row)
	}
	raw := toldWeb.Load()

	// The relay's name: its certificate, over h2.
	resp, body := relayGet(t, r.addr, muxAuthDomain, nil)
	if resp.ProtoMajor != 2 || resp.TLS.PeerCertificates[0].Subject.CommonName != "relay" || !strings.HasPrefix(body, "relay HTTP/2.0 127.0.0.1:") {
		t.Fatalf("relay name: %s %q", resp.Proto, body)
	}
	// Terminated by the relay: names it owns, names nobody has, no name, a
	// private service's names, and acme-tls/1 with a tcp service's name.
	for _, c := range []struct {
		sni  string
		alpn []string
	}{
		{"dash.relay.example.com", []string{"h2"}},
		{"nobody.example.com", []string{"h2", "http/1.1"}},
		{"", []string{"http/1.1"}},
		{"vault.example.com", nil},
		{"s-vault.tcp.example.com", nil},
		{"rdp.example.com", []string{acmeTLSALPN}},
		{"s-web.tcp.example.com", []string{acmeTLSALPN}},
	} {
		tc, cn, err := muxHandshake(r.addr, c.sni, c.alpn, nil)
		if err != nil || cn != "relay" {
			t.Fatalf("%q %v: certificate %q %v", c.sni, c.alpn, cn, err)
		}
		if len(c.alpn) == 1 && tc.ConnectionState().NegotiatedProtocol != c.alpn[0] {
			t.Fatalf("%q: negotiated %q", c.sni, tc.ConnectionState().NegotiatedProtocol)
		}
		_ = tc.Close()
	}
	if _, body := relayGet(t, r.addr, "nobody.example.com", nil); !strings.HasPrefix(body, "relay HTTP/2.0") {
		t.Fatalf("unknown name: %q", body)
	}
	settle()
	if toldWeb.Load() != raw || toldVault.Load() != 0 {
		t.Fatalf("the home clients were told of terminated connections: web %d (raw %d), vault %d", toldWeb.Load(), raw, toldVault.Load())
	}
	if n := r.srv.passRefusals.count(muxRefusalKey, PassthroughRefusedUnknownName); n != 0 {
		t.Fatalf("%d unknown-name refusals: the mux terminates those", n)
	}
}

// Bytes that are no TLS, bytes that only start like a PROXY header, a
// record too long for a hello and a ClientHello split over two records
// reach the TLS server with the bytes put back: it answers plain HTTP with
// its 400, and completes the split handshake. The mux closes none of them.
func TestMux_WhatCannotBeReadFallsThrough(t *testing.T) {
	r := newMuxRelay(t, 19701, 19701, nil, nil)
	if rr := r.c.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	for name, first := range map[string][]byte{
		"plain http":        []byte("GET / HTTP/1.1\r\nHost: s-web.tcp.example.com\r\n\r\n"),
		"a carriage return": []byte("\r\nGET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		"a too long record": {0x16, 0x03, 0x01, 0xff, 0xff, 0x01, 0, 0, 0},
	} {
		before := r.tlsConns.Load()
		conn, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = conn.Write(first)
		got, _ := io.ReadAll(conn)
		_ = conn.Close()
		r.waitFor(name+" reaches the TLS server", func() bool { return r.tlsConns.Load() > before })
		if name == "plain http" && !strings.HasPrefix(string(got), "HTTP/1.0 400") {
			t.Errorf("plain http: %q", got)
		}
	}
	for _, reason := range []string{PassthroughRefusedNotTLS, PassthroughRefusedBadHello, PassthroughRefusedTimeout} {
		if n := r.srv.passRefusals.count(muxRefusalKey, reason); n != 0 {
			t.Errorf("%d %s refusals", n, reason)
		}
	}
	// A hello split over two records: tcpsniff does not read it, the TLS
	// server does.
	conn, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	tc := tls.Client(&splitHelloConn{Conn: conn}, &tls.Config{ServerName: "s-web.tcp.example.com", InsecureSkipVerify: true}) //nolint:gosec // self-signed
	if err := tc.Handshake(); err != nil {
		t.Fatalf("split hello: %v", err)
	}
	if cn := tc.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "relay" {
		t.Fatalf("split hello reached %q", cn)
	}
	settle()
	if r.told.Load() != 0 {
		t.Fatalf("the home client was told of %d connections", r.told.Load())
	}
}

// splitHelloConn writes the first TLS record it is given as two records.
type splitHelloConn struct {
	net.Conn
	done bool
}

func (c *splitHelloConn) Write(p []byte) (int, error) {
	if c.done || len(p) < 5 || p[0] != 0x16 {
		return c.Conn.Write(p)
	}
	c.done = true
	n := int(binary.BigEndian.Uint16(p[3:5]))
	body, rest := p[5:5+n], p[5+n:]
	rec := func(b []byte) []byte {
		return append(append([]byte{0x16, p[1], p[2]}, byte(len(b)>>8), byte(len(b))), b...)
	}
	out := append(append(rec(body[:n/2]), rec(body[n/2:])...), rest...)
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// The bytes read for the name reach the service unchanged, and so does
// everything after them; a terminated connection keeps the visitor's
// address as its RemoteAddr.
func TestMux_ReplaysAndKeepsTheAddress(t *testing.T) {
	r := newMuxRelay(t, 19702, 19702, nil, nil)
	if rr := r.c.register(t, proto.TunnelRegister{Name: "raw", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	msg := append(clientHello(t, "s-raw.tcp.example.com"), "and more"...)
	if got, err := rawVisit(r.addr, msg, 5*time.Second); err != nil || string(got) != string(msg) {
		t.Fatalf("echo %d of %d bytes: %v", len(got), len(msg), err)
	}
	// The RemoteAddr of a terminated connection is the client's own.
	conn, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	tc := tls.Client(conn, &tls.Config{ServerName: muxAuthDomain, NextProtos: []string{"http/1.1"}, InsecureSkipVerify: true}) //nolint:gosec // self-signed
	if _, err := tc.Write([]byte("GET / HTTP/1.1\r\nHost: " + muxAuthDomain + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if want := "relay HTTP/1.1 " + conn.LocalAddr().String() + " " + muxAuthDomain; string(body) != want {
		t.Fatalf("body %q, want %q", body, want)
	}
	if r.tlsConns.Load() == 0 {
		t.Fatal("the HTTPS server saw no *tls.Conn")
	}
}

// A PROXY v2 header is read as on the dedicated listener: from the
// trusted proxies it is required and names the visitor of passed-through
// and terminated connections alike; from anyone else it is closed.
func TestMux_ProxyHeaderFromTrustedProxiesOnly(t *testing.T) {
	r := newMuxRelay(t, 19703, 19703, func(o *Options) {
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	}, func(s *Server) { s.helloTimeout = time.Second })
	r.access.set("svc-guard", TCPAccess{Mode: TCPAccessIP, AllowCIDRs: []string{"198.51.100.0/24"}})
	if rr := r.c.register(t, proto.TunnelRegister{Name: "guard", Type: "tcp", LocalAddr: "x", Access: "ip"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	header := proxyV2("198.51.100.7:4242", "10.0.0.2:443")
	hello := clientHello(t, "s-guard.tcp.example.com")
	if got, err := rawVisitN(r.addr, append(append([]byte{}, header...), hello...), len(hello), 5*time.Second); err != nil || string(got) != string(hello) {
		t.Fatalf("passed through: %v", err)
	}
	if nc := r.c.notice(t); nc.SourceIP != "198.51.100.7:4242" || nc.DestAddr != "10.0.0.2:443" {
		t.Fatalf("new_connection %+v", nc)
	}
	row := r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.Status == observe.OutcomeClean })
	if row.SourceIP != "198.51.100.7" || row.SNI != "s-guard.tcp.example.com" {
		t.Fatalf("row %+v", row)
	}
	// Terminated: the HTTPS server sees the visitor.
	if _, body := relayGet(t, r.addr, muxAuthDomain, header); !strings.HasPrefix(body, "relay HTTP/2.0 198.51.100.7:4242 ") {
		t.Fatalf("terminated behind the proxy: %q", body)
	}
	// Without a header, or with a LOCAL one, the trusted proxy is closed.
	local := append(append([]byte{}, proxyproto.Signature[:]...), 0x20, 0x00, 0, 0)
	for name, first := range map[string][]byte{"no header": clientHello(t, muxAuthDomain), "a LOCAL header": append(local, hello...)} {
		conn, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write(first)
		if err := closedWithoutAByte(conn, 3*time.Second); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = conn.Close()
	}
	if n := r.srv.passRefusals.count(muxRefusalKey, PassthroughRefusedProxyMissing); n != 2 {
		t.Fatalf("%d proxy_missing refusals", n)
	}

	// A relay that trusts no proxy: a header is closed and counted, and the
	// same peer without one is served (raw and terminated).
	u := newMuxRelay(t, 19704, 19704, nil, nil)
	if rr := u.c.register(t, proto.TunnelRegister{Name: "open", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	conn, err := net.Dial("tcp", u.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write(append(proxyV2("198.51.100.7:4242", "10.0.0.2:443"), clientHello(t, muxAuthDomain)...))
	if err := closedWithoutAByte(conn, 3*time.Second); err != nil {
		t.Fatalf("untrusted header: %v", err)
	}
	if n := u.srv.passRefusals.count(muxRefusalKey, PassthroughRefusedProxyUntrusted); n != 1 {
		t.Fatalf("%d untrusted refusals", n)
	}
	if _, err := rawVisit(u.addr, clientHello(t, "s-open.tcp.example.com"), 5*time.Second); err != nil {
		t.Fatalf("no header, raw: %v", err)
	}
	if _, body := relayGet(t, u.addr, muxAuthDomain, nil); !strings.HasPrefix(body, "relay HTTP/2.0 127.0.0.1:") {
		t.Fatalf("no header, terminated: %q", body)
	}
}

// A hello that does not come in time is closed when the read times out,
// as is half a hello; a full budget closes at once (busy).
func TestMux_TheHelloReadIsBounded(t *testing.T) {
	const timeout = time.Second
	r := newMuxRelay(t, 19705, 19705, nil, func(s *Server) {
		s.helloTimeout = timeout
		s.muxPeek.max, s.muxPeek.perPeer = 3, 2
	})
	timed := func(name string, first []byte, atLeast, atMost time.Duration) {
		t.Helper()
		conn, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		start := time.Now()
		if len(first) > 0 {
			_, _ = conn.Write(first)
		}
		if err := closedWithoutAByte(conn, atMost+time.Second); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if took := time.Since(start); took < atLeast || took > atMost {
			t.Fatalf("%s: closed after %v, want %v..%v", name, took, atLeast, atMost)
		}
	}
	hello := clientHello(t, muxAuthDomain)
	timed("nothing", nil, timeout-100*time.Millisecond, timeout+time.Second)
	timed("half a hello", hello[:len(hello)/2], timeout-100*time.Millisecond, timeout+time.Second)
	timed("a record header", hello[:3], timeout-100*time.Millisecond, timeout+time.Second)
	if n := r.srv.passRefusals.count(muxRefusalKey, PassthroughRefusedTimeout); n != 3 {
		t.Fatalf("%d timeouts counted", n)
	}
	held := holdSilent(t, r.addr, 2)
	timed("a third from one peer", nil, 0, timeout/2)
	if n := r.srv.passRefusals.count(muxRefusalKey, PassthroughRefusedBusy); n != 1 {
		t.Fatalf("%d busy", n)
	}
	for _, c := range held {
		_ = c.Close()
	}
	r.waitFor("the waits end", func() bool { return r.srv.muxPeek.waiting() == 0 })
	if _, body := relayGet(t, r.addr, muxAuthDomain, nil); !strings.HasPrefix(body, "relay ") {
		t.Fatalf("after the waits: %q", body)
	}
}

// The accept loop never waits for a hello: with 1000 silent connections
// waiting, a good one is served at once.
func TestMux_AcceptNeverBlocks(t *testing.T) {
	r := newMuxRelay(t, 19706, 19706, nil, func(s *Server) { s.muxPeek.perPeer = s.muxPeek.max })
	holdSilent(t, r.addr, 1000)
	r.waitFor("the silent ones wait", func() bool { return r.srv.muxPeek.waiting() == 1000 })
	start := time.Now()
	if _, body := relayGet(t, r.addr, muxAuthDomain, nil); !strings.HasPrefix(body, "relay ") {
		t.Fatalf("body %q", body)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("served after %v", took)
	}
}

// A name a tcp service has but the http side owns (a relay name, a name
// under the auth domain, a custom domain) is terminated, with one warning
// per name; a failing custom-domain lookup terminates too.
func TestMux_TheHTTPSideWinsAName(t *testing.T) {
	lookupFails := atomic.Bool{}
	r := newMuxRelay(t, 19710, 19713, func(o *Options) {
		o.ServicesDomain = "apps.example.com"
		o.CustomDomainCovers = func(_ context.Context, name string) (bool, error) {
			if lookupFails.Load() {
				return false, errors.New("db down")
			}
			return name == "shop.example.com", nil
		}
	}, nil)
	for _, name := range []string{"shop.example.com", "apps.example.com", "app.relay.example.com", "free.example.com"} {
		r.res.mu.Lock()
		r.res.hosts["svc-"+strings.Split(name, ".")[0]] = name
		r.res.mu.Unlock()
		if rr := r.c.register(t, proto.TunnelRegister{Name: strings.Split(name, ".")[0], Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
			t.Fatalf("register %s: %+v", name, rr)
		}
	}
	for _, name := range []string{"shop.example.com", "apps.example.com", "app.relay.example.com"} {
		for range 2 {
			tc, cn, err := muxHandshake(r.addr, name, nil, nil)
			if err != nil || cn != "relay" {
				t.Fatalf("%s: %q %v", name, cn, err)
			}
			_ = tc.Close()
		}
		if _, ok := r.srv.shadowWarned.Load(name); !ok {
			t.Errorf("no warning for %s", name)
		}
	}
	// A name of its own reaches the service; with the lookup failing it is
	// terminated.
	if got, err := rawVisit(r.addr, clientHello(t, "free.example.com"), 5*time.Second); err != nil || len(got) == 0 {
		t.Fatalf("free: %v", err)
	}
	lookupFails.Store(true)
	tc, cn, err := muxHandshake(r.addr, "free.example.com", nil, nil)
	if err != nil || cn != "relay" {
		t.Fatalf("lookup failing: %q %v", cn, err)
	}
	_ = tc.Close()
}

// Closing the mux (http.Server.Shutdown, or the end of its context) closes
// every connection that waits for its hello or for Accept, and leaves no
// goroutine behind; Accept then reports net.ErrClosed. The mux marks the
// relay as reaching tcp services by name.
func TestMux_CloseStopsPendingPeeks(t *testing.T) {
	// A server that is not served: the mux needs none of the control port.
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{TLSCert: filepath.Join(dir, "dev-server.pem"), TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: fakeAuth{uid: "u1"}})
	if err != nil {
		t.Fatal(err)
	}
	if srv.passthroughOn.Load() {
		t.Fatal("passthrough on before the mux")
	}
	for _, viaCtx := range []bool{false, true} {
		leak := testutil.AssertNoGoroutineLeak(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		mux := srv.SplitPassthrough(ctx, ln)
		if !srv.passthroughOn.Load() {
			t.Fatal("the mux does not mark passthrough on")
		}
		var held []net.Conn
		for i := range 6 {
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				_, _ = c.Write(clientHello(t, "x.example.com")[:20])
			}
			held = append(held, c)
		}
		// One more is waiting for Accept, which nobody calls.
		if _, err := rawVisitN(ln.Addr().String(), []byte("GET / HTTP/1.0\r\n\r\n"), 0, time.Second); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for srv.muxPeek.waiting() != len(held) {
			if time.Now().After(deadline) {
				t.Fatalf("%d waiting", srv.muxPeek.waiting())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if viaCtx {
			cancel()
		} else {
			_ = mux.Close()
		}
		for i, c := range held {
			if err := closedWithoutAByte(c, 3*time.Second); err != nil {
				t.Fatalf("held %d: %v", i, err)
			}
			_ = c.Close()
		}
		if _, err := mux.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after close: %v", err)
		}
		if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
			t.Fatal("still listening")
		}
		_ = mux.Close()
		cancel()
		if srv.muxPeek.waiting() != 0 {
			t.Fatalf("%d still waiting", srv.muxPeek.waiting())
		}
		leak()
	}
}
