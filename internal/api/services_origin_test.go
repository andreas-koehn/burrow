package api

// services_origin_test.go — the router with BURROW_SERVICES_DOMAIN set: the
// services host serves /svc/* and /__burrow/* only, the dashboard's host
// redirects /svc/ there and keeps only the handoff of the gate.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/go-chi/chi/v5"
)

const (
	originDash = "burrow.example.com"
	originSvc  = "apps.example.com"
)

// originRecorder records which of the router's parts answered a request.
type originRecorder struct {
	hit                 string
	prefix, publicHost  string
	synthesizedHost     string
	cookieAtTunnelProxy string
	gatePath            string
}

func originRouter(t *testing.T, servicesDomain string) (*originRecorder, http.Handler) {
	t.Helper()
	rec := &originRecorder{}
	tunnel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hit = "tunnel"
		rec.prefix = proxy.PathPrefix(r.Context())
		rec.publicHost = proxy.PublicHost(r.Context())
		rec.synthesizedHost = r.Host
		rec.cookieAtTunnelProxy = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusTeapot)
	})
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hit = "gate"
		rec.gatePath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	})
	spa := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hit = "spa"
		w.WriteHeader(http.StatusOK)
	})
	return rec, NewRouter(Deps{
		Users: &fakeUserStore{}, Log: discardLog(),
		AuthDomain: originDash, ServicesDomain: servicesDomain,
		TunnelProxy: tunnel, Gate: gate, SPA: spa,
	})
}

