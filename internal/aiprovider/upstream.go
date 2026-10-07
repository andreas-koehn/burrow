package aiprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"

	"github.com/ankoehn/burrow/internal/httpduplex"
)

// ErrNotConfigured is returned when the provider's credential slot is empty.
var ErrNotConfigured = errors.New("aiprovider: credential slot is not set")

// Config is what the upstream handler needs to know about a direct provider.
type Config struct {
	Slug           string
	BaseURL        string
	CredentialSlot string
	AuthHeader     string // "" = "Authorization"
	AuthFormat     string // "" = "Bearer {key}"; must contain "{key}"
	ExtraHeaders   map[string]string
}

// Vault reads upstream credentials by slot.
type Vault interface {
	Get(slot string) (string, bool)
}

// ErrorWriter writes an error response in the caller's wire format.
type ErrorWriter func(w http.ResponseWriter, status int, code, message string)

// errUpstreamRejected marks responses the handler replaces with its own error.
type errUpstreamRejected struct {
	status        int
	code, message string
}

func (e *errUpstreamRejected) Error() string { return e.code }

// UpstreamPath maps the path a client used under /ai/<provider> onto the
// upstream. Clients address every provider as ".../v1/..."; the provider's
// base URL already carries the upstream's own version segment, so a leading
// "/v1" segment is dropped.
func UpstreamPath(basePath, rest string) string {
	if rest == "/v1" {
		rest = ""
	} else if after, ok := strings.CutPrefix(rest, "/v1/"); ok {
		rest = "/" + after
	}
	return strings.TrimRight(basePath, "/") + rest
}

// validHeaderName reports whether s is an HTTP token (RFC 9110 §5.6.2).
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validHeaderValue rejects control characters, which is what lets a value
// start a new header line.
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// failureReason names the cause of a transport error for the operator's log.
// The error itself is not logged: it can quote the upstream URL.
func failureReason(err error) string {
	var (
		ne      net.Error
		op      *net.OpError
		certErr *tls.CertificateVerificationError
		recErr  tls.RecordHeaderError
		alert   tls.AlertError
	)
	switch {
	case errors.Is(err, ErrBlockedAddress):
		return "address_blocked"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.As(err, &certErr), errors.As(err, &recErr), errors.As(err, &alert):
		return "tls"
	case errors.As(err, &op) && op.Op == "dial":
		return "dial"
	case errors.As(err, new(*net.DNSError)):
		return "dns"
	}
	return "other"
}

// TimeoutNoter is a response writer that wants to know when the upstream did
// not answer in time: the transport gave up waiting (its header timeout is
// 120 s, see newTransport) or a deadline passed. The client is answered like
// any other failure; the note is for the caller's own records.
type TimeoutNoter interface {
	NoteUpstreamTimeout()
}

// isNil reports whether an interface holds nothing or a nil pointer.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// DropSetCookie removes the cookies an upstream tries to set. Everything
// under /ai/ is answered on the dashboard's origin, where a cookie named like
// the session or CSRF cookie would replace the dashboard's own.
func DropSetCookie(h http.Header) {
	h.Del("Set-Cookie")
	h.Del("Set-Cookie2")
}

// stripped are inbound headers that never leave the relay; Rewrite has
// already removed Forwarded and X-Forwarded-For/-Host/-Proto.
var stripped = []string{"Authorization", "X-Api-Key", "Cookie", "X-Forwarded-Port", "X-Real-Ip", "X-Burrow-Path-Prefix"}

