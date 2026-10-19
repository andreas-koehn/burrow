package proxy_test

// gate_services_test.go — the gate with BURROW_SERVICES_DOMAIN set: no password
// form on the services host, and the one-time code handoff that gives a signed-in
// dashboard user a session of the services host's own.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

type gateClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *gateClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *gateClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// servicesGate is a gate on dashboard host gateAuthDomain and services host
// servicesHost, with user-1 (role user) signed in to the dashboard as dash-1,
// and service grafana open to the role user.
func servicesGate(t *testing.T) (*fakeGateStore, *gateClock, http.Handler) {
	t.Helper()
	st := newFakeGateStore()
	st.user = db.User{ID: "user-1", Email: "alice@example.com", Role: "user", Status: "active"}
	st.sessionMap["dash-1"] = "user-1"
	st.serviceMap["grafana"] = db.Service{ID: "svc-g", Name: "Grafana", Subdomain: "grafana"}
	st.policyMap["svc-g"] = []string{"user"}
	clk := &gateClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	g := proxy.NewGate(st, gateAuthDomain, false, testLog(),
		proxy.WithServicesDomain(servicesHost), proxy.WithGateClock(clk.now))
	return st, clk, g
}

// gateDo sends one request to the gate as the browser would on host.
func gateDo(g http.Handler, method, host, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	req.RemoteAddr = "1.2.3.4:5678"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

func gatePostForm(g http.Handler, host, target string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	req.Host = host
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

const svcReturn = "https://" + servicesHost + "/svc/grafana/d/x?q=1"

func handoffTarget(ret string) string { return "/__burrow/handoff?return=" + url.QueryEscape(ret) }

func setCookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// handoff runs the dashboard half and returns the callback address.
func handoff(t *testing.T, g http.Handler, ret string) *url.URL {
	t.Helper()
	rec := gateDo(g, "GET", gateAuthDomain, handoffTarget(ret), &http.Cookie{Name: "burrow_session", Value: "dash-1"})
	if rec.Code != http.StatusFound {
		t.Fatalf("handoff: status %d, want 302: %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Scheme != "https" || loc.Host != servicesHost || loc.Path != "/__burrow/callback" {
		t.Fatalf("handoff Location %q, want the callback on %s", loc, servicesHost)
	}
	if loc.Query().Get("return") != ret || len(loc.Query().Get("code")) < 32 {
		t.Fatalf("handoff Location %q: wrong return or no code", loc)
	}
	return loc
}

func callback(g http.Handler, loc *url.URL) *httptest.ResponseRecorder {
	return gateDo(g, "GET", servicesHost, loc.RequestURI())
}

// A visitor without a services session is sent to the dashboard's handoff,
// never shown a password form on the services host.
func TestGateServices_LoginGoesToHandoff(t *testing.T) {
	_, _, g := servicesGate(t)
	rec := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302: %s", rec.Code, rec.Body)
	}
	if want := "https://" + gateAuthDomain + handoffTarget(svcReturn); rec.Header().Get("Location") != want {
		t.Fatalf("Location %q, want %q", rec.Header().Get("Location"), want)
	}
	// The password form is not offered on the services host at all.
	post := gatePostForm(g, servicesHost, "/__burrow/login", url.Values{
		"email": {"alice@example.com"}, "password": {"correct-password"}, "next": {svcReturn},
	})
	if post.Code != http.StatusNotFound || setCookieNamed(post, "burrow_session") != nil {
		t.Fatalf("POST login on the services host: status %d, cookies %v; want 404 and no session", post.Code, post.Result().Cookies())
	}
}

// Each half of the handoff answers only on its own host, and the gate's
// services-host routes not at all on the dashboard's host.
func TestGateServices_RoutesOnlyOnTheirHost(t *testing.T) {
	_, _, g := servicesGate(t)
	dash := &http.Cookie{Name: "burrow_session", Value: "dash-1"}
	for _, tc := range []struct{ method, host, target string }{
		{"GET", servicesHost, handoffTarget(svcReturn)},
		{"GET", gateAuthDomain, "/__burrow/callback?code=x&return=" + url.QueryEscape(svcReturn)},
		{"GET", gateAuthDomain, "/__burrow/login?next=" + url.QueryEscape(svcReturn)},
		{"POST", gateAuthDomain, "/__burrow/login"},
		{"POST", gateAuthDomain, "/__burrow/logout"},
	} {
		if rec := gateDo(g, tc.method, tc.host, tc.target, dash); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s%s: status %d, want 404", tc.method, tc.host, tc.target, rec.Code)
		}
	}
}

// The whole handoff: the dashboard issues a code, the services host exchanges
// it for a session of its own in a host-only cookie and sends the visitor on.
func TestGateServices_HandoffHappyPath(t *testing.T) {
	st, _, g := servicesGate(t)
	loc := handoff(t, g, svcReturn)
	rec := callback(g, loc)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != svcReturn {
		t.Fatalf("callback: status %d Location %q, want 302 to %q", rec.Code, rec.Header().Get("Location"), svcReturn)
	}
	c := setCookieNamed(rec, "burrow_svc_session")
	if c == nil {
		t.Fatalf("no burrow_svc_session cookie: %v", rec.Header().Values("Set-Cookie"))
	}
	if c.Domain != "" || c.Path != "/" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie %+v, want host-only, Path=/, HttpOnly, Secure, SameSite=Lax", c)
	}
	if c.Value == "dash-1" || st.sessionMap[c.Value] != "user-1" {
		t.Fatalf("cookie value %q: want a new session of user-1, not the dashboard's", c.Value)
	}
	// The services host sets none of the dashboard's cookies.
	if setCookieNamed(rec, "burrow_session") != nil || setCookieNamed(rec, "burrow_csrf") != nil {
		t.Fatalf("callback set a dashboard cookie: %v", rec.Header().Values("Set-Cookie"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("callback Referrer-Policy %q, want no-referrer", rec.Header().Get("Referrer-Policy"))
	}

	// With that session the gate sends the visitor straight on.
	again := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn), c)
	if again.Code != http.StatusFound || again.Header().Get("Location") != svcReturn {
		t.Fatalf("login with a services session: %d %q", again.Code, again.Header().Get("Location"))
	}
}

