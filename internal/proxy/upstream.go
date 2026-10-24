package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/ankoehn/burrow/internal/proto"
)

// The protocol the proxy speaks to the local service of an http tunnel
// (HTTP/2 and gRPC, H2–H4). Resolved.UpstreamProtocol names it; the proxy
// does the TLS and HTTP/2 framing over the tunnel stream, so the home client
// pipes bytes whatever the protocol, and an older client serves an h2c or h2
// service unchanged.
//
// Every request gets a transport of its own and every connection of that
// transport is a tunnel stream of its own, as for HTTP/1.1: one yamux stream
// per request or gRPC call. A pooled h2 connection per tunnel is out of scope
// (spec H2), so a settings change or a new tunnel applies from the next
// request on, and no transport outlives its request. What is kept per
// service is the TLS configuration built from its settings (the parsed CA),
// replaced when the tunnel or the settings change.

// ReasonUpstreamTLS is the connection-log reason of a request whose TLS
// handshake with the local service failed, ALPN included (H3).
const ReasonUpstreamTLS = "upstream_tls"

// errUpstreamTLS marks a dial failure in the TLS handshake with the local
// service: a bad certificate, a CA that does not parse, or no h2 by ALPN.
type errUpstreamTLS struct{ err error }

func (e *errUpstreamTLS) Error() string { return "upstream tls: " + e.err.Error() }
func (e *errUpstreamTLS) Unwrap() error { return e.err }

// IsUpstreamTLSError reports whether err, as the reverse proxy's error
// handler gets it, is a failed TLS handshake with the local service.
func IsUpstreamTLSError(err error) bool {
	var e *errUpstreamTLS
	return errors.As(err, &e)
}

// effectiveUpstream is res's upstream protocol, proto.UpstreamHTTP1 for none
// or for a value the proxy does not know.
func effectiveUpstream(res *Resolved) string {
	if proto.ValidUpstreamProtocol(res.UpstreamProtocol) {
		return res.UpstreamProtocol
	}
	return proto.UpstreamHTTP1
}

// isUpgrade reports whether r asks to switch protocols (a WebSocket): its
// Connection header has the token "upgrade" and it names what to upgrade to.
// Such a request always goes upstream as HTTP/1.1.
func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// upstreamTransport is the transport of one request to the local service.
type upstreamTransport struct {
	// rt carries the request; scheme is the scheme of its URL.
	rt     *http.Transport
	scheme string
	// protocol is what the request is sent as: res's protocol, or HTTP/1.1
	// (plain for h2c, TLS for h2) for an upgrade.
	protocol string
	mu       sync.Mutex
	conns    []net.Conn
	tracked  bool
}

// done releases the transport when the request is over: its idle
// connections are closed and, for TLS and HTTP/2, every tunnel stream it
// dialled. The reverse proxy returns only after the response, trailers
// included, was copied to the visitor and an upgraded connection was done,
// so nothing in use is closed. HTTP/2 needs the explicit close: its idle
// check may still count the stream that just ended, and a connection it
// skipped would hold the tunnel stream open for good.
func (u *upstreamTransport) done() {
	u.rt.CloseIdleConnections()
	if !u.tracked {
		return
	}
	u.mu.Lock()
	conns := u.conns
	u.conns = nil
	u.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (u *upstreamTransport) track(c net.Conn) {
	u.mu.Lock()
	u.conns = append(u.conns, c)
	u.mu.Unlock()
}

// newUpstreamTransport builds the transport of r to res's local service,
// whose streams dial opens. upstreamHost is the Host the request is sent
// with: the default TLS server name.
//
// http1 is the transport of the releases before, field for field. An
// upgrade goes as HTTP/1.1 on every protocol: plain for h2c, over TLS for h2.
func (p *Proxy) newUpstreamTransport(r *http.Request, res *Resolved, upstreamHost string,
	dial func(ctx context.Context) (net.Conn, error),
) *upstreamTransport {
	protocol := effectiveUpstream(res)
	if isUpgrade(r) {
		switch protocol {
		case proto.UpstreamH2C:
			protocol = proto.UpstreamHTTP1
		case proto.UpstreamH2:
			protocol = proto.UpstreamHTTPS
		}
	}
	u := &upstreamTransport{protocol: protocol, scheme: "http"}
	switch protocol {
	case proto.UpstreamH2C:
		var protos http.Protocols
		protos.SetUnencryptedHTTP2(true)
		u.tracked = true
		u.rt = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := dial(ctx)
				if err != nil {
					return nil, err
				}
				u.track(c)
				return c, nil
			},
			DisableCompression: true,
			Protocols:          &protos,
		}
	case proto.UpstreamHTTPS, proto.UpstreamH2:
		alpn := "http/1.1"
		var protos http.Protocols
		if protocol == proto.UpstreamH2 {
			alpn = "h2"
			protos.SetHTTP2(true)
		} else {
			protos.SetHTTP1(true)
		}
		u.scheme, u.tracked = "https", true
		u.rt = &http.Transport{
			DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := p.dialUpstreamTLS(ctx, res, upstreamHost, alpn, dial)
				if err != nil {
					return nil, err
				}
				u.track(c)
				return c, nil
			},
			DisableCompression: true,
			Protocols:          &protos,
		}
	default:
		u.rt = &http.Transport{
			// DialContext ignores the addr argument: we always connect to the
			// tunnel stream of the resolved service. The Transport calls this
			// for each new connection: once per proxied request, since this
			// transport serves one request and is not used again.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx)
			},
			DisableCompression: true,
			// HTTP/1.1 to the upstream: the protocol is http1.
			ForceAttemptHTTP2: false,
		}
	}
	return u
}

