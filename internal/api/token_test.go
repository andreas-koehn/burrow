package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

type tokUsers struct {
	authUsers
	issued     string
	issuedUID  string
	issuedExp  *time.Time
	tokens     []db.ClientToken
	listedUID  string
	revoked    string
	revokedUID string
	revErr     error
}

func (u *tokUsers) IssueClientTokenUntil(_ context.Context, uid, name string, exp *time.Time) (string, error) {
	u.issued = name
	u.issuedUID = uid
	u.issuedExp = exp
	return "bur_PLAINTEXT", nil
}
func (u *tokUsers) ListClientTokens(_ context.Context, uid string) ([]db.ClientToken, error) {
	u.listedUID = uid
	return u.tokens, nil
}
func (u *tokUsers) RevokeClientToken(_ context.Context, id, uid string) error {
	u.revokedUID = uid
	if u.revErr != nil {
		return u.revErr
	}
	u.revoked = id
	return nil
}

// loggedInClient logs in and returns the http.Client (with cookie jar) and
// the CSRF token extracted from the Set-Cookie header returned by login.
// The CSRF token must be sent as X-CSRF-Token on every state-changing request.
func loggedInClientWithCSRF(t *testing.T, ts *httptestServer) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejarNew()
	cl := &http.Client{Jar: jar}
	r, _ := cl.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"a@x","password":"pw"}`))
	r.Body.Close()
	// Extract the CSRF token from the Set-Cookie response headers.
	var csrfToken string
	for _, c := range r.Cookies() {
		if c.Name == csrfCookieName {
			csrfToken = c.Value
		}
	}
	// Confirm the password so the routes of RequireRecentAuth are open. A
	// fake store that refuses "pw" leaves the session unconfirmed.
	rq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/reauth", strings.NewReader(`{"password":"pw"}`))
	rq.Header.Set("Content-Type", "application/json")
	rq.Header.Set("X-CSRF-Token", csrfToken)
	if rr, err := cl.Do(rq); err == nil {
		rr.Body.Close()
	}
	return cl, csrfToken
}

// loggedInClient is a convenience wrapper kept for tests that only need GET
// (no CSRF token required for safe methods).
func loggedInClient(t *testing.T, ts *httptestServer) *http.Client {
	t.Helper()
	cl, _ := loggedInClientWithCSRF(t, ts)
	return cl
}

// doWithCSRF sets the X-CSRF-Token header on req and executes it via cl.
func doWithCSRF(t *testing.T, cl *http.Client, req *http.Request, csrfToken string) *http.Response {
	t.Helper()
	req.Header.Set("X-CSRF-Token", csrfToken)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestCreateAndListTokens(t *testing.T) {
	now := time.Now()
	u := &tokUsers{tokens: []db.ClientToken{{ID: "t1", UserID: "u1", Name: "laptop", TokenHash: "SECRETHASH", CreatedAt: now}}}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl, csrf := loggedInClientWithCSRF(t, &httptestServer{ts})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/tokens", strings.NewReader(`{"name":"laptop"}`))
	req.Header.Set("Content-Type", "application/json")
	cr := doWithCSRF(t, cl, req, csrf)
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create: status=%v", cr.StatusCode)
	}
	body := readBody(t, cr)
	if !strings.Contains(body, `"token":"bur_PLAINTEXT"`) || !strings.Contains(body, `"name":"laptop"`) {
		t.Fatalf("create body missing plaintext/name: %s", body)
	}
	if u.issued != "laptop" {
		t.Fatalf("issued name = %q", u.issued)
	}
	if u.issuedUID != "u1" {
		t.Fatalf("IssueClientToken must receive authenticated userID, got %q", u.issuedUID)
	}

	lr, _ := cl.Get(ts.URL + "/api/v1/tokens")
	lb := readBody(t, lr)
	if lr.StatusCode != http.StatusOK || !strings.Contains(lb, `"id":"t1"`) || !strings.Contains(lb, `"name":"laptop"`) {
		t.Fatalf("list status=%d body=%s", lr.StatusCode, lb)
	}
	if strings.Contains(lb, "SECRETHASH") || strings.Contains(lb, "token_hash") {
		t.Fatalf("list MUST NOT leak token hash: %s", lb)
	}
	if u.listedUID != "u1" {
		t.Fatalf("ListClientTokens must receive authenticated userID, got %q", u.listedUID)
	}
}

func TestRevokeToken(t *testing.T) {
	u := &tokUsers{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl, csrf := loggedInClientWithCSRF(t, &httptestServer{ts})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/tokens/t1", nil)
	dr := doWithCSRF(t, cl, req, csrf)
	dr.Body.Close()
	if dr.StatusCode != http.StatusNoContent || u.revoked != "t1" {
		t.Fatalf("revoke status=%d revoked=%q", dr.StatusCode, u.revoked)
	}
	if u.revokedUID != "u1" {
		t.Fatalf("RevokeClientToken must receive authenticated userID, got %q", u.revokedUID)
	}

	u.revErr = db.ErrNotFound
	req2, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/tokens/missing", nil)
	req2.Header.Set("X-CSRF-Token", csrf)
	dr2, _ := cl.Do(req2)
	dr2.Body.Close()
	if dr2.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke unknown want 404, got %d", dr2.StatusCode)
	}
}

func TestCreateTokenEmptyName(t *testing.T) {
	u := &tokUsers{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl, csrf := loggedInClientWithCSRF(t, &httptestServer{ts})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/tokens", strings.NewReader(`{"name":""}`))
	req.Header.Set("Content-Type", "application/json")
	r := doWithCSRF(t, cl, req, csrf)
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty name want 400, got %d", r.StatusCode)
	}
	if u.issued != "" {
		t.Fatalf("must not issue a token for empty name (issued=%q)", u.issued)
	}
}

func TestListTokensEmptyIsJSONArray(t *testing.T) {
	u := &tokUsers{tokens: nil}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	// GET is a safe method; no CSRF header required.
	cl := loggedInClient(t, &httptestServer{ts})
	r, _ := cl.Get(ts.URL + "/api/v1/tokens")
	b := strings.TrimSpace(readBody(t, r))
	if r.StatusCode != http.StatusOK || b != "[]" {
		t.Fatalf("empty token list must be JSON [] (status=%d body=%q)", r.StatusCode, b)
	}
}

func postToken(t *testing.T, u *tokUsers, body string) (int, string) {
	t.Helper()
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl, csrf := loggedInClientWithCSRF(t, &httptestServer{ts})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r := doWithCSRF(t, cl, req, csrf)
	return r.StatusCode, readBody(t, r)
}

func TestCreateToken_ExpiresAt(t *testing.T) {
	want := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	u := &tokUsers{}
	code, body := postToken(t, u, `{"name":"laptop","expires_at":"`+want.Format(time.RFC3339)+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if u.issuedExp == nil || !u.issuedExp.Equal(want) {
		t.Fatalf("issued expiry = %v, want %v", u.issuedExp, want)
	}
	if !strings.Contains(body, `"expires_at":"`+want.Format(time.RFC3339)) {
		t.Errorf("response lacks expires_at: %s", body)
	}

	u = &tokUsers{}
	if code, body = postToken(t, u, `{"name":"laptop"}`); code != http.StatusCreated || u.issuedExp != nil {
		t.Fatalf("no expiry asked: status=%d exp=%v", code, u.issuedExp)
	} else if !strings.Contains(body, `"expires_at":null`) {
		t.Errorf("response lacks expires_at null: %s", body)
	}
}

