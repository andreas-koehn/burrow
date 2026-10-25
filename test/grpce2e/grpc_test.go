package grpce2e

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// dialH2C opens a prior-knowledge h2c connection to addr that names
// authority as :authority (the custom domain).
func dialH2C(t *testing.T, addr, authority string) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithAuthority(authority))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return cc
}

// dialTLS opens gRPC over TLS (ALPN h2) to addr with SNI serverName, verified
// against the run's CA.
func dialTLS(t *testing.T, addr, serverName string) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: e.ca.pool, ServerName: serverName})),
		grpc.WithAuthority(serverName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return cc
}

func unary(ctx context.Context, cc *grpc.ClientConn, msg string, opts ...grpc.CallOption) (string, error) {
	in, out := []byte(msg), []byte(nil)
	err := cc.Invoke(ctx, "/e2e.Echo/Unary", &in, &out, append(opts, grpc.CallContentSubtype(callRaw))...)
	return string(out), err
}

func sleepCall(ctx context.Context, cc *grpc.ClientConn, ms int) error {
	in, out := []byte(fmt.Sprint(ms)), []byte(nil)
	return cc.Invoke(ctx, "/e2e.Echo/Sleep", &in, &out, grpc.CallContentSubtype(callRaw))
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, what string, err error, want codes.Code) *status.Status {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != want {
		t.Fatalf("%s: code %v (%q), want %v", what, st.Code(), st.Message(), want)
	}
	return st
}

// checkHealthUnaryBidi is the basic gRPC round: health, one unary call and
// 1000 bidi messages, each answered before the next one is sent.
func checkHealthUnaryBidi(t *testing.T, cc *grpc.ClientConn, backend string) {
	t.Helper()
	hc := healthpb.NewHealthClient(cc)
	for _, svc := range []string{"", "e2e.Echo"} {
		resp, err := hc.Check(ctxT(t, 10*time.Second), &healthpb.HealthCheckRequest{Service: svc})
		if err != nil {
			t.Fatalf("health %q: %v", svc, err)
		}
		if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Fatalf("health %q: %v", svc, resp.GetStatus())
		}
	}
	// NotFound is a trailers-only reply of the service: it must reach the
	// caller as one HEADERS frame with END_STREAM every time (it lost the
	// race with ReverseProxy's flush timer about one call in ten).
	for i := 0; i < 200; i++ {
		_, err := hc.Check(ctxT(t, 10*time.Second), &healthpb.HealthCheckRequest{Service: "nope"})
		wantCode(t, fmt.Sprintf("health of an unknown service, call %d", i), err, codes.NotFound)
	}

	got, err := unary(ctxT(t, 10*time.Second), cc, "hello")
	if err != nil {
		t.Fatalf("unary: %v", err)
	}
	if got != backend+":hello" {
		t.Fatalf("unary answer %q, want %q", got, backend+":hello")
	}

	ctx := ctxT(t, 60*time.Second)
	st, err := cc.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/e2e.Echo/Bidi", grpc.CallContentSubtype(callRaw))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		msg := []byte(fmt.Sprintf("m%04d", i))
		if err := st.SendMsg(&msg); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		var out []byte
		if err := st.RecvMsg(&out); err != nil {
			t.Fatalf("recv %d: %v", i, err)
		}
		if want := backend + ":" + string(msg); string(out) != want {
			t.Fatalf("bidi %d: %q, want %q", i, out, want)
		}
	}
	if err := st.CloseSend(); err != nil {
		t.Fatal(err)
	}
	var out []byte
	if err := st.RecvMsg(&out); err != io.EOF {
		t.Fatalf("bidi end: %v, want io.EOF (status OK)", err)
	}
}

// addDomain binds an automatic custom domain (no certificate: the plain
// relay is behind a proxy) and returns nothing; it fails the test otherwise.
func addAutoDomain(t *testing.T, r *relay, serviceID, host string) {
	t.Helper()
	b := r.api.must(t, 201, "POST", "/services/"+serviceID+"/domains", map[string]string{"hostname": host, "cert_source": "auto"})
	var d struct {
		CertStatus string `json:"cert_status"`
	}
	_ = json.Unmarshal(b, &d)
	if d.CertStatus != "not_needed" {
		t.Fatalf("auto domain %s behind a proxy: cert_status %q, want not_needed", host, d.CertStatus)
	}
}

