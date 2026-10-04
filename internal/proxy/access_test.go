package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/proxy"
)

// fakeValidator is a minimal APIKeyValidator for unit tests.
// goodKey is the key that returns (true, nil); everything else is (false, nil).
type fakeValidator struct {
	goodKey  string
	failWith error // if non-nil, every call returns (false, failWith)
}

func (f *fakeValidator) ValidateAPIKey(_ context.Context, _, presented string) (bool, error) {
	if f.failWith != nil {
		return false, f.failWith
	}
	return presented == f.goodKey, nil
}

const testAuthDomain = "auth.example.com"
const testGoodKey = "buk_goodkey123"

// newChecker builds an accessChecker backed by fakeValidator.
func newChecker(v proxy.APIKeyValidator) proxy.AccessChecker {
	return proxy.NewAccessChecker(v, testAuthDomain)
}

// --------------------------------------------------------------------------
// Unit tests: AccessChecker.Allow
// --------------------------------------------------------------------------

func TestAccessChecker_OpenMode(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	res := &proxy.Resolved{ServiceID: "svc1", AccessMode: "open"}
	req := httptest.NewRequest("GET", "http://svc1.auth.example.com/", nil)

	ok, status, body, _ := ac.Allow(context.Background(), res, req)
	if !ok {
		t.Fatalf("open mode: want ok=true, got ok=false (status=%d body=%q)", status, body)
	}
}

func TestAccessChecker_APIKey_Missing(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	res := &proxy.Resolved{ServiceID: "svc2", AccessMode: "api_key"}
	// No Authorization header.
	req := httptest.NewRequest("GET", "http://svc2.auth.example.com/", nil)

	ok, status, body, hdr := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("api_key missing credential: want ok=false, got ok=true")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("want status 401, got %d", status)
	}
	if body != `{"error":"missing api key"}` {
		t.Errorf("body mismatch: got %q", body)
	}
	if hdr.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("want WWW-Authenticate: Bearer, got %q", hdr.Get("WWW-Authenticate"))
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Errorf("want Content-Type: application/json, got %q", hdr.Get("Content-Type"))
	}
}

func TestAccessChecker_APIKey_WrongKey(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	res := &proxy.Resolved{ServiceID: "svc3", AccessMode: "api_key"}
	req := httptest.NewRequest("GET", "http://svc3.auth.example.com/", nil)
	req.Header.Set("Authorization", "Bearer buk_wrongkey")

	ok, status, body, hdr := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("api_key wrong key: want ok=false, got ok=true")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("want status 401, got %d", status)
	}
	if body != `{"error":"invalid api key"}` {
		t.Errorf("body mismatch: got %q", body)
	}
	if hdr.Get("WWW-Authenticate") != "" {
		t.Errorf("invalid key: WWW-Authenticate should be absent, got %q", hdr.Get("WWW-Authenticate"))
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Errorf("want Content-Type: application/json, got %q", hdr.Get("Content-Type"))
	}
}

func TestAccessChecker_APIKey_CorrectKey_DefaultHeader(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	// APIKeyHeader empty → default Authorization header with Bearer prefix.
	res := &proxy.Resolved{ServiceID: "svc4", AccessMode: "api_key", APIKeyHeader: ""}
	req := httptest.NewRequest("GET", "http://svc4.auth.example.com/", nil)
	req.Header.Set("Authorization", "Bearer "+testGoodKey)

	ok, _, _, _ := ac.Allow(context.Background(), res, req)
	if !ok {
		t.Fatal("api_key correct key (default header): want ok=true, got ok=false")
	}
}

func TestAccessChecker_APIKey_CorrectKey_CustomHeader(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	// Custom header: X-Api-Key (no Bearer stripping).
	res := &proxy.Resolved{ServiceID: "svc5", AccessMode: "api_key", APIKeyHeader: "X-Api-Key"}
	req := httptest.NewRequest("GET", "http://svc5.auth.example.com/", nil)
	req.Header.Set("X-Api-Key", testGoodKey) // no "Bearer " prefix for custom headers

	ok, _, _, _ := ac.Allow(context.Background(), res, req)
	if !ok {
		t.Fatal("api_key correct key (custom header): want ok=true, got ok=false")
	}
}

func TestAccessChecker_BurrowLogin_Redirect(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	res := &proxy.Resolved{ServiceID: "svc6", AccessMode: "burrow_login"}
	req := httptest.NewRequest("GET", "http://svc6.auth.example.com/some/path?q=1", nil)
	// Set r.Host so the checker can build the absolute original URL.
	req.Host = "svc6.auth.example.com"

	ok, status, body, hdr := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("burrow_login: want ok=false, got ok=true")
	}
	if status != http.StatusFound {
		t.Errorf("want status 302, got %d", status)
	}
	if body != "" {
		t.Errorf("want empty body, got %q", body)
	}
	loc := hdr.Get("Location")
	if loc == "" {
		t.Fatal("burrow_login: want Location header, got empty")
	}
	// Location must start with the gate URL prefix.
	wantPrefix := "https://" + testAuthDomain + "/__burrow/login?next="
	if !strings.HasPrefix(loc, wantPrefix) {
		t.Errorf("Location %q does not start with %q", loc, wantPrefix)
	}
	// The next= value must contain the encoded original URL.
	if !strings.Contains(loc, "svc6.auth.example.com") {
		t.Errorf("Location %q should contain original host", loc)
	}
	if !strings.Contains(loc, "some") {
		t.Errorf("Location %q should contain original path fragment", loc)
	}
}

