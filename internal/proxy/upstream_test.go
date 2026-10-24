package proxy_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxy"
)

// testCA is a CA and a leaf it signed, for a local service with TLS.
type testCA struct {
	caPEM []byte
	leaf  tls.Certificate
}

// newTestCA makes a CA and a leaf for the names dns and the IP 127.0.0.1.
func newTestCA(t *testing.T, dns ...string) testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "local service"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: dns, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{
		caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		leaf:  tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key},
	}
}

// localTLS is the TLS config of a local service with ca's leaf, offering
// nextProtos by ALPN; it records the server name each client sent.
func (ca testCA) localTLS(nextProtos ...string) (*tls.Config, func() []string) {
	var mu sync.Mutex
	var names []string
	cfg := &tls.Config{
		Certificates: []tls.Certificate{ca.leaf},
		NextProtos:   nextProtos,
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			names = append(names, h.ServerName)
			mu.Unlock()
			return nil, nil
		},
	}
	return cfg, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), names...) }
}

// seen is what the local service saw of a request.
type seen struct {
	mu         sync.Mutex
	protoMajor int
	tls        bool
	alpn       string
	host       string
}

func (s *seen) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.protoMajor, s.tls, s.host = r.ProtoMajor, r.TLS != nil, r.Host
	if r.TLS != nil {
		s.alpn = r.TLS.NegotiatedProtocol
	}
	s.mu.Unlock()
	_, _ = io.WriteString(w, "ok "+r.Proto)
}

func (s *seen) get() (int, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.protoMajor, s.tls, s.alpn
}

// connLogRec keeps the connection-log entries the proxy recorded.
type connLogRec struct {
	mu sync.Mutex
	es []proxy.ConnLogEntry
}

func (c *connLogRec) Record(_ context.Context, e proxy.ConnLogEntry) error {
	c.mu.Lock()
	c.es = append(c.es, e)
	c.mu.Unlock()
	return nil
}

