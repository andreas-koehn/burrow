package main

// e2e_services_origin_test.go — BURROW_SERVICES_DOMAIN through the real router,
// gate, proxy, store, control server and client: the dashboard's cookies never
// reach the services host's upstream, and a burrow_login service works through
// the one-time code handoff.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

const e2eServicesHost = "apps." + e2eAuthDomain

func TestE2EServicesOrigin_BurrowLoginHandoff(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	s := bootE2EStack(t, withServicesDomain(e2eServicesHost))

	var upstreamCookie atomic.Value // string
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		upstreamCookie.Store(r.Header.Get("Cookie"))
		_, _ = io.WriteString(w, "secret-app")
	})
	must(t, s.store.SetServiceAccessMode(
		context.Background(), s.userID, "admin", s.serviceID, "burrow_login", "", nil),
		"SetServiceAccessMode(burrow_login)")
	must(t, s.store.SetAccessPolicy(
		context.Background(), s.userID, "admin", s.serviceID, []string{"admin"}),
		"SetAccessPolicy(admin)")

	hc := s.pathClient(t)
	dashURL := s.pathURL("/protected?x=1")
	svcURL := "https://" + e2eServicesHost + "/svc/" + s.subdomain + "/protected?x=1"

	// next follows one redirect and checks where it points.
	next := func(from string, want int, wantHost, wantPath string) *url.URL {
		t.Helper()
		resp, err := hc.Get(from)
		must(t, err, "GET "+from)
		body := readAllString(t, resp)
		if resp.StatusCode != want {
			t.Fatalf("GET %s: status %d, want %d: %s", from, resp.StatusCode, want, body)
		}
		loc, err := resp.Location()
		must(t, err, "Location of "+from)
		if loc.Host != wantHost || loc.Path != wantPath {
			t.Fatalf("GET %s: Location %s, want %s%s", from, loc, wantHost, wantPath)
		}
		return loc
	}

	// The old address on the dashboard's host moves to the services host.
	if loc := next(dashURL, http.StatusPermanentRedirect, e2eServicesHost, "/svc/"+s.subdomain+"/protected"); loc.String() != svcURL {
		t.Fatalf("308 Location %s, want %s", loc, svcURL)
	}
	// The service sends the visitor to the gate on its own host, which sends
	// them to the dashboard's handoff.
	gate := next(svcURL, http.StatusFound, e2eServicesHost, "/__burrow/login")
	handoff := next(gate.String(), http.StatusFound, e2eAuthDomain, "/__burrow/handoff")
	if handoff.Query().Get("return") != svcURL {
		t.Fatalf("handoff return %q, want %q", handoff.Query().Get("return"), svcURL)
	}

	// Not signed in to the dashboard: the handoff asks for the password on
	// the dashboard's host, then issues the code.
	resp, err := hc.Get(handoff.String())
	must(t, err, "GET handoff")
	if body := readAllString(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, `action="/__burrow/handoff"`) {
		t.Fatalf("handoff without a session: %d %s", resp.StatusCode, body)
	}
	resp, err = hc.PostForm("https://"+e2eAuthDomain+"/__burrow/handoff", url.Values{
		"email": {e2eAdminEmail}, "password": {e2eAdminPassword}, "return": {svcURL},
	})
	must(t, err, "POST handoff")
	_ = readAllString(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("POST handoff: status %d", resp.StatusCode)
	}
	cb, err := resp.Location()
	must(t, err, "callback Location")
	if cb.Host != e2eServicesHost || cb.Path != "/__burrow/callback" {
		t.Fatalf("POST handoff Location %s, want the callback", cb)
	}
	if final := next(cb.String(), http.StatusFound, e2eServicesHost, "/svc/"+s.subdomain+"/protected"); final.String() != svcURL {
		t.Fatalf("callback Location %s, want %s", final, svcURL)
	}

	// The jar now holds the dashboard session on the dashboard's host and the
	// services session on the services host; the browser sends each only to
	// its own host. Add an app cookie on the services host.
	svcHost, _ := url.Parse("https://" + e2eServicesHost + "/")
	hc.Jar.SetCookies(svcHost, []*http.Cookie{{Name: "app", Value: "1", Path: "/", Secure: true}})
	dashHost, _ := url.Parse("https://" + e2eAuthDomain + "/")
	var names []string
	for _, c := range hc.Jar.Cookies(dashHost) {
		names = append(names, c.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "burrow_session") {
		t.Fatalf("no dashboard session after the handoff sign-in: %v", names)
	}
	for _, c := range hc.Jar.Cookies(svcHost) {
		if c.Name == "burrow_session" || c.Name == "burrow_csrf" {
			t.Fatalf("the dashboard cookie %s is sent to the services host", c.Name)
		}
	}

	resp, err = hc.Get(svcURL)
	must(t, err, "GET service")
	if body := readAllString(t, resp); resp.StatusCode != http.StatusOK || body != "secret-app" {
		t.Fatalf("service after the handoff: %d %q", resp.StatusCode, body)
	}
	if got, _ := upstreamCookie.Load().(string); got != "app=1" {
		t.Fatalf("upstream Cookie %q, want only the app's own", got)
	}

	// Even a dashboard cookie forced onto the services host goes nowhere.
	req, _ := http.NewRequest("GET", svcURL, nil)
	req.Header.Set("Cookie", "burrow_session=x; burrow_csrf=y; app=2")
	plain := s.pathClient(t)
	resp, err = plain.Do(req)
	must(t, err, "GET with forged cookies")
	_ = readAllString(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("a dashboard cookie opened the service on the services host: %d", resp.StatusCode)
	}

	// The services host offers nothing of the dashboard.
	for _, p := range []string{"/", "/api/v1/me", "/link"} {
		resp, err := hc.Get("https://" + e2eServicesHost + p)
		must(t, err, "GET services host "+p)
		_ = readAllString(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("services host %s: status %d, want 404", p, resp.StatusCode)
		}
	}
}
