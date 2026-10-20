package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// ipCaptureStore records the ip argument passed to CreateSession.
type ipCaptureStore struct {
	fakeUsers
	capturedIP string
}

func (s *ipCaptureStore) VerifyUserPassword(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
func (s *ipCaptureStore) GetUserByEmail(_ context.Context, e string) (db.User, error) {
	return db.User{ID: "u1", Email: e, Role: "admin"}, nil
}
func (s *ipCaptureStore) CreateSession(_ context.Context, _, _, ip string) (string, error) {
	s.capturedIP = ip
	return "sid-tp", nil
}
func (s *ipCaptureStore) ValidateSession(_ context.Context, _ string) (string, error) {
	return "u1", nil
}
func (s *ipCaptureStore) ValidateBareSession(ctx context.Context, id string) (string, error) {
	return s.ValidateSession(ctx, id)
}

// TestTrustedProxyMiddlewareEmpty_XFFIgnored asserts that when TrustedProxies
// is empty (the safe default), a request with X-Forwarded-For from an
// untrusted peer does NOT have its RemoteAddr overwritten.
// This is the core anti-spoofing guarantee: spoofed XFF cannot change the IP
// seen by downstream handlers.
func TestTrustedProxyMiddlewareEmpty_XFFIgnored(t *testing.T) {
	var seen string
	h := TrustedProxyMiddleware(nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:55555"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	h.ServeHTTP(rr, req)
	if seen != "1.2.3.4:55555" {
		t.Errorf("empty TrustedProxies: RemoteAddr should stay as raw peer, got %q", seen)
	}
}

// TestTrustedProxyMiddlewareTrusted_XFFHonored asserts that when the TCP peer
// is within a trusted CIDR, X-Forwarded-For IS honored and RemoteAddr is
// rewritten to the forwarded IP.
func TestTrustedProxyMiddlewareTrusted_XFFHonored(t *testing.T) {
	var seen string
	h := TrustedProxyMiddleware([]string{"10.0.0.0/8"})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.1.2.3:4444"
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	h.ServeHTTP(rr, req)
	host, _, err := net.SplitHostPort(seen)
	if err != nil {
		t.Fatalf("RemoteAddr %q not parseable: %v", seen, err)
	}
	if host != "203.0.113.5" {
		t.Errorf("trusted proxy: expected forwarded IP 203.0.113.5, got host %q from RemoteAddr %q", host, seen)
	}
}

// TestTrustedProxyMiddlewareUntrusted_XFFIgnored asserts that when TrustedProxies
// is non-empty but the TCP peer is NOT in the list, XFF is still ignored.
func TestTrustedProxyMiddlewareUntrusted_XFFIgnored(t *testing.T) {
	var seen string
	h := TrustedProxyMiddleware([]string{"10.0.0.0/8"})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:55555"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	h.ServeHTTP(rr, req)
	if seen != "1.2.3.4:55555" {
		t.Errorf("non-trusted peer: RemoteAddr should stay as raw peer, got %q", seen)
	}
}

// TestTrustedProxyMiddlewareBareIPTrusted asserts that a bare IP (no CIDR mask)
// in TrustedProxies also works.
func TestTrustedProxyMiddlewareBareIPTrusted(t *testing.T) {
	var seen string
	h := TrustedProxyMiddleware([]string{"192.168.1.1"})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.1:9999"
	req.Header.Set("X-Real-IP", "203.0.113.9")
	h.ServeHTTP(rr, req)
	host, _, err := net.SplitHostPort(seen)
	if err != nil {
		t.Fatalf("RemoteAddr %q not parseable: %v", seen, err)
	}
	if host != "203.0.113.9" {
		t.Errorf("bare IP trusted proxy: expected 203.0.113.9, got %q", host)
	}
}

// TestLoginStoresHostOnly asserts that Login stores the IP host without the port
// in the session record — even when RemoteAddr is in the usual host:port form.
func TestLoginStoresHostOnly(t *testing.T) {
	store := &ipCaptureStore{}
	store.fakeUsers = fakeUsers{validate: func(_ string) (string, error) { return "u1", nil }}
	ts := newTestServer(Deps{Users: store, Log: discardLog()})
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/auth/login", strings.NewReader(`{"email":"a@x","password":"pw"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login want 200, got %d", resp.StatusCode)
	}
	// The captured IP must be a bare host (no colon + port suffix).
	ip := store.capturedIP
	if strings.Contains(ip, ":") {
		t.Errorf("session.ip should be host only, got %q (contains ':')", ip)
	}
	if ip == "" {
		t.Error("session.ip must not be empty")
	}
}

