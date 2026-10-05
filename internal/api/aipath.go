package api

import (
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/store"
)

// AIPathHandler serves /ai/{provider}/*: it strips the "/ai/<provider>"
// prefix and hands the request to the AI gateway. Mounted on /ai and /ai/ as
// well, where no provider is named, it answers with the JSON 404.
func AIPathHandler(g *aigateway.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Lets an operator match a client's failed call to the relay's logs.
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set("Burrow-Request-Id", id)
		}
		// A panic must not reach chi's Recoverer: it answers with an empty
		// 500, and every error under /ai/ is JSON.
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			g.Log.Error("aigateway: panic", "panic", rec, "stack", string(debug.Stack()))
			aigateway.WriteError(w, http.StatusInternalServerError, "internal_error", "internal error")
		}()
		slug := chi.URLParam(r, "provider")
		if !store.ValidProviderSlug(slug) {
			aigateway.WriteError(w, http.StatusNotFound, "provider_not_found", "unknown provider")
			return
		}
		prefix := "/ai/" + slug
		r2 := r.Clone(r.Context())
		r2.URL.Path = ensureLeadingSlash(strings.TrimPrefix(r.URL.Path, prefix))
		if r.URL.RawPath != "" {
			r2.URL.RawPath = ensureLeadingSlash(strings.TrimPrefix(r.URL.RawPath, prefix))
		}
		g.Serve(w, r2, slug)
	}
}

func ensureLeadingSlash(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}
