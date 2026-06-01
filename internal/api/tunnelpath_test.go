package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestTunnelPathHandler_RewritesHostAndStripsPrefix(t *testing.T) {
	var gotHost, gotPath, gotPrefix string
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		gotPrefix = r.Header.Get("X-Burrow-Path-Prefix")
		w.WriteHeader(http.StatusOK)
	})
	h := TunnelPathHandler(proxy, "burrow.example.com")

	r := chi.NewRouter()
	r.Handle("/t/{id}/*", h)
	r.Handle("/t/{id}", h)

	req := httptest.NewRequest("GET", "https://burrow.example.com/t/abc123/foo/bar?q=1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if gotHost != "abc123.burrow.example.com" {
		t.Fatalf("host = %q, want abc123.burrow.example.com", gotHost)
	}
	if gotPath != "/foo/bar" {
		t.Fatalf("path = %q, want /foo/bar", gotPath)
	}
	if gotPrefix != "/t/abc123" {
		t.Fatalf("prefix = %q, want /t/abc123", gotPrefix)
	}
}

func TestTunnelPathHandler_RootPath(t *testing.T) {
	var gotPath string
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})
	h := TunnelPathHandler(proxy, "burrow.example.com")
	r := chi.NewRouter()
	r.Handle("/t/{id}", h)
	req := httptest.NewRequest("GET", "https://burrow.example.com/t/abc123", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if gotPath != "/" {
		t.Fatalf("path = %q, want /", gotPath)
	}
}

func TestTunnelPathHandler_PreservesEncodedPath(t *testing.T) {
	var gotEscaped string
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
	})
	h := TunnelPathHandler(proxy, "burrow.example.com")
	r := chi.NewRouter()
	r.Handle("/t/{id}/*", h)
	req := httptest.NewRequest("GET", "https://burrow.example.com/t/abc123/foo%2Fbar", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if gotEscaped != "/foo%2Fbar" {
		t.Fatalf("escaped path = %q, want /foo%%2Fbar", gotEscaped)
	}
}