func (c *connLogRec) last(t *testing.T) proxy.ConnLogEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.es)
		var e proxy.ConnLogEntry
		if n > 0 {
			e = c.es[n-1]
		}
		c.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			return e
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// upstreamGet sends one GET for the service sub through ts and returns the status.
func upstreamGet(t *testing.T, ts *httptest.Server, sub string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/x", nil)
	req.Host = sub + "." + authDomain
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// streamsClosed waits until every tunnel stream the proxy dialled is closed.
func streamsClosed(t *testing.T, d *fakeDialer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for d.closed.Load() != d.opened.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("tunnel streams: %d opened, %d closed", d.opened.Load(), d.closed.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Each upstream protocol reaches the local service as what it names, over
// the tunnel stream, and the stream is closed when the request is done.
func TestUpstream_EachProtocolEndToEnd(t *testing.T) {
	ca := newTestCA(t, "app.internal")
	for _, c := range []struct {
		protocol   string
		h2c        bool
		alpn       []string // the local service's; nil: plain
		wantMajor  int
		wantTLS    bool
		wantALPN   string
		wantProtoS string
	}{
		{"", false, nil, 1, false, "", "HTTP/1.1"},
		{proto.UpstreamHTTP1, true, nil, 1, false, "", "HTTP/1.1"},
		{proto.UpstreamHTTPS, false, []string{"h2", "http/1.1"}, 1, true, "http/1.1", "HTTP/1.1"},
		{proto.UpstreamH2C, true, nil, 2, false, "", "HTTP/2.0"},
		{proto.UpstreamH2, false, []string{"h2", "http/1.1"}, 2, true, "h2", "HTTP/2.0"},
	} {
		t.Run("protocol "+c.protocol, func(t *testing.T) {
			s := &seen{}
			d := newFakeDialer(http.HandlerFunc(s.handler))
			d.h2c = c.h2c
			if c.alpn != nil {
				d.tlsConfig, _ = ca.localTLS(c.alpn...)
			}
			d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:3000", UpstreamProtocol: c.protocol})
			ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog()))
			defer ts.Close()
			for i := 0; i < 3; i++ {
				code, body := upstreamGet(t, ts, "app")
				if code != http.StatusOK || body != "ok "+c.wantProtoS {
					t.Fatalf("got %d %q", code, body)
				}
			}
			major, isTLS, alpn := s.get()
			if major != c.wantMajor || isTLS != c.wantTLS || alpn != c.wantALPN {
				t.Errorf("local service saw HTTP/%d tls=%v alpn=%q; want HTTP/%d tls=%v alpn=%q", major, isTLS, alpn, c.wantMajor, c.wantTLS, c.wantALPN)
			}
			if s.host != "127.0.0.1:3000" {
				t.Errorf("host %q", s.host)
			}
			if d.opened.Load() != 3 {
				t.Errorf("one tunnel stream per request: %d for 3", d.opened.Load())
			}
			streamsClosed(t, d)
		})
	}
}

// The TLS settings apply as H3 says: no verification by default, the system
// roots with upstream_tls_verify, only the CA when one is set; a failed
// handshake is a 502 logged closed_error with reason upstream_tls.
func TestUpstream_TLSVerification(t *testing.T) {
	ca := newTestCA(t, "app.internal")
	other := newTestCA(t, "app.internal")
	for _, c := range []struct {
		name   string
		verify bool
		caPEM  []byte
		sn     string
		want   int
	}{
		{"verify off trusts a self-signed service", false, nil, "", http.StatusOK},
		{"verify on, system roots", true, nil, "", http.StatusBadGateway},
		{"the service's CA", false, ca.caPEM, "", http.StatusOK},
		{"the service's CA with verify on", true, ca.caPEM, "", http.StatusOK},
		{"another CA", false, other.caPEM, "", http.StatusBadGateway},
		{"the CA and a name in the certificate", false, ca.caPEM, "app.internal", http.StatusOK},
		{"the CA and a name not in the certificate", false, ca.caPEM, "other.internal", http.StatusBadGateway},
		{"a CA that is no PEM", false, []byte("garbage"), "", http.StatusBadGateway},
	} {
		for _, protocol := range []string{proto.UpstreamHTTPS, proto.UpstreamH2} {
			t.Run(c.name+" "+protocol, func(t *testing.T) {
				s := &seen{}
				d := newFakeDialer(http.HandlerFunc(s.handler))
				d.tlsConfig, _ = ca.localTLS("h2", "http/1.1")
				d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:3000",
					UpstreamProtocol: protocol, UpstreamTLSVerify: c.verify, UpstreamTLSCAPEM: c.caPEM, UpstreamTLSServerName: c.sn})
				logs := &connLogRec{}
				ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithConnLogSink(logs)))
				defer ts.Close()
				if code, body := upstreamGet(t, ts, "app"); code != c.want {
					t.Fatalf("got %d %q, want %d", code, body, c.want)
				}
				e := logs.last(t)
				if c.want == http.StatusOK && (e.Status != proxy.StatusClosedClean || e.Reason != "") {
					t.Errorf("log %q %q", e.Status, e.Reason)
				}
				if c.want != http.StatusOK && (e.Status != proxy.StatusClosedError || e.Reason != proxy.ReasonUpstreamTLS) {
					t.Errorf("log %q %q, want closed_error upstream_tls", e.Status, e.Reason)
				}
				streamsClosed(t, d)
			})
		}
	}
}

// The server name is upstream_tls_server_name, else the host of the local
// host header; an IP sends no SNI.
func TestUpstream_TLSServerName(t *testing.T) {
	ca := newTestCA(t, "app.internal")
	for _, c := range []struct {
		localHost, sn, want string
	}{
		{"127.0.0.1:3000", "", ""},
		{"app.internal:8443", "", "app.internal"},
		{"app.internal", "", "app.internal"},
		{"127.0.0.1:3000", "app.internal", "app.internal"},
		{"[::1]:3000", "", ""},
	} {
		d := newFakeDialer(http.HandlerFunc((&seen{}).handler))
		var names func() []string
		d.tlsConfig, names = ca.localTLS("h2")
		d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: c.localHost,
			UpstreamProtocol: proto.UpstreamH2, UpstreamTLSServerName: c.sn})
		ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog()))
		if code, body := upstreamGet(t, ts, "app"); code != http.StatusOK {
			t.Fatalf("%+v: %d %q", c, code, body)
		}
		ts.Close()
		if got := names(); len(got) != 1 || got[0] != c.want {
			t.Errorf("%+v: SNI %q, want %q", c, got, c.want)
		}
	}
}

