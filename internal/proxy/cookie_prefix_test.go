package proxy_test

// cookie_prefix_test.go — the gate and the proxy with __Host- cookie names:
// what they write, which cookie they read when a bare one of the same base
// name tries to shadow it, and what an upstream never sees or sets.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

// assertHostCookie fails unless c is set, Secure, Path=/ and has no Domain:
// the attributes a browser demands of a __Host- cookie.
func assertHostCookie(t *testing.T, c *http.Cookie, name string) {
	t.Helper()
	if c == nil {
		t.Fatalf("no %s cookie set", name)
	}
	if !c.Secure || c.Path != "/" || c.Domain != "" {
		t.Errorf("%s: Secure=%v Path=%q Domain=%q, want Secure, / and no Domain", name, c.Secure, c.Path, c.Domain)
	}
}

// With Secure cookies the gate's password form writes the dashboard's two
// cookies under their __Host- names.
func TestGatePostLogin_SecureWritesHostCookies(t *testing.T) {
	st := newFakeGateStore()
	st.user = db.User{ID: "user-1", Email: "alice@example.com", Role: "user", Status: "active"}
	g := proxy.NewGate(st, gateAuthDomain, true, testLog())
	rec := postLoginForm(g, map[string]string{"email": "alice@example.com", "password": "correct-password", "next": "https://" + gateAuthDomain + "/svc/grafana/"})
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	assertHostCookie(t, setCookieNamed(rec, "__Host-burrow_session"), "__Host-burrow_session")
	assertHostCookie(t, setCookieNamed(rec, "__Host-burrow_csrf"), "__Host-burrow_csrf")
	for _, bare := range []string{"burrow_session", "burrow_csrf"} {
		if c := setCookieNamed(rec, bare); c != nil && c.MaxAge >= 0 {
			t.Errorf("the bare %s was written: %+v", bare, c)
		}
	}
}

// A bare burrow_session planted with Domain= by another host of the site
// does not shadow the __Host- session at the gate or the access checker.
func TestGateAndChecker_HostSessionCannotBeShadowed(t *testing.T) {
	st, ac := loginChecker()
	st.sessionMap["sess-planted"] = "user-2" // another user, no access
	res := &proxy.Resolved{ServiceID: "svc-g", AccessMode: "burrow_login"}
	req := svcRequest("/", "")
	req.Header.Set("Cookie", "burrow_session=sess-planted; __Host-burrow_session=sess-1")
	if ok, status, _, _ := ac.Allow(context.Background(), res, req); !ok {
		t.Fatalf("the __Host- session was shadowed by a bare cookie (status %d)", status)
	}
	// On the services host the same holds for the services session.
	req = svcRequest("/", servicesHost)
	req.Header.Set("Cookie", "burrow_svc_session=nope; __Host-burrow_svc_session=svc-s1")
	if ok, status, _, _ := ac.Allow(context.Background(), res, req); !ok {
		t.Fatalf("the __Host- services session was shadowed (status %d)", status)
	}

	gst := newFakeGateStore()
	gst.user = db.User{ID: "user-1", Email: "alice@example.com", Role: "user", Status: "active"}
	gst.sessionMap["sess-1"] = "user-1"
	g := proxy.NewGate(gst, gateAuthDomain, true, testLog())
	next := "https://" + gateAuthDomain + "/svc/grafana/"
	greq := httptest.NewRequest("GET", "/__burrow/login?next="+url.QueryEscape(next), nil)
	greq.Header.Set("Cookie", "burrow_session=nope; __Host-burrow_session=sess-1")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, greq)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != next {
		t.Fatalf("gate login with a shadowed session: %d %q, want the redirect to next", rec.Code, rec.Header().Get("Location"))
	}
}

// The services host's cookies are always Secure and so always carry the
// __Host- prefix: the handoff nonce (now on Path=/) and the session.
func TestGateServices_HostCookies(t *testing.T) {
	_, _, g := servicesGate(t)
	loc, nonce := handoff(t, g, svcReturn)
	if !strings.HasPrefix(nonce.Name, "__Host-burrow_svc_nonce_") {
		t.Fatalf("nonce cookie %q, want the __Host- name", nonce.Name)
	}
	assertHostCookie(t, nonce, nonce.Name)
	rec := callback(g, loc, nonce)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	assertHostCookie(t, setCookieNamed(rec, "__Host-burrow_svc_session"), "__Host-burrow_svc_session")
	if c := setCookieNamed(rec, nonce.Name); c == nil || c.MaxAge >= 0 || c.Path != "/" {
		t.Errorf("callback did not clear the nonce on /: %+v", c)
	}
}

