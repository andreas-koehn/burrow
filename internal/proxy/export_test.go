package proxy

import "crypto/tls"

// SetTLSBaseForTest exposes the unexported tlsBase field so the external
// proxy_test package (which uses httptest.StartTLS and only gets a populated
// *tls.Config after Start) can register the listener's base config on the
// proxy. Production code wires this via WithTLSBase at construction time.
func SetTLSBaseForTest(p *Proxy, base *tls.Config) { p.tlsBase = base }

// RewriteLocationPrefix exposes the unexported rewriteLocationPrefix helper
// to the external proxy_test package for TDD unit testing.
var RewriteLocationPrefix = rewriteLocationPrefix
