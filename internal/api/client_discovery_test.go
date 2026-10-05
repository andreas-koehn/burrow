package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/version"
)

// discoveryOf asks the router for the discovery document as a request with the
// given Host header and returns the status, the headers and the decoded body.
func discoveryOf(t *testing.T, d Deps, host string) (*http.Response, map[string]any) {
	t.Helper()
	if d.Log == nil {
		d.Log = discardLog()
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/client/discovery", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	NewRouter(d).ServeHTTP(rec, req)
	resp := rec.Result()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("body is not JSON: %v: %s", err, raw)
		}
	}
	return resp, body
}

func TestClientDiscovery_PublicAndComplete(t *testing.T) {
	resp, body := discoveryOf(t, Deps{ControlListen: ":7000", MinClientVersion: "0.6.0"}, "burrow.example.com:443")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d without a session, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", cc)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("discovery set a cookie")
	}
	want := map[string]any{
		"control":            "burrow.example.com:7000",
		"version":            version.Version,
		"min_client_version": "0.6.0",
		"protocol_version":   float64(proto.ProtocolVersion),
	}
	// Exactly the spec's four fields: nothing about users, tokens, the build
	// or the relay's own addresses.
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "control,min_client_version,protocol_version,version" {
		t.Fatalf("fields %v", keys)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
}

func TestClientDiscovery_NoMinimumByDefault(t *testing.T) {
	_, body := discoveryOf(t, Deps{ControlListen: ":7000"}, "burrow.example.com")
	if v, ok := body["min_client_version"]; !ok || v != "" {
		t.Fatalf("min_client_version = %v (present %v), want an empty string", v, ok)
	}
}

// The control endpoint is the one helper's answer for both endpoints.
func TestControlEndpoint(t *testing.T) {
	cases := []struct {
		name, listen, host, want string
	}{
		{"port only", ":7000", "burrow.example.com:443", "burrow.example.com:7000"},
		{"port only, host without port", ":7000", "burrow.example.com", "burrow.example.com:7000"},
		{"full address", "relay.example.com:7000", "burrow.example.com", "relay.example.com:7000"},
		{"empty", "", "burrow.example.com:8080", "burrow.example.com:8080"},
		{"ipv6 host with port", ":7000", "[::1]:8080", "[::1]:7000"},
		{"ipv6 host without port", ":7000", "[::1]", "[::1]:7000"},
		// A bind address is not where a client connects, and not something to
		// tell the world: it is treated like the port alone.
		{"bind all v4", "0.0.0.0:7000", "burrow.example.com:443", "burrow.example.com:7000"},
		{"bind all v6", "[::]:7000", "burrow.example.com:443", "burrow.example.com:7000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Deps{ControlListen: tc.listen}
			_, body := discoveryOf(t, d, tc.host)
			if body["control"] != tc.want {
				t.Fatalf("discovery control = %v, want %q", body["control"], tc.want)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tc.host
			if got := d.controlEndpoint(req); got != tc.want {
				t.Fatalf("controlEndpoint = %q, want %q", got, tc.want)
			}
		})
	}
}

// An older client and the dashboard keep what they had: connect-info still
// needs a session and still answers {"server": …} with the same value.
func TestClientDiscovery_ConnectInfoUnchanged(t *testing.T) {
	d := Deps{Users: &fakeUserStore{}, Log: discardLog(), ControlListen: ":7000"}
	srv := newTestServer(d)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/clients/connect-info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("connect-info without a session: %d, want 401", resp.StatusCode)
	}

	c := authedClient(t, srv)
	resp = c.get(t, "/api/v1/clients/connect-info")
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect-info: %d %s", resp.StatusCode, raw)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["server"] != "127.0.0.1:7000" {
		t.Fatalf("connect-info = %v", got)
	}

	dresp, err := http.Get(srv.URL + "/api/v1/client/discovery")
	if err != nil {
		t.Fatal(err)
	}
	var disc map[string]any
	if err := json.NewDecoder(dresp.Body).Decode(&disc); err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if disc["control"] != got["server"] {
		t.Fatalf("discovery says %v, connect-info says %v", disc["control"], got["server"])
	}
}

func TestClientDiscovery_OnlyGET(t *testing.T) {
	srv := newTestServer(Deps{Log: discardLog()})
	defer srv.Close()
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(m, srv.URL+"/api/v1/client/discovery", strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", m, resp.StatusCode)
		}
	}
}

// The SPA catch-all must not answer for the discovery path.
func TestClientDiscovery_BeforeTheSPA(t *testing.T) {
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html>")
	})
	resp, body := discoveryOf(t, Deps{SPA: spa, ControlListen: ":7000"}, "burrow.example.com")
	if resp.StatusCode != http.StatusOK || body["control"] != "burrow.example.com:7000" {
		t.Fatalf("status %d, body %v", resp.StatusCode, body)
	}
}

func TestClientDiscovery_RateLimitedPerIP(t *testing.T) {
	if DiscoveryRateLimitPerIP != 60 {
		t.Fatalf("DiscoveryRateLimitPerIP = %d, want 60", DiscoveryRateLimitPerIP)
	}
	h := NewRouter(Deps{Log: discardLog(), ControlListen: ":7000"})
	get := func(remote string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/client/discovery", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 1; i <= DiscoveryRateLimitPerIP; i++ {
		if code := get("192.0.2.10:5000"); code != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i, code)
		}
	}
	if code := get("192.0.2.10:5001"); code != http.StatusTooManyRequests {
		t.Fatalf("request %d: %d, want 429", DiscoveryRateLimitPerIP+1, code)
	}
	// Another address is not affected, and neither is the login route.
	if code := get("192.0.2.11:5000"); code != http.StatusOK {
		t.Fatalf("another source IP: %d, want 200", code)
	}
}

func TestClientDiscovery_RateLimitBodyIsJSON(t *testing.T) {
	h := NewRouter(Deps{Log: discardLog(), DiscoveryRateLimitPerIPOverride: 1})
	var last *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/client/discovery", nil)
		req.RemoteAddr = "192.0.2.20:1"
		last = httptest.NewRecorder()
		h.ServeHTTP(last, req)
	}
	if last.Code != http.StatusTooManyRequests || !strings.Contains(last.Body.String(), `"error"`) {
		t.Fatalf("status %d, body %q", last.Code, last.Body.String())
	}
}
