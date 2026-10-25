package grpce2e

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// env is everything the tests share: the binaries, the CA, the two relays
// and the local services behind the clients.
type env struct {
	dir     string
	burrowd string
	burrow  string
	ca      *testCA
	caPath  string

	// plain: dashboard listener without TLS, h2c on (as behind Traefik).
	plain *relay
	// tlsR: dashboard listener with TLS and the 443 mux on.
	tlsR *relay

	// grpcPlain is the local h2c gRPC server, grpcTLS the local gRPC server
	// over TLS (upstream h2), httpsAddr a local HTTP/1.1-over-TLS service.
	grpcPlain *echoBackend
	grpcTLS   *echoBackend
	httpsAddr string
	upstream  leaf // certificate of the local TLS services

	// proxyAddr is a TLS-terminating stand-in for Traefik in front of the
	// plain relay: h2 from the caller, h2c (scheme=h2c) to the relay.
	proxyAddr string
}

var e *env

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grpce2e setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (code int, err error) {
	// The run's directory sits in the repository's bin/ (gitignored) when it
	// can, so its logs outlive a container the tests ran in.
	base := ""
	if st, err := os.Stat("../../bin"); err == nil && st.IsDir() {
		base = "../../bin"
	}
	dir, err := os.MkdirTemp(base, "grpce2e-")
	if err != nil {
		return 0, err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return 0, err
	}
	e = &env{dir: dir}
	defer func() {
		if e.plain != nil {
			e.plain.stop()
		}
		if e.tlsR != nil {
			e.tlsR.stop()
		}
		if code != 0 || err != nil || os.Getenv("BURROW_E2E_KEEP") == "1" {
			fmt.Fprintln(os.Stderr, "grpce2e: logs kept in", dir)
			for _, r := range []*relay{e.plain, e.tlsR} {
				if r != nil && r.proc != nil {
					fmt.Fprintf(os.Stderr, "--- %s relay log (tail)\n%s\n", r.name, r.proc.tail(60))
				}
			}
			return
		}
		_ = os.RemoveAll(dir)
	}()

	if e.burrowd, e.burrow, err = binaries(dir); err != nil {
		return 0, err
	}
	if e.ca, err = newTestCA(); err != nil {
		return 0, err
	}
	e.caPath = filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(e.caPath, e.ca.certPEM, 0o600); err != nil {
		return 0, err
	}

	// Local services.
	if e.grpcPlain, err = startEchoBackend("h2c", nil); err != nil {
		return 0, err
	}
	defer e.grpcPlain.srv.Stop()
	if e.upstream, err = e.ca.issue("upstream", "upstream.e2e.test", "127.0.0.1"); err != nil {
		return 0, err
	}
	upCert, err := e.upstream.tlsCert()
	if err != nil {
		return 0, err
	}
	if e.grpcTLS, err = startEchoBackend("h2tls", &tls.Config{Certificates: []tls.Certificate{upCert}}); err != nil {
		return 0, err
	}
	defer e.grpcTLS.srv.Stop()
	var stopHTTPS func()
	if e.httpsAddr, stopHTTPS, err = startHTTPSBackend(upCert); err != nil {
		return 0, err
	}
	defer stopHTTPS()

	// Relay with a plain dashboard listener. BURROW_AUTH_DOMAIN makes its
	// own host known, which host dispatch of custom domains needs.
	if e.plain, err = startRelay(e, relayOpts{name: "plain", env: []string{
		"BURROW_AUTH_DOMAIN=dash.p.test",
		"BURROW_TRUSTED_PROXIES=127.0.0.1/32",
	}}); err != nil {
		return 0, err
	}
	// Relay that terminates TLS itself, with the SNI mux on its HTTPS port.
	dash, err := e.ca.issue("dashboard", "dash.t.test", "127.0.0.1")
	if err != nil {
		return 0, err
	}
	if e.tlsR, err = startRelay(e, relayOpts{name: "tls", dashCert: &dash, env: []string{
		"BURROW_TLS_PASSTHROUGH_ON_HTTPS=true",
		"BURROW_TCP_DOMAIN=tcp.t.test",
	}}); err != nil {
		return 0, err
	}

	stopProxy, err := startStandInProxy()
	if err != nil {
		return 0, err
	}
	defer stopProxy()

	return m.Run(), nil
}

// binaries returns burrowd and burrow: from BURROW_E2E_BURROWD and
// BURROW_E2E_BURROW, or built from the repository into dir.
func binaries(dir string) (string, string, error) {
	d, c := os.Getenv("BURROW_E2E_BURROWD"), os.Getenv("BURROW_E2E_BURROW")
	if d != "" && c != "" {
		return d, c, nil
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		return "", "", err
	}
	d, c = filepath.Join(dir, "burrowd"), filepath.Join(dir, "burrow")
	for out, pkg := range map[string]string{d: "./cmd/server", c: "./cmd/client"} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = root
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return "", "", fmt.Errorf("go build %s: %w", pkg, err)
		}
	}
	return d, c, nil
}

// startStandInProxy terminates TLS for the plain relay's custom domains and
// forwards over h2c, as a Traefik router with scheme=h2c does.
func startStandInProxy() (func(), error) {
	cert, err := e.ca.issue("proxy", "grpc.p.test", "127.0.0.1")
	if err != nil {
		return nil, err
	}
	tc, err := cert.tlsCert()
	if err != nil {
		return nil, err
	}
	target, _ := url.Parse("http://" + e.plain.http)
	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: &http.Transport{Protocols: &protos},
		// No FlushInterval -1: it flushes the headers on a timer goroutine,
		// which splits a trailers-only reply (Content-Length 0) into headers
		// and an empty body when it wins the race with the end of the
		// handler. Streams (no Content-Length) are flushed at once anyway.
	}
	// Listen first and serve that listener: no window in which another
	// process could take a port picked and released before.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var in http.Protocols
	in.SetHTTP1(true)
	in.SetHTTP2(true)
	srv := &http.Server{
		Handler:           rp,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{tc}},
		Protocols:         &in,
		ReadHeaderTimeout: 10 * time.Second,
	}
	e.proxyAddr = ln.Addr().String()
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	return func() { _ = srv.Close() }, nil
}
