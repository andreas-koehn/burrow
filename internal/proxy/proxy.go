package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/pkg/clientip"
)

// AccessChecker enforces the service's access-mode policy before the proxy
// opens a tunnel stream. The concrete implementation is provided by Task 8;
// this task defines the interface and uses a permissive stub in tests.
//
// Return values:
//   - ok=true: allow the request to proceed.
//   - ok=false: write status + body (plain text unless hdr overrides Content-Type)
//   - optional extra headers from hdr, then return without proxying.
type AccessChecker interface {
	Allow(ctx context.Context, res *Resolved, r *http.Request) (ok bool, status int, body string, hdr http.Header)
}

// AIChain is the v0.4.0 middleware chain (internal/aigw.Chain) the proxy
// dispatches into between Access.Allow and ReverseProxy.ServeHTTP, but ONLY
// for services with a non-default service_ai_config blob. When AIChain is
// nil (the default, and the v0.3.0 build), the proxy serves every request
// through the v0.3.0 pass-through path.
//
// Interface — rather than a direct dependency on *aigw.Chain — to keep the
// proxy → aigw direction one-way (aigw never imports proxy). The proxy
// hands the chain its own Resolved metadata plus the downstream
// ReverseProxy handler; the chain calls back into the proxy via that
// handler when it decides to forward upstream.
//
// The chain itself is responsible for resolving + decoding the AI config
// blob (so the proxy never needs to depend on JSON shape). A nil-zero
// config means: pass through to proxyHandler unchanged.
type AIChain interface {
	// Dispatch runs the chain for one request. serviceID is the resolved
	// service identity; localHost / apiKeyHeader come from the Resolved
	// metadata. apiKeyID is the matched service API key ("" outside api_key
	// mode). proxyHandler is the v0.3.0 ReverseProxy handler the chain
	// delegates to on a cache MISS / no short-circuit. The chain MUST
	// either short-circuit (writing status + body to w) OR forward via
	// proxyHandler — never both.
	Dispatch(w http.ResponseWriter, r *http.Request,
		serviceID, localHost, apiKeyHeader, apiKeyID string,
		proxyHandler http.Handler)
}

// Proxy is the vhost reverse proxy. It satisfies http.Handler and must be
// registered on the ingress listener (the TLS front-door, not the API port).
type Proxy struct {
	dialer         StreamDialer
	checker        AccessChecker
	authDomain     string
	gate           http.Handler // /__burrow/* gate; nil → 404 until Task 9
	log            *slog.Logger
	trustedProxies []*net.IPNet
	ingressPort    string // optional; included in X-Forwarded-Port when non-empty

	// v0.4.0 AI middleware chain; nil → v0.3.0 pass-through for every request.
	// Wired via WithAIChain. The chain itself resolves whether a given
	// service has an AI config blob — proxy never decodes JSON.
	aiChain AIChain

	// v0.4.0 Task 16: base TLS config the SNI router clones into a per-vhost
	// config when an mtls service matches. cmd/server sets this to the
	// listener's own *tls.Config (with the server certificates). When nil,
	// GetConfigForClient still works — it constructs a minimal config that
	// requires + verifies the client cert, and the standard library uses
	// the listener's own certificates for the server side.
	tlsBase *tls.Config

	// v0.5.0 Task 7: custom-domain host routing. When non-nil, ServeHTTP
	// falls back to this lookup when the incoming Host header does not match
	// the subdomain pattern. The function returns the serviceID whose backend
	// should receive the request, or ok=false when no custom domain matches.
	// cmd/server wires this via WithCustomDomainLookup against
	// v05.CustomDomainStore.LookupBySNI (F-13 closure, v0.5.0 integration).
	customDomainLookup func(ctx context.Context, host string) (serviceID string, ok bool, err error)

	// v0.5.0 Task 8: per-connection log sink. When non-nil, the proxy calls
	// connLogSink.Record on each request close with kind=http_proxy. The
	// conn-level byte accounting and deferred Record call are implemented in
	// serveResolved / serveCustomDomain.
	//
	// ConnLogSink is an interface alias to avoid importing internal/connlog
	// from the proxy package (which would add connlog to the data-plane import
	// graph). The concrete *connlog.SQLSink satisfies it.
	connLogSink ConnLogSink

	// v0.5.1 P2.3: per-request idle timeout. When > 0, serveResolved and
	// serveCustomDomain wrap the inbound request context with a
	// context.WithTimeout of this duration before forwarding to the upstream.
	// If the upstream does not send response headers within this window, the
	// context fires → ErrorHandler receives context.DeadlineExceeded →
	// logStatus is set to "closed_idle".
	//
	// Production default: 0 (no timeout). Tests use 2 s via WithProxyIdleTimeout.
	// The zero value preserves all pre-P2.3 behaviour byte-for-byte.
	idleTimeout time.Duration

	// auditLogger is the optional audit.Logger used to emit
	// audit.ActionAIUpstreamError rows when an AI-gateway upstream returns
	// a 5xx or fails entirely. Nil means audit emission is disabled (tests
	// and pre-v0.4 builds). Wired via WithAuditLogger in cmd/server/main.go.
	auditLogger *audit.Logger

	// summaries is told of every request that was answered for an http
	// service. nil = nobody is told. Wired via WithSummarySink.
	summaries SummarySink
}

// SummarySink receives one summary for each request the proxy has answered for
// an http service, when the response is complete (or the connection was taken
// over). The summary names the tunnel that was resolved for the request.
//
// Both methods are called on the goroutine of the request and must not block.
type SummarySink interface {
	// SummaryWanted reports whether anybody reads the summaries of requests
	// served by this tunnel. The proxy asks before it builds one, so a
	// service whose client did not ask costs a request this one call.
	SummaryWanted(tunnelID string) bool
	RequestSummary(serviceID string, s proto.RequestSummary)
}

