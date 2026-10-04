package api

import (
	"net/http"
	"strings"

	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/go-chi/chi/v5"
)

// ServicePathHandler adapts a /svc/{slug}/* request into a host-based request
// the host-routing proxy handler understands: it rewrites Host to
// "<slug>.<authDomain>", strips the "/svc/<slug>" prefix from the path, and
// records the prefix in the request context (proxy.WithPathPrefix) so the
// proxy can rewrite Location headers and report the public host upstream. The
// proxy trusts only that context value, never a client-sent header. Access control, connection logging,
// streaming, and stream dialing all come from the delegated proxy handler.
func ServicePathHandler(next http.Handler, authDomain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		if !auth.ValidSlug(slug) {
			http.NotFound(w, r)
			return
		}
		prefix := "/svc/" + slug
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}

		r2 := r.Clone(proxy.WithPathPrefix(r.Context(), prefix))
		r2.URL.Path = rest
		// Preserve percent-encoding: if RawPath is set (path had encoded bytes
		// like %2F), strip the prefix from it too; otherwise leave it empty so
		// net/http re-derives the encoded form from Path.
		if r.URL.RawPath != "" {
			rawRest := strings.TrimPrefix(r.URL.RawPath, prefix)
			if !strings.HasPrefix(rawRest, "/") {
				rawRest = "/" + rawRest
			}
			r2.URL.RawPath = rawRest
		} else {
			r2.URL.RawPath = ""
		}
		r2.Host = slug + "." + authDomain
		r2.Header.Set("X-Burrow-Path-Prefix", prefix)

		next.ServeHTTP(w, r2)
	}
}
