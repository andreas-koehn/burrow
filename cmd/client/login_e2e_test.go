package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// realRelay is the relay's real API router over a real store (SQLite), served
// with TLS, with an admin who can log in to the dashboard. The clock of the
// sign-in requests is the test's: the client's waits move it.
type realRelay struct {
	*relayServer
	st    *store.Store
	admin string // user id of admin@x

	mu  sync.Mutex
	now time.Time
}

func (r *realRelay) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *realRelay) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

func newRealRelay(t *testing.T) *realRelay {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	r := &realRelay{st: store.New(d), now: time.Now().UTC()}
	r.st.SetClientLoginClock(r.clock)
	ctx := context.Background()
	if err := r.st.SeedAdmin(ctx, "admin@x", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := r.st.GetUserByEmail(ctx, "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	r.admin = u.ID
	router := api.NewRouter(api.Deps{
		Users: r.st, ClientLogins: r.st, ControlListen: ":7000",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The test polls faster than a person's client would.
		ClientLoginPollLimitOverride: 100000,
	})
	r.relayServer = newRelayServer(t, router.ServeHTTP)
	return r
}

// dashboard is a browser session of admin@x: logged in, with the CSRF token
// the dashboard sends along.
type dashboard struct {
	t    *testing.T
	base string
	hc   *http.Client
	csrf string
}

func (r *realRelay) logIn(t *testing.T) *dashboard {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := r.Client()
	hc.Jar = jar
	resp, err := hc.Post(r.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"email":"admin@x","password":"password1"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard login: status %d", resp.StatusCode)
	}
	dash := &dashboard{t: t, base: r.URL, hc: hc}
	u, _ := url.Parse(r.URL)
	for _, ck := range jar.Cookies(u) {
		if ck.Name == "burrow_csrf" {
			dash.csrf = ck.Value
		}
	}
	if dash.csrf == "" {
		t.Fatal("no CSRF cookie after the dashboard login")
	}
	return dash
}

// decide approves or denies the sign-in request with the user code, as the
// /link page does.
func (d *dashboard) decide(userCode, action, body string) {
	d.t.Helper()
	req, err := http.NewRequest(http.MethodPost, d.base+"/api/v1/client/login/requests/"+userCode+"/"+action, strings.NewReader(body))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", d.csrf)
	resp, err := d.hc.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.t.Fatalf("%s: status %d: %s", action, resp.StatusCode, raw)
	}
	var v struct {
		Hostname string `json:"hostname"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Hostname != "kohns-laptop" {
		d.t.Fatalf("%s: the request is not this machine's: %s", action, raw)
	}
}

var shownCode = regexp.MustCompile(`shows the code ([A-Z0-9]{4}-[A-Z0-9]{4})\.`)

// signInAgainst runs `burrow login <relay> --no-browser` against the real
// router. While the client waits between two polls, the relay's clock moves by
// that wait, and before the first poll onWait runs with the code the client
// shows.
func signInAgainst(t *testing.T, r *realRelay, onWait func(userCode string), args ...string) (*harness, int) {
	t.Helper()
	h := browserHarness(t)
	h.realDiscover = true
	first := true
	h.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if first {
			first = false
			// The command is in its wait; nothing else writes to stdout now.
			m := shownCode.FindStringSubmatch(h.stdout.String())
			if m == nil {
				t.Errorf("no code on the screen before the wait: %q", h.stdout.String())
				return context.Canceled
			}
			if onWait != nil {
				onWait(m[1])
			}
		}
		r.advance(d)
		return nil
	}
	code := h.execWithin(append([]string{"login", r.URL, "--cacert", r.caFile(t), "--no-browser"}, args...)...)
	return h, code
}

// The whole sign-in against the relay's real handlers and store: start, an
// approval in a dashboard session while the client waits, and a token that
// then authenticates as the user who approved.
func TestLoginBrowser_EndToEnd(t *testing.T) {
	r := newRealRelay(t)
	dash := r.logIn(t)
	h, code := signInAgainst(t, r, func(userCode string) {
		dash.decide(userCode, "approve", `{"token_name":"e2e-laptop"}`)
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	got := h.stored(h.cfgPath)
	if got.Relay != r.URL || got.Control != net.JoinHostPort(r.host(t), "7000") || got.TokenName != "e2e-laptop" {
		t.Fatalf("stored relay %q, control %q, name %q", got.Relay, got.Control, got.TokenName)
	}
	user, name, err := r.st.AuthenticateNamed(context.Background(), got.Token)
	if err != nil || user != r.admin || name != "e2e-laptop" {
		t.Fatalf("the stored token authenticates as %q (token %q), err %v; want the approving user", user, name, err)
	}
	page := r.URL + "/link?code="
	if !strings.Contains(h.stdout.String(), "  "+page) || !strings.Contains(h.stdout.String(), `signed in as admin@x (token "e2e-laptop")`) {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
	if len(h.opened) != 0 {
		t.Fatal("--no-browser opened the browser")
	}
	// Neither secret is on the screen, and the token is in no request: the
	// relay made it and sent it, once.
	if strings.Contains(h.stdout.String()+h.stderr.String(), got.Token) {
		t.Fatal("the token was printed")
	}
	polls := 0
	for _, req := range r.requests() {
		if bytes.Contains([]byte(req), []byte(got.Token)) {
			t.Fatal("the token is in a request")
		}
		if strings.HasPrefix(req, "POST /api/v1/client/login/poll\n") {
			polls++
			var body struct {
				DeviceCode string `json:"device_code"`
			}
			if err := json.Unmarshal([]byte(req[strings.LastIndex(req, "\n")+1:]), &body); err != nil || body.DeviceCode == "" {
				t.Fatal("a poll without a device code")
			}
			if strings.Contains(h.stdout.String()+h.stderr.String(), body.DeviceCode) {
				t.Fatal("the device code was printed")
			}
		}
	}
	// The interval the relay asked for was kept: no poll was answered slow_down.
	if polls != 1 {
		t.Fatalf("%d polls, want 1 (the approval was there at the first)", polls)
	}
	// The client connects with what was stored.
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("http: exit %d: %s", code, h.stderr.String())
	}
	if c := h.oneRun().creds; c.Token != got.Token || c.Control != got.Control {
		t.Fatalf("connected to %q", c.Control)
	}
}

func TestLoginBrowser_EndToEnd_Denied(t *testing.T) {
	r := newRealRelay(t)
	dash := r.logIn(t)
	h, code := signInAgainst(t, r, func(userCode string) { dash.decide(userCode, "deny", "") })
	if code != exitGeneral || h.stderr.String() != "The sign-in was denied in the dashboard.\n" || h.hasConfig() {
		t.Fatalf("exit %d, stderr %q, stored %v", code, h.stderr.String(), h.hasConfig())
	}
	if ts, err := r.st.ListClientTokens(context.Background(), r.admin); err != nil || len(ts) != 0 {
		t.Fatalf("%d tokens exist after a denial, err %v", len(ts), err)
	}
}

// Nobody approves: the client polls at the relay's pace for the ten minutes
// the request lives and then stops by itself.
func TestLoginBrowser_EndToEnd_Expires(t *testing.T) {
	r := newRealRelay(t)
	h, code := signInAgainst(t, r, nil)
	if code != exitGeneral || h.stderr.String() != "The code expired. Run burrow login again.\n" || h.hasConfig() {
		t.Fatalf("exit %d, stderr %q, stored %v", code, h.stderr.String(), h.hasConfig())
	}
	var waited time.Duration
	for _, d := range h.sleeps() {
		waited += d
	}
	if waited < 9*time.Minute || waited > 10*time.Minute+time.Second {
		t.Fatalf("waited %v, want the request's ten minutes", waited)
	}
	// At the relay's pace: about one poll per 2.5 s of the ten minutes.
	polls := 0
	for _, req := range r.requests() {
		if strings.HasPrefix(req, "POST /api/v1/client/login/poll\n") {
			polls++
		}
	}
	if polls < 200 || polls > 240 {
		t.Fatalf("%d polls in ten minutes, want one per 2.5 s", polls)
	}
}

// An older client knows nothing of the sign-in endpoints and is not touched by
// them: `connect` and a stored token work against the new relay as before.
// What it needs from the relay's web address is discovery at most.
func TestLoginBrowser_EndToEnd_TokenPathAgainstTheNewRelay(t *testing.T) {
	r := newRealRelay(t)
	token, err := r.st.IssueClientToken(context.Background(), r.admin, "minted-in-the-dashboard")
	if err != nil {
		t.Fatal(err)
	}
	h := browserHarness(t)
	h.realDiscover = true
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, token+"\n")
		_ = pw.Close()
	}()
	h.stdinR = pr
	if code := h.execWithin("login", r.URL, "--cacert", r.caFile(t), "--token", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if h.stored(h.cfgPath).Token != token {
		t.Fatal("the token from stdin was not stored")
	}
	h.noToken(token)
	for _, req := range r.requests() {
		if strings.Contains(req, "/client/login/") {
			t.Fatal("login --token asked a sign-in endpoint")
		}
	}
}