// WithSummarySink registers the receiver of request summaries.
func WithSummarySink(s SummarySink) Option {
	return func(p *Proxy) { p.summaries = s }
}

// ConnLogSink is the interface the proxy uses to record a per-request
// connection log entry. *connlog.SQLSink satisfies it. Declared here (not in
// internal/connlog) to keep the proxy package free of the connlog import on
// the data-plane hot path — the concrete type is injected by cmd/server via
// WithConnLogSink.
//
// closed_error / closed_idle status detection landed in v0.5.1 P2.3.
// byte-counting transport wrapper + bytes_in / bytes_out accounting are
// active since v0.5.0 Task 8 (sink field declared and wired here).
type ConnLogSink interface {
	Record(ctx context.Context, e ConnLogEntry) error
}

// Connection-log status values the proxy emits. Mirrors connlog.Status
// byte-for-byte but kept here as plain string constants so the proxy stays
// import-free of connlog. The adapter in cmd/server converts these into the
// typed connlog.Status equivalents.
//
// Precedence (highest wins):
//
//	closed_error > closed_idle > closed_clean
//
// "closed_error" wins even if the context deadline also fired, because the
// upstream actually flushed a response (even if it was a 5xx).  If the
// deadline fires before any response is written, "closed_idle" is recorded.
// The early access-deny path always sets "rejected" before any upstream
// contact occurs — it is never overridden.
const (
	StatusClosedClean = "closed_clean"
	StatusClosedError = "closed_error"
	StatusClosedIdle  = "closed_idle"
	StatusRejected    = "rejected"
)

// ConnLogEntry is the subset of connlog.Entry the proxy needs to record an
// http_proxy connection. cmd/server passes a thin adapter that converts this
// into a connlog.Entry before calling connlog.SQLSink.Record. The adapter
// lives in cmd/server so the proxy package stays import-free of connlog.
type ConnLogEntry struct {
	Kind            string // "http_proxy" | "tcp_proxy"
	ServiceID       string
	TunnelID        string
	UserID          string
	ClientSessionID string
	SourceIP        string
	UserAgent       string
	StartedAt       time.Time
	EndedAt         time.Time
	BytesIn         int64
	BytesOut        int64
	Status          string // "closed_clean" | "closed_error" | "closed_idle" | "rejected"
	Reason          string
}

// WithConnLogSink registers the v0.5.0 connection-log sink. When non-nil,
// the proxy will call sink.Record on each closed request.
func WithConnLogSink(sink ConnLogSink) Option {
	return func(p *Proxy) { p.connLogSink = sink }
}

// WithProxyIdleTimeout sets a per-request idle timeout (v0.5.1 P2.3). When
// d > 0, each inbound request context is wrapped with context.WithTimeout(d)
// before being forwarded upstream. If the upstream does not respond within d,
// the context deadline fires and the connection log records "closed_idle".
//
// Production callers should leave this at 0 (no timeout) unless they
// intentionally want to enforce a maximum upstream response-header latency.
// Tests set 2 s so the idle-timeout seam test completes quickly.
func WithProxyIdleTimeout(d time.Duration) Option {
	return func(p *Proxy) { p.idleTimeout = d }
}

// WithAuditLogger registers the audit.Logger used to record
// audit.ActionAIUpstreamError events when an AI-gateway upstream fails
// (5xx response or connection error). When nil (the default), no audit
// row is written — pre-v0.4 builds and tests are unaffected.
func WithAuditLogger(l *audit.Logger) Option {
	return func(p *Proxy) { p.auditLogger = l }
}