func startGRPCService(t *testing.T, r *relay, name string, args ...string) string {
	t.Helper()
	id, _, err := r.client(e, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// (a) h2c through the plain listener: directly, and behind a TLS-terminating
// proxy that forwards h2c (the Traefik scheme=h2c recipe).
func TestGRPC_H2CThroughThePlainListener(t *testing.T) {
	id := startGRPCService(t, e.plain, "g-h2c", "grpc", e.grpcPlain.addr)
	addAutoDomain(t, e.plain, id, "grpc.p.test")
	var hs struct {
		Effective string `json:"effective_upstream_protocol"`
		Client    string `json:"client_upstream_protocol"`
	}
	_ = json.Unmarshal(e.plain.api.must(t, 200, "GET", "/services/"+id+"/http-settings", nil), &hs)
	if hs.Effective != "h2c" || hs.Client != "h2c" {
		t.Fatalf("burrow grpc: client %q effective %q, want h2c set by the client", hs.Client, hs.Effective)
	}

	t.Run("direct h2c", func(t *testing.T) {
		checkHealthUnaryBidi(t, dialH2C(t, e.plain.http, "grpc.p.test"), "h2c")
	})
	t.Run("behind a TLS-terminating proxy", func(t *testing.T) {
		checkHealthUnaryBidi(t, dialTLS(t, e.proxyAddr, "grpc.p.test"), "h2c")
	})
	t.Run("the dashboard host is not dispatched", func(t *testing.T) {
		_, err := unary(ctxT(t, 10*time.Second), dialH2C(t, e.plain.http, "dash.p.test"), "x")
		if status.Code(err) == codes.OK {
			t.Fatal("a call to the dashboard host reached a service")
		}
	})
}

// (b) gRPC over TLS with ALPN h2 on the relay's own TLS listener, reached by
// a custom domain with an uploaded certificate.
func TestGRPC_TLSOnTheRelaysListener(t *testing.T) {
	id := startGRPCService(t, e.tlsR, "t-grpc", "grpc", e.grpcPlain.addr)
	cert, err := e.ca.issue("grpc.t.test", "grpc.t.test")
	if err != nil {
		t.Fatal(err)
	}
	b := e.tlsR.api.must(t, 201, "POST", "/services/"+id+"/domains", map[string]string{
		"hostname": "grpc.t.test", "cert_source": "uploaded", "cert_pem": string(cert.certPEM), "key_pem": string(cert.keyPEM),
	})
	if !strings.Contains(string(b), `"cert_status":"ready"`) {
		t.Fatalf("uploaded domain: %s", b)
	}
	cc := dialTLS(t, e.tlsR.http, "grpc.t.test")
	checkHealthUnaryBidi(t, cc, "h2c")

	// The relay served the uploaded certificate, over h2.
	var p peer.Peer
	if _, err := unary(ctxT(t, 10*time.Second), cc, "who", grpc.Peer(&p)); err != nil {
		t.Fatal(err)
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		t.Fatalf("auth info %T", p.AuthInfo)
	}
	if got := ti.State.PeerCertificates[0].Subject.CommonName; got != "grpc.t.test" {
		t.Fatalf("certificate %q, want the uploaded grpc.t.test", got)
	}
	if ti.State.NegotiatedProtocol != "h2" {
		t.Fatalf("ALPN %q, want h2", ti.State.NegotiatedProtocol)
	}
}

// (c) deadlines: the client's grpc-timeout reaches the service, and
// grpc_max_duration_s caps calls without (or with a longer) deadline.
func TestGRPC_Deadlines(t *testing.T) {
	id := startGRPCService(t, e.plain, "g-cap", "grpc", e.grpcPlain.addr)
	addAutoDomain(t, e.plain, id, "cap.p.test")
	cc := dialH2C(t, e.plain.http, "cap.p.test")

	t.Run("client deadline", func(t *testing.T) {
		start := time.Now()
		err := sleepCall(ctxT(t, 500*time.Millisecond), cc, 2001)
		wantCode(t, "sleep past the deadline", err, codes.DeadlineExceeded)
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("took %v", el)
		}
		left, ok := e.grpcPlain.deadline("2001")
		if !ok || left < 0 || left > 500*time.Millisecond {
			t.Fatalf("service saw deadline %v (%t), want <= 500ms", left, ok)
		}
	})

	e.plain.api.must(t, 200, "PUT", "/services/"+id+"/http-settings", map[string]any{"grpc_max_duration_s": 1})
	t.Run("cap without a deadline", func(t *testing.T) {
		start := time.Now()
		err := sleepCall(ctxT(t, 30*time.Second), cc, 3001)
		wantCode(t, "capped call", err, codes.DeadlineExceeded)
		if el := time.Since(start); el < 900*time.Millisecond || el > 3*time.Second {
			t.Fatalf("capped call ended after %v, want about 1-2 s", el)
		}
		left, ok := e.grpcPlain.deadline("3001")
		if !ok || left < 0 || left > time.Second {
			t.Fatalf("service saw deadline %v (%t), want <= 1s (grpc-timeout rewritten to the cap)", left, ok)
		}
	})
	t.Run("cap below a longer deadline", func(t *testing.T) {
		err := sleepCall(ctxT(t, 20*time.Second), cc, 3002)
		wantCode(t, "capped call", err, codes.DeadlineExceeded)
		if left, _ := e.grpcPlain.deadline("3002"); left < 0 || left > time.Second {
			t.Fatalf("service saw deadline %v, want <= 1s", left)
		}
	})
	t.Run("a short call passes", func(t *testing.T) {
		if err := sleepCall(ctxT(t, 10*time.Second), cc, 10); err != nil {
			t.Fatal(err)
		}
	})
	// The relay's own cut is logged with code 4 and promoted to closed_error.
	if err := eventually(10*time.Second, func() error {
		for _, r := range e.plain.api.connLogs(t, id) {
			if r.GRPCMethod == "/e2e.Echo/Sleep" && r.GRPCStatus != nil && *r.GRPCStatus == 4 && r.Status == "closed_error" {
				return nil
			}
		}
		return fmt.Errorf("no closed_error row with grpc_status 4")
	}); err != nil {
		t.Fatal(err)
	}
}

// (d) refusals as gRPC status: api_key in metadata, ip denial, offline tunnel.
func TestGRPC_Refusals(t *testing.T) {
	t.Run("api_key", func(t *testing.T) {
		id := startGRPCService(t, e.plain, "g-key", "grpc", e.grpcPlain.addr)
		addAutoDomain(t, e.plain, id, "key.p.test")
		e.plain.api.must(t, 204, "PUT", "/services/"+id+"/access-mode", map[string]string{"access_mode": "api_key"})
		var k struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(e.plain.api.must(t, 201, "POST", "/services/"+id+"/api-keys", map[string]string{"name": "e2e"}), &k)
		if k.Key == "" {
			t.Fatal("no api key")
		}
		cc := dialH2C(t, e.plain.http, "key.p.test")
		var err error
		if err = eventually(10*time.Second, func() error {
			_, err := unary(ctxT(t, 5*time.Second), cc, "nokey")
			if status.Code(err) != codes.Unauthenticated {
				return fmt.Errorf("no key: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_, err = unary(metadata.AppendToOutgoingContext(ctxT(t, 5*time.Second), "authorization", "Bearer wrong"), cc, "x")
		wantCode(t, "wrong key", err, codes.Unauthenticated)
		got, err := unary(metadata.AppendToOutgoingContext(ctxT(t, 5*time.Second), "authorization", "Bearer "+k.Key), cc, "withkey")
		if err != nil || got != "h2c:withkey" {
			t.Fatalf("with the key in metadata: %q %v", got, err)
		}

		// A configured header name is read from metadata the same way.
		e.plain.api.must(t, 204, "PUT", "/services/"+id+"/access-mode", map[string]string{"access_mode": "api_key", "api_key_header": "x-api-key"})
		if err := eventually(10*time.Second, func() error {
			_, err := unary(metadata.AppendToOutgoingContext(ctxT(t, 5*time.Second), "x-api-key", k.Key), cc, "hdr")
			return err
		}); err != nil {
			t.Fatalf("key in x-api-key metadata: %v", err)
		}
	})

	t.Run("ip denial", func(t *testing.T) {
		id := startGRPCService(t, e.plain, "g-ip", "grpc", e.grpcPlain.addr)
		addAutoDomain(t, e.plain, id, "ip.p.test")
		cc := dialH2C(t, e.plain.http, "ip.p.test")
		if _, err := unary(ctxT(t, 5*time.Second), cc, "before"); err != nil {
			t.Fatal(err)
		}
		e.plain.api.must(t, 204, "PUT", "/services/"+id+"/ip-geo", map[string]any{"enabled": true, "block_cidrs": []string{"127.0.0.0/8"}})
		if err := eventually(10*time.Second, func() error {
			_, err := unary(ctxT(t, 5*time.Second), cc, "blocked")
			if status.Code(err) != codes.PermissionDenied {
				return fmt.Errorf("blocked source: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("offline tunnel", func(t *testing.T) {
		id, p, err := e.plain.client(e, "g-off", "grpc", e.grpcPlain.addr)
		if err != nil {
			t.Fatal(err)
		}
		addAutoDomain(t, e.plain, id, "off.p.test")
		cc := dialH2C(t, e.plain.http, "off.p.test")
		if _, err := unary(ctxT(t, 5*time.Second), cc, "online"); err != nil {
			t.Fatal(err)
		}
		p.stop()
		if err := eventually(15*time.Second, func() error {
			if e.plain.api.service(t, id).Connected {
				return fmt.Errorf("still connected")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_, err = unary(ctxT(t, 5*time.Second), cc, "offline")
		wantCode(t, "offline service", err, codes.Unavailable)
	})

	t.Run("unknown host", func(t *testing.T) {
		// Not a custom domain: the relay's router answers, never a service.
		_, err := unary(ctxT(t, 5*time.Second), dialH2C(t, e.plain.http, "nobody.p.test"), "x")
		if status.Code(err) == codes.OK {
			t.Fatal("unknown host served")
		}
	})
}

// (e) a service with AI controls switched on refuses gRPC.
func TestGRPC_AIControlsRefuse(t *testing.T) {
	id := startGRPCService(t, e.plain, "g-ai", "grpc", e.grpcPlain.addr)
	addAutoDomain(t, e.plain, id, "ai.p.test")
	cc := dialH2C(t, e.plain.http, "ai.p.test")
	// Sections present but off (as the dashboard saves them): served.
	cfg := map[string]any{
		"cache":      map[string]any{"enabled": true, "ttl_seconds": 300},
		"redaction":  map[string]any{"enabled": false, "redact_for_logs_only": false, "rule_ids": []string{}, "presidio_enabled": false},
		"guardrails": map[string]any{"enabled": false, "action": "log_only"},
		"inspector":  map[string]any{"enabled": true, "max_requests": 100},
	}
	e.plain.api.must(t, 204, "PUT", "/services/"+id+"/ai-config", cfg)
	if err := eventually(10*time.Second, func() error {
		_, err := unary(ctxT(t, 5*time.Second), cc, "ai-off")
		return err
	}); err != nil {
		t.Fatalf("inspector and cache only: %v", err)
	}
	cfg["guardrails"] = map[string]any{"enabled": true, "action": "log_only"}
	e.plain.api.must(t, 204, "PUT", "/services/"+id+"/ai-config", cfg)
	if err := eventually(10*time.Second, func() error {
		_, err := unary(ctxT(t, 5*time.Second), cc, "ai-on")
		if status.Code(err) != codes.PermissionDenied {
			return fmt.Errorf("guardrails on: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// (f) the 443 mux: one TLS port carries the dashboard over h2, gRPC by its
// custom domain (TestGRPC_TLSOnTheRelaysListener) and raw TLS to a tcp
// service by its SNI. A private tcp service and acme-tls/1 hellos are
// terminated by the relay.
func TestMux_OnePortForDashboardGRPCAndPassthrough(t *testing.T) {
	echoCert, err := e.ca.issue("echo", "*.tcp.t.test")
	if err != nil {
		t.Fatal(err)
	}
	ec, _ := echoCert.tlsCert()
	open, err := startTLSEcho("open", ec)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := startTLSEcho("priv", ec)
	if err != nil {
		t.Fatal(err)
	}
	openID := startGRPCService(t, e.tlsR, "echo-open", "tcp", open.addr, "--access", "open")
	privID := startGRPCService(t, e.tlsR, "echo-priv", "tcp", priv.addr)
	openSlug := e.tlsR.api.service(t, openID).Slug
	privSvc := e.tlsR.api.service(t, privID)
	if privSvc.AccessMode != "private" {
		t.Fatalf("echo-priv access %q, want private", privSvc.AccessMode)
	}

	t.Run("dashboard over h2", func(t *testing.T) {
		resp, err := e.tlsR.api.c.Get(e.tlsR.baseURL() + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.ProtoMajor != 2 {
			t.Fatalf("dashboard: %d %s", resp.StatusCode, resp.Proto)
		}
	})

	t.Run("tcp name goes raw", func(t *testing.T) {
		name := openSlug + ".tcp.t.test"
		if err := eventually(15*time.Second, func() error {
			c, err := tls.Dial("tcp", e.tlsR.http, &tls.Config{RootCAs: e.ca.pool, ServerName: name})
			if err != nil {
				return err
			}
			defer c.Close()
			if cn := c.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "echo" {
				return fmt.Errorf("certificate %q, want the service's own (echo)", cn)
			}
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Write([]byte("ping\n")); err != nil {
				return err
			}
			line, err := bufio.NewReader(c).ReadString('\n')
			if err != nil {
				return err
			}
			if line != "open: ping\n" {
				return fmt.Errorf("echo %q", line)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("private tcp service is never reached", func(t *testing.T) {
		for _, name := range []string{privSvc.Slug + ".tcp.t.test", strings.ToUpper(privSvc.Slug) + ".TCP.t.test"} {
			c, err := tls.Dial("tcp", e.tlsR.http, &tls.Config{InsecureSkipVerify: true, ServerName: name})
			if err != nil {
				continue // refused by the relay's TLS: also not reached
			}
			cn := c.ConnectionState().PeerCertificates[0].Subject.CommonName
			c.Close()
			if cn != "dashboard" {
				t.Fatalf("%s: certificate %q, want the relay's own", name, cn)
			}
		}
		time.Sleep(300 * time.Millisecond)
		if n := priv.accepted.Load(); n != 0 {
			t.Fatalf("the private service saw %d connections", n)
		}
	})

	t.Run("acme-tls/1 hello is terminated", func(t *testing.T) {
		before := open.accepted.Load()
		c, err := tls.Dial("tcp", e.tlsR.http, &tls.Config{InsecureSkipVerify: true, ServerName: openSlug + ".tcp.t.test", NextProtos: []string{"acme-tls/1"}})
		if err == nil {
			cn := c.ConnectionState().PeerCertificates[0].Subject.CommonName
			c.Close()
			if cn == "echo" {
				t.Fatal("acme-tls/1 hello reached the tcp service")
			}
		} else if !strings.Contains(err.Error(), "no application protocol") && !strings.Contains(err.Error(), "alert") {
			t.Fatalf("acme-tls/1 hello: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
		if open.accepted.Load() != before {
			t.Fatal("acme-tls/1 hello reached the tcp service")
		}
	})

	t.Run("gRPC still served on the same port", func(t *testing.T) {
		if got, err := unary(ctxT(t, 10*time.Second), dialTLS(t, e.tlsR.http, "grpc.t.test"), "mux"); err != nil || got != "h2c:mux" {
			t.Fatalf("%q %v", got, err)
		}
	})

	t.Run("passthrough row", func(t *testing.T) {
		if err := eventually(10*time.Second, func() error {
			for _, r := range e.tlsR.api.connLogs(t, openID) {
				if r.Protocol == "tls" && r.SNI == openSlug+".tcp.t.test" {
					return nil
				}
			}
			return fmt.Errorf("no tls row with the sni")
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// (g) upstream protocols h2 and https over a local TLS service, verify off,
// on, with a CA and a server name.
func TestUpstream_TLSProtocols(t *testing.T) {
	put := func(t *testing.T, id string, body map[string]any) {
		e.plain.api.must(t, 200, "PUT", "/services/"+id+"/http-settings", body)
	}

	t.Run("h2", func(t *testing.T) {
		id := startGRPCService(t, e.plain, "g-h2", "http", "https://"+e.grpcTLS.addr, "--upstream-protocol", "h2")
		addAutoDomain(t, e.plain, id, "h2.p.test")
		cc := dialH2C(t, e.plain.http, "h2.p.test")
		call := func() error {
			got, err := unary(ctxT(t, 10*time.Second), cc, "tls")
			if err == nil && got != "h2tls:tls" {
				return fmt.Errorf("answer %q", got)
			}
			return err
		}
		if err := call(); err != nil {
			t.Fatalf("verify off: %v", err)
		}
		checkHealthUnaryBidi(t, cc, "h2tls")

		put(t, id, map[string]any{"upstream_tls_verify": true})
		if err := eventually(10*time.Second, func() error {
			if status.Code(call()) != codes.Unavailable {
				return fmt.Errorf("verify on without the CA: want UNAVAILABLE, got %v", call())
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		put(t, id, map[string]any{"upstream_tls_ca_pem": string(e.ca.certPEM)})
		if err := eventually(10*time.Second, call); err != nil {
			t.Fatalf("with the CA (IP SAN): %v", err)
		}
		put(t, id, map[string]any{"upstream_tls_server_name": "wrong.e2e.test"})
		if err := eventually(10*time.Second, func() error {
			if status.Code(call()) != codes.Unavailable {
				return fmt.Errorf("wrong server name: want UNAVAILABLE")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		put(t, id, map[string]any{"upstream_tls_server_name": "upstream.e2e.test"})
		if err := eventually(10*time.Second, call); err != nil {
			t.Fatalf("right server name: %v", err)
		}
		// The failed handshakes are logged as upstream_tls.
		if err := eventually(10*time.Second, func() error {
			for _, r := range e.plain.api.connLogs(t, id) {
				if r.Reason == "upstream_tls" && r.GRPCStatus != nil && *r.GRPCStatus == 14 {
					return nil
				}
			}
			return fmt.Errorf("no upstream_tls row")
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("https", func(t *testing.T) {
		id := startGRPCService(t, e.plain, "w-https", "http", "https://"+e.httpsAddr)
		addAutoDomain(t, e.plain, id, "web.p.test")
		var hs struct {
			Effective string `json:"effective_upstream_protocol"`
		}
		_ = json.Unmarshal(e.plain.api.must(t, 200, "GET", "/services/"+id+"/http-settings", nil), &hs)
		if hs.Effective != "https" {
			t.Fatalf("effective %q, want https", hs.Effective)
		}
		get := func() (string, error) {
			req, _ := http.NewRequest("GET", "http://"+e.plain.http+"/", nil)
			req.Host = "web.p.test"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				return "", fmt.Errorf("status %d: %s", resp.StatusCode, b)
			}
			return string(b), nil
		}
		body, err := get()
		if err != nil || !strings.HasPrefix(body, "proto=HTTP/1.1 tls=true") {
			t.Fatalf("https upstream: %q %v", body, err)
		}
		put(t, id, map[string]any{"upstream_tls_verify": true, "upstream_tls_ca_pem": string(e.ca.certPEM), "upstream_tls_server_name": "upstream.e2e.test"})
		if err := eventually(10*time.Second, func() error {
			body, err := get()
			if err != nil {
				return err
			}
			if !strings.Contains(body, "sni=upstream.e2e.test") {
				return fmt.Errorf("body %q", body)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// Native gRPC to an HTTP/1.1 upstream is refused with a hint.
		_, err = unary(ctxT(t, 5*time.Second), dialH2C(t, e.plain.http, "web.p.test"), "x")
		st := wantCode(t, "grpc to an https upstream", err, codes.Unavailable)
		if !strings.Contains(st.Message(), "HTTP/1.1") {
			t.Fatalf("message %q", st.Message())
		}
	})
}

// (h) connection-log rows and the Prometheus counter of gRPC calls. The test
// makes every call it looks for itself, so it holds in any order
// (-shuffle=on).
func TestGRPC_ObservabilityOfCalls(t *testing.T) {
	api := e.plain.api
	id := startGRPCService(t, e.plain, "g-obs", "grpc", e.grpcPlain.addr)
	addAutoDomain(t, e.plain, id, "obs.p.test")
	cc := dialH2C(t, e.plain.http, "obs.p.test")
	// OK: unary, bidi, health.
	checkHealthUnaryBidi(t, cc, "h2c")
	// DEADLINE_EXCEEDED (4): the relay's cap.
	api.must(t, 200, "PUT", "/services/"+id+"/http-settings", map[string]any{"grpc_max_duration_s": 1})
	wantCode(t, "capped call", sleepCall(ctxT(t, 20*time.Second), cc, 3003), codes.DeadlineExceeded)
	// UNAUTHENTICATED (16): api_key without a key.
	api.must(t, 204, "PUT", "/services/"+id+"/access-mode", map[string]string{"access_mode": "api_key"})
	if err := eventually(10*time.Second, func() error {
		if _, err := unary(ctxT(t, 5*time.Second), cc, "nokey"); status.Code(err) != codes.Unauthenticated {
			return fmt.Errorf("no key: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// PERMISSION_DENIED (7): the ip rule (checked before the access mode).
	api.must(t, 204, "PUT", "/services/"+id+"/ip-geo", map[string]any{"enabled": true, "block_cidrs": []string{"127.0.0.0/8"}})
	if err := eventually(10*time.Second, func() error {
		if _, err := unary(ctxT(t, 5*time.Second), cc, "blocked"); status.Code(err) != codes.PermissionDenied {
			return fmt.Errorf("blocked: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// UNAVAILABLE (14): native gRPC to an HTTP/1.1 upstream (H4).
	h1 := startGRPCService(t, e.plain, "w-obs", "http", e.grpcPlain.addr)
	addAutoDomain(t, e.plain, h1, "obs-h1.p.test")
	_, err := unary(ctxT(t, 5*time.Second), dialH2C(t, e.plain.http, "obs-h1.p.test"), "x")
	wantCode(t, "gRPC to an http1 upstream", err, codes.Unavailable)

	find := func(t *testing.T, id, method string, code int, rowStatus string) {
		t.Helper()
		if err := eventually(10*time.Second, func() error {
			for _, r := range api.connLogs(t, id) {
				if r.GRPCMethod == method && r.GRPCStatus != nil && *r.GRPCStatus == code && r.Protocol == "grpc" && (rowStatus == "" || r.Status == rowStatus) {
					return nil
				}
			}
			return fmt.Errorf("no row %s code %d status %s", method, code, rowStatus)
		}); err != nil {
			t.Fatal(err)
		}
	}
	find(t, id, "/e2e.Echo/Unary", 0, "closed_clean")
	find(t, id, "/e2e.Echo/Bidi", 0, "closed_clean")
	find(t, id, "/grpc.health.v1.Health/Check", 0, "")
	find(t, id, "/e2e.Echo/Sleep", 4, "closed_error")
	find(t, id, "/e2e.Echo/Unary", 16, "rejected")
	find(t, id, "/e2e.Echo/Unary", 7, "rejected")
	find(t, h1, "/e2e.Echo/Unary", 14, "closed_error")

	b := api.must(t, 200, "GET", "/metrics", nil)
	got := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		for _, svc := range []string{id, h1} {
			if strings.HasPrefix(l, `burrow_grpc_requests_total{service="`+svc+`",code="`) {
				code := strings.TrimPrefix(l, `burrow_grpc_requests_total{service="`+svc+`",code="`)
				got[code[:strings.Index(code, `"`)]] = true
			}
		}
	}
	for _, code := range []string{"OK", "DEADLINE_EXCEEDED", "UNAUTHENTICATED", "PERMISSION_DENIED", "UNAVAILABLE"} {
		if !got[code] {
			t.Errorf("burrow_grpc_requests_total has no sample for these services with code %s (got %v)", code, got)
		}
	}
}