func TestAccessChecker_BurrowLogin_EmptyAuthDomain(t *testing.T) {
	// authDomain="" → fail closed with 500.
	ac := proxy.NewAccessChecker(&fakeValidator{goodKey: testGoodKey}, "")
	res := &proxy.Resolved{ServiceID: "svc7", AccessMode: "burrow_login"}
	req := httptest.NewRequest("GET", "http://svc7.example.com/", nil)

	ok, status, body, hdr := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("empty authDomain: want ok=false, got ok=true")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("want status 500, got %d", status)
	}
	if body != `{"error":"burrow_login requires auth_domain"}` {
		t.Errorf("body mismatch: got %q", body)
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Errorf("want Content-Type: application/json, got %q", hdr.Get("Content-Type"))
	}
}

func TestAccessChecker_UnknownMode(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	res := &proxy.Resolved{ServiceID: "svc8", AccessMode: "bogus_mode"}
	req := httptest.NewRequest("GET", "http://svc8.auth.example.com/", nil)

	ok, status, body, hdr := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("unknown mode: want ok=false, got ok=true")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("want status 500, got %d", status)
	}
	if body != `{"error":"unknown access mode"}` {
		t.Errorf("body mismatch: got %q", body)
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Errorf("want Content-Type: application/json, got %q", hdr.Get("Content-Type"))
	}
}

func TestAccessChecker_ValidatorError_DoesNotLeak(t *testing.T) {
	// When validator returns an error, response body must be "invalid api key",
	// not the internal error text.
	internalErr := errors.New("db connection refused: secret internal detail")
	ac := newChecker(&fakeValidator{failWith: internalErr})
	res := &proxy.Resolved{ServiceID: "svc9", AccessMode: "api_key"}
	req := httptest.NewRequest("GET", "http://svc9.auth.example.com/", nil)
	req.Header.Set("Authorization", "Bearer buk_somekey")

	ok, status, body, _ := ac.Allow(context.Background(), res, req)
	if ok {
		t.Fatal("validator error: want ok=false, got ok=true")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("want status 401, got %d", status)
	}
	// Must NOT leak the internal error text.
	if strings.Contains(body, "db connection") || strings.Contains(body, "secret") {
		t.Errorf("validator error leaked in body: %q", body)
	}
	if body != `{"error":"invalid api key"}` {
		t.Errorf("body mismatch: got %q", body)
	}
}

// --------------------------------------------------------------------------
// Integration test: burrow_login redirect through Proxy.ServeHTTP
// --------------------------------------------------------------------------

func TestProxyBurrowLoginRedirect(t *testing.T) {
	const domain = "tunnels.example.com"

	// The upstream should never be reached for burrow_login requests.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream reached despite burrow_login redirect")
		w.WriteHeader(http.StatusInternalServerError)
	})
	d := newFakeDialer(upstream)
	d.register("app", &proxy.Resolved{
		ServiceID:  "svc-login",
		AccessMode: "burrow_login",
		LocalHost:  "127.0.0.1:3000",
	})

	ac := proxy.NewAccessChecker(&fakeValidator{goodKey: testGoodKey}, domain)
	p := proxy.New(d, ac, domain, testLog())

	ts := httptest.NewServer(p)
	defer ts.Close()

	// Build request without following redirects.
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest("GET", ts.URL+"/protected", nil)
	req.Host = "app." + domain

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("want 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("want Location header, got empty")
	}
	wantPrefix := "https://" + domain + "/__burrow/login?next="
	if !strings.HasPrefix(loc, wantPrefix) {
		t.Errorf("Location %q does not start with %q", loc, wantPrefix)
	}
	if !strings.Contains(loc, "app."+domain) {
		t.Errorf("Location %q should contain original host", loc)
	}
}

// A path-routed request arrives with a synthetic Host. The gate redirect must
// send the visitor back to the public path URL, never to the internal label.
func TestAccessChecker_BurrowLogin_PathRouted_NextStaysOnOrigin(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	res := &proxy.Resolved{ServiceID: "svc6", AccessMode: "burrow_login"}
	req := httptest.NewRequest("GET", "http://grafana."+testAuthDomain+"/d/abc?q=1", nil)
	req.Host = "grafana." + testAuthDomain
	req.Header.Set("X-Burrow-Path-Prefix", "/svc/grafana")

	_, status, _, hdr := ac.Allow(context.Background(), res, req)
	if status != http.StatusFound {
		t.Fatalf("status = %d, want 302", status)
	}
	u, err := url.Parse(hdr.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://" + testAuthDomain + "/svc/grafana/d/abc?q=1"
	if got := u.Query().Get("next"); got != want {
		t.Fatalf("next = %q, want %q", got, want)
	}
}

// The /svc/{slug} adapter strips the prefix from URL.Path but leaves
// RequestURI as the visitor sent it. The prefix must appear exactly once.
func TestAccessChecker_BurrowLogin_PathRouted_PrefixNotDoubled(t *testing.T) {
	ac := newChecker(&fakeValidator{goodKey: testGoodKey})
	res := &proxy.Resolved{ServiceID: "svc6", AccessMode: "burrow_login"}
	req := httptest.NewRequest("GET", "https://"+testAuthDomain+"/svc/grafana/d/a%2Fb?q=1", nil)
	req.URL.Path = "/d/a/b"
	req.URL.RawPath = "/d/a%2Fb"
	req.Host = "grafana." + testAuthDomain
	req.Header.Set("X-Burrow-Path-Prefix", "/svc/grafana")

	_, _, _, hdr := ac.Allow(context.Background(), res, req)
	u, err := url.Parse(hdr.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://" + testAuthDomain + "/svc/grafana/d/a%2Fb?q=1"
	if got := u.Query().Get("next"); got != want {
		t.Fatalf("next = %q, want %q", got, want)
	}
}
