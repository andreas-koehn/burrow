package main

// tcp complete (E1): TLS passthrough by SNI through the whole relay, and the
// trusted proxies of the host-routed ingress.

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
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/proxyproto"
	"github.com/ankoehn/burrow/internal/server"
)

// The resolver and the access loader read a tcp service's own host name,
// so that the relay has it at registration and on every refresh.
func TestTCPHostname_ResolverAndLoaderReadIt(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	res := serviceResolverAdapter{db: x}
	r, err := res.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{Access: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if r.TCPHostname != "" || r.Slug == "" {
		t.Fatalf("new service: %+v", r)
	}
	if err := x.SetServiceTCPHostname(ctx, r.ServiceID, "rdp.example.com"); err != nil {
		t.Fatal(err)
	}
	again, err := res.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{})
	if err != nil || again.TCPHostname != "rdp.example.com" {
		t.Fatalf("resolved %+v %v", again, err)
	}
	acc, err := tcpAccessAdapter{db: x}.TCPAccess(ctx, r.ServiceID)
	if err != nil || acc.Hostname != "rdp.example.com" || acc.HostnameOnly {
		t.Fatalf("access %+v %v", acc, err)
	}
	// By host name only: both read it.
	if err := x.SetServiceTCPHostnameOnly(ctx, r.ServiceID, true); err != nil {
		t.Fatal(err)
	}
	if again, err := res.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{}); err != nil || !again.HostnameOnly {
		t.Fatalf("resolved %+v %v", again, err)
	}
	if acc, err := (tcpAccessAdapter{db: x}).TCPAccess(ctx, r.ServiceID); err != nil || !acc.HostnameOnly {
		t.Fatalf("access %+v %v", acc, err)
	}
}