func TestGateServices_CodeIsSingleUse(t *testing.T) {
	_, _, g := servicesGate(t)
	loc := handoff(t, g, svcReturn)
	if rec := callback(g, loc); rec.Code != http.StatusFound {
		t.Fatalf("first callback: %d", rec.Code)
	}
	rec := callback(g, loc)
	if rec.Code != http.StatusBadRequest || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("second callback: status %d cookies %v, want 400 and no cookie", rec.Code, rec.Result().Cookies())
	}
}

func TestGateServices_CodeExpiresAfter60s(t *testing.T) {
	_, clk, g := servicesGate(t)
	loc := handoff(t, g, svcReturn)
	clk.add(61 * time.Second)
	if rec := callback(g, loc); rec.Code != http.StatusBadRequest || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("expired code: status %d, want 400 and no cookie", rec.Code)
	}
	loc = handoff(t, g, svcReturn)
	clk.add(59 * time.Second)
	if rec := callback(g, loc); rec.Code != http.StatusFound {
		t.Fatalf("code at 59 s: status %d, want 302", rec.Code)
	}
}

// The return address must be a /svc/ address on the services host, at the
// handoff and at the callback, and a code is good only for the address it was
// issued for.
func TestGateServices_ForeignReturnRefused(t *testing.T) {
	_, _, g := servicesGate(t)
	dash := &http.Cookie{Name: "burrow_session", Value: "dash-1"}
	for _, ret := range []string{
		"https://evil.example.com/svc/grafana/",
		"https://" + gateAuthDomain + "/svc/grafana/",
		"https://" + servicesHost + "/api/v1/me",
		"https://" + servicesHost + "/svc/grafana/../../api/v1/me",
		"http://" + servicesHost + "/svc/grafana/",
		"https://sub." + servicesHost + "/svc/grafana/",
		"//" + servicesHost + "/svc/grafana/",
		"",
	} {
		rec := gateDo(g, "GET", gateAuthDomain, handoffTarget(ret), dash)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("handoff return %q: status %d Location %q, want 400", ret, rec.Code, rec.Header().Get("Location"))
		}
	}

	loc := handoff(t, g, svcReturn)
	q := loc.Query()
	q.Set("return", "https://"+servicesHost+"/svc/other/")
	loc.RawQuery = q.Encode()
	if rec := callback(g, loc); rec.Code != http.StatusBadRequest || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("code with another return address: status %d, want 400 and no cookie", rec.Code)
	}
	// The code was spent by that attempt.
	q.Set("return", svcReturn)
	loc.RawQuery = q.Encode()
	if rec := callback(g, loc); rec.Code != http.StatusBadRequest {
		t.Fatalf("code after a refused exchange: status %d, want 400", rec.Code)
	}
}

