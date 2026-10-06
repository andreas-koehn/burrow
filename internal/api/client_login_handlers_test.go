package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// loginTestClock is the injected clock of the sign-in tests.
type loginTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *loginTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *loginTestClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// loginAudit records the audit events the store appends.
type loginAudit struct {
	mu sync.Mutex
	ev []audit.Event
}

func (a *loginAudit) Append(_ context.Context, e any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev = append(a.ev, e.(audit.Event))
	return nil
}

func (a *loginAudit) byAction(action string) []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.Event
	for _, e := range a.ev {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func (a *loginAudit) dump() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	for _, e := range a.ev {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s|%s|%s\n", e.Action, e.ActorID, e.ActorEmail, e.SubjectID,
			e.SubjectLabel, e.SourceIP, e.RequestID, e.Payload)
	}
	return b.String()
}

// loginHarness is the router over a real store (SQLite) with an injected
// clock, an admin "admin@x" who can log in, and a recording audit log.
type loginHarness struct {
	srv   *httptest.Server
	st    *store.Store
	clk   *loginTestClock
	audit *loginAudit
	admin string // user id
}

func newLoginHarness(t *testing.T, mod func(*Deps)) *loginHarness {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st := store.New(d)
	clk := &loginTestClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	st.SetClientLoginClock(clk.now)
	al := &loginAudit{}
	st.SetAuditLogger(al)
	ctx := context.Background()
	if err := st.SeedAdmin(ctx, "admin@x", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(ctx, "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{Users: st, ClientLogins: st, Log: discardLog()}
	if mod != nil {
		mod(&deps)
	}
	srv := httptest.NewServer(NewRouter(deps))
	t.Cleanup(srv.Close)
	return &loginHarness{srv: srv, st: st, clk: clk, audit: al, admin: u.ID}
}

// anonPost sends a JSON POST without any cookie or header of a session.
func anonPost(t *testing.T, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case string:
		rdr = strings.NewReader(b)
	default:
		rdr = mustJSON(body)
	}
	resp, err := http.Post(url, "application/json", rdr)
	if err != nil {
		t.Fatal(err)
	}
	return resp, decodeMap(t, resp)
}

func decodeMap(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	raw := readBody(t, resp)
	m := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("status %d: body is not a JSON object: %s", resp.StatusCode, raw)
		}
	}
	return m
}

var startBody = map[string]string{"hostname": "laptop.local", "os": "linux", "arch": "amd64", "client_version": "0.6.0"}

func (h *loginHarness) start(t *testing.T) (deviceCode, userCode string) {
	t.Helper()
	resp, body := anonPost(t, h.srv.URL+"/api/v1/client/login/start", startBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start: status %d body %v", resp.StatusCode, body)
	}
	return body["device_code"].(string), body["user_code"].(string)
}

func (h *loginHarness) poll(t *testing.T, deviceCode string) (*http.Response, map[string]any) {
	t.Helper()
	return anonPost(t, h.srv.URL+"/api/v1/client/login/poll", map[string]string{"device_code": deviceCode})
}

func TestClientLoginStart_Anonymous(t *testing.T) {
	h := newLoginHarness(t, nil)
	resp, body := anonPost(t, h.srv.URL+"/api/v1/client/login/start", startBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d without a session, want 200: %v", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", cc)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("start set a cookie")
	}
	if len(body) != 5 {
		t.Fatalf("body has %d fields, want exactly the spec's five: %v", len(body), body)
	}
	device, _ := body["device_code"].(string)
	user, _ := body["user_code"].(string)
	if len(device) != 43 {
		t.Fatalf("device_code has %d characters, want 43 (32 bytes, base64url)", len(device))
	}
	if !regexp.MustCompile(`^[A-HJKMNP-Z2-9]{4}-[A-HJKMNP-Z2-9]{4}$`).MatchString(user) {
		t.Fatalf("user_code %q", user)
	}
	host := strings.TrimPrefix(h.srv.URL, "http://")
	if want := "http://" + host + "/link?code=" + user; body["verification_url"] != want {
		t.Fatalf("verification_url %v, want %s", body["verification_url"], want)
	}
	if body["expires_in"] != float64(600) || body["interval"] != float64(2) {
		t.Fatalf("expires_in %v interval %v, want 600 and 2", body["expires_in"], body["interval"])
	}
	// The request records where it came from, as the relay saw it.
	v, err := h.st.GetClientLogin(context.Background(), user)
	if err != nil || v.SourceIP != "127.0.0.1" || v.Hostname != "laptop.local" {
		t.Fatalf("stored request %+v (%v)", v, err)
	}
}