// New constructs a Proxy.
//
//   - d:          StreamDialer used to look up and connect to tunnels.
//   - ac:         AccessChecker enforcing per-service access mode policy.
//   - authDomain: base domain, e.g. "tunnels.example.com". Requests with host
//     exactly equal to authDomain and path starting "/__burrow/" are routed to
//     the gate handler.
//   - log:        structured logger; must not be nil.
//
// Optional configuration is set via functional options; see WithGate,
// WithTrustedProxies, WithIngressPort.
func New(d StreamDialer, ac AccessChecker, authDomain string, log *slog.Logger, opts ...Option) *Proxy {
	p := &Proxy{
		dialer:     d,
		checker:    ac,
		authDomain: authDomain,
		log:        log,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Option is a functional option for Proxy.
type Option func(*Proxy)

// WithGate registers the gate handler that serves /__burrow/* on the auth
// domain. Task 9 calls this when wiring the real gate. When nil (default) the
// proxy responds 404 for those paths.
func WithGate(h http.Handler) Option {
	return func(p *Proxy) { p.gate = h }
}

// WithTrustedProxies sets the CIDRs whose X-Forwarded-For headers are honored
// when building the authoritative X-Forwarded-For header sent upstream.
// When empty (the default), the raw TCP peer address is always used.
func WithTrustedProxies(cidrs []*net.IPNet) Option {
	return func(p *Proxy) { p.trustedProxies = cidrs }
}

// WithIngressPort sets the port included in the X-Forwarded-Port header sent
// upstream. When empty (the default) the header is omitted.
func WithIngressPort(port string) Option {
	return func(p *Proxy) { p.ingressPort = port }
}

// WithCustomDomainLookup registers the v0.5.0 custom-domain routing hook.
// When non-nil, ServeHTTP falls back to fn when the Host header does not match
// the subdomain pattern (i.e. the host doesn't end with ".<authDomain>").
// fn(ctx, host) returns the serviceID that owns the custom domain, or ok=false
// when no mapping exists. Callers that do not use custom domains (v0.4.0 and
// earlier) should leave this nil — existing ServeHTTP behaviour is unchanged.
func WithCustomDomainLookup(fn func(ctx context.Context, host string) (serviceID string, ok bool, err error)) Option {
	return func(p *Proxy) { p.customDomainLookup = fn }
}

// WithAIChain registers the v0.4.0 middleware chain. When set, the proxy
// dispatches every successful request (post-access-check) through the chain
// before invoking the ReverseProxy. The chain itself decides whether a
// service has an AI config and, if not, calls back into the supplied
// proxyHandler unchanged — byte-for-byte preserving v0.3.0 behaviour.
//
// When nil (the default), the proxy serves every request via the v0.3.0
// pass-through path with no chain overhead.
func WithAIChain(c AIChain) Option {
	return func(p *Proxy) { p.aiChain = c }
}

// WithTLSBase registers the listener's base *tls.Config so the per-vhost
// GetConfigForClient hook can clone it (preserving Certificates / NextProtos
// / GetCertificate / etc.) before overlaying mtls-specific ClientCAs +
// ClientAuth. cmd/server passes its own *tls.Config here; tests using
// httptest.NewUnstartedServer can set this via their own server.TLS.
//
// When nil (the default), GetConfigForClient constructs a minimal config
// from scratch — the stdlib then falls back to the listener's own
// Certificates for the server side, which works when the listener itself
// has Certificates set (the common case).
func WithTLSBase(c *tls.Config) Option {
	return func(p *Proxy) { p.tlsBase = c }
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip optional port from Host.
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// h2c upgrade: not supported.
	// Respond 505 HTTP Version Not Supported. h2c (cleartext HTTP/2 upgrade)
	// is out of scope for Burrow's ingress (see package-level doc).
	if strings.EqualFold(r.Header.Get("Upgrade"), "h2c") {
		http.Error(w, "h2c not supported", http.StatusHTTPVersionNotSupported)
		return
	}

	// Requests to the auth domain itself under /__burrow/* go to the gate.
	if host == p.authDomain && strings.HasPrefix(r.URL.Path, "/__burrow/") {
		if p.gate != nil {
			p.gate.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
		return
	}

	// Expect <label>.<authDomain>.
	suffix := "." + p.authDomain
	if !strings.HasSuffix(host, suffix) {
		// v0.5.0 Task 7: custom-domain fallback. If a CustomDomainLookup is
		// registered, check whether this host is a bound custom domain.
		if p.customDomainLookup != nil {
			ctx := r.Context()
			serviceID, ok, err := p.customDomainLookup(ctx, host)
			if err != nil {
				p.log.Warn("proxy custom domain lookup error", "host", host, "err", err)
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				return
			}
			if ok {
				p.serveCustomDomain(w, r, host, serviceID)
				return
			}
		}
		p.notFound(w)
		return
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		// Multi-level subdomain or empty label: not a direct tunnel label.
		p.notFound(w)
		return
	}

	ctx := r.Context()

	// Step 1: look up service metadata (no stream opened yet).
	res, err := p.dialer.Lookup(ctx, label)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			p.notFound(w)
			return
		}
		p.log.Warn("proxy lookup error", "subdomain", label, "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}

	p.serveResolved(w, r, res, label, suffix)
}

// serveCustomDomain handles a request whose Host matches a registered custom
// domain (v0.5.0 Task 7). It looks up the service by serviceID, runs the
// access checker, and proxies to the tunnel upstream — identical to the
// subdomain path but using the service-ID-based dialer methods.
func (p *Proxy) serveCustomDomain(w http.ResponseWriter, r *http.Request, host, serviceID string) {
	ctx := r.Context()

	res, err := p.dialer.LookupByServiceID(ctx, serviceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			p.notFound(w)
			return
		}
		p.log.Warn("proxy custom domain upstream lookup error", "host", host, "service_id", serviceID, "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}

	// v0.5.0 F-13 connection-log accounting. Wrap before any other work so
	// the body/byte counters cover the full request. The recordOnClose
	// deferral fires on every return below (including the access-deny
	// early return) with logStatus mutated to match the terminal state.
	started := time.Now()
	rc := newCountingBody(r.Body)
	r.Body = rc
	ww := newCountingResponseWriter(w)
	w = ww

	resolvedClientIP := clientip.Resolve(
		r.RemoteAddr,
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-IP"),
		p.trustedProxies,
	)

	logStatus := StatusClosedClean
	defer p.recordOnClose(r, res, resolvedClientIP, started, rc, ww, &logStatus)

	// Step 1b: enforce ip-geo block/allow policy BEFORE access-mode check.
	if denied, reason := p.ipGeoDenied(res, resolvedClientIP); denied {
		p.log.Info("ip-geo deny", "service_id", res.ServiceID, "ip", resolvedClientIP, "reason", reason)
		logStatus = StatusRejected
		http.Error(w, "forbidden: ip not allowed", http.StatusForbidden)
		return
	}

	// Step 2: enforce access mode BEFORE opening a stream.
	ok, status, body, hdr := p.checker.Allow(ctx, res, r)
	if !ok {
		logStatus = StatusRejected
		for k, vs := range hdr {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
		return
	}

	// v0.5.1 P2.3: wrap context with per-request idle timeout when configured.
	// The cancel is called at function return (deferred); the deferred
	// recordOnClose runs first (defer is LIFO) so it sees the final logStatus
	// value before the context is released.
	if p.idleTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.idleTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	upstreamHost := res.LocalHost
	if upstreamHost == "" {
		upstreamHost = host
	}

	ingressPort := p.ingressPort
	capturedServiceID := serviceID

	// The transport serves this one request. When the request is done, its
	// connection — the tunnel stream — would wait in the transport's idle
	// pool for a second request that never comes, and stay open until the
	// local app closes it: a stream and two goroutines here, a goroutine and
	// a connection to the app on the client, for every request answered.
	// Closing the idle connections when the handler returns ends that. It
	// closes nothing that is in use: the reverse proxy returns only after
	// the response body was copied to the visitor, and a connection that was
	// upgraded (a WebSocket) has left the pool and is closed by the reverse
	// proxy when both ends are done. A connection that is handed back to the
	// pool a moment after this call is closed as well: from the call on, the
	// transport closes a connection that becomes idle instead of keeping it
	// until the next request, which never comes. Keep-alives stay on, so the
	// request the app sees carries no "Connection: close".
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.dialer.DialTunnelStreamByServiceID(ctx, capturedServiceID)
		},
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	defer tr.CloseIdleConnections()

	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			stripBurrowCookies(pr.Out)
			pr.Out.Header.Del("X-Burrow-Path-Prefix")
			pr.Out.Header.Del("X-Forwarded-Port")
			pr.Out.Header.Set("X-Forwarded-For", resolvedClientIP)
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", host)
			if ingressPort != "" {
				pr.Out.Header.Set("X-Forwarded-Port", ingressPort)
			}
			pr.Out.URL = &url.URL{
				Scheme:   "http",
				Host:     upstreamHost,
				Path:     pr.In.URL.Path,
				RawQuery: pr.In.URL.RawQuery,
			}
			pr.Out.Host = upstreamHost
		},
		Transport: tr,
		// v0.5.1 P2.3: status mapping in ErrorHandler.
		//
		// Precedence: closed_error > closed_idle > closed_clean.
		// - context.DeadlineExceeded (idle timeout fired) → closed_idle.
		// - Any other upstream error → closed_error.
		// The post-ServeHTTP 5xx check below also promotes closed_clean →
		// closed_error when the upstream successfully responded but with a
		// ≥500 status code (ErrorHandler is NOT called in that case).
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn("proxy custom domain upstream error", "host", host, "err", err)
			if errors.Is(err, context.DeadlineExceeded) {
				logStatus = StatusClosedIdle
			} else {
				logStatus = StatusClosedError
				// Emit audit row for AI-gateway upstream connection failures.
				p.emitAIUpstreamErrorAudit(r.Context(), res.ServiceID, res.APIKeyHeader, upstreamHost, http.StatusBadGateway)
			}
			if errors.Is(err, ErrNotFound) {
				p.notFound(w)
				return
			}
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			dropBurrowSetCookies(resp.Header)
			return nil
		},
	}

	p.log.Debug("proxy custom domain request", "host", host, "service_id", serviceID, "method", r.Method, "path", r.URL.Path)

	if p.aiChain != nil {
		p.aiChain.Dispatch(w, r, res.ServiceID, res.LocalHost, res.APIKeyHeader, res.APIKeyID, rp)
		// Post-dispatch 5xx promotion: if the upstream responded with ≥500 and
		// the ErrorHandler was NOT called, promote closed_clean → closed_error.
		// closed_error and closed_idle are never downgraded (precedence rule).
		if logStatus == StatusClosedClean && ww.statusCode >= 500 {
			logStatus = StatusClosedError
			// Emit audit row for AI-gateway upstream 5xx responses.
			p.emitAIUpstreamErrorAudit(ctx, res.ServiceID, res.APIKeyHeader, upstreamHost, ww.statusCode)
		}
		return
	}
	rp.ServeHTTP(w, r)
	// Post-ServeHTTP 5xx promotion (same rule as above).
	if logStatus == StatusClosedClean && ww.statusCode >= 500 {
		logStatus = StatusClosedError
		// Emit audit row for AI-gateway upstream 5xx responses (non-chain path).
		p.emitAIUpstreamErrorAudit(ctx, res.ServiceID, res.APIKeyHeader, upstreamHost, ww.statusCode)
	}
}

// serveResolved handles a request after subdomain-based dialer.Lookup has
// succeeded. Shared between the normal subdomain path and future extensions.
func (p *Proxy) serveResolved(w http.ResponseWriter, r *http.Request, res *Resolved, label, suffix string) {
	ctx := r.Context()

	// v0.5.0 F-13 connection-log accounting. Wrap before any other work so
	// the body/byte counters see the full request even if downstream code
	// reads r.Body via the writer rebinding.
	started := time.Now()
	rc := newCountingBody(r.Body)
	r.Body = rc
	ww := newCountingResponseWriter(w)
	w = ww

	resolvedClientIP := clientip.Resolve(
		r.RemoteAddr,
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-IP"),
		p.trustedProxies,
	)

	// Defer the connection-log record. logStatus is mutated below before any
	// return so the deferred call sees the correct terminal status. Detached
	// context (context.WithoutCancel via SQLSink.Record's own machinery)
	// keeps the row recordable even on client cancel.
	logStatus := StatusClosedClean
	defer p.recordOnClose(r, res, resolvedClientIP, started, rc, ww, &logStatus)

	// Step 1b: enforce ip-geo block/allow policy BEFORE access-mode check.
	if denied, reason := p.ipGeoDenied(res, resolvedClientIP); denied {
		p.log.Info("ip-geo deny", "service_id", res.ServiceID, "ip", resolvedClientIP, "reason", reason)
		logStatus = StatusRejected
		http.Error(w, "forbidden: ip not allowed", http.StatusForbidden)
		return
	}

	// Step 2: enforce access mode BEFORE opening a stream.
	ok, status, body, hdr := p.checker.Allow(ctx, res, r)
	if !ok {
		logStatus = StatusRejected
		for k, vs := range hdr {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
		return
	}

	// v0.5.1 P2.3: wrap context with per-request idle timeout when configured.
	// The cancel is called at function return (deferred); the deferred
	// recordOnClose runs first (defer is LIFO) so it sees the final logStatus
	// value before the context is released.
	if p.idleTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.idleTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	// Determine upstream Host value.
	upstreamHost := res.LocalHost
	if upstreamHost == "" {
		upstreamHost = label + suffix // fallback: use the public vhost
	}

	// Capture values needed by Rewrite as local vars.
	// resolvedClientIP was computed at function entry (above) so the
	// connection-log deferral has it whether the access check passes or not.
	authDomain := p.authDomain
	ingressPort := p.ingressPort

	// The transport serves this one request. When the request is done, its
	// connection — the tunnel stream — would wait in the transport's idle
	// pool for a second request that never comes, and stay open until the
	// local app closes it: a stream and two goroutines here, a goroutine and
	// a connection to the app on the client, for every request answered.
	// Closing the idle connections when the handler returns ends that. It
	// closes nothing that is in use: the reverse proxy returns only after
	// the response body was copied to the visitor, and a connection that was
	// upgraded (a WebSocket) has left the pool and is closed by the reverse
	// proxy when both ends are done. A connection that is handed back to the
	// pool a moment after this call is closed as well: from the call on, the
	// transport closes a connection that becomes idle instead of keeping it
	// until the next request, which never comes. Keep-alives stay on, so the
	// request the app sees carries no "Connection: close".
	tr := &http.Transport{
		// DialContext ignores the addr argument: we always connect to the
		// tunnel stream of the resolved service. The Transport calls this for each
		// new connection: once per proxied request, since this transport
		// serves one request and is not used again.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := p.dialer.DialTunnelStreamByServiceID(ctx, res.ServiceID)
			if err != nil {
				return nil, err
			}
			return conn, nil
		},
		DisableCompression: true,
		// HTTP/2 to upstream is not applicable: tunnels speak HTTP/1.1
		// to the local server they wrap. Disable to avoid surprises.
		ForceAttemptHTTP2: false,
	}
	defer tr.CloseIdleConnections()

	// Step 3: build a per-request ReverseProxy.
	//
	// We use the Rewrite API (Go 1.20+) instead of Director. When Rewrite is
	// used, httputil.ReverseProxy removes Forwarded, X-Forwarded-For,
	// X-Forwarded-Host, X-Forwarded-Proto (stdlib behavior); we explicitly
	// Del X-Forwarded-Port below since stdlib does not auto-strip it.
	// Hop-by-hop headers are also removed by the stdlib.
	//
	// FlushInterval=-1: disable internal buffering for SSE / chunked streaming.
	// This is mandatory — LLM token streams and SSE must flush immediately.
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,

		Rewrite: func(pr *httputil.ProxyRequest) {
			stripBurrowCookies(pr.Out)

			// Stdlib has stripped Forwarded, X-Forwarded-For, X-Forwarded-Host,
			// and X-Forwarded-Proto from pr.Out before this function is called.
			// X-Forwarded-Port is NOT auto-stripped by stdlib, so we delete it
			// unconditionally here — Burrow is the trust boundary and a visitor
			// must never be able to inject an arbitrary port value upstream.
			pr.Out.Header.Del("X-Forwarded-Port")

			// Set Burrow's authoritative forwarding values.
			pr.Out.Header.Set("X-Forwarded-For", resolvedClientIP)
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			// Path-routed requests reach here with a synthetic Host; the
			// visitor only ever saw the auth domain.
			pfx := PathPrefix(pr.In.Context())
			if pfx != "" {
				pr.Out.Header.Set("X-Forwarded-Host", authDomain)
			} else {
				pr.Out.Header.Set("X-Forwarded-Host", label+"."+authDomain)
			}
			if ingressPort != "" {
				pr.Out.Header.Set("X-Forwarded-Port", ingressPort)
			}

			// Rewrite destination.
			pr.Out.URL = &url.URL{
				Scheme:   "http",
				Host:     upstreamHost,
				Path:     pr.In.URL.Path,
				RawQuery: pr.In.URL.RawQuery,
			}
			pr.Out.Host = upstreamHost

			// Tell the upstream the path-routing prefix. It comes from the
			// request context (set by the /svc/{slug} adapter); a value the
			// client sent in the header is never forwarded.
			pr.Out.Header.Del("X-Burrow-Path-Prefix")
			if pfx != "" {
				pr.Out.Header.Set("X-Burrow-Path-Prefix", pfx)
			}
		},

		Transport: tr,

		// v0.5.1 P2.3: status mapping in ErrorHandler.
		//
		// Precedence: closed_error > closed_idle > closed_clean.
		// - context.DeadlineExceeded (idle timeout fired) → closed_idle.
		// - Any other upstream error → closed_error.
		// The post-ServeHTTP 5xx check below also promotes closed_clean →
		// closed_error when the upstream successfully responded but with a
		// ≥500 status code (ErrorHandler is NOT called in that case).
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn("proxy upstream error", "subdomain", label, "err", err)
			if errors.Is(err, context.DeadlineExceeded) {
				logStatus = StatusClosedIdle
			} else {
				logStatus = StatusClosedError
				// Emit audit row for AI-gateway upstream connection failures.
				p.emitAIUpstreamErrorAudit(r.Context(), res.ServiceID, res.APIKeyHeader, upstreamHost, http.StatusBadGateway)
			}
			if errors.Is(err, ErrNotFound) {
				p.notFound(w)
				return
			}
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},

		ModifyResponse: func(resp *http.Response) error {
			dropBurrowSetCookies(resp.Header)
			if pfx := PathPrefix(resp.Request.Context()); pfx != "" {
				if loc := resp.Header.Get("Location"); loc != "" {
					resp.Header.Set("Location", rewriteLocationPrefix(loc, pfx))
				}
			}
			return nil
		},
	}

	p.log.Debug("proxy request", "subdomain", label, "method", r.Method, "path", r.URL.Path)

	// v0.4.0: if an AI chain is wired, dispatch through it. The chain itself
	// short-circuits to the v0.3.0 pass-through path (i.e. calls
	// rp.ServeHTTP unchanged) when no AI config exists for this service —
	// so a wired-but-unconfigured chain is byte-for-byte identical to
	// v0.3.0.
	if p.aiChain != nil {
		p.aiChain.Dispatch(w, r,
			res.ServiceID, res.LocalHost, res.APIKeyHeader, res.APIKeyID, rp)
		// Post-dispatch 5xx promotion: if the upstream responded with ≥500 and
		// the ErrorHandler was NOT called, promote closed_clean → closed_error.
		// closed_error and closed_idle are never downgraded (precedence rule).
		if logStatus == StatusClosedClean && ww.statusCode >= 500 {
			logStatus = StatusClosedError
			// Emit audit row for AI-gateway upstream 5xx responses.
			p.emitAIUpstreamErrorAudit(ctx, res.ServiceID, res.APIKeyHeader, upstreamHost, ww.statusCode)
		}
		return
	}

	rp.ServeHTTP(w, r)
	// Post-ServeHTTP 5xx promotion (same rule as above).
	if logStatus == StatusClosedClean && ww.statusCode >= 500 {
		logStatus = StatusClosedError
		// Emit audit row for AI-gateway upstream 5xx responses (non-chain path).
		p.emitAIUpstreamErrorAudit(ctx, res.ServiceID, res.APIKeyHeader, upstreamHost, ww.statusCode)
	}
}

