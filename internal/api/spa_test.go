package api

import (
	"net/http"
	"testing"

	"github.com/ankoehn/burrow/internal/aigateway"
)

func spaSpy() (http.Handler, *bool) {
	hit := false
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<div id=\"root\"></div>"))
	}), &hit
}

func TestSPAMountedServesNonAPIAndNotAPI(t *testing.T) {
	spa, hit := spaSpy()
	u := &tokUsers{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog(), SPA: spa})
	defer ts.Close()

	r, _ := http.Get(ts.URL + "/")
	r.Body.Close()
	if r.StatusCode != 200 || !*hit {
		t.Fatalf("/ should hit SPA: status=%d hit=%v", r.StatusCode, *hit)
	}
	*hit = false
	r2, _ := http.Get(ts.URL + "/some/client/route")
	r2.Body.Close()
	if r2.StatusCode != 200 || !*hit {
		t.Fatalf("client route should hit SPA: status=%d hit=%v", r2.StatusCode, *hit)
	}
	*hit = false
	r3, _ := http.Get(ts.URL + "/api/v1/nope")
	r3.Body.Close()
	if *hit {
		t.Fatal("/api/v1/* must never fall through to the SPA")
	}
	*hit = false
	r4, _ := http.Get(ts.URL + "/api/v1/tunnels")
	r4.Body.Close()
	if r4.StatusCode != http.StatusUnauthorized || *hit {
		t.Fatalf("/api/v1/tunnels unauth want 401 JSON not SPA: status=%d hit=%v", r4.StatusCode, *hit)
	}

	// The /api/v1 namespace must NEVER fall through to the SPA — not the bare
	// prefix, not a trailing slash, not a wrong method. (The plan's original
	// r.NotFound + /* form leaked exactly these into the SPA.)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1"},
		{"GET", "/api/v1/"},
		{"PUT", "/api/v1/me"},
	} {
		*hit = false
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		rx, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		rx.Body.Close()
		if *hit {
			t.Fatalf("%s %s leaked into the SPA (API namespace must never serve the SPA)", tc.method, tc.path)
		}
	}
}

func TestNoSPAKeeps4bBehavior(t *testing.T) {
	u := &tokUsers{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()}) // SPA nil
	defer ts.Close()
	r, _ := http.Get(ts.URL + "/")
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("with SPA nil, / must be chi 404 (4b behavior), got %d", r.StatusCode)
	}
}

// /link is where `burrow login` sends the browser. It is a dashboard page:
// the SPA shell answers it, for a visitor without a session too (the page
// itself sends them through the login), with the query left alone and with
// the headers that keep the approval page out of foreign frames.
func TestLinkPageIsServedByTheSPA(t *testing.T) {
	var hit bool
	var gotQuery string
	spa := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<div id=\"root\"></div>"))
	})
	u := &tokUsers{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog(), SPA: spa})
	defer ts.Close()
	// A redirect would be a failure: it could drop the code.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	for _, path := range []string{"/link?code=BRRW-7Q4K", "/link/?code=BRRW-7Q4K", "/link"} {
		hit, gotQuery = false, ""
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !hit {
			t.Fatalf("GET %s: status=%d spa=%v, want 200 from the SPA", path, resp.StatusCode, hit)
		}
		if path != "/link" && gotQuery != "code=BRRW-7Q4K" {
			t.Errorf("GET %s: the SPA saw query %q, want the code untouched", path, gotQuery)
		}
		for name, want := range map[string]string{
			"X-Frame-Options":         "DENY",
			"Content-Security-Policy": "frame-ancestors 'none'",
			"Referrer-Policy":         "no-referrer",
		} {
			if got := resp.Header.Get(name); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, name, got, want)
			}
		}
	}

	// Only reading: nothing is decided by a request to the page itself.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		hit = false
		req, _ := http.NewRequest(method, ts.URL+"/link?code=BRRW-7Q4K", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s /link: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || hit {
			t.Errorf("%s /link: status=%d spa=%v, want 405 without the SPA", method, resp.StatusCode, hit)
		}
	}

	// The other reserved paths stay the relay's own.
	for _, path := range []string{"/install.sh", "/install.ps1", "/download/burrow/linux/amd64", "/download/nope"} {
		hit = false
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if hit {
			t.Errorf("GET %s reached the SPA; it is a reserved relay path", path)
		}
	}
}

// Without a dashboard there is no link page either.
func TestLinkPageWithoutSPAIsNotFound(t *testing.T) {
	u := &tokUsers{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	r, err := http.Get(ts.URL + "/link?code=BRRW-7Q4K")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("with SPA nil, /link must be 404, got %d", r.StatusCode)
	}
}

// React Router matches paths without regard to case, so /LINK renders the
// approval page through the catch-all. Every dashboard shell therefore
// refuses foreign frames, not the named /link route alone. Tunnelled and API
// responses are somebody else's and gain nothing from this.
func TestEveryDashboardShellRefusesFrames(t *testing.T) {
	spa, _ := spaSpy()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream"))
	})
	u := &tokUsers{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{
		Users: u, Log: discardLog(), SPA: spa,
		TunnelProxy: upstream, AuthDomain: "tunnels.example.com",
		AIGateway: &aigateway.Gateway{Providers: noProviders{}, Log: discardLog()},
	})
	defer ts.Close()

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp
	}

	for _, path := range []string{"/", "/services", "/LINK?code=BRRW-7Q4K", "/Link/", "/link?code=BRRW-7Q4K", "/login"} {
		resp := get(path)
		if got := resp.Header.Values("X-Frame-Options"); len(got) != 1 || got[0] != "DENY" {
			t.Errorf("GET %s: X-Frame-Options = %q, want one DENY", path, got)
		}
		if got := resp.Header.Values("Content-Security-Policy"); len(got) != 1 || got[0] != "frame-ancestors 'none'" {
			t.Errorf("GET %s: Content-Security-Policy = %q, want one frame-ancestors 'none'", path, got)
		}
		// The referrer rule belongs to the page whose address holds a code.
		wantReferrer := ""
		if path == "/link?code=BRRW-7Q4K" {
			wantReferrer = "no-referrer"
		}
		if got := resp.Header.Get("Referrer-Policy"); got != wantReferrer {
			t.Errorf("GET %s: Referrer-Policy = %q, want %q", path, got, wantReferrer)
		}
	}

	for _, path := range []string{"/svc/k7p2qx/", "/svc/k7p2qx/page", "/api/v1/me", "/api/v1/nope", "/healthz", "/install.sh",
		"/ai/k7p2qx/v1/models", "/download/burrow/linux/amd64", "/download/nope", "/api/v1/openapi/viewer/"} {
		resp := get(path)
		for _, name := range []string{"X-Frame-Options", "Content-Security-Policy"} {
			if got := resp.Header.Get(name); got != "" {
				t.Errorf("GET %s: %s = %q, want none (not a dashboard shell)", path, name, got)
			}
		}
	}
}