// Without a dashboard session the handoff signs the visitor in on the
// dashboard's host first, then continues with the code.
func TestGateServices_HandoffSignsInFirst(t *testing.T) {
	st, _, g := servicesGate(t)
	rec := gateDo(g, "GET", gateAuthDomain, handoffTarget(svcReturn))
	if rec.Code != http.StatusOK {
		t.Fatalf("handoff without a session: status %d, want the sign-in form", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/__burrow/handoff"`) || !strings.Contains(body, `name="return"`) || !strings.Contains(body, `name="password"`) {
		t.Fatalf("sign-in form missing its parts:\n%s", body)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("sign-in form X-Frame-Options %q, want DENY", rec.Header().Get("X-Frame-Options"))
	}

	bad := gatePostForm(g, gateAuthDomain, "/__burrow/handoff", url.Values{
		"email": {"alice@example.com"}, "password": {"wrong"}, "return": {svcReturn},
	})
	if bad.Code != http.StatusOK || setCookieNamed(bad, "burrow_session") != nil {
		t.Fatalf("wrong password: status %d, want the form again and no session", bad.Code)
	}

	ok := gatePostForm(g, gateAuthDomain, "/__burrow/handoff", url.Values{
		"email": {"alice@example.com"}, "password": {"correct-password"}, "return": {svcReturn},
	})
	if ok.Code != http.StatusFound {
		t.Fatalf("sign-in: status %d, want 302", ok.Code)
	}
	sess := setCookieNamed(ok, "burrow_session")
	if sess == nil || st.sessionMap[sess.Value] != "user-1" || sess.Domain != "" {
		t.Fatalf("sign-in: dashboard session cookie %+v", sess)
	}
	if setCookieNamed(ok, "burrow_csrf") == nil {
		t.Fatal("sign-in: no CSRF cookie for the dashboard")
	}
	loc, _ := url.Parse(ok.Header().Get("Location"))
	if loc.Host != servicesHost || loc.Path != "/__burrow/callback" {
		t.Fatalf("sign-in Location %q, want the callback", loc)
	}
	if rec := callback(g, loc); rec.Code != http.StatusFound || setCookieNamed(rec, "burrow_svc_session") == nil {
		t.Fatalf("callback after sign-in: status %d", rec.Code)
	}
}

// A user suspended between the two halves gets no session.
func TestGateServices_SuspendedAtCallback(t *testing.T) {
	st, _, g := servicesGate(t)
	loc := handoff(t, g, svcReturn)
	st.user.Status = "suspended"
	if rec := callback(g, loc); rec.Code == http.StatusFound || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("suspended user: status %d, got a session", rec.Code)
	}
}

// Signing out on the services host ends the services session only.
func TestGateServices_Logout(t *testing.T) {
	st, _, g := servicesGate(t)
	st.sessionMap["svc-1"] = "user-1"
	rec := gateDo(g, "POST", servicesHost, "/__burrow/logout", &http.Cookie{Name: "burrow_svc_session", Value: "svc-1"})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://"+gateAuthDomain+"/" {
		t.Fatalf("logout: status %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := st.sessionMap["svc-1"]; ok {
		t.Fatal("the services session survived the logout")
	}
	if _, ok := st.sessionMap["dash-1"]; !ok {
		t.Fatal("the dashboard session was ended too")
	}
	c := setCookieNamed(rec, "burrow_svc_session")
	if c == nil || c.MaxAge >= 0 {
		t.Fatalf("logout did not clear burrow_svc_session: %v", rec.Header().Values("Set-Cookie"))
	}
	if setCookieNamed(rec, "burrow_session") != nil || setCookieNamed(rec, "burrow_csrf") != nil {
		t.Fatal("logout on the services host touched a dashboard cookie")
	}
}

// A services session whose role the service does not allow sees the
// access-denied page, whose sign-out stays on the services host.
func TestGateServices_WrongRoleDenied(t *testing.T) {
	st, _, g := servicesGate(t)
	st.policyMap["svc-g"] = []string{"admin"}
	st.sessionMap["svc-1"] = "user-1"
	rec := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn),
		&http.Cookie{Name: "burrow_svc_session", Value: "svc-1"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="https://`+servicesHost+`/__burrow/logout"`) {
		t.Fatalf("access-denied page does not sign out on the services host:\n%s", rec.Body)
	}
}