// The link is built from the relay's own knowledge of its scheme and the
// request's Host, never from a forwarded header.
func TestClientLoginStart_VerificationURL(t *testing.T) {
	post := func(d Deps, host string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
		h := newLoginHarness(t, nil)
		d.Users, d.ClientLogins, d.Log = h.st, h.st, discardLog()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/client/login/start", mustJSON(startBody))
		req.Host = host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		NewRouter(d).ServeHTTP(rec, req)
		m := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec, m
	}
	rec, body := post(Deps{SecureCookies: true}, "burrow.example.com", nil)
	url, _ := body["verification_url"].(string)
	if rec.Code != 200 || !strings.HasPrefix(url, "https://burrow.example.com/link?code=") {
		t.Fatalf("status %d url %q", rec.Code, url)
	}
	rec, body = post(Deps{}, "burrow.example.com:8080", map[string]string{
		"X-Forwarded-Proto": "https", "X-Forwarded-Host": "evil.example",
	})
	url, _ = body["verification_url"].(string)
	if rec.Code != 200 || !strings.HasPrefix(url, "http://burrow.example.com:8080/link?code=") {
		t.Fatalf("status %d url %q: forwarded headers must not shape the link", rec.Code, url)
	}
	// A Host that is not a plain host name gets no request and no link.
	rec, body = post(Deps{}, "evil.example/x?y=", nil)
	if rec.Code != http.StatusBadRequest || body["verification_url"] != nil || body["device_code"] != nil {
		t.Fatalf("odd Host: status %d body %v", rec.Code, body)
	}
}

