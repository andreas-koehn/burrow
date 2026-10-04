package main

// e2e_access_modes_test.go — Task 5 of the v0.3.0 integration plan.
// Real stack (server + client + proxy + gate); exercises both new HTTP
// access modes (api_key + burrow_login) end-to-end with real cookies
// and real /__burrow/login HTML responses.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// api_key enforcement (Part C)
// ---------------------------------------------------------------------------

func TestE2EAccessModes_APIKey_DefaultBearer(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	// Upstream echoes "ok" — when the proxy lets the request through.
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	// Flip the service to api_key mode via the real store (admin caller).
	must(t, s.store.SetServiceAccessMode(
		context.Background(), s.userID, "admin", s.serviceID, "api_key", "Authorization", nil),
		"SetServiceAccessMode(api_key)")
	// Mint a key, retain plaintext.
	_, plaintext, err := s.store.CreateAPIKey(
		context.Background(), s.userID, "admin", s.serviceID, "ci")
	must(t, err, "CreateAPIKey")
	if !strings.HasPrefix(plaintext, "buk_") {
		t.Fatalf("plaintext key shape: want buk_*, got %q", plaintext)
	}

	hc := s.visitorClient(t)
	url := "https://" + s.hostWithPort() + "/"

	// No credential → 401 + WWW-Authenticate: Bearer.
	r1, err := hc.Get(url)
	must(t, err, "GET (no creds)")
	body1 := readAllString(t, r1)
	if r1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body=%s", r1.StatusCode, body1)
	}
	if got := r1.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate: want Bearer, got %q", got)
	}
	if !strings.Contains(body1, "missing api key") {
		t.Errorf(`body: want "missing api key", got %q`, body1)
	}

	// Invalid key → 401 + invalid api key.
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer not-a-real-key")
	r2, err := hc.Do(req)
	must(t, err, "GET (bogus)")
	body2 := readAllString(t, r2)
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus key: want 401, got %d body=%s", r2.StatusCode, body2)
	}
	if !strings.Contains(body2, "invalid api key") {
		t.Errorf(`body: want "invalid api key", got %q`, body2)
	}

	// Valid key → proxied to upstream.
	req3, _ := http.NewRequest("GET", url, nil)
	req3.Header.Set("Authorization", "Bearer "+plaintext)
	r3, err := hc.Do(req3)
	must(t, err, "GET (valid)")
	body3 := readAllString(t, r3)
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("valid key: want 200, got %d body=%s", r3.StatusCode, body3)
	}
	if body3 != "ok" {
		t.Errorf("upstream body: want ok, got %q", body3)
	}
}

func TestE2EAccessModes_APIKey_CustomHeaderOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	// Switch service to api_key with a custom header.
	must(t, s.store.SetServiceAccessMode(
		context.Background(), s.userID, "admin", s.serviceID, "api_key", "X-Api-Key", nil),
		"SetServiceAccessMode(api_key, X-Api-Key)")
	_, plaintext, err := s.store.CreateAPIKey(
		context.Background(), s.userID, "admin", s.serviceID, "ci")
	must(t, err, "CreateAPIKey")

	hc := s.visitorClient(t)
	target := "https://" + s.hostWithPort() + "/"

	// Default Authorization header is now ignored → 401.
	req, _ := http.NewRequest("GET", target, nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r1, err := hc.Do(req)
	must(t, err, "GET (auth hdr ignored)")
	body1 := readAllString(t, r1)
	if r1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Authorization should be ignored when X-Api-Key is configured; got %d body=%s",
			r1.StatusCode, body1)
	}

	// Custom header accepted.
	req2, _ := http.NewRequest("GET", target, nil)
	req2.Header.Set("X-Api-Key", plaintext)
	r2, err := hc.Do(req2)
	must(t, err, "GET (custom hdr)")
	body2 := readAllString(t, r2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("custom header: want 200, got %d body=%s", r2.StatusCode, body2)
	}
	if body2 != "ok" {
		t.Errorf("upstream body: want ok, got %q", body2)
	}
}

// ---------------------------------------------------------------------------
// burrow_login enforcement (Part D)
// ---------------------------------------------------------------------------

