package proxy

import (
	"crypto/tls"
	"time"
)

// SetTLSBaseForTest exposes the unexported tlsBase field so the external
// proxy_test package (which uses httptest.StartTLS and only gets a populated
// *tls.Config after Start) can register the listener's base config on the
// proxy. Production code wires this via WithTLSBase at construction time.
func SetTLSBaseForTest(p *Proxy, base *tls.Config) { p.tlsBase = base }

// RewriteLocationPrefix exposes the unexported rewriteLocationPrefix helper
// to the external proxy_test package for TDD unit testing.
var RewriteLocationPrefix = rewriteLocationPrefix

// SetUpstreamTLSHandshakeTimeoutForTest sets the bound on the TLS handshake
// with a local service; the returned func restores it.
func SetUpstreamTLSHandshakeTimeoutForTest(d time.Duration) func() {
	old := upstreamTLSHandshakeTimeout
	upstreamTLSHandshakeTimeout = d
	return func() { upstreamTLSHandshakeTimeout = old }
}

// SetHeldRefusalTimeoutForTest sets how long a refusal the upstream writes
// is held for its body; the returned func restores it.
func SetHeldRefusalTimeoutForTest(d time.Duration) func() {
	old := heldRefusalTimeout
	heldRefusalTimeout = d
	return func() { heldRefusalTimeout = old }
}
