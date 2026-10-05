package api

import (
	"net"
	"net/http"
	"time"

	"github.com/go-chi/httprate"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/version"
)

// DiscoveryRateLimitPerIP is the default maximum number of discovery requests
// per source IP per minute. A client asks once per `burrow login`, `status`
// or `doctor`; the limit only keeps the unauthenticated endpoint from being
// hammered. Per-IP accuracy comes from TrustedProxyMiddleware, as for login.
const DiscoveryRateLimitPerIP = 60

// ClientDiscovery is the response of GET /api/v1/client/discovery: what a
// client needs to know before it has a token. The endpoint is public, so the
// struct holds these four fields and nothing else — no commit or build date,
// no user data, no address other than the public control endpoint.
type ClientDiscovery struct {
	Control          string `json:"control"`
	Version          string `json:"version"`
	MinClientVersion string `json:"min_client_version"`
	ProtocolVersion  int    `json:"protocol_version"`
}

// controlEndpoint is the host:port a client connects to, as seen from the
// request: Deps.ControlListen when it names a host, otherwise the host the
// request was sent to with the control port.
//
// An empty ControlListen gives the request's Host as it is. That branch is
// for tests that build Deps without it: the relay's configuration always has
// a listen address (`listen` is required and defaults to ":7000"), and
// cmd/server passes it on. A port alone (":7000") and a bind-all address ("0.0.0.0:7000",
// "[::]:7000") take the host from the request: a bind address is not where a
// client connects.
func (d Deps) controlEndpoint(r *http.Request) string {
	listen := d.ControlListen
	if listen == "" {
		return r.Host
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsUnspecified() {
			return listen
		}
	}
	reqHost := r.Host
	if h, _, err := net.SplitHostPort(reqHost); err == nil {
		reqHost = h
	} else if n := len(reqHost); n > 2 && reqHost[0] == '[' && reqHost[n-1] == ']' {
		reqHost = reqHost[1 : n-1] // an IPv6 literal without a port
	}
	if reqHost == "" {
		return ":" + port
	}
	return net.JoinHostPort(reqHost, port)
}

// discoveryRateLimiter builds the per-IP limiter of the discovery route.
func (d Deps) discoveryRateLimiter() func(http.Handler) http.Handler {
	limit := d.DiscoveryRateLimitPerIPOverride
	if limit <= 0 {
		limit = DiscoveryRateLimitPerIP
	}
	return httprate.Limit(limit, time.Minute,
		httprate.WithKeyFuncs(httprate.KeyByIP),
		httprate.WithLimitHandler(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusTooManyRequests, "too many requests")
		}),
	)
}

// GetClientDiscovery tells a client where the control endpoint is and which
// version the relay runs. GET /api/v1/client/discovery, no authentication:
// the control endpoint is a public port, not a secret.
func (d Deps) GetClientDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, ClientDiscovery{
		Control:          d.controlEndpoint(r),
		Version:          version.Version,
		MinClientVersion: d.MinClientVersion,
		ProtocolVersion:  proto.ProtocolVersion,
	})
}
