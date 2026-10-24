package main

import (
	"crypto/tls"
	"net/http"
	"slices"
	"time"
)

// Ingress HTTP/2 settings (spec H1). Every public HTTP listener states its
// protocols explicitly instead of relying on the stdlib defaults.
const (
	ingressIdleTimeout          = 120 * time.Second
	ingressMaxConcurrentStreams = 250
	ingressH2SendPingTimeout    = 60 * time.Second
	ingressH2PingTimeout        = 15 * time.Second
)

// applyIngressProtocols sets Protocols, HTTP2 and IdleTimeout on srv.
//
//   - tlsListener: HTTP/1.1 and HTTP/2 (ALPN h2).
//   - plain listener: HTTP/1.1, plus prior-knowledge h2c when h2c is true. The
//     "Upgrade: h2c" flavour is not enabled by this; the proxy refuses it
//     with 505.
//
// With no active stream an h2 connection closes after IdleTimeout; a dead
// peer is found by the ping.
func applyIngressProtocols(srv *http.Server, tlsListener, h2c bool) {
	var p http.Protocols
	p.SetHTTP1(true)
	if tlsListener {
		p.SetHTTP2(true)
	} else if h2c {
		p.SetUnencryptedHTTP2(true)
	}
	srv.Protocols = &p
	srv.HTTP2 = &http.HTTP2Config{
		MaxConcurrentStreams: ingressMaxConcurrentStreams,
		SendPingTimeout:      ingressH2SendPingTimeout,
		PingTimeout:          ingressH2PingTimeout,
	}
	srv.IdleTimeout = ingressIdleTimeout
}

// ensureIngressNextProtos makes cfg.NextProtos start with "h2", "http/1.1"
// and keeps every other entry, such as acme-tls/1, after them. The per-vhost
// mTLS configs are clones of cfg, so they negotiate h2 because of this list
// and not because the HTTP server mutated a shared config.
func ensureIngressNextProtos(cfg *tls.Config) {
	out := []string{"h2", "http/1.1"}
	for _, p := range cfg.NextProtos {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	cfg.NextProtos = out
}
