package proxy_test

// services_origin_test.go — the proxy side of BURROW_SERVICES_DOMAIN: what the
// access checker and the reverse proxy do for a path-routed request that the
// /svc/ adapter marked with the services host (proxy.WithPublicHost).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

const servicesHost = "apps.example.com"

// svcRequest is a path-routed request for /svc/grafana<rest>, as the adapter
// hands it to the proxy; publicHost "" leaves it on the dashboard's host.
func svcRequest(rest, publicHost string) *http.Request {
	req := httptest.NewRequest("GET", rest, nil)
	req.Host = "grafana." + gateAuthDomain
	ctx := proxy.WithPathPrefix(req.Context(), "/svc/grafana")
	if publicHost != "" {
		ctx = proxy.WithPublicHost(ctx, publicHost)
	}
	return req.WithContext(ctx)
}

func loginChecker() (*fakeGateStore, proxy.AccessChecker) {
	st := newFakeGateStore()
	st.user = db.User{ID: "user-1", Email: "alice@example.com", Role: "user", Status: "active"}
	st.policyMap["svc-g"] = []string{"user"}
	st.sessionMap["sess-1"] = "user-1"
	return st, proxy.NewAccessCheckerWithSessionsAndLogger(&fakeValidator{}, st, gateAuthDomain, testLog())
}

// On the services host the checker sends a visitor without a services session
// to the gate on that same host, and the address it comes back to is the
// services host's.
func TestAccessServicesHost_RedirectsToServicesGate(t *testing.T) {
	_, ac := loginChecker()
	res := &proxy.Resolved{ServiceID: "svc-g", AccessMode: "burrow_login"}
	ok, status, _, hdr := ac.Allow(context.Background(), res, svcRequest("/d/x?q=1", servicesHost))
	if ok || status != http.StatusFound {
		t.Fatalf("ok=%v status=%d, want a 302", ok, status)
	}
	loc, err := url.Parse(hdr.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != servicesHost || loc.Path != "/__burrow/login" {
		t.Fatalf("Location %q, want the gate on %s", hdr.Get("Location"), servicesHost)
	}
	if next, want := loc.Query().Get("next"), "https://"+servicesHost+"/svc/grafana/d/x?q=1"; next != want {
		t.Fatalf("next %q, want %q", next, want)
	}
}

// The dashboard's session cookie opens nothing on the services host: only the
// services host's own session does.
func TestAccessServicesHost_OnlyServicesSessionCounts(t *testing.T) {
	_, ac := loginChecker()
	res := &proxy.Resolved{ServiceID: "svc-g", AccessMode: "burrow_login"}

	dash := svcRequest("/", servicesHost)
	dash.AddCookie(&http.Cookie{Name: "burrow_session", Value: "sess-1"})
	if ok, _, _, _ := ac.Allow(context.Background(), res, dash); ok {
		t.Fatal("the dashboard session cookie opened a burrow_login service on the services host")
	}

	svc := svcRequest("/", servicesHost)
	svc.AddCookie(&http.Cookie{Name: "burrow_svc_session", Value: "sess-1"})
	if ok, status, _, _ := ac.Allow(context.Background(), res, svc); !ok {
		t.Fatalf("a valid services session was refused (status %d)", status)
	}

	// Without a services domain nothing changes: the dashboard session counts,
	// the services cookie does not.
	plain := svcRequest("/", "")
	plain.AddCookie(&http.Cookie{Name: "burrow_svc_session", Value: "sess-1"})
	if ok, _, _, _ := ac.Allow(context.Background(), res, plain); ok {
		t.Fatal("burrow_svc_session opened a service on the dashboard's host")
	}
	plain = svcRequest("/", "")
	plain.AddCookie(&http.Cookie{Name: "burrow_session", Value: "sess-1"})
	if ok, _, _, _ := ac.Allow(context.Background(), res, plain); !ok {
		t.Fatal("the dashboard session no longer opens a service without a services domain")
	}
}

// The upstream sees the host the visitor used, and never the services
// session: not in the request, and an upstream cannot set it either.
func TestProxyServicesHost_ForwardedHostAndCookies(t *testing.T) {
	var gotXFH, gotCookie string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFH = r.Header.Get("X-Forwarded-Host")
		gotCookie = r.Header.Get("Cookie")
		w.Header().Add("Set-Cookie", "burrow_svc_session=fixed; Path=/")
		w.Header().Add("Set-Cookie", "app=2")
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("sv", &proxy.Resolved{ServiceID: "svc-sv", AccessMode: "open", LocalHost: "127.0.0.1:3000"})
	p := proxy.New(d, openChecker{}, authDomain, testLog())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := proxy.WithPublicHost(proxy.WithPathPrefix(r.Context(), "/svc/sv"), servicesHost)
		p.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "sv." + authDomain
	req.Header.Set("Cookie", "burrow_svc_session=secret; app=1; burrow_session=s; burrow_csrf=t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotXFH != servicesHost {
		t.Errorf("X-Forwarded-Host %q, want %q", gotXFH, servicesHost)
	}
	if gotCookie != "app=1" {
		t.Errorf("upstream Cookie %q, want %q", gotCookie, "app=1")
	}
	if got := resp.Header.Values("Set-Cookie"); !reflect.DeepEqual(got, []string{"app=2"}) {
		t.Errorf("Set-Cookie %q, want only the app's own", got)
	}
}