// dialUpstreamTLS opens a tunnel stream and runs the TLS handshake with the
// local service over it, offering alpn only. For h2 the service must choose
// h2; for http/1.1 it may also choose nothing. A failed handshake is an
// errUpstreamTLS; a stream that cannot be opened is dial's own error.
func (p *Proxy) dialUpstreamTLS(ctx context.Context, res *Resolved, upstreamHost, alpn string,
	dial func(ctx context.Context) (net.Conn, error),
) (*tls.Conn, error) {
	base, err := p.upstreamTLS.config(res, upstreamHost)
	if err != nil {
		return nil, &errUpstreamTLS{err}
	}
	raw, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	cfg := base.Clone()
	cfg.NextProtos = []string{alpn}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, &errUpstreamTLS{err}
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got != alpn && (alpn == "h2" || got != "") {
		_ = tc.Close()
		return nil, &errUpstreamTLS{errors.New("the local service did not negotiate " + alpn + " by ALPN (got " + strconv.Quote(got) + ")")}
	}
	return tc, nil
}

// upstreamTLSServerName is the TLS server name for res: its
// UpstreamTLSServerName, else the host of upstreamHost (the service's local
// host header). An IP is verified as the certificate's IP and sent as no SNI.
func upstreamTLSServerName(res *Resolved, upstreamHost string) string {
	if res.UpstreamTLSServerName != "" {
		return res.UpstreamTLSServerName
	}
	host := upstreamHost
	if h, _, err := net.SplitHostPort(upstreamHost); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
}

// buildUpstreamTLS is the TLS configuration of a service's settings (H3):
//   - a CA: verification on, against that CA only;
//   - no CA and UpstreamTLSVerify: verification on, against the system roots;
//   - neither (the default): no verification. Local services mostly have
//     self-signed certificates, and the hop already runs inside the tunnel's
//     TLS.
func buildUpstreamTLS(res *Resolved, upstreamHost string) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: upstreamTLSServerName(res, upstreamHost)}
	switch {
	case len(res.UpstreamTLSCAPEM) > 0:
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(res.UpstreamTLSCAPEM) {
			return nil, errors.New("the service's upstream CA holds no PEM certificate")
		}
		cfg.RootCAs = pool
	case !res.UpstreamTLSVerify:
		cfg.InsecureSkipVerify = true //nolint:gosec // H3: opt-in verification, the default trusts the local service
	}
	return cfg, nil
}

// upstreamTLSCache keeps each service's TLS configuration as long as its
// tunnel and settings are the same, so a CA is parsed once and not for every
// request or gRPC call.
type upstreamTLSCache struct {
	mu sync.Mutex
	m  map[string]upstreamTLSEntry // by service id
}

type upstreamTLSEntry struct {
	key string
	cfg *tls.Config
}

// maxUpstreamTLSEntries bounds the cache; past it, it starts over.
const maxUpstreamTLSEntries = 4096

func upstreamTLSKey(res *Resolved, upstreamHost string) string {
	return strings.Join([]string{res.TunnelID, upstreamHost, res.UpstreamTLSServerName,
		strconv.FormatBool(res.UpstreamTLSVerify), string(res.UpstreamTLSCAPEM)}, "\x00")
}

// config is the TLS configuration for res, built anew when the tunnel or the
// settings changed. The caller clones it before use.
func (c *upstreamTLSCache) config(res *Resolved, upstreamHost string) (*tls.Config, error) {
	key := upstreamTLSKey(res, upstreamHost)
	c.mu.Lock()
	e, ok := c.m[res.ServiceID]
	c.mu.Unlock()
	if ok && e.key == key {
		return e.cfg, nil
	}
	cfg, err := buildUpstreamTLS(res, upstreamHost)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= maxUpstreamTLSEntries {
		c.m = make(map[string]upstreamTLSEntry)
	}
	c.m[res.ServiceID] = upstreamTLSEntry{key: key, cfg: cfg}
	c.mu.Unlock()
	return cfg, nil
}