func TestClientLoginStart_BodyLimitsAndShape(t *testing.T) {
	h := newLoginHarness(t, nil)
	url := h.srv.URL + "/api/v1/client/login/start"
	big := `{"hostname":"` + strings.Repeat("h", 5000) + `"}`
	if resp, _ := anonPost(t, url, big); resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body over 4 KiB: status %d, want 400 or 413", resp.StatusCode)
	}
	for _, bad := range []string{"", "not json", `["a"]`, `{"hostname":5}`} {
		if resp, _ := anonPost(t, url, bad); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q: status %d, want 400", bad, resp.StatusCode)
		}
	}
	// Untrusted display strings are cleaned at the door.
	resp, body := anonPost(t, url, map[string]string{
		"hostname": "\u001b[31mred" + strings.Repeat("h", 400), "os": "li\nnux", "arch": "amd‮64",
		"client_version": "0.6.0", "token_name": "build\u0007-box",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	v, err := h.st.GetClientLogin(context.Background(), body["user_code"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(v.Hostname)) != 253 || strings.ContainsAny(v.Hostname+v.OS+v.Arch+v.SuggestedTokenName, "\x1b\n\x07‮") {
		t.Fatalf("stored %q %q %q %q", v.Hostname[:12], v.OS, v.Arch, v.SuggestedTokenName)
	}
	if v.OS != "linux" || v.Arch != "amd64" || v.SuggestedTokenName != "build-box" {
		t.Fatalf("stored os %q arch %q name %q", v.OS, v.Arch, v.SuggestedTokenName)
	}
	// GET is not a way to start a request.
	if resp, err := http.Get(url); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET start: %v %v", resp.StatusCode, err)
	}
}

func TestClientLoginStart_PendingCapAndRateLimit(t *testing.T) {
	// The cap: 20 pending requests, the 21st is refused.
	h := newLoginHarness(t, func(d *Deps) { d.ClientLoginStartLimitOverride = 1000 })
	for i := 0; i < 20; i++ {
		h.start(t)
	}
	resp, body := anonPost(t, h.srv.URL+"/api/v1/client/login/start", startBody)
	if resp.StatusCode != http.StatusTooManyRequests || body["device_code"] != nil {
		t.Fatalf("21st pending start: status %d body %v, want 429", resp.StatusCode, body)
	}

	// The per-IP limit: 10 a minute by default.
	h = newLoginHarness(t, nil)
	for i := 0; i < ClientLoginStartRateLimitPerIP; i++ {
		h.start(t)
	}
	resp, body = anonPost(t, h.srv.URL+"/api/v1/client/login/start", startBody)
	if resp.StatusCode != http.StatusTooManyRequests || body["device_code"] != nil {
		t.Fatalf("start over the per-IP limit: status %d body %v, want 429", resp.StatusCode, body)
	}
	if ClientLoginStartRateLimitPerIP != 10 || ClientLoginPollRateLimitPerIP != 60 || ClientLoginGuessLimit != 20 {
		t.Fatalf("limits %d/%d/%d, want 10, 60 and 20", ClientLoginStartRateLimitPerIP, ClientLoginPollRateLimitPerIP, ClientLoginGuessLimit)
	}
}

func TestClientLoginPoll_RateLimitPerIP(t *testing.T) {
	h := newLoginHarness(t, func(d *Deps) { d.ClientLoginPollLimitOverride = 3 })
	for i := 0; i < 3; i++ {
		if resp, _ := h.poll(t, "unknown-device-code"); resp.StatusCode != http.StatusGone {
			t.Fatalf("poll %d: status %d, want 410", i, resp.StatusCode)
		}
	}
	resp, body := h.poll(t, "unknown-device-code")
	if resp.StatusCode != http.StatusTooManyRequests || body["error"] == "slow_down" {
		t.Fatalf("poll over the per-IP limit: status %d body %v", resp.StatusCode, body)
	}
}

func TestClientLoginRequest_Get(t *testing.T) {
	h := newLoginHarness(t, nil)
	device, user := h.start(t)
	path := "/api/v1/client/login/requests/"

	// No session: 401, for a real code and for a wrong one alike.
	for _, code := range []string{user, "AAAA-AAAA"} {
		resp, err := http.Get(h.srv.URL + path + code)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no session, code %s: %v %v, want 401", code, resp.StatusCode, err)
		}
		_ = resp.Body.Close()
	}

	c := authedClient(t, h.srv)
	h.clk.add(90 * time.Second)
	resp := c.get(t, path+user)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d %s", resp.StatusCode, raw)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	var v map[string]any
	_ = json.Unmarshal([]byte(raw), &v)
	want := map[string]any{
		"user_code": user, "hostname": "laptop.local", "os": "linux", "arch": "amd64",
		"client_version": "0.6.0", "source_ip": "127.0.0.1", "status": "pending",
		"suggested_token_name": "laptop.local", "age_seconds": float64(90),
		"created_at": "2026-10-06T12:00:00Z", "expires_at": "2026-10-06T12:10:00Z",
	}
	if len(v) != len(want) {
		t.Fatalf("view has %d fields, want %d: %s", len(v), len(want), raw)
	}
	for k, w := range want {
		if v[k] != w {
			t.Fatalf("%s = %v, want %v", k, v[k], w)
		}
	}
	if strings.Contains(raw, device) || strings.Contains(raw, "bur_") || strings.Contains(raw, "device") || strings.Contains(raw, "token\"") {
		t.Fatalf("the view leaks a device code or a token: %s", raw)
	}

	// The code is accepted as a person types it.
	plain := strings.ToLower(strings.ReplaceAll(user, "-", ""))
	if resp := c.get(t, path+plain); resp.StatusCode != http.StatusOK {
		t.Fatalf("lower case without dash: status %d", resp.StatusCode)
	}

	// Unknown and expired are the same 404.
	unknown := c.get(t, path+"AAAA-AAAA")
	unknownBody := readBody(t, unknown)
	h.clk.add(10 * time.Minute)
	expired := c.get(t, path+user)
	expiredBody := readBody(t, expired)
	if unknown.StatusCode != http.StatusNotFound || expired.StatusCode != http.StatusNotFound || unknownBody != expiredBody {
		t.Fatalf("unknown %d %q, expired %d %q: want the same 404", unknown.StatusCode, unknownBody, expired.StatusCode, expiredBody)
	}
}