// rewriteLocationPrefix prepends pathPrefix to a path-absolute Location value
// so a redirect emitted by a path-routed upstream stays under /svc/<slug>.
// Absolute and protocol-relative URLs are left unchanged (documented limit).
func rewriteLocationPrefix(loc, pathPrefix string) string {
	if loc == "" || pathPrefix == "" {
		return loc
	}
	if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
		return pathPrefix + loc
	}
	return loc
}

// ipGeoDenied reports whether the resolved client IP is denied by the
// service's ip-geo policy. Returns (false, "") when no policy is set or the
// IP can't be parsed (fail-open only on a missing/empty policy, never on a
// configured block).
func (p *Proxy) ipGeoDenied(res *Resolved, clientIP string) (bool, string) {
	policy, err := CompileIPGeoPolicy(res.IPAllowCIDRs, res.IPBlockCIDRs, res.IPAllowCountries, res.IPBlockCountries)
	if err != nil || policy.IsEmpty() {
		return false, ""
	}
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false, ""
	}
	allowed, reason := NewIPGeoEngine(policy, nil).Allow(ip)
	return !allowed, reason
}

// IPGeoDenied reports whether the service's IP/geo policy denies this
// request. It lets other entry points (the /ai/ gateway) enforce the same
// policy as path- and host-routed traffic.
func (p *Proxy) IPGeoDenied(res *Resolved, r *http.Request) bool {
	ip := clientip.Resolve(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Real-IP"), p.trustedProxies)
	denied, _ := p.ipGeoDenied(res, ip)
	return denied
}

// emitAIUpstreamErrorAudit writes an audit.ActionAIUpstreamError row when
// all of the following hold:
//   - p.auditLogger is non-nil (wired in production via WithAuditLogger),
//   - serviceID is non-empty (defensive: skip if resolution was incomplete),
//   - apiKeyHeader is non-empty (indicates an AI-gateway service; plain
//     tunnels without an API-key header are not AI-gateway traffic).
//
// Called from ErrorHandler (connection-level failure → status=502) and from
// the post-dispatch 5xx promotion path (upstream replied ≥500). The call is
// non-blocking for the hot path: audit.Logger.Append acquires a short-lived
// mutex for aggregation checks and then serialises a single DB transaction;
// the proxy has already written its response to the client before this runs
// (it is called after rp.ServeHTTP / aiChain.Dispatch returns).
func (p *Proxy) emitAIUpstreamErrorAudit(ctx context.Context, serviceID, apiKeyHeader, upstream string, statusCode int) {
	if p.auditLogger == nil || serviceID == "" || apiKeyHeader == "" {
		return
	}
	_ = p.auditLogger.Append(ctx, audit.Event{
		Action:    audit.ActionAIUpstreamError,
		SubjectID: serviceID,
		Result:    "error",
		Payload: audit.MustJSON(map[string]any{
			"status":   statusCode,
			"upstream": upstream,
		}),
	})
}

// notFound writes the canonical 404 for this package: text/plain "tunnel not
// found" with no JSON envelope (this is the data plane, not the management API).
func (p *Proxy) notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprint(w, "tunnel not found")
}