// A handoff begun before the upgrade holds its nonce under the bare name on
// /__burrow/: its callback still works, and clears that cookie on its path.
func TestGateServices_LegacyNonceStillWorks(t *testing.T) {
	_, clk, g := servicesGate(t)
	loc, nonce := handoff(t, g, svcReturn)
	clk.add(time.Second)
	legacy := &http.Cookie{Name: strings.TrimPrefix(nonce.Name, "__Host-"), Value: nonce.Value}
	rec := callback(g, loc, legacy)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback with the bare nonce name: %d %s", rec.Code, rec.Body)
	}
	if c := setCookieNamed(rec, legacy.Name); c == nil || c.MaxAge >= 0 || c.Path != "/__burrow/" {
		t.Errorf("the bare nonce was not cleared on its path: %+v", c)
	}
}

// The upstream never sees the __Host- cookies and cannot set them.
func TestProxy_HostCookiesStrippedAndDropped(t *testing.T) {
	var gotCookie string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		for _, n := range []string{"__Host-burrow_session", "__Host-burrow_csrf", "__Host-burrow_svc_session", "__Host-burrow_svc_nonce_0123456789abcdef"} {
			w.Header().Add("Set-Cookie", n+"=fixed; Path=/; Secure")
		}
		w.Header().Add("Set-Cookie", "app=2")
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("sv", &proxy.Resolved{ServiceID: "svc-sv", AccessMode: "open", LocalHost: "127.0.0.1:3000"})
	p := proxy.New(d, openChecker{}, authDomain, testLog())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(w, r.WithContext(proxy.WithPathPrefix(r.Context(), "/svc/sv")))
	}))
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "sv." + authDomain
	req.Header.Set("Cookie", "__Host-burrow_session=s; app=1; __Host-burrow_csrf=t; __Host-burrow_svc_session=v; __Host-burrow_svc_nonce_0123456789abcdef=n")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotCookie != "app=1" {
		t.Errorf("upstream Cookie %q, want %q", gotCookie, "app=1")
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 1 || got[0] != "app=2" {
		t.Errorf("Set-Cookie reaching the browser: %q, want only app=2", got)
	}
}

// Login CSRF on the gate: the password form (single origin) and the
// handoff's password form refuse what a page on another origin sends,
// a services host included, before the password is checked.
func TestGate_PasswordFormsRefuseCrossSite(t *testing.T) {
	_, gate := newTestGateWithUser("user", "active")
	_, _, sgate := servicesGate(t)
	next := "https://" + gateAuthDomain + "/svc/grafana/"
	forms := []struct {
		name string
		g    http.Handler
		path string
		form url.Values
	}{
		{"login", gate, "/__burrow/login", url.Values{"email": {"alice@example.com"}, "password": {"correct-password"}, "next": {next}}},
		{"handoff", sgate, "/__burrow/handoff", url.Values{"email": {"alice@example.com"}, "password": {"correct-password"}, "return": {svcReturn}, "nonce": {testNonceHash}}},
	}
	for _, f := range forms {
		for _, h := range []map[string]string{
			{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
			{"Sec-Fetch-Site": "same-site", "Origin": "https://" + servicesHost},
			{"Origin": "https://evil.example"},
		} {
			req := httptest.NewRequest("POST", f.path, strings.NewReader(f.form.Encode()))
			req.Host = gateAuthDomain
			req.RemoteAddr = "1.2.3.4:5678"
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range h {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			f.g.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) > 0 {
				t.Errorf("%s with %v: status %d cookies %v, want 403 and none", f.name, h, rec.Code, rec.Result().Cookies())
			}
		}
		// The form submitted from the gate's own page still signs in.
		req := httptest.NewRequest("POST", f.path, strings.NewReader(f.form.Encode()))
		req.Host = gateAuthDomain
		req.RemoteAddr = "1.2.3.4:5678"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		f.g.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Errorf("%s same-origin: status %d, want 302: %s", f.name, rec.Code, rec.Body)
		}
	}
}
