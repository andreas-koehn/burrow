package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ankoehn/burrow/internal/proxy"
)

// passChain stands in for the AI chain on a service without AI config: it
// hands the request straight to the proxy handler.
type passChain struct{}

func (passChain) Dispatch(w http.ResponseWriter, r *http.Request, _, _, _, _ string, h http.Handler) {
	h.ServeHTTP(w, r)
}

// Tunnelled apps share the dashboard's origin, so a Set-Cookie for the
// dashboard's own cookie names would fix or clear the dashboard session. Those
// lines are dropped; every other cookie of the app passes through unchanged
// and in order.
func TestProxyDropsBurrowSetCookies(t *testing.T) {
	sent := []string{
		`first=1; Path=/`,
		`burrow_session=evil; Path=/; HttpOnly`,
		`burrow_sessionx=keep; Path=/`,
		`prefs={"a":1}; Path=/app; SameSite=Lax`,
		`burrow_csrf=evil`,
		`BURROW_SESSION=other-name`,
		`burrow_csrf =spaced; Max-Age=0`,
		`x=burrow_session=1`,
		`last=a b c; Expires=Wed, 21 Oct 2026 07:28:00 GMT`,
	}
	want := []string{
		`first=1; Path=/`,
		`burrow_sessionx=keep; Path=/`,
		`prefs={"a":1}; Path=/app; SameSite=Lax`,
		`BURROW_SESSION=other-name`,
		`x=burrow_session=1`,
		`last=a b c; Expires=Wed, 21 Oct 2026 07:28:00 GMT`,
	}
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range sent {
			w.Header().Add("Set-Cookie", c)
		}
		w.WriteHeader(http.StatusOK)
	})

	const customHost = "cookies.example.org"
	lookup := func(_ context.Context, host string) (string, bool, error) {
		if host == customHost {
			return "svc-sc", true, nil
		}
		return "", false, nil
	}

	for _, tc := range []struct {
		name   string
		host   string
		prefix string
		opts   []proxy.Option
	}{
		{name: "host route", host: "sc." + authDomain},
		{name: "path route", host: "sc." + authDomain, prefix: "/svc/sc"},
		{name: "custom domain", host: customHost, opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup)}},
		{name: "host route through the AI chain", host: "sc." + authDomain, opts: []proxy.Option{proxy.WithAIChain(passChain{})}},
		{name: "custom domain through the AI chain", host: customHost,
			opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup), proxy.WithAIChain(passChain{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDialer(upstream)
			d.register("sc", &proxy.Resolved{ServiceID: "svc-sc", AccessMode: "open", LocalHost: "127.0.0.1:3000"})
			p := proxy.New(d, openChecker{}, authDomain, testLog(), tc.opts...)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.prefix != "" {
					r = r.WithContext(proxy.WithPathPrefix(r.Context(), tc.prefix))
				}
				p.ServeHTTP(w, r)
			}))
			defer ts.Close()

			req, _ := http.NewRequest("GET", ts.URL+"/", nil)
			req.Host = tc.host
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Values("Set-Cookie"); !reflect.DeepEqual(got, want) {
				t.Fatalf("Set-Cookie lines:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// A response that sets only dashboard cookie names ends up with no
// Set-Cookie header at all.
func TestProxyDropsBurrowSetCookies_OnlyBurrowCookies(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "burrow_session=; Max-Age=0")
		w.Header().Add("Set-Cookie", "burrow_csrf=x")
		w.WriteHeader(http.StatusOK)
	})
	d := newFakeDialer(upstream)
	d.register("sc2", &proxy.Resolved{ServiceID: "svc-sc2", AccessMode: "open", LocalHost: "127.0.0.1:3000"})
	p := proxy.New(d, openChecker{}, authDomain, testLog())
	ts := httptest.NewServer(p)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "sc2." + authDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if got, ok := resp.Header["Set-Cookie"]; ok {
		t.Fatalf("Set-Cookie present: %q", got)
	}
}