// TestLoginStoresHostOnly_TrustedProxy asserts that when a trusted proxy rewrites
// RemoteAddr to "<clientIP>:0", Login still stores only the host part.
func TestLoginStoresHostOnly_TrustedProxy(t *testing.T) {
	store := &ipCaptureStore{}
	store.fakeUsers = fakeUsers{validate: func(_ string) (string, error) { return "u1", nil }}
	ts := newTestServer(Deps{
		Users:          store,
		Log:            discardLog(),
		TrustedProxies: []string{"127.0.0.0/8"}, // httptest server peer is 127.0.0.1
	})
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/auth/login", strings.NewReader(`{"email":"a@x","password":"pw"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login want 200, got %d", resp.StatusCode)
	}
	// TrustedProxyMiddleware should have set RemoteAddr = "203.0.113.7:0";
	// Login must strip the ":0" and store "203.0.113.7".
	ip := store.capturedIP
	if ip != "203.0.113.7" {
		t.Errorf("session.ip should be forwarded IP 203.0.113.7, got %q", ip)
	}
}

// TestSpoofedXFFCannotBypassPerIPLimiter is the key security regression test.
// It proves that with empty TrustedProxies, an attacker who uses a UNIQUE
// X-Forwarded-For on every request from the SAME TCP peer still hits the
// per-IP rate limit — because the limiter keys on RemoteAddr (the real peer),
// not the spoofed header.
//
// Discrimination: a middleware that (wrongly) honours untrusted XFF would key
// each request on a distinct IP and would never reach the per-IP limit, so no
// 429 would be returned and the final assertion would FAIL.  The correct
// middleware ignores XFF from an untrusted peer, keys all requests on the single
// real TCP peer, and DOES trip the limit — so the test PASSES.
func TestSpoofedXFFCannotBypassPerIPLimiter(t *testing.T) {
	const perIPLimit = 3
	const sendCount = 8 // comfortably exceeds perIPLimit

	au := &authUsers{verify: func(_, _ string) (bool, error) { return false, nil }}
	ts := newTestServer(Deps{
		Users:                        au,
		Log:                          discardLog(),
		TrustedProxies:               nil, // safe default: no forwarded headers trusted
		LoginRateLimitPerIPOverride:  perIPLimit,
		LoginRateLimitGlobalOverride: 1000,
	})
	defer ts.Close()

	got429 := false
	first429At := -1
	for i := 0; i < sendCount; i++ {
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/auth/login",
			strings.NewReader(`{"email":"a@x","password":"pw"}`))
		req.Header.Set("Content-Type", "application/json")
		// Every request carries a UNIQUE spoofed XFF value. A middleware that
		// honours this header would see sendCount distinct "client IPs" and
		// would never accumulate enough hits on any single key to reach perIPLimit.
		// The correct middleware ignores these and keys on the one real TCP peer.
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && !got429 {
			got429 = true
			first429At = i
		}
	}
	if !got429 {
		t.Errorf("expected at least one 429 from per-IP rate-limiter (limit=%d, sent=%d requests); "+
			"spoofed unique XFF values must not bypass the limiter keyed on the real TCP peer",
			perIPLimit, sendCount)
	}
	// The 429 must not arrive before the limit is exhausted (sanity: first
	// perIPLimit-1 requests should all succeed).
	if got429 && first429At < perIPLimit {
		t.Errorf("429 arrived at request index %d, but limit is %d; "+
			"rate-limiter should allow at least %d requests before throttling",
			first429At, perIPLimit, perIPLimit)
	}
}