// h2 needs the local service to choose h2 by ALPN; https takes a service
// that chooses nothing.
func TestUpstream_ALPN(t *testing.T) {
	ca := newTestCA(t, "app.internal")
	for _, c := range []struct {
		name     string
		protocol string
		alpn     []string
		want     int
	}{
		{"h2 to a service without ALPN", proto.UpstreamH2, []string{}, http.StatusBadGateway},
		{"h2 to an HTTP/1.1-only service", proto.UpstreamH2, []string{"http/1.1"}, http.StatusBadGateway},
		{"https to a service without ALPN", proto.UpstreamHTTPS, []string{}, http.StatusOK},
		// Go's TLS server answers an http/1.1-only offer without ALPN even
		// when it lists h2 alone; the client takes that as HTTP/1.1.
		{"https to a service that lists h2 only", proto.UpstreamHTTPS, []string{"h2"}, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newFakeDialer(http.HandlerFunc((&seen{}).handler))
			d.tlsConfig, _ = ca.localTLS(c.alpn...)
			d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:3000", UpstreamProtocol: c.protocol})
			logs := &connLogRec{}
			ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithConnLogSink(logs)))
			defer ts.Close()
			if code, body := upstreamGet(t, ts, "app"); code != c.want {
				t.Fatalf("got %d %q, want %d", code, body, c.want)
			}
			if e := logs.last(t); c.want != http.StatusOK && e.Reason != proxy.ReasonUpstreamTLS {
				t.Errorf("reason %q", e.Reason)
			}
			streamsClosed(t, d)
		})
	}
}

// An upgrade (a WebSocket) goes upstream as HTTP/1.1 on every protocol:
// plain for h2c, over TLS for h2. The upgraded connection carries bytes both
// ways.
func TestUpstream_UpgradeIsHTTP1(t *testing.T) {
	ca := newTestCA(t, "app.internal")
	for _, c := range []struct {
		protocol string
		wantTLS  bool
	}{
		{proto.UpstreamH2C, false},
		{proto.UpstreamH2, true},
		{proto.UpstreamHTTPS, true},
		{proto.UpstreamHTTP1, false},
	} {
		t.Run(c.protocol, func(t *testing.T) {
			var mu sync.Mutex
			var major int
			var isTLS bool
			local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				major, isTLS = r.ProtoMajor, r.TLS != nil
				mu.Unlock()
				if r.Header.Get("Upgrade") != "websocket" {
					http.Error(w, "want an upgrade", http.StatusBadRequest)
					return
				}
				conn, brw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				_ = brw.Flush()
				line, _ := brw.ReadString('\n')
				_, _ = brw.WriteString("echo " + line)
				_ = brw.Flush()
			})
			d := newFakeDialer(local)
			if c.wantTLS {
				d.tlsConfig, _ = ca.localTLS("h2", "http/1.1")
			} else {
				d.h2c = true
			}
			d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:3000", UpstreamProtocol: c.protocol})
			ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog()))
			defer ts.Close()

			conn, err := net.Dial("tcp", ts.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: app.%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", authDomain)
			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status %d", resp.StatusCode)
			}
			_, _ = io.WriteString(conn, "ping\n")
			line, err := br.ReadString('\n')
			if err != nil || line != "echo ping\n" {
				t.Fatalf("echo %q %v", line, err)
			}
			// The upgraded connection's tunnel stream is closed once both
			// ends are done, on every protocol.
			_ = conn.Close()
			streamsClosed(t, d)
			mu.Lock()
			defer mu.Unlock()
			if major != 1 || isTLS != c.wantTLS {
				t.Errorf("local service saw HTTP/%d tls=%v; want HTTP/1 tls=%v", major, isTLS, c.wantTLS)
			}
		})
	}
}

