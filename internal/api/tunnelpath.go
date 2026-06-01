package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// TunnelPathHandler adapts a /t/{id}/* request into a host-based request the
// existing host-routing proxy handler understands: it rewrites Host to
// "<id>.<authDomain>", strips the "/t/<id>" prefix from the path, and sets
// X-Burrow-Path-Prefix so the proxy can rewrite Location headers. Access
// control, connection logging, streaming, and stream dialing all come from the
// delegated proxy handler.
func TunnelPathHandler(proxy http.Handler, authDomain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.NotFound(w, r)
			return
		}
		prefix := "/t/" + id
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}

		r2 := r.Clone(r.Context())
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
		r2.Host = id + "." + authDomain
		r2.Header.Set("X-Burrow-Path-Prefix", prefix)

		proxy.ServeHTTP(w, r2)
	}
}
