// test/harness/upstream/main_test.go
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: want application/json, got %q", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body: want status=ok, got %+v", body)
	}
}

func TestEchoGET(t *testing.T) {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/echo?x=1", nil)
	r.Header.Set("X-Test", "hello")
	handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	var got struct {
		Method  string              `json:"method"`
		Path    string              `json:"path"`
		Headers map[string][]string `json:"headers"`
		Body    string              `json:"body"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Method != "GET" {
		t.Fatalf("method: want GET, got %q", got.Method)
	}
	if got.Path != "/echo?x=1" {
		t.Fatalf("path: want /echo?x=1, got %q", got.Path)
	}
	if vals, ok := got.Headers["X-Test"]; !ok || len(vals) == 0 || vals[0] != "hello" {
		t.Fatalf("X-Test header missing or wrong: %+v", got.Headers)
	}
	if got.Body != "" {
		t.Fatalf("body: want empty for GET, got %q", got.Body)
	}
}

func TestEchoPOSTBody(t *testing.T) {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("payload-bytes"))
	r.Header.Set("Content-Type", "text/plain")
	handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	var got struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Body != "payload-bytes" {
		t.Fatalf("body: want payload-bytes, got %q", got.Body)
	}
}

func TestRedirect(t *testing.T) {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/redirect", nil)
	handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusFound {
		t.Fatalf("status: want 302, got %d", rr.Code)
	}
	// Location must be path-absolute so the proxy's prefix rewrite applies.
	if loc := rr.Header().Get("Location"); loc != "/redirected" {
		t.Fatalf("location: want /redirected, got %q", loc)
	}
}

func TestRedirected(t *testing.T) {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/redirected", nil)
	handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["landed"] != "redirected" {
		t.Fatalf("body: want landed=redirected, got %+v", body)
	}
}

func TestEchoBodyCap(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 70*1024) // 70 KiB > 64 KiB cap
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(big))
	handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	all, _ := io.ReadAll(rr.Body)
	var got struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(all, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Body) != 64*1024 {
		t.Fatalf("body cap: want exactly 65536 bytes, got %d", len(got.Body))
	}
}