// NewUpstream returns a handler that forwards requests to the provider's
// upstream with the upstream's credential. The inbound request's own
// credentials and forwarding headers never leave the relay.
func NewUpstream(cfg Config, v Vault, rt http.RoundTripper, writeErr ErrorWriter) (http.Handler, error) {
	// A nil transport would make ReverseProxy fall back to
	// http.DefaultTransport: no address guard, environment proxies honoured.
	if isNil(rt) {
		return nil, fmt.Errorf("aiprovider: %s: transport is missing", cfg.Slug)
	}
	if writeErr == nil || isNil(v) {
		return nil, fmt.Errorf("aiprovider: %s: error writer or vault is missing", cfg.Slug)
	}
	base, err := ValidateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	authHeader := cfg.AuthHeader
	if authHeader == "" {
		authHeader = "Authorization"
	}
	authFormat := cfg.AuthFormat
	if authFormat == "" {
		authFormat = "Bearer {key}"
	}
	if !strings.Contains(authFormat, "{key}") || !validHeaderName(authHeader) {
		return nil, fmt.Errorf("aiprovider: %s: auth header or format is invalid", cfg.Slug)
	}
	for k, val := range cfg.ExtraHeaders {
		if !validHeaderName(k) || !validHeaderValue(val) {
			return nil, fmt.Errorf("aiprovider: %s: invalid extra header %q", cfg.Slug, k)
		}
	}
	key, ok := v.Get(cfg.CredentialSlot)
	if !ok || key == "" {
		return nil, ErrNotConfigured
	}
	credential := strings.Replace(authFormat, "{key}", key, 1)
	if !validHeaderValue(credential) {
		return nil, fmt.Errorf("aiprovider: %s: credential contains characters not allowed in a header", cfg.Slug)
	}

	proxy := &httputil.ReverseProxy{
		Transport:     rt,
		FlushInterval: -1, // token streams must not be buffered
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Rewrite starts from a copy with Forwarded/X-Forwarded-* and the
			// hop-by-hop headers removed, so nothing the caller lists in
			// Connection can take out what is set below.
			pr.Out.URL = &url.URL{
				Scheme:   base.Scheme,
				Host:     base.Host,
				Path:     UpstreamPath(base.Path, pr.In.URL.Path),
				RawPath:  UpstreamPath(base.EscapedPath(), pr.In.URL.EscapedPath()),
				RawQuery: pr.In.URL.RawQuery,
			}
			pr.Out.Host = base.Host
			for _, h := range stripped {
				pr.Out.Header.Del(h)
			}
			for k, val := range cfg.ExtraHeaders {
				pr.Out.Header.Set(k, val)
			}
			// Last, and with Set: neither the caller nor an extra header can
			// add to or replace the credential.
			pr.Out.Header.Set(authHeader, credential)
		},
		ModifyResponse: func(resp *http.Response) error {
			switch {
			case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
				return &errUpstreamRejected{http.StatusBadGateway, "upstream_auth_failed", "the provider rejected the relay's credential"}
			case resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified:
				return &errUpstreamRejected{http.StatusBadGateway, "upstream_redirect", "the provider answered with a redirect"}
			}
			DropSetCookie(resp.Header)
			return nil
		},
		// The client gets one neutral error whatever the cause; the cause goes
		// to the log, as a reason only, because the transport's error can
		// quote the upstream URL.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var rej *errUpstreamRejected
			if errors.As(err, &rej) {
				slog.Warn("ai upstream response replaced", "provider", cfg.Slug, "reason", rej.code)
				writeErr(w, rej.status, rej.code, rej.message)
				return
			}
			// A caller that hung up is not a fault of the provider.
			level := slog.LevelWarn
			reason := failureReason(err)
			if reason == "canceled" {
				level = slog.LevelDebug
			}
			slog.Log(r.Context(), level, "ai upstream request failed", "provider", cfg.Slug, "reason", reason)
			if tn, ok := w.(TimeoutNoter); ok && reason == "timeout" {
				tn.NoteUpstreamTimeout()
			}
			writeErr(w, http.StatusBadGateway, "upstream_unavailable", "the provider did not answer")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The base URL's path is the scope the operator configured; the
		// credential must not travel outside it.
		if hasDotSegment(r.URL.Path) {
			writeErr(w, http.StatusBadRequest, "invalid_path", "the path must not contain . or .. segments")
			return
		}
		// A caller may hand over the server's own, unbuffered request body.
		// Go's HTTP/1 server consumes what is left of it when the response
		// headers are written; a provider that answers before it has read
		// the body would then get a short one, lose its connection, and the
		// client a truncated response. Full duplex keeps the body readable;
		// httpduplex also keeps the transport's reads of the body from
		// outliving this handler.
		httpduplex.Serve(w, r, proxy)
	}), nil
}
