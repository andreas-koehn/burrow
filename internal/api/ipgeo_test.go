package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// fakeIPGeoStore is an in-memory IPGeoStore for the handler tests.
type fakeIPGeoStore struct {
	mu   sync.Mutex
	rows map[string]db.ServiceIPGeoConfig
}

func newFakeIPGeoStore() *fakeIPGeoStore {
	return &fakeIPGeoStore{rows: map[string]db.ServiceIPGeoConfig{}}
}

func (f *fakeIPGeoStore) GetServiceIPGeo(_ context.Context, serviceID string) (db.ServiceIPGeoConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cfg, ok := f.rows[serviceID]; ok {
		return cfg, nil
	}
	// Match the real DB: no row → empty default.
	return db.ServiceIPGeoConfig{
		ServiceID:      serviceID,
		AllowCIDRs:     []string{},
		BlockCIDRs:     []string{},
		AllowCountries: []string{},
		BlockCountries: []string{},
	}, nil
}

func (f *fakeIPGeoStore) SetServiceIPGeo(_ context.Context, cfg db.ServiceIPGeoConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[cfg.ServiceID] = cfg
	return nil
}

// fakeOwnerLookup is a fake ServiceOwnerLookup. rows[id]=ownerID.
type fakeOwnerLookup struct {
	rows map[string]string
}

func (f fakeOwnerLookup) GetServiceByID(_ context.Context, id string) (db.Service, error) {
	owner, ok := f.rows[id]
	if !ok {
		return db.Service{}, db.ErrNotFound
	}
	return db.Service{ID: id, UserID: owner, Type: "http"}, nil
}

// fakeGeoLookupSurface lets the geo/status handler render arbitrary
// enabled/path/age values.
type fakeGeoLookupSurface struct {
	enabled bool
	dbPath  string
	dbAge   int64
}

func (f fakeGeoLookupSurface) Enabled() bool       { return f.enabled }
func (f fakeGeoLookupSurface) DBPath() string      { return f.dbPath }
func (f fakeGeoLookupSurface) DBAgeSeconds() int64 { return f.dbAge }

// makeIPGeoDeps wires a Deps tuned for the ip-geo handler suite.
func makeIPGeoDeps(t *testing.T, role string, owner string) (Deps, *fakeIPGeoStore) {
	t.Helper()
	store := newFakeIPGeoStore()
	d := Deps{
		Log:           discardLog(),
		Users:         &fakeUserStore{role: role, selfID: "u-self"},
		IPGeo:         store,
		IPGeoServices: fakeOwnerLookup{rows: map[string]string{"svc-1": owner}},
	}
	return d, store
}

// TestIPGeo_Get_OwnerDefault verifies that the owner can GET an empty config
// (no row written yet) and receives non-null empty arrays.
func TestIPGeo_Get_OwnerDefault(t *testing.T) {
	d, _ := makeIPGeoDeps(t, "user", "u-self")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/services/svc-1/ip-geo")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out ipGeoResp
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Enabled {
		t.Errorf("want enabled=false, got true")
	}
	for name, sl := range map[string][]string{
		"allow_cidrs":     out.AllowCIDRs,
		"block_cidrs":     out.BlockCIDRs,
		"allow_countries": out.AllowCountries,
		"block_countries": out.BlockCountries,
	} {
		if sl == nil {
			t.Errorf("%s: nil slice (want []) — JSON null leaked", name)
		}
	}
}

// TestIPGeo_Put_OwnerRoundTrip verifies an owner can PUT and then GET back.
func TestIPGeo_Put_OwnerRoundTrip(t *testing.T) {
	d, store := makeIPGeoDeps(t, "user", "u-self")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	body := ipGeoReq{
		Enabled:        true,
		AllowCIDRs:     []string{"203.0.113.0/24"},
		BlockCIDRs:     []string{"10.0.0.0/8"},
		AllowCountries: []string{"us"},
		BlockCountries: []string{"KP"},
	}
	r := c.put(t, "/api/v1/services/svc-1/ip-geo", body)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("put status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	got := store.rows["svc-1"]
	if !got.Enabled || len(got.AllowCIDRs) != 1 || got.AllowCIDRs[0] != "203.0.113.0/24" {
		t.Errorf("store not updated: %+v", got)
	}

	r = c.get(t, "/api/v1/services/svc-1/ip-geo")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out ipGeoResp
	_ = json.NewDecoder(r.Body).Decode(&out)
	if !out.Enabled {
		t.Error("get enabled=true lost")
	}
	if len(out.BlockCIDRs) != 1 || out.BlockCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("block_cidrs round-trip: %+v", out.BlockCIDRs)
	}
}