// selfSignedTLS is a certificate for any name under example.com.
func selfSignedTLS(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "tls echo"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"*.example.com", "*.tcp.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tlsEcho is a local TLS server that greets with "echo\n" and echoes.
func tlsEcho(t *testing.T) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSignedTLS(t)}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte("echo\n"))
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// passVisit is a TLS client through the passthrough listener at addr with
// server name sni; before the hello it writes pre (a PROXY header).
func passVisit(addr, sni string, pre []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if len(pre) > 0 {
		if _, err := conn.Write(pre); err != nil {
			return err
		}
	}
	tc := tls.Client(conn, &tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // the echo's certificate is self-signed
	br := bufio.NewReader(tc)
	if line, err := br.ReadString('\n'); err != nil || line != "echo\n" {
		return errors.New("no greeting")
	}
	if _, err := tc.Write([]byte("ping")); err != nil {
		return err
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		return errors.New("no echo")
	}
	return nil
}

// The whole relay: a tcp service is reached by its slug name under
// BURROW_TCP_DOMAIN and, once it is set through the API, by its own host
// name; TLS runs from the visitor to the local TLS echo with nothing
// terminated in between. A trusted proxy's PROXY header names the visitor
// in the connection log. A name that is taken is refused, an unknown one
// closed.
func TestServe_TLSPassthrough(t *testing.T) {
	passAddr := freeAddr(t)
	r := bootObsRelayEnv(t, map[string]string{
		"BURROW_TLS_PASSTHROUGH_LISTEN": passAddr,
		"BURROW_TCP_DOMAIN":             "tcp.example.com",
		// The passthrough's own proxies; BURROW_TRUSTED_PROXIES (the
		// dashboard's) is not one of them.
		"BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES": "127.0.0.1",
	})
	target := tlsEcho(t)
	var token struct {
		Token string `json:"token"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "passthrough"}, http.StatusCreated, &token)
	stop := r.connectWith(token.Token,
		client.TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: target},
		client.TunnelSpec{Name: "other", Type: "tcp", LocalAddr: target, Access: "private"})
	defer stop()
	tns := r.tunnels()
	id, other := tns["rdp"].ServiceID, tns["other"].ServiceID

	type names struct {
		TCPHostname string   `json:"tcp_hostname"`
		Hostnames   []string `json:"hostnames"`
		Passthrough bool     `json:"passthrough"`
		Reachable   bool     `json:"reachable"`
	}
	var got names
	r.mustAdmin(http.MethodGet, "/api/v1/services/"+id+"/tcp-hostname", nil, http.StatusOK, &got)
	if len(got.Hostnames) != 1 || !strings.HasSuffix(got.Hostnames[0], ".tcp.example.com") || !got.Passthrough || !got.Reachable {
		t.Fatalf("GET %+v", got)
	}
	slugName := got.Hostnames[0]
	header := passProxyV2("198.51.100.7:4242", "10.0.0.2:443")
	if err := passVisit(passAddr, slugName, header); err != nil {
		t.Fatalf("by slug name: %v", err)
	}
	// From the trusted proxy every connection names its visitor.
	if err := passVisit(passAddr, slugName, nil); err == nil {
		t.Fatal("the trusted proxy without a header got through")
	}
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-hostname", map[string]string{"tcp_hostname": "RDP.example.com"}, http.StatusOK, &got)
	if got.TCPHostname != "rdp.example.com" || !slices.Equal(got.Hostnames, []string{slugName, "rdp.example.com"}) {
		t.Fatalf("PUT %+v", got)
	}
	if err := passVisit(passAddr, "rdp.example.com", header); err != nil {
		t.Fatalf("by host name behind the proxy: %v", err)
	}
	// The other service cannot take the name, and a private one is not
	// reachable by its name.
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+other+"/tcp-hostname", map[string]string{"tcp_hostname": "rdp.example.com"}, http.StatusConflict, nil)
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+other+"/tcp-hostname", map[string]string{"tcp_hostname": "vault.example.com"}, http.StatusOK, &got)
	if got.Reachable {
		t.Fatalf("private service reachable: %+v", got)
	}
	for _, sni := range []string{"vault.example.com", "nobody.example.com"} {
		if err := passVisit(passAddr, sni, header); err == nil {
			t.Fatalf("%s reached a service", sni)
		}
	}
	r.eventually("the row of the proxied visitor", func() bool {
		var rows []struct {
			SourceIP string `json:"source_ip"`
			SNI      string `json:"sni"`
			Protocol string `json:"protocol"`
		}
		r.mustAdmin(http.MethodGet, "/api/v1/connection-logs?kind=tcp_proxy&service_id="+id, nil, http.StatusOK, &rows)
		for _, row := range rows {
			if row.SNI == "rdp.example.com" && row.SourceIP == "198.51.100.7" && row.Protocol == "tls" {
				return true
			}
		}
		return false
	})
	// The write is audited.
	r.eventually("the audit row", func() bool {
		_, body := r.admin(http.MethodGet, "/api/v1/audit/events?action=service.tcp_hostname.update", nil)
		return strings.Count(string(body), "service.tcp_hostname.update") >= 2
	})

	// By host name only: the client registers again and has no public port;
	// the names still reach the service. The port on again brings it back.
	port := tns["rdp"].RemotePort
	if port == 0 {
		t.Fatal("rdp has no port to begin with")
	}
	var only struct {
		HostnameOnly bool `json:"hostname_only"`
	}
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-hostname", map[string]bool{"hostname_only": true}, http.StatusOK, &only)
	if !only.HostnameOnly {
		t.Fatalf("PUT hostname_only: %+v", only)
	}
	r.eventually("rdp served without a port", func() bool { tn, ok := r.tunnels()["rdp"]; return ok && tn.RemotePort == 0 })
	if err := passVisit(passAddr, "rdp.example.com", header); err != nil {
		t.Fatalf("by host name only: %v", err)
	}
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-hostname", map[string]bool{"hostname_only": false}, http.StatusOK, &only)
	r.eventually("rdp served with a port again", func() bool { tn, ok := r.tunnels()["rdp"]; return ok && tn.RemotePort != 0 })
	r.eventually("the hostname-only audit rows", func() bool {
		_, body := r.admin(http.MethodGet, "/api/v1/audit/events?action=service.tcp_hostname_only.update", nil)
		return strings.Count(string(body), "service.tcp_hostname_only.update") >= 2
	})
}

// passProxyV2 is a PROXY protocol v2 header from src to dst (IPv4).
func passProxyV2(src, dst string) []byte {
	s, d := netip.MustParseAddrPort(src), netip.MustParseAddrPort(dst)
	b := append([]byte{}, proxyproto.Signature[:]...)
	b = append(b, 0x21, 0x11, 0, 12)
	sa, da := s.Addr().As4(), d.Addr().As4()
	b = append(append(b, sa[:]...), da[:]...)
	b = binary.BigEndian.AppendUint16(b, s.Port())
	return binary.BigEndian.AppendUint16(b, d.Port())
}

// The host-routed ingress (BURROW_HTTP_PROXY_LISTEN) reads the visitor's
// address from X-Forwarded-For when its peer is a trusted proxy, as the
// dashboard's /svc/ path does: behind Traefik an ip rule judges the visitor,
// not Traefik. From any other peer the header is ignored.
func TestServe_IngressTrustsItsProxies(t *testing.T) {
	proxyAddr := freeAddr(t)
	r := bootObsRelayEnv(t, map[string]string{
		"BURROW_HTTP_PROXY_LISTEN": proxyAddr,
		"BURROW_TRUSTED_PROXIES":   "127.0.0.1",
	})
	app := newObsApp(t)
	var token struct {
		Token string `json:"token"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "ingress"}, http.StatusCreated, &token)
	stop := r.connectWith(token.Token, client.TunnelSpec{Name: "web", Type: "http", LocalAddr: app.addr()})
	defer stop()
	tn := r.tunnels()["web"]
	var svc struct {
		Slug string `json:"slug"`
	}
	r.mustAdmin(http.MethodGet, "/api/v1/services/"+tn.ServiceID, nil, http.StatusOK, &svc)
	if svc.Slug == "" {
		t.Fatalf("no slug: %+v", tn)
	}
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+tn.ServiceID+"/ip-geo", map[string]any{
		"enabled": true, "block_cidrs": []string{"198.51.100.0/24"},
	}, http.StatusNoContent, nil)
	get := func(xff string) int {
		req, err := http.NewRequest(http.MethodGet, "http://"+proxyAddr+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = svc.Slug + "." + obsAuthDomain
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(""); code != http.StatusOK {
		t.Fatalf("the proxy itself: %d", code)
	}
	if code := get("198.51.100.7"); code != http.StatusForbidden {
		t.Fatalf("a blocked visitor behind the proxy: %d, want 403", code)
	}
	if code := get("203.0.113.9"); code != http.StatusOK {
		t.Fatalf("another visitor behind the proxy: %d", code)
	}
}