// GetConfigForClient is the SNI-driven per-vhost TLS config seam. cmd/server
// wires it on the ingress *tls.Config so the TLS layer can require + verify a
// client cert ONLY for services whose access_mode == "mtls" with a configured
// mtls_ca_pem.
//
// Behaviour:
//   - SNI label matches an mtls service with a non-empty CA → return a clone
//     with ClientCAs set to that CA + ClientAuth = RequireAndVerifyClientCert.
//   - SNI label matches a non-mtls service, or no service at all, or the CA
//     is empty/invalid → return nil to let the listener use the default
//     (no client cert required).
//
// Returning nil error + nil config means: use the default config. We never
// return a non-nil error here — a lookup miss must not break TLS handshakes
// for unrelated vhosts.
//
// This method is safe to call from many goroutines (one TLS handshake per
// new connection).
func (p *Proxy) GetConfigForClient(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	if hello == nil {
		return nil, nil
	}
	sni := hello.ServerName
	if sni == "" {
		return nil, nil
	}
	// Strip optional port (rare for SNI but be defensive).
	if h, _, err := net.SplitHostPort(sni); err == nil {
		sni = h
	}
	// Expect <label>.<authDomain>.
	suffix := "." + p.authDomain
	if !strings.HasSuffix(sni, suffix) {
		return nil, nil
	}
	label := strings.TrimSuffix(sni, suffix)
	if label == "" || strings.Contains(label, ".") {
		return nil, nil
	}

	// Use the connection's context so lookups inherit handshake cancellation.
	ctx := hello.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := p.dialer.Lookup(ctx, label)
	if err != nil {
		// Unknown vhost during handshake → default config; ServeHTTP will
		// 404 on the request itself.
		return nil, nil
	}
	if res == nil || res.AccessMode != AccessModeMTLS || len(res.MTLSCAPEM) == 0 {
		return nil, nil
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(res.MTLSCAPEM) {
		p.log.Warn("proxy GetConfigForClient: ca pem appended no certs",
			"service_id", res.ServiceID,
			"sni", sni)
		// Misconfigured CA: keep the default config rather than break TLS.
		// The access checker's checkMTLS will still refuse with 500 when
		// it sees the unusable CA on the request path.
		return nil, nil
	}

	// Clone the base config (when registered via WithTLSBase) so the
	// per-vhost overlay inherits Certificates / NextProtos / GetCertificate.
	// Without WithTLSBase we return a minimal config; the stdlib then keeps
	// using the listener's own Certificates for the server-half of the
	// handshake.
	var cfg *tls.Config
	if p.tlsBase != nil {
		cfg = p.tlsBase.Clone()
	} else {
		cfg = &tls.Config{}
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	// Avoid recursion: a cloned base config carrying GetConfigForClient
	// would re-enter here on every handshake. The returned config is
	// already the "final" one for this vhost.
	cfg.GetConfigForClient = nil
	return cfg, nil
}

// ---------------------------------------------------------------------------
// v0.5.0 F-13: connection-log accounting helpers.
// ---------------------------------------------------------------------------

// TODO(P2-6): the per-request rc.bytes()/ww.bytes() values below are written
// to the connection log but NOT folded back into the live registry's
// tunnel.BytesIn/BytesOut atomic counters that the dashboard's Traffic
// column reads from. The TCP data plane updates them via
// internal/server/data.go (bridge.Pipe(... &tun.BytesIn, &tun.BytesOut)),
// so TCP tunnels show traffic while HTTP tunnels stay at zero. Adding a
// narrow `LiveTunnelCounter` interface and routing the per-request totals
// through it would close the gap; tracked in docs/BACKLOG.md.
//
// recordOnClose builds a ConnLogEntry from the captured per-request counters
// and dispatches it through the registered ConnLogSink. Safe to call when
// p.connLogSink is nil — recordOnClose returns immediately. Called from a
// deferred closure in serveResolved / serveCustomDomain so it fires on every
// terminal exit (happy path, access-deny, panics recovered by the stdlib).
//
// Notes:
//   - TunnelID, UserID, ClientSessionID are populated from res (v0.5.1 P2.4).
//     Resolved carries them since the proxyDialerAdapter was widened to call
//     SnapshotSessions on every Lookup / LookupByServiceID.
//   - Status mapping covers all four states: rejected, closed_clean,
//     closed_error, closed_idle (v0.5.1 P2.3). Precedence is
//     closed_error > closed_idle > closed_clean, enforced by the
//     ErrorHandler closure and post-ServeHTTP 5xx promotion in
//     serveResolved / serveCustomDomain.
//   - Reason is always empty.
func (p *Proxy) recordOnClose(
	r *http.Request,
	res *Resolved,
	sourceIP string,
	started time.Time,
	rc *countingBody,
	ww *countingResponseWriter,
	status *string,
) {
	p.summarise(r, res, started, ww)
	if p.connLogSink == nil {
		return
	}
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	entry := ConnLogEntry{
		Kind:            "http_proxy",
		ServiceID:       res.ServiceID,
		TunnelID:        res.TunnelID,
		UserID:          res.UserID,
		ClientSessionID: res.ClientSessionID,
		SourceIP:        sourceIP,
		UserAgent:       ua,
		StartedAt:       started,
		EndedAt:         time.Now(),
		BytesIn:         rc.bytes(),
		BytesOut:        ww.bytes(),
		Status:          *status,
	}
	// SQLSink.Record spawns its own goroutine + uses context.WithoutCancel,
	// so passing r.Context() is safe even if the handler has already
	// returned. Errors are swallowed (the sink logs them).
	_ = p.connLogSink.Record(r.Context(), entry)
}

// summarise reports the request to the summary sink. It runs where the
// connection log is written, so once per request that reached a service and
// after its response: also for one the access check or the IP policy refused,
// one the AI chain answered itself, and one whose upstream failed.
//
// A summary carries the method, the path and the status, and nothing else of
// the request or the response: no header, no cookie, no query string (it is
// not part of URL.Path), no body. Method and path are the visitor's words and
// are bounded and made printable here.
//
// Requests to the /ai/ gateway do not pass through the proxy and are not
// summarised; tcp tunnels have no requests.
func (p *Proxy) summarise(r *http.Request, res *Resolved, started time.Time, ww *countingResponseWriter) {
	if p.summaries == nil || res == nil || !p.summaries.SummaryWanted(res.TunnelID) {
		return
	}
	status := ww.statusCode
	switch {
	case ww.hijacked:
		// The connection was handed over, which the reverse proxy does for an
		// upgrade the upstream accepted; no status passed through WriteHeader.
		status = http.StatusSwitchingProtocols
	case status == 0:
		status = http.StatusOK // a response without WriteHeader
	}
	p.summaries.RequestSummary(res.ServiceID, proto.RequestSummary{
		TunnelID:   res.TunnelID,
		Time:       started.UTC().Format(time.RFC3339),
		Method:     proto.SummaryMethod(r.Method),
		Path:       proto.SummaryPath(r.URL.Path),
		Status:     status,
		DurationMs: time.Since(started).Milliseconds(),
	})
}

// countingBody wraps the inbound request body, counting bytes read into
// BytesIn. Read errors are propagated unchanged so handlers that introspect
// io.EOF / io.ErrUnexpectedEOF continue to behave correctly.
type countingBody struct {
	r io.ReadCloser
	n int64
}

func newCountingBody(r io.ReadCloser) *countingBody {
	if r == nil {
		return &countingBody{r: io.NopCloser(strings.NewReader(""))}
	}
	return &countingBody{r: r}
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.n += int64(n)
	}
	return n, err
}

func (b *countingBody) Close() error { return b.r.Close() }

func (b *countingBody) bytes() int64 { return b.n }

// countingResponseWriter wraps the outbound writer, totalling every byte
// that escapes through Write into BytesOut. Implements http.Flusher and
// http.Hijacker by delegating when the underlying writer supports them —
// this preserves SSE flush behaviour and WebSocket upgrades that the
// httputil.ReverseProxy depends on.
//
// Hijack returns an io.Writer wrapped net.Conn that continues to count
// bytes — without this, hijacked responses (WebSocket frames, raw TCP
// upgrades) would record bytes_out = 0 forever.
//
// statusCode captures the HTTP status code written by WriteHeader. It is
// used by P2.3 to distinguish closed_error (upstream returned ≥500) from
// closed_clean. The zero value means WriteHeader was never called (e.g. a
// 200 response with no explicit WriteHeader call); in that case the
// post-ServeHTTP check skips the 5xx test.
type countingResponseWriter struct {
	http.ResponseWriter
	n          int64
	statusCode int  // set by WriteHeader; 0 = not yet called
	hijacked   bool // the connection was taken over through Hijack
}

func newCountingResponseWriter(w http.ResponseWriter) *countingResponseWriter {
	return &countingResponseWriter{ResponseWriter: w}
}

func (w *countingResponseWriter) WriteHeader(code int) {
	if w.statusCode == 0 {
		w.statusCode = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.n += int64(n)
	}
	return n, err
}

func (w *countingResponseWriter) bytes() int64 { return w.n }

// Flush delegates to the underlying ResponseWriter when it implements
// http.Flusher. SSE / chunked streaming relies on this — ReverseProxy
// (FlushInterval=-1) calls Flush after every write.
func (w *countingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying ResponseWriter when it implements
// http.Hijacker. Returns ErrNotSupported when the underlying writer does
// not support hijacking (mirrors Go's stdlib convention). The returned
// net.Conn is wrapped in countingConn so bytes written by the hijacker
// (WebSocket frames, raw TCP) continue to count toward bytes_out.
func (w *countingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	c, rw, err := h.Hijack()
	if err != nil {
		return c, rw, err
	}
	w.hijacked = true
	return &countingConn{Conn: c, n: &w.n}, rw, nil
}

// countingConn wraps a hijacked net.Conn and accumulates the byte count
// of every Write into the shared *int64 owned by the countingResponseWriter
// — so bytes_out remains accurate even when the response is hijacked.
type countingConn struct {
	net.Conn
	n *int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		*c.n += int64(n)
	}
	return n, err
}

// burrowCookies are the dashboard's own cookies. Path-routed services share
// the dashboard's origin, so browsers attach these to every request.
var burrowCookies = map[string]bool{"burrow_session": true, "burrow_csrf": true}

// stripBurrowCookies removes the dashboard's cookies from an outbound request
// and leaves every other cookie untouched. It filters the raw header text
// instead of parsing it: the upstream's cookies must arrive byte for byte,
// including values Go's cookie parser would reject or re-quote.
func stripBurrowCookies(out *http.Request) {
	lines := out.Header.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		pairs := strings.Split(line, ";")
		rest := pairs[:0]
		for _, pair := range pairs {
			name, _, _ := strings.Cut(pair, "=")
			if !burrowCookies[strings.TrimSpace(name)] {
				rest = append(rest, pair)
			}
		}
		if len(rest) == len(pairs) {
			kept = append(kept, line)
			continue
		}
		if line = strings.TrimSpace(strings.Join(rest, ";")); line != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		out.Header.Del("Cookie")
		return
	}
	out.Header["Cookie"] = kept
}