func TestCreateToken_ExpiresAtValidation(t *testing.T) {
	now := time.Now().UTC()
	for name, at := range map[string]string{
		"past":       `"` + now.Add(-time.Hour).Format(time.RFC3339) + `"`,
		"too far":    `"` + now.Add(366*24*time.Hour).Format(time.RFC3339) + `"`,
		"not a date": `"tomorrow"`,
		"a number":   `12345`,
	} {
		t.Run(name, func(t *testing.T) {
			u := &tokUsers{}
			code, body := postToken(t, u, `{"name":"laptop","expires_at":`+at+`}`)
			if code != http.StatusBadRequest || u.issued != "" {
				t.Fatalf("status=%d issued=%q body=%s", code, u.issued, body)
			}
		})
	}
	// The limit itself is allowed.
	u := &tokUsers{}
	if code, body := postToken(t, u, `{"name":"x","expires_at":"`+now.Add(364*24*time.Hour).Format(time.RFC3339)+`"}`); code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", code, body)
	}
}

func TestListTokens_ShowsExpiry(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	u := &tokUsers{tokens: []db.ClientToken{
		{ID: "t1", Name: "a", CreatedAt: time.Now(), ExpiresAt: &exp},
		{ID: "t2", Name: "b", CreatedAt: time.Now()},
	}}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl, _ := loggedInClientWithCSRF(t, &httptestServer{ts})
	r, _ := cl.Get(ts.URL + "/api/v1/tokens")
	body := readBody(t, r)
	if !strings.Contains(body, `"expires_at":"`+exp.Format(time.RFC3339)) || !strings.Contains(body, `"expires_at":null`) {
		t.Fatalf("list = %s", body)
	}
}

