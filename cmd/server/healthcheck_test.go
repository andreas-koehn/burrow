package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/config"
)

func TestHealthcheckURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.ServerConfig
		want string
	}{
		{"default plain http", config.ServerConfig{HTTPListen: ":8080"}, "http://127.0.0.1:8080/healthz"},
		{"custom port", config.ServerConfig{HTTPListen: ":9090"}, "http://127.0.0.1:9090/healthz"},
		{"explicit host", config.ServerConfig{HTTPListen: "10.1.2.3:8080"}, "http://10.1.2.3:8080/healthz"},
		{"wildcard host", config.ServerConfig{HTTPListen: "0.0.0.0:8080"}, "http://127.0.0.1:8080/healthz"},
		{"file cert", config.ServerConfig{HTTPListen: ":8080", HTTPTLSCert: "c.pem"}, "https://127.0.0.1:8080/healthz"},
		{"acme promotes stock port", config.ServerConfig{HTTPListen: ":8080", ACMEDomain: "x.example.com"}, "https://127.0.0.1:443/healthz"},
		{"acme keeps custom port", config.ServerConfig{HTTPListen: ":9443", ACMEDomain: "x.example.com"}, "https://127.0.0.1:9443/healthz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthcheckURL(&tc.cfg); got != tc.want {
				t.Fatalf("healthcheckURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	tlsOK := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsOK.Close()
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()

	var stderr bytes.Buffer
	if err := runHealthcheck(context.Background(), ok.URL+"/healthz", &stderr); err != nil {
		t.Fatalf("200: want nil, got %v (%s)", err, stderr.String())
	}
	if err := runHealthcheck(context.Background(), tlsOK.URL+"/healthz", &stderr); err != nil {
		t.Fatalf("self-signed TLS 200: want nil, got %v (%s)", err, stderr.String())
	}

	stderr.Reset()
	if err := runHealthcheck(context.Background(), bad.URL+"/healthz", &stderr); err == nil {
		t.Fatal("503: want error, got nil")
	}
	if !strings.Contains(stderr.String(), "returned 503") {
		t.Fatalf("503: stderr = %q, want it to name the status", stderr.String())
	}

	stderr.Reset()
	if err := runHealthcheck(context.Background(), downURL+"/healthz", &stderr); err == nil {
		t.Fatal("connection refused: want error, got nil")
	}
	if !strings.Contains(stderr.String(), "unhealthy:") {
		t.Fatalf("connection refused: stderr = %q", stderr.String())
	}
}