// A changed setting applies from the next request on: the protocol, the CA
// and the server name are read anew, not kept from the request before.
func TestUpstream_ChangedSettingsApply(t *testing.T) {
	ca := newTestCA(t, "app.internal")
	other := newTestCA(t, "app.internal")
	s := &seen{}
	d := newFakeDialer(http.HandlerFunc(s.handler))
	d.tlsConfig, _ = ca.localTLS("h2", "http/1.1")
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog()))
	defer ts.Close()

	steps := []struct {
		res       proxy.Resolved
		wantCode  int
		wantMajor int
	}{
		{proxy.Resolved{UpstreamProtocol: proto.UpstreamH2, UpstreamTLSCAPEM: ca.caPEM}, http.StatusOK, 2},
		{proxy.Resolved{UpstreamProtocol: proto.UpstreamHTTPS, UpstreamTLSCAPEM: ca.caPEM}, http.StatusOK, 1},
		{proxy.Resolved{UpstreamProtocol: proto.UpstreamHTTPS, UpstreamTLSCAPEM: other.caPEM}, http.StatusBadGateway, 0},
		{proxy.Resolved{UpstreamProtocol: proto.UpstreamH2, UpstreamTLSCAPEM: ca.caPEM}, http.StatusOK, 2},
		{proxy.Resolved{UpstreamProtocol: proto.UpstreamH2, UpstreamTLSCAPEM: ca.caPEM, UpstreamTLSServerName: "nope.internal"}, http.StatusBadGateway, 0},
		{proxy.Resolved{UpstreamProtocol: proto.UpstreamH2, UpstreamTLSCAPEM: ca.caPEM, UpstreamTLSServerName: "app.internal"}, http.StatusOK, 2},
		{proxy.Resolved{UpstreamProtocol: proto.UpstreamH2, UpstreamTLSCAPEM: ca.caPEM, UpstreamTLSServerName: "app.internal", TunnelID: "t2"}, http.StatusOK, 2},
	}
	for i, st := range steps {
		res := st.res
		res.ServiceID, res.AccessMode, res.LocalHost = "s1", "open", "127.0.0.1:3000"
		d.register("app", &res)
		code, body := upstreamGet(t, ts, "app")
		if code != st.wantCode {
			t.Fatalf("step %d: %d %q, want %d", i, code, body, st.wantCode)
		}
		if major, _, _ := s.get(); code == http.StatusOK && major != st.wantMajor {
			t.Fatalf("step %d: HTTP/%d, want HTTP/%d", i, major, st.wantMajor)
		}
	}
	streamsClosed(t, d)
}

// An h2c response body reaches the visitor as it is written, not when it
// ends, through an HTTP/1.1 and an HTTP/2 front.
func TestUpstream_H2CStreamsUnbuffered(t *testing.T) {
	for _, front := range []string{"h1", "h2"} {
		t.Run(front, func(t *testing.T) {
			release := make(chan struct{})
			local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					http.Error(w, "want h2c", http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, "first\n")
				w.(http.Flusher).Flush()
				<-release
				_, _ = io.WriteString(w, "second\n")
			})
			d := newFakeDialer(local)
			d.h2c = true
			d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:3000", UpstreamProtocol: proto.UpstreamH2C})
			ts := frontServer(proxy.New(d, openChecker{}, authDomain, testLog()), front)
			defer ts.Close()

			req, _ := http.NewRequest("GET", ts.URL+"/stream", nil)
			req.Host = "app." + authDomain
			resp, err := ts.Client().Do(req)
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			defer resp.Body.Close()
			lines := make(chan string, 1)
			br := bufio.NewReader(resp.Body)
			go func() { l, _ := br.ReadString('\n'); lines <- l }()
			select {
			case l := <-lines:
				if l != "first\n" {
					t.Fatalf("first line %q", l)
				}
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("the first line was held back until the body ends")
			}
			close(release)
			rest, _ := io.ReadAll(br)
			if string(rest) != "second\n" {
				t.Fatalf("rest %q", rest)
			}
			streamsClosed(t, d)
		})
	}
}

// Trailers of an h2c response reach the visitor, announced ones and ones
// sent with http.TrailerPrefix, through an HTTP/1.1 and an HTTP/2 front.
func TestUpstream_H2CTrailers(t *testing.T) {
	for _, front := range []string{"h1", "h2"} {
		t.Run(front, func(t *testing.T) {
			local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					http.Error(w, "want h2c", http.StatusBadRequest)
					return
				}
				w.Header().Set("Trailer", "Grpc-Status")
				w.Header().Set("Content-Type", "application/grpc")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "body")
				// A gRPC server streams: no Content-Length, so an
				// HTTP/1.1 front can send the trailers too.
				w.(http.Flusher).Flush()
				w.Header().Set("Grpc-Status", "0")
				w.Header().Set(http.TrailerPrefix+"Grpc-Message", "fine")
			})
			d := newFakeDialer(local)
			d.h2c = true
			d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:3000", UpstreamProtocol: proto.UpstreamH2C})
			ts := frontServer(proxy.New(d, openChecker{}, authDomain, testLog()), front)
			defer ts.Close()

			req, _ := http.NewRequest("POST", ts.URL+"/pkg.Svc/Call", strings.NewReader("req"))
			req.Host = "app." + authDomain
			req.Header.Set("Content-Type", "application/grpc")
			req.Header.Set("Te", "trailers")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || string(b) != "body" {
				t.Fatalf("%d %q", resp.StatusCode, b)
			}
			if got := resp.Trailer.Get("Grpc-Status"); got != "0" {
				t.Errorf("Grpc-Status trailer %q (trailers %v)", got, resp.Trailer)
			}
			if got := resp.Trailer.Get("Grpc-Message"); got != "fine" {
				t.Errorf("Grpc-Message trailer %q (trailers %v)", got, resp.Trailer)
			}
			streamsClosed(t, d)
		})
	}
}