// A token that expires mints a client token that expires, no later than itself.
func TestCreateToken_CappedByAutomationToken(t *testing.T) {
	tokenExp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	sooner := tokenExp.Add(-24 * time.Hour)
	later := tokenExp.Add(24 * time.Hour)
	cases := []struct {
		name string
		body string
		want time.Time
	}{
		{"default is the token's expiry", `{"name":"ci"}`, tokenExp},
		{"later is capped", `{"name":"ci","expires_at":"` + later.Format(time.RFC3339) + `"}`, tokenExp},
		{"sooner is kept", `{"name":"ci","expires_at":"` + sooner.Format(time.RFC3339) + `"}`, sooner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &tokUsers{}
			u.verify = func(_, _ string) (bool, error) { return true, nil }
			auto := newFakeAutomationStore()
			_, tok, err := auto.MintAutomationToken(context.Background(), "u1", "admin", "ci", []string{"tokens:manage:own"}, &tokenExp)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(NewRouter(Deps{Users: u, Automation: auto, Bearer: auto, Log: discardLog()}))
			defer srv.Close()
			var in map[string]any
			_ = json.Unmarshal([]byte(tc.body), &in)
			wantStatus(t, bearerDo(t, srv, tok, http.MethodPost, "/api/v1/tokens", in), http.StatusCreated)
			if u.issuedExp == nil || !u.issuedExp.Equal(tc.want) {
				t.Fatalf("issued expiry = %v, want %v", u.issuedExp, tc.want)
			}
		})
	}

	// An automation token that outlives the longest client token does not
	// give the default: it is capped at 365 days.
	{
		far := time.Now().Add(2 * 365 * 24 * time.Hour).UTC().Truncate(time.Second)
		u := &tokUsers{}
		u.verify = func(_, _ string) (bool, error) { return true, nil }
		auto := newFakeAutomationStore()
		_, tok, _ := auto.MintAutomationToken(context.Background(), "u1", "admin", "ci", []string{"tokens:manage:own"}, &far)
		srv := httptest.NewServer(NewRouter(Deps{Users: u, Automation: auto, Bearer: auto, Log: discardLog()}))
		defer srv.Close()
		wantStatus(t, bearerDo(t, srv, tok, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "ci"}), http.StatusCreated)
		max := time.Now().Add(maxClientTokenLife)
		if u.issuedExp == nil || u.issuedExp.After(max) || u.issuedExp.Before(max.Add(-time.Minute)) {
			t.Fatalf("issued expiry = %v, want about %v", u.issuedExp, max)
		}
	}

	// An automation token that never expires leaves the request alone.
	u := &tokUsers{}
	auto := newFakeAutomationStore()
	_, tok, _ := auto.MintAutomationToken(context.Background(), "u1", "admin", "ci", []string{"tokens:manage:own"}, nil)
	srv := httptest.NewServer(NewRouter(Deps{Users: u, Automation: auto, Bearer: auto, Log: discardLog()}))
	defer srv.Close()
	wantStatus(t, bearerDo(t, srv, tok, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "ci"}), http.StatusCreated)
	if u.issuedExp != nil {
		t.Fatalf("issued expiry = %v, want none", u.issuedExp)
	}
}
