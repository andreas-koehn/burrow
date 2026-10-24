package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
)

// The whole relay, a plain dashboard listener as behind Traefik (H10): an
// auto custom domain needs no certificate (not_needed), a request whose
// Host is that name reaches the service through the dashboard listener,
// the dashboard's own host keeps the dashboard, and after the delete the
// name is the dashboard's again.
func TestServe_CustomDomainOnTheDashboardListener(t *testing.T) {
	r := bootObsRelayEnv(t, nil)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "from-the-app")
	}))
	defer app.Close()

	var token struct {
		Token string `json:"token"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "cd-client"}, http.StatusCreated, &token)
	stop := r.connectWith(token.Token, client.TunnelSpec{Name: "cd-web", Type: "http", LocalAddr: app.Listener.Addr().String()})
	defer stop()
	sid := r.tunnels()["cd-web"].ServiceID

	var created struct {
		ID         string `json:"id"`
		CertSource string `json:"cert_source"`
		CertStatus string `json:"cert_status"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/services/"+sid+"/domains",
		map[string]string{"hostname": "grpc.customer.test", "cert_source": "auto"}, http.StatusCreated, &created)
	if created.CertSource != "auto" || created.CertStatus != "not_needed" {
		t.Fatalf("created %+v", created)
	}

	get := func(host string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, r.base+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("grpc.customer.test"); code != http.StatusOK || body != "from-the-app" {
		t.Fatalf("custom domain: %d %q", code, body)
	}
	if code, body := get("GRPC.customer.test.:8080"); code != http.StatusOK || body != "from-the-app" {
		t.Fatalf("custom domain, port and case: %d %q", code, body)
	}
	if _, body := get(obsAuthDomain); strings.Contains(body, "from-the-app") {
		t.Fatal("the dashboard's host reached the service")
	}

	r.mustAdmin(http.MethodDelete, "/api/v1/services/"+sid+"/domains/"+created.ID, nil, http.StatusNoContent, nil)
	if _, body := get("grpc.customer.test"); strings.Contains(body, "from-the-app") {
		t.Fatal("a deleted custom domain still reaches the service")
	}
}

// The dashboard listener with a file certificate (H10): an uploaded custom
// domain's certificate answers its name there and the request reaches the
// service over HTTP/2; the dashboard keeps its own certificate; auto is
// refused, since nobody could serve it.
func TestServe_CustomDomainOnTheTLSDashboard(t *testing.T) {
	r := bootObsRelayEnv(t, map[string]string{"BURROW_HTTP_TLS_CERT": obsDevCert})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "from-the-app")
	}))
	defer app.Close()
	var token struct {
		Token string `json:"token"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "cd-client"}, http.StatusCreated, &token)
	stop := r.connectWith(token.Token, client.TunnelSpec{Name: "cd-web", Type: "http", LocalAddr: app.Listener.Addr().String()})
	defer stop()
	sid := r.tunnels()["cd-web"].ServiceID

	if code, body := r.admin(http.MethodPost, "/api/v1/services/"+sid+"/domains",
		map[string]string{"hostname": "grpc.customer.test", "cert_source": "auto"}); code != http.StatusBadRequest || !strings.Contains(string(body), "ACME") {
		t.Fatalf("auto with a file certificate: %d %s", code, body)
	}
	var created struct {
		CertStatus string `json:"cert_status"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/services/"+sid+"/domains", map[string]string{
		"hostname": obsCustomDomain, "cert_pem": string(r.certPEM), "key_pem": string(r.keyPEM),
	}, http.StatusCreated, &created)
	if created.CertStatus != "ready" {
		t.Fatalf("created %+v", created)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(r.certPEM)
	addr := strings.TrimPrefix(r.base, "https://")
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: obsCustomDomain, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+net.JoinHostPort(obsCustomDomain, portOf(t, addr))+"/", nil)
	hc.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("custom domain over TLS: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "from-the-app" || resp.ProtoMajor != 2 {
		t.Fatalf("custom domain over TLS: %d %s %q", resp.StatusCode, resp.Proto, b)
	}
	if got := resp.TLS.PeerCertificates[0].DNSNames; len(got) != 1 || got[0] != obsCustomDomain {
		t.Fatalf("served certificate for %v", got)
	}
	// The dashboard's own name keeps the relay's certificate (r.hc trusts
	// the dev CA only).
	if code, _ := r.admin(http.MethodGet, "/api/v1/me", nil); code != http.StatusOK {
		t.Fatalf("dashboard: %d", code)
	}
}

func portOf(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}
