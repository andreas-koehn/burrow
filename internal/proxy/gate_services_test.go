package proxy_test

// gate_services_test.go — the gate with BURROW_SERVICES_DOMAIN set: no password
// form on the services host, and the one-time code handoff that gives a signed-in
// dashboard user a session of the services host's own.

import (
	"fmt"
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

// testNonceHash is a well-formed nonce hash for handoffs started by hand.
const testNonceHash = "0000000000000000000000000000000000000000000000000000000000000000"

func handoffTarget(ret string) string {
	return "/__burrow/handoff?return=" + url.QueryEscape(ret) + "&nonce=" + testNonceHash
}

// startLogin is the services half that starts a handoff: the gate's login
// without a services session. It returns the nonce cookie the browser now
// holds and the handoff address it is sent to.
func startLogin(t *testing.T, g http.Handler, ret string, cookies ...*http.Cookie) (*http.Cookie, *url.URL) {
	t.Helper()
	rec := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(ret), cookies...)
	if rec.Code != http.StatusFound {
		t.Fatalf("login: status %d, want 302: %s", rec.Code, rec.Body)
	}
	nonce := nonceCookieIn(rec)
	if nonce == nil {
		t.Fatalf("login set no nonce cookie: %v", rec.Header().Values("Set-Cookie"))
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return nonce, loc
}

// nonceCookieIn is the handoff nonce cookie an answer sets (or clears).
func nonceCookieIn(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, "burrow_svc_nonce_") {
			return c
		}
	}
	return nil
}

func setCookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// handoff runs the login on the services host and the dashboard half, and
// returns the callback address and the browser's nonce cookie.
func handoff(t *testing.T, g http.Handler, ret string) (*url.URL, *http.Cookie) {
	t.Helper()
	nonce, start := startLogin(t, g, ret)
	rec := gateDo(g, "GET", gateAuthDomain, start.RequestURI(), &http.Cookie{Name: "burrow_session", Value: "dash-1"})
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
	return loc, nonce
}

func callback(g http.Handler, loc *url.URL, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return gateDo(g, "GET", servicesHost, loc.RequestURI(), cookies...)
}

// A visitor without a services session is sent to the dashboard's handoff,
// never shown a password form on the services host.
func TestGateServices_LoginGoesToHandoff(t *testing.T) {
	_, _, g := servicesGate(t)
	rec := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302: %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Host != gateAuthDomain || loc.Path != "/__burrow/handoff" || loc.Query().Get("return") != svcReturn {
		t.Fatalf("Location %q, want the handoff for %q", loc, svcReturn)
	}
	// The browser is given a nonce; the handoff carries only its hash.
	nonce := nonceCookieIn(rec)
	if nonce == nil || nonce.Domain != "" || !nonce.HttpOnly || !nonce.Secure || nonce.SameSite != http.SameSiteLaxMode || nonce.MaxAge != 120 {
		t.Fatalf("nonce cookie %+v, want host-only, HttpOnly, Secure, SameSite=Lax, 2 minutes", nonce)
	}
	if h := loc.Query().Get("nonce"); len(h) != 64 || h == nonce.Value {
		t.Fatalf("handoff nonce %q: want the 64-hex hash of the cookie, not the cookie", h)
	} else if nonce.Name != "burrow_svc_nonce_"+h[:16] {
		t.Fatalf("nonce cookie %q: want it named after the first 16 characters of the hash", nonce.Name)
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
	loc, nonce := handoff(t, g, svcReturn)
	rec := callback(g, loc, nonce)
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
	if c.Value == "dash-1" || st.svcSessions[c.Value] != "user-1" || st.parents[c.Value] != "dash-1" {
		t.Fatalf("cookie value %q: want a new services session of user-1 handed off by dash-1, not the dashboard's", c.Value)
	}
	// The services host sets none of the dashboard's cookies.
	if setCookieNamed(rec, "burrow_session") != nil || setCookieNamed(rec, "burrow_csrf") != nil {
		t.Fatalf("callback set a dashboard cookie: %v", rec.Header().Values("Set-Cookie"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("callback Referrer-Policy %q, want no-referrer", rec.Header().Get("Referrer-Policy"))
	}
	if n := setCookieNamed(rec, nonce.Name); n == nil || n.MaxAge >= 0 {
		t.Errorf("callback did not clear the nonce cookie")
	}

	// With that session the gate sends the visitor straight on.
	again := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn), c)
	if again.Code != http.StatusFound || again.Header().Get("Location") != svcReturn {
		t.Fatalf("login with a services session: %d %q", again.Code, again.Header().Get("Location"))
	}
}

func TestGateServices_CodeIsSingleUse(t *testing.T) {
	_, _, g := servicesGate(t)
	loc, nonce := handoff(t, g, svcReturn)
	if rec := callback(g, loc, nonce); rec.Code != http.StatusFound {
		t.Fatalf("first callback: %d", rec.Code)
	}
	rec := callback(g, loc, nonce)
	if rec.Code != http.StatusBadRequest || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("second callback: status %d cookies %v, want 400 and no cookie", rec.Code, rec.Result().Cookies())
	}
}