// dropBurrowSetCookies removes Set-Cookie lines of an upstream response that
// name one of the dashboard's cookies, so a tunnelled app on the shared origin
// cannot fix or clear the dashboard session. The name is matched the way
// browsers read it: the text before the first "=" of the name-value pair,
// whitespace trimmed, case-sensitive. Every other line stays byte for byte
// and in order.
func dropBurrowSetCookies(h http.Header) {
	lines := h["Set-Cookie"]
	if len(lines) == 0 {
		return
	}
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		pair, _, _ := strings.Cut(line, ";")
		name, _, hasValue := strings.Cut(pair, "=")
		if hasValue && burrowCookies[strings.TrimSpace(name)] {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == len(lines) {
		return
	}
	if len(kept) == 0 {
		delete(h, "Set-Cookie")
		return
	}
	h["Set-Cookie"] = kept
}

// pathPrefixKey is the context key under which the /svc/{slug} adapter records
// the public path prefix of a path-routed request.
type pathPrefixKey struct{}

// WithPathPrefix marks a request as path-routed under prefix ("/svc/<slug>").
// Only the /svc/{slug} adapter calls it; a request header cannot, so clients
// of the host-routed listener are unable to claim a prefix.
func WithPathPrefix(ctx context.Context, prefix string) context.Context {
	return context.WithValue(ctx, pathPrefixKey{}, prefix)
}

// PathPrefix returns the prefix recorded by WithPathPrefix, or "" for a
// host-routed request.
func PathPrefix(ctx context.Context) string {
	pfx, _ := ctx.Value(pathPrefixKey{}).(string)
	return pfx
}