// TestIPGeo_Put_BadCIDR verifies that malformed CIDRs return 400.
func TestIPGeo_Put_BadCIDR(t *testing.T) {
	d, _ := makeIPGeoDeps(t, "user", "u-self")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	body := ipGeoReq{AllowCIDRs: []string{"not-a-cidr"}}
	r := c.put(t, "/api/v1/services/svc-1/ip-geo", body)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestIPGeo_Put_BadCountryCode verifies that a bad ISO code returns 400.
func TestIPGeo_Put_BadCountryCode(t *testing.T) {
	d, _ := makeIPGeoDeps(t, "user", "u-self")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	body := ipGeoReq{BlockCountries: []string{"USA"}}
	r := c.put(t, "/api/v1/services/svc-1/ip-geo", body)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestIPGeo_Put_NonOwnerForbidden verifies that a non-owner user is rejected
// with 403.
func TestIPGeo_Put_NonOwnerForbidden(t *testing.T) {
	d, _ := makeIPGeoDeps(t, "user", "someone-else")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv) // logs in as u-self via fakeUserStore

	body := ipGeoReq{Enabled: true}
	r := c.put(t, "/api/v1/services/svc-1/ip-geo", body)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestIPGeo_Put_AdminOK verifies that admin (with implicit ipgeo:manage:any)
// can write to any service regardless of owner.
func TestIPGeo_Put_AdminOK(t *testing.T) {
	d, store := makeIPGeoDeps(t, "admin", "someone-else")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	body := ipGeoReq{Enabled: true, AllowCIDRs: []string{"203.0.113.0/24"}}
	r := c.put(t, "/api/v1/services/svc-1/ip-geo", body)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	if !store.rows["svc-1"].Enabled {
		t.Error("admin write did not land")
	}
}

// TestIPGeo_Get_NotFound verifies that an unknown service id returns 404.
func TestIPGeo_Get_NotFound(t *testing.T) {
	d, _ := makeIPGeoDeps(t, "user", "u-self")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/services/no-such-svc/ip-geo")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestIPGeo_Status_DefaultBuild verifies that GET /geo/status returns
// enabled=false in the default build (no geo tag, no GeoLookup wired).
func TestIPGeo_Status_DefaultBuild(t *testing.T) {
	d := Deps{
		Log:   discardLog(),
		Users: &fakeUserStore{role: "user", selfID: "u-self"},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/geo/status")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got geoStatusResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Error("default build must report enabled=false")
	}
	if got.DBAgeSeconds != nil {
		t.Errorf("default build should omit db_age_seconds (got %v)", got.DBAgeSeconds)
	}
}

// TestSetServiceAccessMode_MTLS_PassesCAPEM verifies the PUT /access-mode
// handler accepts mtls + mtls_ca_pem and forwards them to the store.
func TestSetServiceAccessMode_MTLS_PassesCAPEM(t *testing.T) {
	ss := &fakeServiceStore{}
	d := Deps{
		Users:    &fakeUserStore{role: "user"},
		Services: ss,
		Log:      discardLog(),
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	body := map[string]string{
		"access_mode": "mtls",
		"mtls_ca_pem": "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n",
	}
	r := c.put(t, "/api/v1/services/svc-1/access-mode", body)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	if ss.lastMode != "mtls" {
		t.Errorf("store called with mode=%q, want mtls", ss.lastMode)
	}
	if len(ss.lastCAPEM) == 0 {
		t.Error("store called without CA PEM")
	}
}

// TestSetServiceAccessMode_MTLS_MissingCAReturns400 verifies that when the
// store returns ErrMTLSCARequired the handler surfaces 400.
func TestSetServiceAccessMode_MTLS_MissingCAReturns400(t *testing.T) {
	ss := &fakeServiceStore{setModeErr: store.ErrMTLSCARequired}
	d := Deps{
		Users:    &fakeUserStore{role: "user"},
		Services: ss,
		Log:      discardLog(),
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc-1/access-mode",
		map[string]string{"access_mode": "mtls"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestSetServiceAccessMode_MTLS_InvalidCAPEM verifies the handler maps
// store.ErrInvalidMTLSCAPEM to 400 + "invalid CA PEM".
func TestSetServiceAccessMode_MTLS_InvalidCAPEM(t *testing.T) {
	ss := &fakeServiceStore{setModeErr: store.ErrInvalidMTLSCAPEM}
	d := Deps{
		Users:    &fakeUserStore{role: "user"},
		Services: ss,
		Log:      discardLog(),
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc-1/access-mode",
		map[string]string{"access_mode": "mtls", "mtls_ca_pem": "junk"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if !json.Valid([]byte(body)) {
		t.Fatalf("body not JSON: %q", body)
	}
	if !strings.Contains(body, `"invalid CA PEM"`) {
		t.Errorf("body should contain 'invalid CA PEM', got %s", body)
	}
}

// TestIPGeo_Status_Enabled verifies that when a GeoLookup is wired with
// enabled=true, /geo/status surfaces the path + age.
func TestIPGeo_Status_Enabled(t *testing.T) {
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "user", selfID: "u-self"},
		GeoLookup: fakeGeoLookupSurface{enabled: true, dbPath: "/var/lib/burrow/geo.mmdb", dbAge: 3600},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/geo/status")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got geoStatusResp
	_ = json.NewDecoder(r.Body).Decode(&got)
	if !got.Enabled {
		t.Error("want enabled=true")
	}
	if got.DBPath != "/var/lib/burrow/geo.mmdb" {
		t.Errorf("want db_path, got %q", got.DBPath)
	}
	if got.DBAgeSeconds == nil || *got.DBAgeSeconds != 3600 {
		t.Errorf("want db_age_seconds=3600, got %v", got.DBAgeSeconds)
	}
}