func TestClientLogin_ApproveAndCollect(t *testing.T) {
	h := newLoginHarness(t, nil)
	ctx := context.Background()
	device, user := h.start(t)
	c := authedClient(t, h.srv)

	// Pending.
	resp, body := h.poll(t, device)
	if resp.StatusCode != http.StatusAccepted || body["status"] != "pending" || len(body) != 1 {
		t.Fatalf("poll while pending: %d %v, want 202 {status: pending}", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control on 202 %q", cc)
	}
	// Too fast.
	resp, body = h.poll(t, device)
	if resp.StatusCode != http.StatusTooManyRequests || body["error"] != "slow_down" {
		t.Fatalf("second poll at once: %d %v, want 429 slow_down", resp.StatusCode, body)
	}

	// Approve.
	ar := c.post(t, "/api/v1/client/login/requests/"+user+"/approve", map[string]string{"token_name": "laptop"})
	araw := readBody(t, ar)
	if ar.StatusCode != http.StatusOK || !strings.Contains(araw, `"status":"approved"`) {
		t.Fatalf("approve: %d %s", ar.StatusCode, araw)
	}
	if strings.Contains(araw, "bur_") || strings.Contains(araw, device) {
		t.Fatalf("the approval answer carries a secret: %s", araw)
	}
	if ts, _ := h.st.ListClientTokens(ctx, h.admin); len(ts) != 0 {
		t.Fatal("approval minted the token; the first poll after it must")
	}
	// Approving again changes nothing.
	again := c.post(t, "/api/v1/client/login/requests/"+user+"/approve", map[string]string{"token_name": "other"})
	if again.StatusCode != http.StatusConflict {
		t.Fatalf("second approve: status %d, want 409", again.StatusCode)
	}
	_ = again.Body.Close()
	if resp := c.post(t, "/api/v1/client/login/requests/"+user+"/deny", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("deny after approve: status %d, want 409", resp.StatusCode)
	}

	// Collect: once.
	h.clk.add(3 * time.Second)
	resp, body = h.poll(t, device)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll after approval: %d %v", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control on the token answer %q, want no-store", cc)
	}
	token, _ := body["token"].(string)
	if len(body) != 3 || !strings.HasPrefix(token, "bur_") || body["token_name"] != "laptop" || body["email"] != "admin@x" {
		t.Fatalf("poll answer has fields %d name %v email %v", len(body), body["token_name"], body["email"])
	}
	// The token is a working client token of the approver: this is the check
	// the control connection's handshake makes.
	uid, name, err := h.st.AuthenticateNamed(ctx, token)
	if err != nil || uid != h.admin || name != "laptop" {
		t.Fatalf("token authenticates as %q/%q (%v)", uid, name, err)
	}

	h.clk.add(3 * time.Second)
	resp, body = h.poll(t, device)
	if resp.StatusCode != http.StatusGone || body["token"] != nil || body["error"] != "expired_token" {
		t.Fatalf("second poll: %d, want 410 without a token", resp.StatusCode)
	}
	if ts, _ := h.st.ListClientTokens(ctx, h.admin); len(ts) != 1 {
		t.Fatalf("%d tokens, want exactly 1", len(ts))
	}
	// The request is gone for the dashboard too.
	if resp := c.get(t, "/api/v1/client/login/requests/"+user); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after collection: %d", resp.StatusCode)
	}

	// Audit: started, approved (by the approver, with the request's hostname
	// and source IP), token minted; no code and no token anywhere.
	for _, action := range []string{"client.login.started", "client.login.approved", "token.mint"} {
		evs := h.audit.byAction(action)
		if len(evs) != 1 {
			t.Fatalf("%d %s events, want 1", len(evs), action)
		}
		p := string(evs[0].Payload)
		if !strings.Contains(p, `"hostname":"laptop.local"`) || !strings.Contains(p, `"source_ip":"127.0.0.1"`) {
			t.Fatalf("%s payload %s", action, p)
		}
		if action != "client.login.started" && (evs[0].ActorID != h.admin || evs[0].ActorEmail != "admin@x") {
			t.Fatalf("%s actor %q %q, want the approver", action, evs[0].ActorID, evs[0].ActorEmail)
		}
	}
	dump := h.audit.dump()
	for _, secret := range []string{device, user, strings.ReplaceAll(user, "-", ""), token} {
		if strings.Contains(dump, secret) {
			t.Fatal("the audit log holds a code or a token")
		}
	}
}

