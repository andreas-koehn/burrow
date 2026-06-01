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
		r2.URL.RawPath = "" // let net/http re-derive from Path
		r2.Host = id + "." + authDomain
		r2.Header.Set("X-Burrow-Path-Prefix", prefix)

		proxy.ServeHTTP(w, r2)
	}
}