func originDo(h http.Handler, method, host, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The dashboard's host answers every /svc/ address with a permanent redirect
// to the same address on the services host, method, path and query kept.
func TestServicesOrigin_DashboardRedirectsSvc(t *testing.T) {
	rec, h := originRouter(t, originSvc)
	for _, tc := range []struct{ method, target string }{
		{"GET", "/svc/grafana/"},
		{"GET", "/svc/grafana/d/a%20b?x=1&y=%2F"},
		{"POST", "/svc/grafana/api/save"},
		{"GET", "/svc/grafana"},
	} {
		rec.hit = ""
		rr := originDo(h, tc.method, originDash, tc.target)
		if rr.Code != http.StatusPermanentRedirect {
			t.Errorf("%s %s: status %d, want 308", tc.method, tc.target, rr.Code)
		}
		if want := "https://" + originSvc + tc.target; rr.Header().Get("Location") != want {
			t.Errorf("%s %s: Location %q, want %q", tc.method, tc.target, rr.Header().Get("Location"), want)
		}
		if rec.hit != "" {
			t.Errorf("%s %s: reached %s on the dashboard's host", tc.method, tc.target, rec.hit)
		}
	}
}

// The services host serves the tunnelled apps, marked as on that host.
func TestServicesOrigin_ServicesHostServesSvc(t *testing.T) {
	rec, h := originRouter(t, originSvc)
	for _, host := range []string{originSvc, originSvc + ":443", "APPS.example.com"} {
		*rec = originRecorder{}
		req := httptest.NewRequest("GET", "/svc/grafana/x?q=1", nil)
		req.Host = host
		req.Header.Set("Cookie", "burrow_svc_session=s; app=1")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusTeapot || rec.hit != "tunnel" {
			t.Fatalf("host %s: status %d hit %q, want the tunnel proxy", host, rr.Code, rec.hit)
		}
		if rec.prefix != "/svc/grafana" || rec.publicHost != originSvc || rec.synthesizedHost != "grafana."+originDash {
			t.Fatalf("host %s: prefix %q public host %q host %q", host, rec.prefix, rec.publicHost, rec.synthesizedHost)
		}
	}
}

// The services host serves nothing but /svc/* and the gate: no dashboard, no
// API, no MCP, no AI, no probes.
func TestServicesOrigin_ServicesHostServesNothingElse(t *testing.T) {
	rec, h := originRouter(t, originSvc)
	for _, target := range []string{"/", "/link", "/login", "/api/v1/me", "/api/v1/auth/login", "/healthz", "/metrics",
		"/mcp/burrow", "/openai/v1/models", "/install.sh", "/svc", "/svcx/a", "/assets/index.js"} {
		for _, method := range []string{"GET", "POST"} {
			rec.hit = ""
			rr := originDo(h, method, originSvc, target)
			if rr.Code != http.StatusNotFound || rec.hit != "" {
				t.Errorf("%s %s on the services host: status %d hit %q, want 404 from nobody", method, target, rr.Code, rec.hit)
			}
		}
	}
	rec.hit = ""
	if rr := originDo(h, "GET", originSvc, "/__burrow/login?next=x"); rec.hit != "gate" || rr.Code != http.StatusAccepted {
		t.Fatalf("gate on the services host: status %d hit %q", rr.Code, rec.hit)
	}
}

// On the dashboard's host the gate answers the handoff only.
func TestServicesOrigin_DashboardGateIsHandoffOnly(t *testing.T) {
	rec, h := originRouter(t, originSvc)
	for _, target := range []string{"/__burrow/login", "/__burrow/callback?code=x", "/__burrow/logout"} {
		rec.hit = ""
		if rr := originDo(h, "GET", originDash, target); rr.Code != http.StatusNotFound || rec.hit != "" {
			t.Errorf("GET %s on the dashboard's host: status %d hit %q, want 404", target, rr.Code, rec.hit)
		}
	}
	rec.hit = ""
	if rr := originDo(h, "GET", originDash, "/__burrow/handoff?return=x"); rec.hit != "gate" || rr.Code != http.StatusAccepted {
		t.Fatalf("handoff on the dashboard's host: status %d hit %q", rr.Code, rec.hit)
	}
	// The dashboard itself is unchanged.
	rec.hit = ""
	if rr := originDo(h, "GET", originDash, "/services"); rec.hit != "spa" || rr.Code != http.StatusOK {
		t.Fatalf("dashboard page: status %d hit %q", rr.Code, rec.hit)
	}
}

// Without a services domain everything stays on one host, as before.
func TestServicesOrigin_UnsetKeepsOneHost(t *testing.T) {
	rec, h := originRouter(t, "")
	if rr := originDo(h, "GET", originDash, "/svc/grafana/"); rr.Code != http.StatusTeapot || rec.publicHost != "" {
		t.Fatalf("/svc/ without a services domain: status %d public host %q", rr.Code, rec.publicHost)
	}
	rec.hit = ""
	if rr := originDo(h, "GET", originDash, "/__burrow/login"); rec.hit != "gate" {
		t.Fatalf("gate without a services domain: status %d hit %q", rr.Code, rec.hit)
	}
}

// Every service address the API prints is on the services host when one is
// set, on the dashboard's host otherwise, and absent for a gateway-only one.
func TestServicesOrigin_ServiceURL(t *testing.T) {
	for _, tc := range []struct {
		d    Deps
		slug string
		gw   bool
		want string
	}{
		{Deps{AuthDomain: originDash, ServicesDomain: originSvc}, "k7p2qx", false, "https://" + originSvc + "/svc/k7p2qx/"},
		{Deps{AuthDomain: originDash}, "k7p2qx", false, "https://" + originDash + "/svc/k7p2qx/"},
		{Deps{AuthDomain: originDash, ServicesDomain: originSvc}, "k7p2qx", true, ""},
		{Deps{AuthDomain: originDash, ServicesDomain: originSvc}, "", false, ""},
		{Deps{}, "k7p2qx", false, ""},
	} {
		if got := tc.d.serviceURL(tc.slug, tc.gw); got != tc.want {
			t.Errorf("%+v slug %q gw %v: %q, want %q", tc.d.ServicesDomain, tc.slug, tc.gw, got, tc.want)
		}
	}
}

// ServicePathHandler marks the request with the services host only when it
// is given one.
func TestServicePathHandler_PublicHost(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = proxy.PublicHost(r.Context()) })
	for _, host := range []string{originSvc, ""} {
		r := chi.NewRouter()
		r.Handle("/svc/{slug}/*", ServicePathHandler(next, originDash, host))
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/svc/abc123/x", nil))
		if got != host {
			t.Fatalf("public host %q, want %q", got, host)
		}
	}
}