func TestGateServices_CodeExpiresAfter60s(t *testing.T) {
	_, clk, g := servicesGate(t)
	loc, nonce := handoff(t, g, svcReturn)
	clk.add(61 * time.Second)
	if rec := callback(g, loc, nonce); rec.Code != http.StatusBadRequest || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("expired code: status %d, want 400 and no cookie", rec.Code)
	}
	loc, nonce = handoff(t, g, svcReturn)
	clk.add(59 * time.Second)
	if rec := callback(g, loc, nonce); rec.Code != http.StatusFound {
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

	loc, nonce := handoff(t, g, svcReturn)
	q := loc.Query()
	q.Set("return", "https://"+servicesHost+"/svc/other/")
	loc.RawQuery = q.Encode()
	if rec := callback(g, loc, nonce); rec.Code != http.StatusBadRequest || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("code with another return address: status %d, want 400 and no cookie", rec.Code)
	}
	// The code was spent by that attempt.
	q.Set("return", svcReturn)
	loc.RawQuery = q.Encode()
	if rec := callback(g, loc, nonce); rec.Code != http.StatusBadRequest {
		t.Fatalf("code after a refused exchange: status %d, want 400", rec.Code)
	}
}

// Without a dashboard session the handoff signs the visitor in on the
// dashboard's host first, then continues with the code.
func TestGateServices_HandoffSignsInFirst(t *testing.T) {
	st, _, g := servicesGate(t)
	nonce, start := startLogin(t, g, svcReturn)
	rec := gateDo(g, "GET", gateAuthDomain, start.RequestURI())
	if rec.Code != http.StatusOK {
		t.Fatalf("handoff without a session: status %d, want the sign-in form", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/__burrow/handoff"`) || !strings.Contains(body, `name="return"`) || !strings.Contains(body, `name="password"`) ||
		!strings.Contains(body, `name="nonce" value="`+start.Query().Get("nonce")+`"`) {
		t.Fatalf("sign-in form missing its parts:\n%s", body)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("sign-in form X-Frame-Options %q, want DENY", rec.Header().Get("X-Frame-Options"))
	}

	bad := gatePostForm(g, gateAuthDomain, "/__burrow/handoff", url.Values{
		"email": {"alice@example.com"}, "password": {"wrong"}, "return": {svcReturn}, "nonce": {start.Query().Get("nonce")},
	})
	if bad.Code != http.StatusOK || setCookieNamed(bad, "burrow_session") != nil {
		t.Fatalf("wrong password: status %d, want the form again and no session", bad.Code)
	}

	ok := gatePostForm(g, gateAuthDomain, "/__burrow/handoff", url.Values{
		"email": {"alice@example.com"}, "password": {"correct-password"}, "return": {svcReturn}, "nonce": {start.Query().Get("nonce")},
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
	if rec := callback(g, loc, nonce); rec.Code != http.StatusFound || setCookieNamed(rec, "burrow_svc_session") == nil {
		t.Fatalf("callback after sign-in: status %d", rec.Code)
	}
}

// A user suspended between the two halves gets no session.
func TestGateServices_SuspendedAtCallback(t *testing.T) {
	st, _, g := servicesGate(t)
	loc, nonce := handoff(t, g, svcReturn)
	st.user.Status = "suspended"
	if rec := callback(g, loc, nonce); rec.Code == http.StatusFound || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("suspended user: status %d, got a session", rec.Code)
	}
}

// Signing out on the services host ends the services session only.
func TestGateServices_Logout(t *testing.T) {
	st, _, g := servicesGate(t)
	st.svcSessions["svc-1"] = "user-1"
	rec := gateDo(g, "POST", servicesHost, "/__burrow/logout", &http.Cookie{Name: "burrow_svc_session", Value: "svc-1"})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://"+gateAuthDomain+"/" {
		t.Fatalf("logout: status %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := st.svcSessions["svc-1"]; ok {
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
	st.svcSessions["svc-1"] = "user-1"
	rec := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn),
		&http.Cookie{Name: "burrow_svc_session", Value: "svc-1"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="https://`+servicesHost+`/__burrow/logout"`) {
		t.Fatalf("access-denied page does not sign out on the services host:\n%s", rec.Body)
	}
}

// The code is bound to the browser that started the login: a callback link
// replayed into another browser (login CSRF: the attacker's own code sent to
// the victim) is refused, and the code is spent.
func TestGateServices_CallbackNeedsTheNonce(t *testing.T) {
	_, _, g := servicesGate(t)
	loc, nonce := handoff(t, g, svcReturn)
	for _, wrong := range []func(n *http.Cookie) []*http.Cookie{
		func(*http.Cookie) []*http.Cookie { return nil },
		func(n *http.Cookie) []*http.Cookie { return []*http.Cookie{{Name: n.Name, Value: "someone-elses"}} },
		func(n *http.Cookie) []*http.Cookie { return []*http.Cookie{{Name: n.Name, Value: ""}} },
		// the right value under the name of another handoff, or the bare name
		func(n *http.Cookie) []*http.Cookie {
			return []*http.Cookie{{Name: "burrow_svc_nonce_0123456789abcdef", Value: n.Value}}
		},
		func(n *http.Cookie) []*http.Cookie { return []*http.Cookie{{Name: "burrow_svc_nonce", Value: n.Value}} },
	} {
		cookies := wrong(nonce)
		if rec := callback(g, loc, cookies...); rec.Code != http.StatusBadRequest || setCookieNamed(rec, "burrow_svc_session") != nil {
			t.Fatalf("callback with nonce cookies %v: status %d, want 400 and no session", cookies, rec.Code)
		}
		loc, nonce = handoff(t, g, svcReturn)
	}
	if rec := callback(g, loc, nonce); rec.Code != http.StatusFound {
		t.Fatalf("callback with the right nonce: %d", rec.Code)
	}
	// A refused callback spends the code.
	loc, nonce = handoff(t, g, svcReturn)
	_ = callback(g, loc)
	if rec := callback(g, loc, nonce); rec.Code != http.StatusBadRequest {
		t.Fatalf("code after a refused callback: %d, want 400", rec.Code)
	}
}

// The handoff refuses to issue a code without a well-formed nonce hash.
func TestGateServices_HandoffNeedsNonceHash(t *testing.T) {
	_, _, g := servicesGate(t)
	dash := &http.Cookie{Name: "burrow_session", Value: "dash-1"}
	for _, n := range []string{"", "abc", strings.Repeat("g", 64), strings.Repeat("A", 64)} {
		target := "/__burrow/handoff?return=" + url.QueryEscape(svcReturn) + "&nonce=" + n
		if rec := gateDo(g, "GET", gateAuthDomain, target, dash); rec.Code != http.StatusBadRequest {
			t.Errorf("nonce %q: status %d, want 400", n, rec.Code)
		}
	}
}

// A new services session replaces the browser's previous one, whose row is
// deleted instead of left to expire.
func TestGateServices_CallbackReplacesOldSession(t *testing.T) {
	st, _, g := servicesGate(t)
	// A row that no longer opens anything (its user is gone), so the login
	// starts a handoff while the browser still sends the old cookie.
	st.svcSessions["svc-old"] = "user-gone"
	old := &http.Cookie{Name: "burrow_svc_session", Value: "svc-old"}
	nonce, start := startLogin(t, g, svcReturn, old)
	rec := gateDo(g, "GET", gateAuthDomain, start.RequestURI(), &http.Cookie{Name: "burrow_session", Value: "dash-1"})
	loc, _ := url.Parse(rec.Header().Get("Location"))
	res := callback(g, loc, nonce, old)
	if res.Code != http.StatusFound {
		t.Fatalf("callback: %d", res.Code)
	}
	if _, ok := st.svcSessions["svc-old"]; ok {
		t.Fatal("the previous services session row is still there")
	}
	if _, ok := st.sessionMap["dash-1"]; !ok {
		t.Fatal("the dashboard session was deleted")
	}
}

// A host name with the trailing dot of a fully qualified name is the same
// host.
func TestGateServices_TrailingDotHost(t *testing.T) {
	_, _, g := servicesGate(t)
	loc, nonce := handoff(t, g, svcReturn)
	if rec := gateDo(g, "GET", servicesHost+".", loc.RequestURI(), nonce); rec.Code != http.StatusFound || setCookieNamed(rec, "burrow_svc_session") == nil {
		t.Fatalf("callback on %s.: status %d", servicesHost, rec.Code)
	}
	if rec := gateDo(g, "GET", servicesHost+".:443", handoffTarget(svcReturn), &http.Cookie{Name: "burrow_session", Value: "dash-1"}); rec.Code != http.StatusNotFound {
		t.Fatalf("handoff on the services host with a trailing dot: status %d, want 404", rec.Code)
	}
}

// browserJar keeps the cookies one browser holds on the services host, by
// name, as the gate's answers set and clear them.
type browserJar map[string]*http.Cookie

func (j browserJar) take(rec *httptest.ResponseRecorder) {
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(j, c.Name)
			continue
		}
		j[c.Name] = c
	}
}

func (j browserJar) all() []*http.Cookie {
	var out []*http.Cookie
	for _, c := range j {
		out = append(out, c)
	}
	return out
}

// Round 2: two handoffs of one browser at the same time (two tabs opening
// two services) each keep their own nonce: both callbacks succeed, whatever
// the order, and each spends only its own nonce.
func TestGateServices_ParallelHandoffs(t *testing.T) {
	st, _, g := servicesGate(t)
	st.serviceMap["wiki"] = db.Service{ID: "svc-w", Name: "Wiki", Subdomain: "wiki"}
	st.policyMap["svc-w"] = []string{"user"}
	ret2 := "https://" + servicesHost + "/svc/wiki/page"
	dash := &http.Cookie{Name: "burrow_session", Value: "dash-1"}
	jar := browserJar{}

	starts := map[string]*url.URL{}
	for _, ret := range []string{svcReturn, ret2} {
		rec := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(ret), jar.all()...)
		if rec.Code != http.StatusFound {
			t.Fatalf("login %s: %d", ret, rec.Code)
		}
		jar.take(rec)
		starts[ret], _ = url.Parse(rec.Header().Get("Location"))
	}
	if len(jar) != 2 {
		t.Fatalf("the browser holds %d nonce cookies after two logins, want 2: %v", len(jar), jar)
	}
	callbacks := map[string]*url.URL{}
	for ret, start := range starts {
		rec := gateDo(g, "GET", gateAuthDomain, start.RequestURI(), dash)
		if rec.Code != http.StatusFound {
			t.Fatalf("handoff %s: %d %s", ret, rec.Code, rec.Body)
		}
		callbacks[ret], _ = url.Parse(rec.Header().Get("Location"))
	}
	// The second one started finishes first.
	for _, ret := range []string{ret2, svcReturn} {
		rec := callback(g, callbacks[ret], jar.all()...)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != ret || setCookieNamed(rec, "burrow_svc_session") == nil {
			t.Fatalf("callback %s: %d %q %s", ret, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
		jar.take(rec)
	}
	for name := range jar {
		if strings.HasPrefix(name, "burrow_svc_nonce") {
			t.Errorf("nonce cookie %s left after both callbacks", name)
		}
	}
}

// A browser holding many unfinished handoffs (tabs that never came back) is
// cleared at the next login, so its cookies cannot pile up.
func TestGateServices_NonceCookiesDoNotPileUp(t *testing.T) {
	_, _, g := servicesGate(t)
	var held []*http.Cookie
	for i := 0; i < 8; i++ {
		held = append(held, &http.Cookie{Name: fmt.Sprintf("burrow_svc_nonce_%016x", i), Value: "v"})
	}
	rec := gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn), held...)
	cleared, set := 0, 0
	for _, c := range rec.Result().Cookies() {
		switch {
		case strings.HasPrefix(c.Name, "burrow_svc_nonce_") && c.MaxAge < 0:
			cleared++
		case strings.HasPrefix(c.Name, "burrow_svc_nonce_"):
			set++
		}
	}
	if cleared != 8 || set != 1 {
		t.Fatalf("login with 8 nonces held: %d cleared, %d set; want 8 and 1", cleared, set)
	}
	// Fewer are left alone.
	rec = gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn), held[:7]...)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			t.Fatalf("login with 7 nonces held cleared %s", c.Name)
		}
	}
}

// Round 2: each host takes only its own kind of session. A services session
// sent to the dashboard's host as burrow_session does not sign anybody in
// there (the handoff shows the sign-in form instead of issuing a code), and a
// dashboard session sent as burrow_svc_session opens nothing on the services
// host.
func TestGateServices_EachHostTakesItsOwnSessionKind(t *testing.T) {
	st, _, g := servicesGate(t)
	st.svcSessions["svc-1"] = "user-1"
	rec := gateDo(g, "GET", gateAuthDomain, handoffTarget(svcReturn), &http.Cookie{Name: "burrow_session", Value: "svc-1"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatalf("handoff with a services session as burrow_session: %d %q, want the sign-in form", rec.Code, rec.Header().Get("Location"))
	}
	rec = gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn), &http.Cookie{Name: "burrow_svc_session", Value: "dash-1"})
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || !strings.HasPrefix(loc, "https://"+gateAuthDomain+"/__burrow/handoff") {
		t.Fatalf("services login with a dashboard session as burrow_svc_session: %d %q, want the handoff", rec.Code, loc)
	}
	rec = gateDo(g, "GET", servicesHost, "/__burrow/login?next="+url.QueryEscape(svcReturn), &http.Cookie{Name: "burrow_svc_session", Value: "svc-1"})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != svcReturn {
		t.Fatalf("services login with a services session: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// The dashboard's sign-out ends the services sessions it handed off: the
	// store does that; a callback after the dashboard session ended issues none.
	loc, nonce := handoff(t, g, svcReturn)
	delete(st.sessionMap, "dash-1")
	if rec := callback(g, loc, nonce); rec.Code == http.StatusFound || setCookieNamed(rec, "burrow_svc_session") != nil {
		t.Fatalf("callback after the dashboard session ended: %d, want no services session", rec.Code)
	}
}