// frontServer serves h as a visitor reaches it: HTTP/1.1 in plain text
// ("h1"), or HTTP/2 over TLS ("h2").
func frontServer(h http.Handler, front string) *httptest.Server {
	if front == "h1" {
		return httptest.NewServer(h)
	}
	ts := httptest.NewUnstartedServer(h)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	return ts
}

// multiDialer routes each subdomain to a dialer of its own, so services can
// have local services with TLS certificates of their own.
type multiDialer map[string]*fakeDialer

func (m multiDialer) Lookup(ctx context.Context, sub string) (*proxy.Resolved, error) {
	if d, ok := m[sub]; ok {
		return d.Lookup(ctx, sub)
	}
	return nil, proxy.ErrNotFound
}

func (m multiDialer) LookupByServiceID(ctx context.Context, id string) (*proxy.Resolved, error) {
	for _, d := range m {
		if res, err := d.LookupByServiceID(ctx, id); err == nil {
			return res, nil
		}
	}
	return nil, proxy.ErrNotFound
}

func (m multiDialer) DialTunnelStream(ctx context.Context, sub string) (net.Conn, error) {
	if d, ok := m[sub]; ok {
		return d.DialTunnelStream(ctx, sub)
	}
	return nil, proxy.ErrNotFound
}

func (m multiDialer) DialTunnelStreamByServiceID(ctx context.Context, id string) (net.Conn, error) {
	for _, d := range m {
		if _, err := d.LookupByServiceID(ctx, id); err == nil {
			return d.DialTunnelStreamByServiceID(ctx, id)
		}
	}
	return nil, proxy.ErrNotFound
}

// Two services with CAs of their own keep their own TLS configuration: each
// verifies against its own CA, request after request.
func TestUpstream_TwoServicesTwoCAs(t *testing.T) {
	m := multiDialer{}
	for _, sub := range []string{"a", "b"} {
		ca := newTestCA(t, sub+".internal")
		s := &seen{}
		d := newFakeDialer(http.HandlerFunc(s.handler))
		d.tlsConfig, _ = ca.localTLS("h2", "http/1.1")
		d.register(sub, &proxy.Resolved{ServiceID: "svc-" + sub, TunnelID: "t-" + sub, AccessMode: "open",
			LocalHost: sub + ".internal:443", UpstreamProtocol: proto.UpstreamH2, UpstreamTLSCAPEM: ca.caPEM})
		m[sub] = d
	}
	ts := httptest.NewServer(proxy.New(m, openChecker{}, authDomain, testLog()))
	defer ts.Close()
	for i, sub := range []string{"a", "b", "a", "b", "b", "a"} {
		if code, body := upstreamGet(t, ts, sub); code != http.StatusOK {
			t.Fatalf("request %d to %s: %d %q", i, sub, code, body)
		}
	}
	for _, d := range m {
		streamsClosed(t, d)
	}
}

// A local service that accepts the stream and never answers the TLS
// handshake is given up after the handshake bound: 502, reason upstream_tls.
func TestUpstream_TLSHandshakeBounded(t *testing.T) {
	defer proxy.SetUpstreamTLSHandshakeTimeoutForTest(200 * time.Millisecond)()
	d := newFakeDialer(http.NotFoundHandler())
	d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:443", UpstreamProtocol: proto.UpstreamH2})
	logs := &connLogRec{}
	ts := httptest.NewServer(proxy.New(silentDialer{d}, openChecker{}, authDomain, testLog(), proxy.WithConnLogSink(logs)))
	defer ts.Close()
	start := time.Now()
	code, _ := upstreamGet(t, ts, "app")
	if code != http.StatusBadGateway {
		t.Fatalf("status %d", code)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the handshake held the request %v", took)
	}
	if e := logs.last(t); e.Reason != proxy.ReasonUpstreamTLS || e.Status != proxy.StatusClosedError {
		t.Fatalf("row %+v", e)
	}
}

// silentDialer's streams take every byte and never answer.
type silentDialer struct{ *fakeDialer }

func (silentDialer) DialTunnelStreamByServiceID(context.Context, string) (net.Conn, error) {
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, b) }()
	return a, nil
}