func TestE2EAccessModes_BurrowLogin_FullFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t)

	// The upstream records the cookies it is sent: the dashboard's own cookies
	// must never reach it.
	var upstreamCookies atomic.Value // []string
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		var names []string
		for _, ck := range r.Cookies() {
			names = append(names, ck.Name)
		}
		upstreamCookies.Store(names)
		_, _ = io.WriteString(w, "secret-app")
	})

	// Configure service: burrow_login, policy = ["admin"].
	must(t, s.store.SetServiceAccessMode(
		context.Background(), s.userID, "admin", s.serviceID, "burrow_login", "", nil),
		"SetServiceAccessMode(burrow_login)")
	must(t, s.store.SetAccessPolicy(
		context.Background(), s.userID, "admin", s.serviceID, []string{"admin"}),
		"SetAccessPolicy(admin)")

	// Step 1 — unauthenticated visit to the path URL → 302 to
	// /__burrow/login?next=... on the same origin.
	hc := s.pathClient(t)
	svcURL := s.pathURL("/protected")

	r1, err := hc.Get(svcURL)
	must(t, err, "GET (unauth)")
	_ = readAllString(t, r1)
	if r1.StatusCode != http.StatusFound {
		t.Fatalf("want 302, got %d", r1.StatusCode)
	}
	loc1, err := r1.Location()
	must(t, err, "Location parse")
	if loc1.Host != e2eAuthDomain {
		t.Errorf("redirect host: want %s, got %s", e2eAuthDomain, loc1.Host)
	}
	if loc1.Path != "/__burrow/login" {
		t.Errorf("redirect path: want /__burrow/login, got %s", loc1.Path)
	}
	if loc1.Query().Get("next") != svcURL {
		t.Errorf("next param: want %q, got %q", svcURL, loc1.Query().Get("next"))
	}

	// Step 2 — GET the gate; verify 200 HTML + "Sign in to continue".
	r2, err := hc.Get(loc1.String())
	must(t, err, "GET gate")
	body2 := readAllString(t, r2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("gate GET: want 200, got %d body=%s", r2.StatusCode, body2)
	}
	if !strings.Contains(body2, "Sign in to continue") {
		t.Errorf(`gate body missing "Sign in to continue", got: %s`, body2)
	}
	if !strings.Contains(body2, s.subdomain) {
		t.Errorf("gate body missing service label %q, got: %s", s.subdomain, body2)
	}

	// Step 3 — POST credentials → 302 back to next + sets a host-only cookie.
	form := url.Values{
		"email":    {e2eAdminEmail},
		"password": {e2eAdminPassword},
		"next":     {svcURL},
	}
	postURL := "https://" + e2eAuthDomain + "/__burrow/login"
	r3, err := hc.PostForm(postURL, form)
	must(t, err, "POST gate")
	_ = readAllString(t, r3)
	if r3.StatusCode != http.StatusFound {
		t.Fatalf("POST gate: want 302, got %d", r3.StatusCode)
	}
	if got := r3.Header.Get("Location"); got != svcURL {
		t.Errorf("POST gate Location: want %q, got %q", svcURL, got)
	}
	// Verify the cookie is host-only (no Domain attribute).
	var sso *http.Cookie
	for _, ck := range r3.Cookies() {
		if ck.Name == "burrow_session" {
			sso = ck
			break
		}
	}
	if sso == nil {
		t.Fatal("POST gate: missing burrow_session cookie")
	}
	if sso.Domain != "" {
		t.Errorf("cookie Domain: want none (host-only), got %q", sso.Domain)
	}
	// A dashboard CSRF cookie rides along on the shared origin as well.
	mainURL, _ := url.Parse("https://" + e2eAuthDomain + "/")
	hc.Jar.SetCookies(mainURL, []*http.Cookie{
		{Name: "burrow_csrf", Value: "tok", Path: "/", Secure: true},
		{Name: "app", Value: "1", Path: "/", Secure: true},
	})

	// Step 4 — re-visit the service with the cookie → 200 proxied.
	r4, err := hc.Get(svcURL)
	must(t, err, "GET service (authed)")
	body4 := readAllString(t, r4)
	if r4.StatusCode != http.StatusOK {
		t.Fatalf("authed visit: want 200, got %d body=%s", r4.StatusCode, body4)
	}
	if body4 != "secret-app" {
		t.Errorf("authed body: want secret-app, got %q", body4)
	}
	// The upstream sees its own cookie but neither of the dashboard's.
	got, _ := upstreamCookies.Load().([]string)
	if len(got) != 1 || got[0] != "app" {
		t.Errorf("upstream cookies: want [app], got %v", got)
	}

	// Step 5 — restrict policy to {"user"} only; admin (us) is now denied.
	must(t, s.store.SetAccessPolicy(
		context.Background(), s.userID, "admin", s.serviceID, []string{"user"}),
		"SetAccessPolicy(user-only)")

	// Re-request the service: the proxy 302s to /__burrow/login (the access
	// checker doesn't read the session — it always redirects), and the gate
	// then sees the valid session but the role isn't in policy → 403 HTML.
	r5, err := hc.Get(svcURL)
	must(t, err, "GET (denied)")
	_ = readAllString(t, r5)
	if r5.StatusCode != http.StatusFound {
		t.Fatalf("expected proxy 302 to gate, got %d", r5.StatusCode)
	}
	loc5, err := r5.Location()
	must(t, err, "Location parse")
	r6, err := hc.Get(loc5.String())
	must(t, err, "GET gate (denied)")
	body6 := readAllString(t, r6)
	if r6.StatusCode != http.StatusForbidden {
		t.Fatalf("gate role-deny: want 403, got %d body=%s", r6.StatusCode, body6)
	}
	if !strings.Contains(body6, "Access denied") {
		t.Errorf(`access-denied body missing "Access denied", got: %s`, body6)
	}
	if !strings.Contains(body6, "Sign out") {
		t.Errorf(`access-denied body missing logout link, got: %s`, body6)
	}

	// Step 6 — global logout: POST /__burrow/logout → 302 to login + clears cookie.
	logoutURL := "https://" + e2eAuthDomain + "/__burrow/logout"
	r7, err := hc.PostForm(logoutURL, nil)
	must(t, err, "POST logout")
	_ = readAllString(t, r7)
	if r7.StatusCode != http.StatusFound {
		t.Fatalf("logout: want 302, got %d", r7.StatusCode)
	}
	// Verify the cookie clear (MaxAge<0) was sent.
	cleared := false
	for _, ck := range r7.Cookies() {
		if ck.Name == "burrow_session" && (ck.MaxAge < 0 || ck.Value == "") {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout did not clear burrow_session cookie")
	}
}
