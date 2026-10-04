package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestServicePathHandler_RewritesHostAndStripsPrefix(t *testing.T) {
	var gotHost, gotPath, gotPrefix string
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		gotPrefix = r.Header.Get("X-Burrow-Path-Prefix")
		w.WriteHeader(http.StatusOK)
	})
	h := ServicePathHandler(proxy, "burrow.example.com")

	r := chi.NewRouter()
	r.Handle("/svc/{slug}/*", h)
	r.Handle("/svc/{slug}", h)

	req := httptest.NewRequest("GET", "https://burrow.example.com/svc/abc123/foo/bar?q=1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if gotHost != "abc123.burrow.example.com" {
		t.Fatalf("host = %q, want abc123.burrow.example.com", gotHost)
	}
	if gotPath != "/foo/bar" {
		t.Fatalf("path = %q, want /foo/bar", gotPath)
	}
	if gotPrefix != "/svc/abc123" {
		t.Fatalf("prefix = %q, want /svc/abc123", gotPrefix)
	}
}

func TestServicePathHandler_RootPath(t *testing.T) {
	var gotPath string
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})
	h := ServicePathHandler(proxy, "burrow.example.com")
	r := chi.NewRouter()
	r.Handle("/svc/{slug}", h)
	req := httptest.NewRequest("GET", "https://burrow.example.com/svc/abc123", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if gotPath != "/" {
		t.Fatalf("path = %q, want /", gotPath)
	}
}

func TestServicePathHandler_PreservesEncodedPath(t *testing.T) {
	var gotEscaped string
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
	})
	h := ServicePathHandler(proxy, "burrow.example.com")
	r := chi.NewRouter()
	r.Handle("/svc/{slug}/*", h)
	req := httptest.NewRequest("GET", "https://burrow.example.com/svc/abc123/foo%2Fbar", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if gotEscaped != "/foo%2Fbar" {
		t.Fatalf("escaped path = %q, want /foo%%2Fbar", gotEscaped)
	}
}

func TestServicePathHandler_RejectsInvalidSlug(t *testing.T) {
	called := false
	proxy := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h := ServicePathHandler(proxy, "burrow.example.com")
	r := chi.NewRouter()
	r.Handle("/svc/{slug}/*", h)
	r.Handle("/svc/{slug}", h)
	for _, p := range []string{"/svc/a.b/x", "/svc/AB/x", "/svc/-ab/x", "/svc/ab/x"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", "https://burrow.example.com"+p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
	if called {
		t.Fatal("proxy was called for an invalid slug")
	}
}

func TestRouter_MountsGate(t *testing.T) {
	hit := false
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true })
	h := NewRouter(Deps{Gate: gate, Log: discardLog()})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/__burrow/login", nil))
	if !hit {
		t.Fatal("/__burrow/login did not reach the gate")
	}
}

func TestRouter_GatePostNotBlockedByCSRF(t *testing.T) {
	status := 0
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { status = http.StatusFound; w.WriteHeader(status) })
	h := NewRouter(Deps{Gate: gate, Log: discardLog()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/__burrow/login", strings.NewReader("email=a&password=b")))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 from the gate", rec.Code)
	}
}
