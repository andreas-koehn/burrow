package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/db"
)

type noProviders struct{}

func (noProviders) ProviderBySlug(_ context.Context, _ string) (db.AIProvider, error) {
	return db.AIProvider{}, db.ErrNotFound
}

func TestAIPathHandler_UnknownProviderIsJSON404(t *testing.T) {
	g := &aigateway.Gateway{Providers: noProviders{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := chi.NewRouter()
	h := AIPathHandler(g)
	r.Handle("/ai/{provider}", h)
	r.Handle("/ai/{provider}/*", h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/ai/nope/v1/models", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d content-type %q body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestAIPathHandler_ReservedAndInvalidSlugs(t *testing.T) {
	g := &aigateway.Gateway{Providers: noProviders{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := chi.NewRouter()
	h := AIPathHandler(g)
	r.Handle("/ai/{provider}/*", h)
	for _, p := range []string{"/ai/v1/models", "/ai/A.B/v1/models"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

// Token streams only work if nothing between the listener and the gateway
// buffers. The probe sits where the gate goes: same middleware stack as /ai/.
func TestRouter_TopLevelMiddlewareKeepsFlush(t *testing.T) {
	flushed := make(chan error, 1)
	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: one\n\n"))
		flushed <- http.NewResponseController(w).Flush()
	})
	srv := httptest.NewServer(NewRouter(Deps{Gate: probe, Log: discardLog()}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/__burrow/probe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if flushErr := <-flushed; flushErr != nil {
		t.Fatalf("a middleware wraps the ResponseWriter without Flush support: %v", flushErr)
	}
}

func TestRouter_MountsAIGateway(t *testing.T) {
	g := &aigateway.Gateway{Providers: noProviders{}, Log: discardLog()}
	h := NewRouter(Deps{AIGateway: g, Log: discardLog()})
	for _, p := range []string{"/ai/nope", "/ai/nope/v1/models", "/ai/v1/models"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: status %d content-type %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Burrow-Request-Id") == "" {
			t.Errorf("%s: missing Burrow-Request-Id", p)
		}
	}
}
