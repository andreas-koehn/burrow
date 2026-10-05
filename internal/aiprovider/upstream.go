package aiprovider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
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

// hasDotSegment reports whether path contains a "." or ".." segment.
func hasDotSegment(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// isTimeout reports whether a transport error is a deadline running out.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// stripped are inbound headers that never leave the relay; Rewrite has
// already removed Forwarded and X-Forwarded-For/-Host/-Proto.
var stripped = []string{"Authorization", "X-Api-Key", "Cookie", "X-Forwarded-Port", "X-Real-Ip", "X-Burrow-Path-Prefix"}

// NewUpstream returns a handler that forwards requests to the provider's
// upstream with the upstream's credential. The inbound request's own
// credentials and forwarding headers never leave the relay.
func NewUpstream(cfg Config, v Vault, rt http.RoundTripper, writeErr ErrorWriter) (http.Handler, error) {
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
			return nil
		},
		// The transport's error can quote the upstream URL and is not passed
		// on or logged; the client gets a fixed message per cause.
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var rej *errUpstreamRejected
			switch {
			case errors.As(err, &rej):
				writeErr(w, rej.status, rej.code, rej.message)
			case errors.Is(err, ErrBlockedAddress):
				writeErr(w, http.StatusBadGateway, "upstream_address_blocked", "the provider's address is not public")
			case isTimeout(err):
				writeErr(w, http.StatusGatewayTimeout, "upstream_timeout", "the provider did not answer in time")
			default:
				writeErr(w, http.StatusBadGateway, "upstream_unavailable", "the provider did not answer")
			}
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The base URL's path is the scope the operator configured; the
		// credential must not travel outside it.
		if hasDotSegment(r.URL.Path) {
			writeErr(w, http.StatusBadRequest, "invalid_path", "the path must not contain . or .. segments")
			return
		}
		proxy.ServeHTTP(w, r)
	}), nil
}
