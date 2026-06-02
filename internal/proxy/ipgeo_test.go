package proxy_test

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/proxy"
)

// fakeGeo implements proxy.GeoLookup with a fixed country map.
type fakeGeo struct {
	enabled bool
	m       map[string]string // ip → country
	err     error             // if non-nil, every Country() returns it
}

func (f fakeGeo) Country(ip net.IP) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.m[ip.String()], nil
}
func (f fakeGeo) Enabled() bool       { return f.enabled }
func (f fakeGeo) DBPath() string      { return "/dev/null" }
func (f fakeGeo) DBAgeSeconds() int64 { return 0 }

// --------------------------------------------------------------------------
// Unit tests: IPGeoEngine.Allow
// --------------------------------------------------------------------------

func TestIPGeo_AllowEmptyPolicy(t *testing.T) {
	p, err := proxy.CompileIPGeoPolicy(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsEmpty() {
		t.Error("empty policy should report IsEmpty")
	}
	e := proxy.NewIPGeoEngine(p, nil)
	ok, _ := e.Allow(net.ParseIP("203.0.113.7"))
	if !ok {
		t.Error("empty policy should allow all")
	}
}

func TestIPGeo_AllowCIDRsPass(t *testing.T) {
	p, _ := proxy.CompileIPGeoPolicy([]string{"203.0.113.0/24"}, nil, nil, nil)
	e := proxy.NewIPGeoEngine(p, nil)
	ok, _ := e.Allow(net.ParseIP("203.0.113.7"))
	if !ok {
		t.Errorf("203.0.113.7 should be allowed by 203.0.113.0/24")
	}
}

func TestIPGeo_AllowCIDRsDeny(t *testing.T) {
	p, _ := proxy.CompileIPGeoPolicy([]string{"203.0.113.0/24"}, nil, nil, nil)
	e := proxy.NewIPGeoEngine(p, nil)
	ok, reason := e.Allow(net.ParseIP("198.51.100.4"))
	if ok {
		t.Error("198.51.100.4 should be denied (not in allow list)")
	}
	if reason != "ip_geo" {
		t.Errorf("want reason=ip_geo, got %q", reason)
	}
}

func TestIPGeo_BlockCIDRsDeny(t *testing.T) {
	p, _ := proxy.CompileIPGeoPolicy(nil, []string{"10.0.0.0/8"}, nil, nil)
	e := proxy.NewIPGeoEngine(p, nil)
	ok, reason := e.Allow(net.ParseIP("10.0.0.1"))
	if ok {
		t.Error("10.0.0.1 should be blocked")
	}
	if reason != "ip_geo" {
		t.Errorf("want reason=ip_geo, got %q", reason)
	}
}

func TestIPGeo_BlockOverridesAllow(t *testing.T) {
	// block_cidrs takes priority: 10.0.0.1 is in both lists.
	p, _ := proxy.CompileIPGeoPolicy(
		[]string{"10.0.0.0/8"},
		[]string{"10.0.0.0/24"},
		nil, nil,
	)
	e := proxy.NewIPGeoEngine(p, nil)
	ok, _ := e.Allow(net.ParseIP("10.0.0.1"))
	if ok {
		t.Error("explicit block must override allow")
	}
	// But an IP outside the block CIDR still passes.
	ok, _ = e.Allow(net.ParseIP("10.0.1.1"))
	if !ok {
		t.Error("10.0.1.1 should be allowed (matches /8 allow, outside /24 block)")
	}
}

func TestIPGeo_BareIPAcceptedAsHostCIDR(t *testing.T) {
	// "203.0.113.7" (no mask) → /32 host CIDR.
	p, err := proxy.CompileIPGeoPolicy([]string{"203.0.113.7"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := proxy.NewIPGeoEngine(p, nil)
	ok, _ := e.Allow(net.ParseIP("203.0.113.7"))
	if !ok {
		t.Error("bare IP should be parsed as /32 and match itself")
	}
	ok, _ = e.Allow(net.ParseIP("203.0.113.8"))
	if ok {
		t.Error("neighbour /32 must not match")
	}
}

func TestIPGeo_InvalidCIDR(t *testing.T) {
	_, err := proxy.CompileIPGeoPolicy([]string{"not-a-cidr"}, nil, nil, nil)
	if err == nil {
		t.Fatal("want error on garbage cidr")
	}
	if !strings.Contains(err.Error(), "invalid allow_cidrs") {
		t.Errorf("want label in error, got %q", err.Error())
	}
}

// TestIPGeo_CountryNoopBuild verifies that with the default noop GeoLookup
// (geo build tag OFF), country lists are accepted but never enforced.
func TestIPGeo_CountryNoopBuild(t *testing.T) {
	p, err := proxy.CompileIPGeoPolicy(nil, nil, []string{"US"}, []string{"KP"})
	if err != nil {
		t.Fatal(err)
	}
	// Lists are stored upper-case.
	if len(p.AllowCountries) != 1 || p.AllowCountries[0] != "US" {
		t.Errorf("allow_countries normalisation: %+v", p.AllowCountries)
	}
	e := proxy.NewIPGeoEngine(p, nil) // nil → NoopGeoLookup
	// Noop returns Enabled()=false → country rules skipped entirely.
	ok, _ := e.Allow(net.ParseIP("198.51.100.1"))
	if !ok {
		t.Error("noop build must allow despite country lists")
	}
}

// TestIPGeo_CountryGeoEnabled verifies that with an enabled GeoLookup, the
// engine enforces allow_countries + block_countries. This guards the path
// Task 17 will light up under the geo build tag.
func TestIPGeo_CountryGeoEnabled(t *testing.T) {
	p, _ := proxy.CompileIPGeoPolicy(nil, nil, []string{"us", " de "}, []string{"KP"})
	// Confirm normaliseCountries upper-cases + trims.
	if len(p.AllowCountries) != 2 || p.AllowCountries[0] != "US" || p.AllowCountries[1] != "DE" {
		t.Fatalf("normalise: %+v", p.AllowCountries)
	}
	geo := fakeGeo{enabled: true, m: map[string]string{
		"203.0.113.7":  "US",
		"198.51.100.1": "FR",
		"192.0.2.10":   "KP",
	}}
	e := proxy.NewIPGeoEngine(p, geo)

	// US: allowed (in allow_countries, not in block_countries).
	ok, _ := e.Allow(net.ParseIP("203.0.113.7"))
	if !ok {
		t.Error("US must pass allow list")
	}
	// FR: denied (not in allow list).
	ok, reason := e.Allow(net.ParseIP("198.51.100.1"))
	if ok {
		t.Error("FR must be denied (not in allow list)")
	}
	if reason != "ip_geo" {
		t.Errorf("want reason=ip_geo, got %q", reason)
	}
	// KP: denied (explicit block).
	ok, _ = e.Allow(net.ParseIP("192.0.2.10"))
	if ok {
		t.Error("KP must be denied by block_countries")
	}
}

// TestIPGeo_CountryLookupErrorFailsOpen verifies that a Geo lookup error does
// not block traffic. Operators investigate via logs; visitors aren't punished.
func TestIPGeo_CountryLookupErrorFailsOpen(t *testing.T) {
	p, _ := proxy.CompileIPGeoPolicy(nil, nil, []string{"US"}, nil)
	geo := fakeGeo{enabled: true, err: errors.New("db read failed")}
	e := proxy.NewIPGeoEngine(p, geo)
	ok, _ := e.Allow(net.ParseIP("203.0.113.7"))
	if !ok {
		t.Error("lookup error must fail open")
	}
}

// TestIPGeo_AllowRequestWritesForbiddenEnvelope verifies the HTTP wrapper
// emits the exact spec body {"error":"forbidden","reason":"ip_geo"}.
func TestIPGeo_AllowRequestWritesForbiddenEnvelope(t *testing.T) {
	p, _ := proxy.CompileIPGeoPolicy(nil, []string{"198.51.100.0/24"}, nil, nil)
	e := proxy.NewIPGeoEngine(p, nil)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.4:50000"
	rec := httptest.NewRecorder()
	if e.AllowRequest(rec, req, nil) {
		t.Fatal("AllowRequest should return false on deny")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":"forbidden"`) {
		t.Errorf("missing forbidden envelope: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"ip_geo"`) {
		t.Errorf("missing reason: %q", rec.Body.String())
	}
}

// TestIPGeo_AllowRequestPassThrough verifies the HTTP wrapper returns true
// and writes nothing on allow.
func TestIPGeo_AllowRequestPassThrough(t *testing.T) {
	p, _ := proxy.CompileIPGeoPolicy([]string{"203.0.113.0/24"}, nil, nil, nil)
	e := proxy.NewIPGeoEngine(p, nil)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.7:50000"
	rec := httptest.NewRecorder()
	if !e.AllowRequest(rec, req, nil) {
		t.Fatal("AllowRequest should return true on allow")
	}
	if rec.Code != http.StatusOK {
		// httptest.NewRecorder defaults to 200 when nothing was written.
		t.Errorf("status leaked: %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body leaked: %q", rec.Body.String())
	}
}

// TestIPGeo_AllowRequestTrustedProxyXFF verifies that X-Forwarded-For is
// honored only when the immediate peer is in the trusted-proxy list. Without
// the list it must be IGNORED — preventing spoofing.
func TestIPGeo_AllowRequestTrustedProxyXFF(t *testing.T) {
	p, _ := proxy.CompileIPGeoPolicy([]string{"203.0.113.0/24"}, nil, nil, nil)
	e := proxy.NewIPGeoEngine(p, nil)

	// Visitor TCP peer (NOT in allow list); XFF claims allowed IP.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.99:50000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")

	// No trusted proxies → XFF ignored → peer 198.51.100.99 denied.
	rec := httptest.NewRecorder()
	if e.AllowRequest(rec, req, nil) {
		t.Fatal("untrusted XFF must be ignored (peer denies)")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", rec.Code)
	}

	// With trusted proxy = 198.51.100.99/32 → XFF honored → 203.0.113.7 allowed.
	_, peerCIDR, _ := net.ParseCIDR("198.51.100.99/32")
	rec = httptest.NewRecorder()
	if !e.AllowRequest(rec, req, []*net.IPNet{peerCIDR}) {
		t.Errorf("trusted-XFF should allow: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// TestIPGeo_ValidateCountryCode covers the API-side validator.
func TestIPGeo_ValidateCountryCode(t *testing.T) {
	for _, ok := range []string{"US", "us", "DE"} {
		if err := proxy.ValidateCountryCode(ok); err != nil {
			t.Errorf("want %q valid, got %v", ok, err)
		}
	}
	for _, bad := range []string{"", "U", "USA", "1A", "  "} {
		if err := proxy.ValidateCountryCode(bad); err == nil {
			t.Errorf("want %q invalid", bad)
		}
	}
}

// TestIPGeo_NoopGeoLookup verifies the default-build GeoLookup is the noop.
func TestIPGeo_NoopGeoLookup(t *testing.T) {
	g := proxy.NoopGeoLookup()
	if g.Enabled() {
		t.Error("noop must report disabled")
	}
	c, err := g.Country(net.ParseIP("8.8.8.8"))
	if err != nil || c != "" {
		t.Errorf("noop Country: got %q,%v want (\"\",nil)", c, err)
	}
	if g.DBPath() != "" || g.DBAgeSeconds() != 0 {
		t.Errorf("noop metadata not zero")
	}
}