func TestClientLogin_Deny(t *testing.T) {
	h := newLoginHarness(t, nil)
	device, user := h.start(t)
	c := authedClient(t, h.srv)

	resp := c.post(t, "/api/v1/client/login/requests/"+user+"/deny", nil)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(raw, `"status":"denied"`) {
		t.Fatalf("deny: %d %s", resp.StatusCode, raw)
	}
	evs := h.audit.byAction("client.login.denied")
	if len(evs) != 1 || evs[0].ActorID != h.admin || !strings.Contains(string(evs[0].Payload), `"hostname":"laptop.local"`) ||
		!strings.Contains(string(evs[0].Payload), `"source_ip":"127.0.0.1"`) {
		t.Fatalf("denied events %+v", evs)
	}
	if resp := c.post(t, "/api/v1/client/login/requests/"+user+"/approve", map[string]string{"token_name": "x"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("approve after deny: %d, want 409", resp.StatusCode)
	}

	pr, body := h.poll(t, device)
	if pr.StatusCode != http.StatusForbidden || body["error"] != "access_denied" || body["token"] != nil {
		t.Fatalf("poll after deny: %d %v, want 403 access_denied", pr.StatusCode, body)
	}
	h.clk.add(3 * time.Second)
	if pr, _ := h.poll(t, device); pr.StatusCode != http.StatusGone {
		t.Fatalf("poll after the denial was reported: %d, want 410", pr.StatusCode)
	}
	if ts, _ := h.st.ListClientTokens(context.Background(), h.admin); len(ts) != 0 {
		t.Fatal("a denied request minted a token")
	}
}

func TestClientLoginPoll_UnknownExpiredAndMalformed(t *testing.T) {
	h := newLoginHarness(t, nil)
	device, user := h.start(t)
	c := authedClient(t, h.srv)
	url := h.srv.URL + "/api/v1/client/login/poll"

	// The user code is not a device code.
	for _, guess := range []string{user, strings.ReplaceAll(user, "-", ""), "nope"} {
		resp, body := h.poll(t, guess)
		if resp.StatusCode != http.StatusGone || body["error"] != "expired_token" {
			t.Fatalf("poll with %q: %d %v, want 410", guess, resp.StatusCode, body)
		}
	}
	for _, bad := range []string{"", "x", `{}`, `{"device_code":""}`, `{"device_code":7}`, `{"device_code":"` + strings.Repeat("d", 5000) + `"}`} {
		resp, body := anonPost(t, url, bad)
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("poll body %.30q: status %d, want 400", bad, resp.StatusCode)
		}
		if body["token"] != nil {
			t.Fatal("a malformed poll carried a token")
		}
	}
	if resp, err := http.Get(url + "?device_code=" + device); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET poll: %v %v, want 405", resp.StatusCode, err)
	}

	// Approved, then expired before it was collected: 410, and no token.
	ar := c.post(t, "/api/v1/client/login/requests/"+user+"/approve", map[string]string{"token_name": "laptop"})
	if ar.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d", ar.StatusCode)
	}
	h.clk.add(10*time.Minute + time.Second)
	resp, body := h.poll(t, device)
	if resp.StatusCode != http.StatusGone || body["token"] != nil {
		t.Fatalf("poll after expiry: %d, want 410", resp.StatusCode)
	}
	if ts, _ := h.st.ListClientTokens(context.Background(), h.admin); len(ts) != 0 {
		t.Fatal("an expired request minted a token")
	}
	for _, p := range []string{"/approve", "/deny"} {
		if resp := c.post(t, "/api/v1/client/login/requests/"+user+p, map[string]string{"token_name": "x"}); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s after expiry: %d, want 404", p, resp.StatusCode)
		}
	}
}

// An approval is a state-changing session request: it needs the session, the
// CSRF token and the POST method. A page on another site can supply none of
// the three that matter.
func TestClientLoginApprove_NotForgeable(t *testing.T) {
	h := newLoginHarness(t, nil)
	_, user := h.start(t)
	c := authedClient(t, h.srv)
	base := h.srv.URL + "/api/v1/client/login/requests/" + user

	stillPending := func(why string) {
		t.Helper()
		v, err := h.st.GetClientLogin(context.Background(), user)
		if err != nil || v.Status != "pending" {
			t.Fatalf("%s: the request is %q (%v), want pending", why, v.Status, err)
		}
	}
	send := func(hc *http.Client, method, url string, hdr map[string]string) int {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(`{"token_name":"laptop"}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	for _, action := range []string{"/approve", "/deny"} {
		// No session at all.
		if code := send(http.DefaultClient, http.MethodPost, base+action, nil); code != http.StatusUnauthorized {
			t.Fatalf("%s without a session: %d, want 401", action, code)
		}
		// No session, but a CSRF header an attacker made up.
		if code := send(http.DefaultClient, http.MethodPost, base+action, map[string]string{"X-CSRF-Token": "made-up"}); code != http.StatusUnauthorized {
			t.Fatalf("%s without a session, with a made-up CSRF token: %d, want 401", action, code)
		}
		// The victim's cookies ride along, the header a foreign page cannot set is missing.
		if code := send(c.hc, http.MethodPost, base+action, nil); code != http.StatusForbidden {
			t.Fatalf("%s with the session but without the CSRF token: %d, want 403", action, code)
		}
		if code := send(c.hc, http.MethodPost, base+action, map[string]string{"X-CSRF-Token": "guessed"}); code != http.StatusForbidden {
			t.Fatalf("%s with a wrong CSRF token: %d, want 403", action, code)
		}
		// A link or an image cannot do it either.
		for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
			if code := send(c.hc, m, base+action, map[string]string{"X-CSRF-Token": c.csrf}); code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: %d, want 405", m, action, code)
			}
		}
		// The code in the query string of the page is not an approval.
		if code := send(c.hc, http.MethodGet, base+"?approve=1&token_name=laptop", nil); code != http.StatusOK {
			t.Fatalf("GET of the request: %d", code)
		}
		stillPending(action)
	}
	if len(h.audit.byAction("client.login.approved"))+len(h.audit.byAction("client.login.denied")) != 0 {
		t.Fatal("a refused request was audited as a decision")
	}

	// The token name is checked before anything is changed.
	for _, bad := range []any{nil, map[string]string{}, map[string]string{"token_name": "  "},
		map[string]string{"token_name": "a\nb"}, map[string]string{"token_name": strings.Repeat("x", 121)}} {
		if resp := c.post(t, "/api/v1/client/login/requests/"+user+"/approve", bad); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("approve with body %v: %d, want 400", bad, resp.StatusCode)
		}
	}
	stillPending("bad token names")
}

// permUsers is a fakeUserStore that also plays the sign-in store, so a
// session of any role can be tried without a database.
type fakeClientLogins struct {
	approved, denied, got int
}

func (f *fakeClientLogins) StartClientLogin(context.Context, store.LoginMeta) (store.LoginStart, error) {
	return store.LoginStart{}, nil
}
func (f *fakeClientLogins) GetClientLogin(context.Context, string) (store.LoginRequestView, error) {
	f.got++
	return store.LoginRequestView{Status: "pending"}, nil
}
func (f *fakeClientLogins) ApproveClientLogin(context.Context, string, string, string) (store.LoginRequestView, error) {
	f.approved++
	return store.LoginRequestView{Status: "approved"}, nil
}
func (f *fakeClientLogins) DenyClientLogin(context.Context, string, string) (store.LoginRequestView, error) {
	f.denied++
	return store.LoginRequestView{Status: "denied"}, nil
}
func (f *fakeClientLogins) PollClientLogin(context.Context, string) (store.LoginResult, error) {
	return store.LoginResult{}, store.ErrLoginNotFound
}

func TestClientLoginApprove_NeedsTokenPermission(t *testing.T) {
	for _, tc := range []struct {
		role        string
		wantApprove int
	}{
		{"admin", http.StatusOK},
		{"user", http.StatusOK}, // tokens:manage:own
		{"viewer", http.StatusForbidden},
	} {
		logins := &fakeClientLogins{}
		srv := httptest.NewServer(NewRouter(Deps{
			Users: &fakeUserStore{role: tc.role}, ClientLogins: logins, Log: discardLog(),
		}))
		c := authedClient(t, srv)
		resp := c.post(t, "/api/v1/client/login/requests/ABCD-EFGH/approve", map[string]string{"token_name": "laptop"})
		_ = resp.Body.Close()
		if resp.StatusCode != tc.wantApprove {
			t.Fatalf("role %s: approve status %d, want %d", tc.role, resp.StatusCode, tc.wantApprove)
		}
		if want := map[bool]int{true: 1, false: 0}[tc.wantApprove == http.StatusOK]; logins.approved != want {
			t.Fatalf("role %s: the store was asked to approve %d times, want %d", tc.role, logins.approved, want)
		}
		// Looking at a request and denying it need the session only.
		if resp := c.get(t, "/api/v1/client/login/requests/ABCD-EFGH"); resp.StatusCode != http.StatusOK {
			t.Fatalf("role %s: get status %d", tc.role, resp.StatusCode)
		}
		if resp := c.post(t, "/api/v1/client/login/requests/ABCD-EFGH/deny", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("role %s: deny status %d", tc.role, resp.StatusCode)
		}
		srv.Close()
	}
}

// An automation bearer token is not a dashboard session: it skips the CSRF
// check by design, so it must not be able to approve a sign-in.
func TestClientLoginRequests_RefuseBearerTokens(t *testing.T) {
	bearer := newFakeBearerStore()
	bearer.put("bua_test_0000", AutomationTokenInfo{ID: "atk1", UserID: "u-self",
		Permissions: []string{"tokens:manage:own", "tokens:manage:any"}})
	logins := &fakeClientLogins{}
	srv := httptest.NewServer(NewRouter(Deps{
		Users: &fakeUserStore{role: "admin"}, Bearer: bearer, ClientLogins: logins, Log: discardLog(),
	}))
	defer srv.Close()
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, ""}, {http.MethodPost, "/approve"}, {http.MethodPost, "/deny"},
	} {
		req, _ := http.NewRequest(r.method, srv.URL+"/api/v1/client/login/requests/ABCD-EFGH"+r.path,
			strings.NewReader(`{"token_name":"laptop"}`))
		req.Header.Set("Authorization", "Bearer bua_test_0000")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s with a bearer token: %d, want 403", r.method, r.path, resp.StatusCode)
		}
	}
	if logins.approved+logins.denied+logins.got != 0 {
		t.Fatal("a bearer token reached the sign-in store")
	}
}

// The user code is all that stands between a signed-in user and somebody
// else's request, so wrong codes are counted per user.
func TestClientLoginRequests_GuessLimit(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	h := newLoginHarness(t, func(d *Deps) {
		d.ClientLoginStartLimitOverride = 1000
		d.clientLoginGuessClock = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
	})
	_, user := h.start(t)
	c := authedClient(t, h.srv)
	path := "/api/v1/client/login/requests/"

	// Right codes are not counted, however often they are asked for.
	for i := 0; i < 30; i++ {
		if resp := c.get(t, path+user); resp.StatusCode != http.StatusOK {
			t.Fatalf("get %d of the right code: %d", i, resp.StatusCode)
		}
	}
	// Twenty wrong ones, over all three endpoints and shapes of wrong.
	for i := 0; i < 20; i++ {
		var resp *http.Response
		switch i % 4 {
		case 0:
			resp = c.get(t, path+"AAAA-AAAA")
		case 1:
			resp = c.post(t, path+"BBBB-BBBB/approve", map[string]string{"token_name": "x"})
		case 2:
			resp = c.post(t, path+"CCCC-CCCC/deny", nil)
		case 3:
			resp = c.get(t, path+"not-a-code")
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("wrong code %d: status %d, want 404", i, resp.StatusCode)
		}
	}
	// The 21st attempt is refused, and so is the right code: an attacker
	// learns nothing more this minute.
	for _, p := range []string{"DDDD-DDDD", user} {
		if resp := c.get(t, path+p); resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("get %s after 20 wrong codes: %d, want 429", p, resp.StatusCode)
		}
	}
	if resp := c.post(t, path+user+"/approve", map[string]string{"token_name": "x"}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("approve after 20 wrong codes: %d, want 429", resp.StatusCode)
	}
	if resp := c.post(t, path+user+"/deny", nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("deny after 20 wrong codes: %d, want 429", resp.StatusCode)
	}
	if v, err := h.st.GetClientLogin(context.Background(), user); err != nil || v.Status != "pending" {
		t.Fatalf("a refused attempt changed the request: %+v %v", v, err)
	}
	// A second session of the same user shares the count.
	if resp := authedClient(t, h.srv).get(t, path+user); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second session of the user: %d, want 429", resp.StatusCode)
	}
	// A minute later the user may try again.
	mu.Lock()
	now = now.Add(61 * time.Second)
	mu.Unlock()
	if resp := c.get(t, path+user); resp.StatusCode != http.StatusOK {
		t.Fatalf("after the window: %d, want 200", resp.StatusCode)
	}
}

// Without a sign-in store the endpoints answer as a relay that has none.
func TestClientLogin_NoStore(t *testing.T) {
	srv := httptest.NewServer(NewRouter(Deps{Users: &fakeUserStore{}, Log: discardLog()}))
	defer srv.Close()
	for _, p := range []string{"start", "poll"} {
		resp, body := anonPost(t, srv.URL+"/api/v1/client/login/"+p, map[string]string{"device_code": "x"})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s without a store: %d %v, want 404", p, resp.StatusCode, body)
		}
	}
}
