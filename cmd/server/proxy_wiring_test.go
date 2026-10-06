package main

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// ---------------------------------------------------------------------------
// serviceResolverAdapter tests
// ---------------------------------------------------------------------------

// fakeServiceDB is a test-double for the serviceDB interface. It supports
// configuring UNIQUE-collision failures on SetServiceSubdomain so the
// collision-retry path is exercised without a real database.
type fakeServiceDB struct {
	services       map[string]db.Service // key: userID+":"+name
	subdomainFails int                   // number of leading SetServiceSubdomain calls that return UNIQUE error
	callCount      int
}

func (f *fakeServiceDB) GetOrCreateService(_ context.Context, userID, name, typ string) (db.Service, error) {
	key := userID + ":" + name
	if s, ok := f.services[key]; ok {
		return s, nil
	}
	s := db.Service{
		ID:           "svc-" + userID + "-" + name,
		UserID:       userID,
		Name:         name,
		Type:         typ,
		Subdomain:    "",
		AccessMode:   "open",
		APIKeyHeader: "Authorization",
	}
	f.services[key] = s
	return s, nil
}

// The methods below serve ResolveWithOptions; the tests that use this double
// go through Resolve and never reach them.
func (f *fakeServiceDB) ListServicesByUser(_ context.Context, userID string) ([]db.Service, error) {
	var out []db.Service
	for _, s := range f.services {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeServiceDB) CreateService(context.Context, db.Service) error {
	return errors.New("fakeServiceDB: CreateService is not supported")
}

func (f *fakeServiceDB) GetServiceBySubdomain(context.Context, string) (db.Service, error) {
	return db.Service{}, db.ErrNotFound
}

func (f *fakeServiceDB) GetUserByID(context.Context, string) (db.User, error) {
	return db.User{Role: "user"}, nil
}

func (f *fakeServiceDB) SetServiceSubdomain(_ context.Context, id, sub string) error {
	f.callCount++
	if f.subdomainFails > 0 {
		f.subdomainFails--
		// Return a wrapped UNIQUE constraint error.
		return errors.New("set service subdomain: UNIQUE constraint failed: services.subdomain")
	}
	// Persist the subdomain on the existing record.
	for k, s := range f.services {
		if s.ID == id {
			s.Subdomain = sub
			f.services[k] = s
			return nil
		}
	}
	return db.ErrNotFound
}

// TestServiceResolverAdapter_CreateWithSubdomain checks that Resolve creates a
// service and assigns a 6-character subdomain from the safe alphabet.
func TestServiceResolverAdapter_CreateWithSubdomain(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	if err := st.SeedAdmin(context.Background(), "a@x.com", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(context.Background(), "a@x.com")
	if err != nil {
		t.Fatal(err)
	}

	a := serviceResolverAdapter{db: db.Wrap(d)}
	svcID, sub, err := a.Resolve(context.Background(), u.ID, "myapp", "http")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if svcID == "" {
		t.Error("Resolve returned empty serviceID")
	}
	if len(sub) != 6 {
		t.Errorf("subdomain length: got %d, want 6 (got %q)", len(sub), sub)
	}
	// Validate alphabet: only chars from the safe set.
	const safeAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	for _, c := range sub {
		if !strings.ContainsRune(safeAlphabet, c) {
			t.Errorf("subdomain %q contains character %q outside safe alphabet", sub, c)
		}
	}
}

// TestServiceResolverAdapter_StableIdentity checks that a second Resolve with
// the same (user, name) returns the same serviceID and subdomain.
func TestServiceResolverAdapter_StableIdentity(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	if err := st.SeedAdmin(context.Background(), "b@x.com", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(context.Background(), "b@x.com")
	if err != nil {
		t.Fatal(err)
	}

	a := serviceResolverAdapter{db: db.Wrap(d)}
	id1, sub1, err := a.Resolve(context.Background(), u.ID, "stable", "http")
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	id2, sub2, err := a.Resolve(context.Background(), u.ID, "stable", "http")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if id1 != id2 {
		t.Errorf("serviceID changed between calls: %q → %q", id1, id2)
	}
	if sub1 != sub2 {
		t.Errorf("subdomain changed between calls: %q → %q", sub1, sub2)
	}
}

// TestServiceResolverAdapter_CollisionRetry checks that Resolve retries on a
// UNIQUE constraint failure and succeeds on the second attempt.
func TestServiceResolverAdapter_CollisionRetry(t *testing.T) {
	fake := &fakeServiceDB{
		services:       make(map[string]db.Service),
		subdomainFails: 1, // first SetServiceSubdomain call returns UNIQUE error
	}
	a := serviceResolverAdapter{db: fake}
	svcID, sub, err := a.Resolve(context.Background(), "u1", "app", "http")
	if err != nil {
		t.Fatalf("Resolve with 1 collision: %v", err)
	}
	if svcID == "" || sub == "" {
		t.Errorf("empty result: svcID=%q sub=%q", svcID, sub)
	}
	if fake.callCount < 2 {
		t.Errorf("expected ≥2 SetServiceSubdomain calls (retry), got %d", fake.callCount)
	}
}

// ---------------------------------------------------------------------------
// proxyDialerAdapter — Lookup tests
// ---------------------------------------------------------------------------

// fakeServiceForSubdomain is the narrow interface test double for
// proxyDialerAdapter.Lookup (the store side).
type fakeStoreSubdomain struct {
	svc      db.Service
	err      error
	geoErr   error // returned by GetServiceIPGeo
	geoCalls int
}

// ServiceForSubdomain returns svc for its current slug only.
func (f *fakeStoreSubdomain) ServiceForSubdomain(_ context.Context, sub string) (db.Service, error) {
	if f.err != nil {
		return db.Service{}, f.err
	}
	if f.svc.Subdomain != sub {
		return db.Service{}, db.ErrNotFound
	}
	return f.svc, nil
}

func (f *fakeStoreSubdomain) ServiceByID(_ context.Context, id string) (db.Service, error) {
	if f.err != nil {
		return db.Service{}, f.err
	}
	if f.svc.ID != id {
		return db.Service{}, db.ErrNotFound
	}
	return f.svc, nil
}

func (f *fakeStoreSubdomain) GetServiceIPGeo(_ context.Context, _ string) (db.ServiceIPGeoConfig, error) {
	f.geoCalls++
	if f.geoErr != nil {
		return db.ServiceIPGeoConfig{}, f.geoErr
	}
	return db.ServiceIPGeoConfig{
		AllowCIDRs:     []string{},
		BlockCIDRs:     []string{},
		AllowCountries: []string{},
		BlockCountries: []string{},
	}, nil
}

// httpTunnelLookup is the narrow interface test double for
// proxyDialerAdapter.Lookup (the server side).
type fakeHTTPTunnelLookup struct {
	tn   *server.Tunnel
	ok   bool
	conn net.Conn // returned by OpenTunnelStream when set
}

// LookupHTTPTunnelByServiceID matches strictly on the tunnel's service id,
// like the real registry lookup.
func (f *fakeHTTPTunnelLookup) LookupHTTPTunnelByServiceID(serviceID string) (*server.Tunnel, bool) {
	if !f.ok || f.tn == nil || f.tn.ServiceID != serviceID {
		return nil, false
	}
	return f.tn, true
}

func (f *fakeHTTPTunnelLookup) OpenTunnelStream(_ context.Context, _ *server.Tunnel) (net.Conn, error) {
	if f.conn == nil {
		// Lookup tests never open a stream.
		return nil, errors.New("not implemented in fake")
	}
	return f.conn, nil
}

func (f *fakeHTTPTunnelLookup) SnapshotSessions() []server.SessionSnapshot {
	// Returns nil — Lookup tests don't exercise session-field population.
	return nil
}

// LookupSessionByTunnelID is the v0.5.2 fast-path replacement for the
// SnapshotSessions scan. The Lookup tests don't exercise session-field
// population, so this fake always returns ok=false.
func (f *fakeHTTPTunnelLookup) LookupSessionByTunnelID(_ string) (sessionID, userID string, ok bool) {
	return "", "", false
}

// TestProxyDialerAdapter_Lookup_Found checks that Lookup returns a Resolved
// with all fields correctly composed when both the service row and live tunnel exist.
func TestProxyDialerAdapter_Lookup_Found(t *testing.T) {
	svc := db.Service{
		ID:           "svc-1",
		Subdomain:    "abc123",
		AccessMode:   "api_key",
		APIKeyHeader: "X-Api-Key",
	}
	tn := &server.Tunnel{ServiceID: "svc-1", LocalAddr: "127.0.0.1:3000"}

	a := proxyDialerAdapter{
		st:  &fakeStoreSubdomain{svc: svc},
		srv: &fakeHTTPTunnelLookup{tn: tn, ok: true},
	}

	res, err := a.Lookup(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.ServiceID != "svc-1" {
		t.Errorf("ServiceID: got %q want svc-1", res.ServiceID)
	}
	if res.AccessMode != "api_key" {
		t.Errorf("AccessMode: got %q want api_key", res.AccessMode)
	}
	if res.APIKeyHeader != "X-Api-Key" {
		t.Errorf("APIKeyHeader: got %q want X-Api-Key", res.APIKeyHeader)
	}
	if res.LocalHost != "127.0.0.1:3000" {
		t.Errorf("LocalHost: got %q want 127.0.0.1:3000", res.LocalHost)
	}
}

// A slug rename changes the services row only. The live tunnel still carries
// the slug it registered with; routing must not depend on it.
func TestProxyDialerAdapter_Lookup_AfterSlugRename(t *testing.T) {
	svc := db.Service{ID: "svc1", Name: "web", Type: "http", Subdomain: "newslug", AccessMode: "open"}
	tn := &server.Tunnel{ID: "t1", IsHTTP: true, Subdomain: "oldslug", ServiceID: "svc1", LocalAddr: "127.0.0.1:3000"}
	a := proxyDialerAdapter{
		st:  &fakeStoreSubdomain{svc: svc},
		srv: &fakeHTTPTunnelLookup{tn: tn, ok: true},
	}
	res, err := a.Lookup(context.Background(), "newslug")
	if err != nil {
		t.Fatalf("Lookup(newslug): %v", err)
	}
	if res.ServiceID != "svc1" || res.TunnelID != "t1" {
		t.Fatalf("got %+v", res)
	}
	if _, err := a.Lookup(context.Background(), "oldslug"); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("Lookup(oldslug) err = %v, want ErrNotFound", err)
	}
}

// TestProxyDialerAdapter_Lookup_ServiceMissing checks that Lookup returns
// ErrNotFound when the service row does not exist.
func TestProxyDialerAdapter_Lookup_ServiceMissing(t *testing.T) {
	a := proxyDialerAdapter{
		st:  &fakeStoreSubdomain{err: db.ErrNotFound},
		srv: &fakeHTTPTunnelLookup{},
	}
	_, err := a.Lookup(context.Background(), "xyz")
	if !errors.Is(err, proxy.ErrNotFound) {
		t.Errorf("expected proxy.ErrNotFound, got %v", err)
	}
}

// TestProxyDialerAdapter_Lookup_TunnelGone checks that Lookup returns
// ErrNotFound when the service row exists but the live tunnel is gone.
func TestProxyDialerAdapter_Lookup_TunnelGone(t *testing.T) {
	a := proxyDialerAdapter{
		st:  &fakeStoreSubdomain{svc: db.Service{ID: "svc-1", Subdomain: "abc123"}},
		srv: &fakeHTTPTunnelLookup{tn: nil, ok: false},
	}
	_, err := a.Lookup(context.Background(), "abc123")
	if !errors.Is(err, proxy.ErrNotFound) {
		t.Errorf("expected proxy.ErrNotFound when tunnel gone, got %v", err)
	}
}

// renamedSlugAdapter returns an adapter whose service was renamed to "newslug"
// while its live tunnel still carries the slug it registered with ("oldslug").
func renamedSlugAdapter(conn net.Conn) proxyDialerAdapter {
	svc := db.Service{ID: "svc1", Name: "web", Type: "http", Subdomain: "newslug", AccessMode: "open"}
	tn := &server.Tunnel{ID: "t1", IsHTTP: true, Subdomain: "oldslug", ServiceID: "svc1", LocalAddr: "127.0.0.1:3000"}
	return proxyDialerAdapter{
		st:  &fakeStoreSubdomain{svc: svc},
		srv: &fakeHTTPTunnelLookup{tn: tn, ok: true, conn: conn},
	}
}

// TestProxyDialerAdapter_DialTunnelStream_AfterSlugRename checks that the dial
// goes slug → service row → live tunnel by service id: the current slug dials,
// the slug the tunnel registered with does not.
func TestProxyDialerAdapter_DialTunnelStream_AfterSlugRename(t *testing.T) {
	want, peer := net.Pipe()
	defer want.Close()
	defer peer.Close()
	a := renamedSlugAdapter(want)

	got, err := a.DialTunnelStream(context.Background(), "newslug")
	if err != nil {
		t.Fatalf("DialTunnelStream(newslug): %v", err)
	}
	if got != want {
		t.Fatalf("DialTunnelStream(newslug) returned a different conn")
	}
	if _, err := a.DialTunnelStream(context.Background(), "oldslug"); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("DialTunnelStream(oldslug) err = %v, want ErrNotFound", err)
	}
}

// TestProxyDialerAdapter_DialTunnelStream_TunnelGone checks that a known slug
// without a live tunnel maps to ErrNotFound.
func TestProxyDialerAdapter_DialTunnelStream_TunnelGone(t *testing.T) {
	a := proxyDialerAdapter{
		st:  &fakeStoreSubdomain{svc: db.Service{ID: "svc1", Subdomain: "newslug"}},
		srv: &fakeHTTPTunnelLookup{},
	}
	if _, err := a.DialTunnelStream(context.Background(), "newslug"); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestProxyDialerAdapter_LookupByServiceID_StaleTunnelSlug checks that the
// service row is read by id, not through the slug the live tunnel carries.
func TestProxyDialerAdapter_LookupByServiceID_StaleTunnelSlug(t *testing.T) {
	a := renamedSlugAdapter(nil)

	res, err := a.LookupByServiceID(context.Background(), "svc1")
	if err != nil {
		t.Fatalf("LookupByServiceID(svc1): %v", err)
	}
	if res.ServiceID != "svc1" || res.TunnelID != "t1" || res.AccessMode != "open" || res.LocalHost != "127.0.0.1:3000" {
		t.Fatalf("got %+v", res)
	}
	if _, err := a.LookupByServiceID(context.Background(), "other"); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("LookupByServiceID(other) err = %v, want ErrNotFound", err)
	}
}

// TestProxyDialerAdapter_LookupByServiceID_ServiceMissing checks that a live
// tunnel whose service row is gone maps to ErrNotFound.
func TestProxyDialerAdapter_LookupByServiceID_ServiceMissing(t *testing.T) {
	a := proxyDialerAdapter{
		st:  &fakeStoreSubdomain{err: db.ErrNotFound},
		srv: &fakeHTTPTunnelLookup{tn: &server.Tunnel{ID: "t1", ServiceID: "svc1", Subdomain: "oldslug"}, ok: true},
	}
	if _, err := a.LookupByServiceID(context.Background(), "svc1"); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// liveTunnelLookupAdapter tests
// ---------------------------------------------------------------------------

// fakeHTTPTunnelLister is the narrow interface test double for
// liveTunnelLookupAdapter (the server side).
type fakeHTTPTunnelLister struct {
	tunnels []*server.Tunnel
}

func (f *fakeHTTPTunnelLister) HTTPTunnels() []*server.Tunnel { return f.tunnels }
func (f *fakeHTTPTunnelLister) SnapshotSessions() []server.SessionSnapshot {
	return nil
}

// TestLiveTunnelLookupAdapter_ByServiceID checks that LookupByServiceID returns
// a correct snapshot for a known service and false for an unknown one.
func TestLiveTunnelLookupAdapter_ByServiceID(t *testing.T) {
	tn := &server.Tunnel{
		ID:        "tn-x",
		ServiceID: "svc-1",
		LocalAddr: "127.0.0.1:8080",
		IsHTTP:    true,
	}
	a := liveTunnelLookupAdapter{srv: &fakeHTTPTunnelLister{tunnels: []*server.Tunnel{tn}}}

	snap, ok := a.LookupByServiceID("svc-1")
	if !ok {
		t.Fatal("LookupByServiceID(known): expected ok=true")
	}
	if snap.LocalAddr != "127.0.0.1:8080" {
		t.Errorf("LocalAddr: got %q want 127.0.0.1:8080", snap.LocalAddr)
	}
	if !snap.Connected {
		t.Error("Connected: expected true for live HTTP tunnel")
	}

	_, ok = a.LookupByServiceID("svc-unknown")
	if ok {
		t.Error("LookupByServiceID(unknown): expected ok=false")
	}
}

// TestLiveTunnelLookupAdapter_ByTunnelID checks that LookupByTunnelID returns
// the correct TunnelLocator for a known tunnel ID.
func TestLiveTunnelLookupAdapter_ByTunnelID(t *testing.T) {
	tn := &server.Tunnel{
		ID:        "tn-y",
		ServiceID: "svc-2",
		IsHTTP:    true,
	}
	lister := &fakeHTTPTunnelLister{tunnels: []*server.Tunnel{tn}}

	a := liveTunnelLookupAdapter{srv: lister}

	// LookupByTunnelID for known tunnel — UserID will be "" because
	// fakeHTTPTunnelLister.SnapshotSessions returns nil, but ServiceID must be set.
	loc, ok := a.LookupByTunnelID("tn-y")
	if !ok {
		t.Fatal("LookupByTunnelID(known): expected ok=true")
	}
	if loc.ServiceID != "svc-2" {
		t.Errorf("ServiceID: got %q want svc-2", loc.ServiceID)
	}

	_, ok = a.LookupByTunnelID("tn-unknown")
	if ok {
		t.Error("LookupByTunnelID(unknown): expected ok=false")
	}
}

// TestLiveTunnelLookupAdapter_ByTunnelID_WithUserID checks that LookupByTunnelID
// populates UserID from the session snapshot when available.
func TestLiveTunnelLookupAdapter_ByTunnelID_WithUserID(t *testing.T) {
	tn := &server.Tunnel{
		ID:        "tn-z",
		ServiceID: "svc-3",
		IsHTTP:    true,
	}
	// Create the adapter with the fakeHTTPTunnelLister that returns sessions.
	fakeLister := &fakeHTTPTunnelListerWithSessions{
		tunnels:  []*server.Tunnel{tn},
		sessions: []server.SessionSnapshot{{UserID: "user-abc", Tunnels: []server.TunnelView{{ID: "tn-z"}}}},
	}
	a := liveTunnelLookupAdapter{srv: fakeLister}

	loc, ok := a.LookupByTunnelID("tn-z")
	if !ok {
		t.Fatal("LookupByTunnelID: expected ok=true")
	}
	if loc.UserID != "user-abc" {
		t.Errorf("UserID: got %q want user-abc", loc.UserID)
	}
}

// fakeHTTPTunnelListerWithSessions implements both interfaces.
type fakeHTTPTunnelListerWithSessions struct {
	tunnels  []*server.Tunnel
	sessions []server.SessionSnapshot
}

func (f *fakeHTTPTunnelListerWithSessions) HTTPTunnels() []*server.Tunnel {
	return f.tunnels
}
func (f *fakeHTTPTunnelListerWithSessions) SnapshotSessions() []server.SessionSnapshot {
	return f.sessions
}

// ---------------------------------------------------------------------------
// apiKeyValidatorAdapter test — verify *store.Store satisfies proxy.APIKeyValidator
// directly (no adapter needed if method signature matches).
// ---------------------------------------------------------------------------

// TestStoreDirectlySatisfiesAPIKeyValidator verifies (at compile time) that
// *store.Store can be passed as proxy.APIKeyValidator without wrapping.
func TestStoreDirectlySatisfiesAPIKeyValidator(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	// This assignment compiles only if *store.Store implements proxy.APIKeyValidator.
	var _ proxy.APIKeyValidator = st
}

// TestStoreSatisfiesGateStore verifies *store.Store satisfies proxy.GateStore.
func TestStoreSatisfiesGateStore(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	var _ proxy.GateStore = st
}

// TestStoreSatisfiesAPIServiceStore verifies *store.Store satisfies api.ServiceStore.
func TestStoreSatisfiesAPIServiceStore(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	var _ api.ServiceStore = st
}

// TestAuthGenerateSlug verifies properties of auth.GenerateSlug
// as a cross-check that our adapter alphabet check is correct.
func TestAuthGenerateSlug(t *testing.T) {
	const safeAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	for i := 0; i < 50; i++ {
		s, err := auth.GenerateSlug()
		if err != nil {
			t.Fatalf("GenerateSlug: %v", err)
		}
		if len(s) != 6 {
			t.Errorf("GenerateSlug: length %d want 6 (got %q)", len(s), s)
		}
		for _, c := range s {
			if !strings.ContainsRune(safeAlphabet, c) {
				t.Errorf("GenerateSlug: char %q outside safe alphabet in %q", c, s)
			}
		}
	}
}

// policyStore serves one direct provider's backing service and its policy.
type policyStore struct {
	svc      db.Service
	ipgeo    db.ServiceIPGeoConfig
	ipgeoErr error
}

func (p policyStore) ServiceForSubdomain(context.Context, string) (db.Service, error) {
	return db.Service{}, db.ErrNotFound
}

func (p policyStore) ServiceByID(_ context.Context, id string) (db.Service, error) {
	if id != p.svc.ID {
		return db.Service{}, db.ErrNotFound
	}
	return p.svc, nil
}

func (p policyStore) GetServiceIPGeo(context.Context, string) (db.ServiceIPGeoConfig, error) {
	return p.ipgeo, p.ipgeoErr
}

// A direct provider has no tunnel: its access mode and IP/geo policy come
// from the backing service's rows alone.
func TestDirectServicePolicy(t *testing.T) {
	st := policyStore{
		svc: db.Service{ID: "prov-openrouter", Type: "direct", AccessMode: "api_key", APIKeyHeader: "Authorization"},
		ipgeo: db.ServiceIPGeoConfig{
			Enabled: true, AllowCIDRs: []string{"10.0.0.0/8"}, BlockCIDRs: []string{"10.1.0.0/16"},
			AllowCountries: []string{"DE"}, BlockCountries: []string{"XX"},
		},
	}
	res, err := directServicePolicy(st)(context.Background(), "prov-openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if res.ServiceID != "prov-openrouter" || res.AccessMode != "api_key" || res.TunnelID != "" || res.LocalHost != "" {
		t.Fatalf("resolved = %+v", res)
	}
	if len(res.IPAllowCIDRs) != 1 || len(res.IPBlockCIDRs) != 1 || len(res.IPAllowCountries) != 1 || len(res.IPBlockCountries) != 1 {
		t.Fatalf("policy not carried over: %+v", res)
	}

	st.ipgeo.Enabled = false
	res, err = directServicePolicy(st)(context.Background(), "prov-openrouter")
	if err != nil || len(res.IPAllowCIDRs) != 0 || len(res.IPBlockCountries) != 0 {
		t.Fatalf("a disabled policy was applied: %+v err %v", res, err)
	}

	if _, err := directServicePolicy(st)(context.Background(), "gone"); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("missing service err = %v", err)
	}
	// A provider row pointing at a service of another type must not borrow
	// that service's keys and policy.
	for _, typ := range []string{"http", "tcp", ""} {
		other := st
		other.svc.Type = typ
		if _, err := directServicePolicy(other)(context.Background(), "prov-openrouter"); !errors.Is(err, proxy.ErrNotFound) {
			t.Fatalf("service of type %q: err = %v, want proxy.ErrNotFound", typ, err)
		}
	}

	st.ipgeoErr = errors.New("db down")
	if _, err := directServicePolicy(st)(context.Background(), "prov-openrouter"); err == nil {
		t.Fatal("a failed policy read must not look like an empty policy")
	}
}

// The control server and the dashboard API must announce the same domain:
// with built-in ACME and no auth_domain that is the first ACME domain.
func TestResolveAuthDomain(t *testing.T) {
	for _, tc := range []struct {
		name, authDomain, acmeDomain string
		acmeOn                       bool
		want                         string
	}{
		{"explicit auth domain wins", "burrow.example.com", "other.example.com", true, "burrow.example.com"},
		{"first ACME domain as fallback", "", " a.example.com , b.example.com", true, "a.example.com"},
		{"ACME domain ignored when ACME is off", "", "a.example.com", false, ""},
		{"nothing configured", "", "", false, ""},
	} {
		if got := resolveAuthDomain(tc.authDomain, tc.acmeDomain, tc.acmeOn); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// serviceResolverAdapter.ResolveWithOptions (slug and access on creation)
// ---------------------------------------------------------------------------

// optionsDB opens a migrated database with one admin and returns the wrapped
// handle and the admin's id.
func optionsDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	if err := st.SeedAdmin(context.Background(), "a@x.com", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(context.Background(), "a@x.com")
	if err != nil {
		t.Fatal(err)
	}
	return db.Wrap(d), u.ID
}

// servicesOf returns the user's service rows by name.
func servicesOf(t *testing.T, x *db.DB, userID string) map[string]db.Service {
	t.Helper()
	rows, err := x.ListServicesByUser(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]db.Service{}
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

// refusal fails the test unless err is a refusal with the given code, and
// returns its text.
func refusal(t *testing.T, err error, code string) string {
	t.Helper()
	var re *server.RefusalError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want a refusal with code %q", err, code)
	}
	if re.Code != code {
		t.Fatalf("code = %q (%s), want %q", re.Code, re.Message, code)
	}
	return re.Message
}

func TestResolveWithOptions_CreatesWithSlugAndAccess(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	ctx := context.Background()

	r, err := a.ResolveWithOptions(ctx, uid, "web", "http", server.ResolveOptions{Slug: "my-app", Access: "burrow_login"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ServiceID == "" || r.Slug != "my-app" || r.AccessMode != "burrow_login" || !r.Created || len(r.Ignored) != 0 {
		t.Fatalf("created: %+v", r)
	}
	row := servicesOf(t, x, uid)["web"]
	if row.ID != r.ServiceID || row.Subdomain != "my-app" || row.AccessMode != "burrow_login" || row.Type != "http" || row.APIKeyHeader != "Authorization" {
		t.Fatalf("row: %+v", row)
	}

	// The same name again with other values: nothing changes on the service.
	r2, err := a.ResolveWithOptions(ctx, uid, "web", "http", server.ResolveOptions{Slug: "other", Access: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if r2.ServiceID != r.ServiceID || r2.Slug != "my-app" || r2.AccessMode != "burrow_login" || r2.Created ||
		len(r2.Ignored) != 2 || r2.Ignored[0] != "access" || r2.Ignored[1] != "slug" {
		t.Fatalf("existing service: %+v", r2)
	}
	if got := servicesOf(t, x, uid); len(got) != 1 || got["web"] != row {
		t.Fatalf("the service changed: %+v", got)
	}

	// One wish that matches and one that does not.
	r3, err := a.ResolveWithOptions(ctx, uid, "web", "http", server.ResolveOptions{Slug: "my-app", Access: "api_key"})
	if err != nil || len(r3.Ignored) != 1 || r3.Ignored[0] != "access" {
		t.Fatalf("one differing wish: %+v %v", r3, err)
	}

	// The same values are not "ignored": they are what the service has.
	r4, err := a.ResolveWithOptions(ctx, uid, "web", "http", server.ResolveOptions{Slug: "my-app", Access: "burrow_login"})
	if err != nil || r4.Created || len(r4.Ignored) != 0 {
		t.Fatalf("same values: %+v %v", r4, err)
	}
}

// Without the wishes the service is what Resolve has always made of it.
func TestResolveWithOptions_NoOptionsIsTheOldBehaviour(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	ctx := context.Background()

	r, err := a.ResolveWithOptions(ctx, uid, "web", "http", server.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Created || r.AccessMode != "open" || len(r.Slug) != 6 || !auth.ValidSlug(r.Slug) || len(r.Ignored) != 0 {
		t.Fatalf("created: %+v", r)
	}
	id, sub, err := a.Resolve(ctx, uid, "web", "http")
	if err != nil || id != r.ServiceID || sub != r.Slug {
		t.Fatalf("Resolve sees another service: %q %q %v", id, sub, err)
	}
	r2, err := a.ResolveWithOptions(ctx, uid, "web", "http", server.ResolveOptions{})
	if err != nil || r2.Created || r2.ServiceID != r.ServiceID || r2.Slug != r.Slug {
		t.Fatalf("second time: %+v %v", r2, err)
	}
	// A service Resolve made is found, too.
	id3, sub3, err := a.Resolve(ctx, uid, "legacy", "http")
	if err != nil {
		t.Fatal(err)
	}
	r3, err := a.ResolveWithOptions(ctx, uid, "legacy", "http", server.ResolveOptions{Slug: "wanted", Access: "open"})
	if err != nil || r3.Created || r3.ServiceID != id3 || r3.Slug != sub3 || len(r3.Ignored) != 1 || r3.Ignored[0] != "slug" {
		t.Fatalf("service made by Resolve: %+v %v", r3, err)
	}
}

func TestResolveWithOptions_AccessAloneGetsAGeneratedSlug(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x}
	r, err := a.ResolveWithOptions(context.Background(), uid, "api", "http", server.ResolveOptions{Access: "api_key"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Created || r.AccessMode != "api_key" || len(r.Slug) != 6 || !auth.ValidSlug(r.Slug) {
		t.Fatalf("created: %+v", r)
	}
	if row := servicesOf(t, x, uid)["api"]; row.Subdomain != r.Slug || row.AccessMode != "api_key" {
		t.Fatalf("row: %+v", row)
	}
}

func TestResolveWithOptions_Refusals(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	withDomain := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	noDomain := serviceResolverAdapter{db: x}

	// Another service holds the slug "taken".
	if _, err := withDomain.ResolveWithOptions(ctx, uid, "first", "http", server.ResolveOptions{Slug: "taken"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		a    serviceResolverAdapter
		o    server.ResolveOptions
		code string
		text string
	}{
		{"slug with capitals", withDomain, server.ResolveOptions{Slug: "Bad_Slug"}, "slug_invalid", auth.SlugRule},
		{"slug too short", withDomain, server.ResolveOptions{Slug: "ab"}, "slug_invalid", auth.SlugRule},
		{"slug with a path", withDomain, server.ResolveOptions{Slug: "a/../b"}, "slug_invalid", auth.SlugRule},
		{"slug taken", withDomain, server.ResolveOptions{Slug: "taken"}, "slug_taken", "slug already in use"},
		{"mtls", withDomain, server.ResolveOptions{Access: "mtls"}, "access_invalid", "open, api_key, burrow_login"},
		{"cli name", withDomain, server.ResolveOptions{Access: "login"}, "access_invalid", "open, api_key, burrow_login"},
		{"nonsense", withDomain, server.ResolveOptions{Access: "x"}, "access_invalid", "open, api_key, burrow_login"},
		{"login without an auth domain", noDomain, server.ResolveOptions{Access: "burrow_login"}, "access_invalid", "burrow_login requires a configured auth_domain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.a.ResolveWithOptions(ctx, uid, "new", "http", c.o)
			msg := refusal(t, err, c.code)
			if !strings.Contains(msg, c.text) {
				t.Fatalf("message %q does not contain %q", msg, c.text)
			}
			if got := servicesOf(t, x, uid); len(got) != 1 {
				t.Fatalf("a refusal left a service behind: %+v", got)
			}
		})
	}

	t.Run("a taken slug comes with a free one to try", func(t *testing.T) {
		_, err := withDomain.ResolveWithOptions(ctx, uid, "new", "http", server.ResolveOptions{Slug: "taken"})
		msg := refusal(t, err, "slug_taken")
		i := strings.LastIndex(msg, "try: ")
		if i < 0 {
			t.Fatalf("no suggestion in %q", msg)
		}
		free := msg[i+len("try: "):]
		if !auth.ValidSlug(free) || free == "taken" {
			t.Fatalf("suggestion %q", free)
		}
		if _, err := x.GetServiceBySubdomain(ctx, free); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("the suggested slug is in use: %v", err)
		}
	})

	// A value that could never be applied is refused for a service that
	// exists as well: the relay checks what it is sent.
	t.Run("an existing service does not excuse a malformed value", func(t *testing.T) {
		_, err := withDomain.ResolveWithOptions(ctx, uid, "first", "http", server.ResolveOptions{Access: "mtls"})
		refusal(t, err, "access_invalid")
		_, err = withDomain.ResolveWithOptions(ctx, uid, "first", "http", server.ResolveOptions{Slug: "Bad_Slug"})
		refusal(t, err, "slug_invalid")
	})
	// Whether login is available only matters when the mode would be set.
	t.Run("login on a relay without an auth domain is ignored for an existing service", func(t *testing.T) {
		r, err := noDomain.ResolveWithOptions(ctx, uid, "first", "http", server.ResolveOptions{Access: "burrow_login"})
		if err != nil || len(r.Ignored) != 1 || r.Ignored[0] != "access" || r.AccessMode != "open" {
			t.Fatalf("%+v %v", r, err)
		}
	})
}

// Choosing a slug or an access mode is configuring a service. A role that may
// not do that in the dashboard cannot do it through the client either.
func TestResolveWithOptions_NeedsThePermissionToConfigure(t *testing.T) {
	x, _ := optionsDB(t)
	ctx := context.Background()
	if err := x.CreateUser(ctx, db.User{ID: "u-limited", Email: "l@x.com", PasswordHash: "x", Role: "no-such-role"}); err != nil {
		t.Fatal(err)
	}
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	for _, o := range []server.ResolveOptions{{Slug: "my-app"}, {Access: "api_key"}, {Access: "burrow_login"}, {Slug: "my-app", Access: "open"}} {
		_, err := a.ResolveWithOptions(ctx, "u-limited", "web", "http", o)
		refusal(t, err, "forbidden")
	}
	if got := servicesOf(t, x, "u-limited"); len(got) != 0 {
		t.Fatalf("a refusal left a service behind: %+v", got)
	}
	// What that user could always do still works: a service with the
	// defaults, whether the default mode is named or not.
	r, err := a.ResolveWithOptions(ctx, "u-limited", "web", "http", server.ResolveOptions{})
	if err != nil || !r.Created || r.AccessMode != "open" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = a.ResolveWithOptions(ctx, "u-limited", "named", "http", server.ResolveOptions{Access: "open"})
	if err != nil || !r.Created || r.AccessMode != "open" || len(r.Slug) != 6 || len(r.Ignored) != 0 {
		t.Fatalf("access open alone: %+v %v", r, err)
	}
}

// The backing row of a direct AI provider is never handed to a tunnel.
func TestResolveWithOptions_DirectProviderNameStaysReserved(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	if err := x.CreateService(ctx, db.Service{ID: "direct-1", UserID: uid, Name: "llm", Type: "direct", AccessMode: "api_key"}); err != nil {
		t.Fatal(err)
	}
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	// The text is the one Resolve has always given; the control loop puts
	// "resolve service: " in front of it for the client.
	const want = "resolve service: get-or-create: db: service name is used by a direct AI provider"
	if _, _, err := a.Resolve(ctx, uid, "llm", "http"); err == nil || err.Error() != want {
		t.Fatalf("Resolve: %v", err)
	}
	for _, o := range []server.ResolveOptions{{}, {Slug: "my-app", Access: "open"}} {
		_, err := a.ResolveWithOptions(ctx, uid, "llm", "http", o)
		if !errors.Is(err, db.ErrServiceNameReserved) || err.Error() != want {
			t.Fatalf("options %+v: error = %v", o, err)
		}
	}
}

// Created is true for the one registration that made the service, also when
// nothing is wished for, and the access mode is the one the service has.
func TestResolveWithOptions_ConcurrentCreationWithoutOptions(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	type out struct {
		r   server.Resolved
		err error
	}
	for round := 0; round < 5; round++ {
		name := "web" + string(rune('a'+round))
		res := make(chan out, 8)
		for i := 0; i < 8; i++ {
			go func() {
				r, err := a.ResolveWithOptions(context.Background(), uid, name, "http", server.ResolveOptions{})
				res <- out{r, err}
			}()
		}
		created, slug := 0, ""
		for i := 0; i < 8; i++ {
			o := <-res
			if o.err != nil {
				t.Fatalf("registration %d: %v", i, o.err)
			}
			if slug == "" {
				slug = o.r.Slug
			}
			if o.r.Slug != slug || len(slug) != 6 || o.r.AccessMode != "open" {
				t.Fatalf("registration %d: %+v, slug of the others %q", i, o.r, slug)
			}
			if o.r.Created {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("round %d: created %d times", round, created)
		}
	}
	// A service whose mode was changed since reports that mode, wish or not.
	if err := x.SetServiceAccessMode(context.Background(), servicesOf(t, x, uid)["weba"].ID, "api_key", "Authorization"); err != nil {
		t.Fatal(err)
	}
	for _, o := range []server.ResolveOptions{{}, {Access: "open"}} {
		r, err := a.ResolveWithOptions(context.Background(), uid, "weba", "http", o)
		if err != nil || r.Created || r.AccessMode != "api_key" {
			t.Fatalf("options %+v: %+v %v", o, r, err)
		}
	}
}

// A service that exists without a slug (made in the dashboard for a client
// that connects later) gets one, and keeps its mode.
func TestResolveWithOptions_ExistingServiceWithoutASlug(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	if err := x.CreateService(ctx, db.Service{ID: "pre-1", UserID: uid, Name: "web", Type: "http", AccessMode: "burrow_login"}); err != nil {
		t.Fatal(err)
	}
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	r, err := a.ResolveWithOptions(ctx, uid, "web", "http", server.ResolveOptions{Slug: "wanted"})
	if err != nil || r.Created || r.ServiceID != "pre-1" || len(r.Slug) != 6 || r.AccessMode != "burrow_login" ||
		len(r.Ignored) != 1 || r.Ignored[0] != "slug" {
		t.Fatalf("%+v %v", r, err)
	}
}

// Two registrations of one new name at the same moment end with one service.
func TestResolveWithOptions_ConcurrentCreation(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	type out struct {
		r   server.Resolved
		err error
	}
	res := make(chan out, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r, err := a.ResolveWithOptions(context.Background(), uid, "web", "http", server.ResolveOptions{Slug: "my-app", Access: "api_key"})
			res <- out{r, err}
		}()
	}
	created := 0
	for i := 0; i < 8; i++ {
		o := <-res
		if o.err != nil {
			t.Fatalf("registration %d: %v", i, o.err)
		}
		if o.r.Slug != "my-app" || o.r.AccessMode != "api_key" || len(o.r.Ignored) != 0 {
			t.Fatalf("registration %d: %+v", i, o.r)
		}
		if o.r.Created {
			created++
		}
	}
	if created != 1 || len(servicesOf(t, x, uid)) != 1 {
		t.Fatalf("created %d times, %d rows", created, len(servicesOf(t, x, uid)))
	}
}

// lastRegistration is a client observer that keeps the last registration.
type lastRegistration struct {
	mu  sync.Mutex
	reg *client.RegisteredTunnel
}

func (l *lastRegistration) State(client.ConnState, string, time.Duration) {}
func (l *lastRegistration) Connection(string, time.Time, string)          {}
func (l *lastRegistration) ConnectionClosed(string)                       {}
func (l *lastRegistration) Latency(time.Duration)                         {}
func (l *lastRegistration) LocalTarget(string, bool)                      {}
func (l *lastRegistration) Request(string, time.Time, string, string, int) {
}
func (l *lastRegistration) Registered(t client.RegisteredTunnel) {
	l.mu.Lock()
	l.reg = &t
	l.mu.Unlock()
}

// The whole way: the client of this code base, the control server, the
// resolver and a real database, wired as main wires them.
func TestCreateOptions_ClientToDatabase(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	ctx := context.Background()
	if err := st.SeedAdmin(ctx, "a@x.com", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(ctx, "a@x.com")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.IssueClientToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	x := db.Wrap(d)

	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"), TLSKey: filepath.Join(dir, "dev-server-key.pem"),
		PublicBind: "127.0.0.1", Auth: st, Logger: slog.New(slog.DiscardHandler),
		Services:   serviceResolverAdapter{db: x, authDomain: "burrow.example.com"},
		AuthDomain: "burrow.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	go func() { _ = srv.Serve(sctx) }()
	t.Cleanup(func() { cancel(); srv.Wait() })
	for i := 0; i < 200 && srv.Addr() == ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	// register runs a client with one http tunnel until the relay answered,
	// and returns the registration or the error Run ended with.
	register := func(spec client.TunnelSpec) (*client.RegisteredTunnel, error) {
		t.Helper()
		obs := &lastRegistration{}
		c := client.New(client.Options{
			Server: srv.Addr(), Token: token, RootCAs: pool, ServerName: "localhost",
			Tunnels: []client.TunnelSpec{spec}, Observer: obs, StopOnRefusal: true,
			Logger: slog.New(slog.DiscardHandler),
		})
		cctx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- c.Run(cctx) }()
		defer func() { stop() }()
		deadline := time.After(5 * time.Second)
		for {
			obs.mu.Lock()
			reg := obs.reg
			obs.mu.Unlock()
			if reg != nil {
				stop()
				<-done
				return reg, nil
			}
			select {
			case err := <-done:
				return nil, err
			case <-deadline:
				t.Fatal("the relay did not answer the registration")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}

	reg, err := register(client.TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app", Access: "api_key"})
	if err != nil {
		t.Fatal(err)
	}
	row := servicesOf(t, x, u.ID)["web"]
	if row.Subdomain != "my-app" || row.AccessMode != "api_key" {
		t.Fatalf("row: %+v", row)
	}
	if reg.URL != "https://burrow.example.com/svc/my-app/" || reg.AccessMode != "api_key" || !reg.Created ||
		reg.DashboardURL != "https://burrow.example.com/services/"+row.ID || reg.Ignored.Any() || reg.SlugUnacknowledged {
		t.Fatalf("registration: %+v", reg)
	}
	// Creating a service with api-key access makes no key: none exists that
	// could travel to the client or into a log.
	if keys, err := x.ListServiceAPIKeys(ctx, row.ID); err != nil || len(keys) != 0 {
		t.Fatalf("keys: %d %v", len(keys), err)
	}

	// Other values for the service that exists: nothing changes.
	reg, err = register(client.TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "other", Access: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if reg.URL != "https://burrow.example.com/svc/my-app/" || reg.AccessMode != "api_key" || reg.Created ||
		reg.Ignored != (client.OptionSet{Slug: true, Access: true}) {
		t.Fatalf("existing service: %+v", reg)
	}
	if got := servicesOf(t, x, u.ID)["web"]; got != row {
		t.Fatalf("the service changed: %+v", got)
	}

	// A slug another service has: the client ends with the relay's reason,
	// and no service is left behind.
	_, err = register(client.TunnelSpec{Name: "second", Type: "http", LocalAddr: "127.0.0.1:3001", Slug: "my-app"})
	var re *client.RefusedError
	if !errors.As(err, &re) || re.Code != "slug_taken" || !strings.Contains(re.Message, "try: ") {
		t.Fatalf("taken slug: %v", err)
	}
	if got := servicesOf(t, x, u.ID); len(got) != 1 {
		t.Fatalf("services after a refusal: %+v", got)
	}

	// No wishes: the service the relay has always made.
	reg, err = register(client.TunnelSpec{Name: "plain", Type: "http", LocalAddr: "127.0.0.1:3002"})
	if err != nil {
		t.Fatal(err)
	}
	if plain := servicesOf(t, x, u.ID)["plain"]; plain.AccessMode != "open" || len(plain.Subdomain) != 6 ||
		reg.AccessMode != "open" || !reg.Created || reg.URL != "https://burrow.example.com/svc/"+plain.Subdomain+"/" {
		t.Fatalf("row %+v, registration %+v", plain, reg)
	}
}

// A gateway-only service is not found by the subdomain lookup, and nothing further is
// read once the flag is seen: no tunnel registry, no ip-geo query. That keeps
// it indistinguishable from a missing service in status and timing, even when
// a later query would have failed.
func TestProxyDialerAdapter_Lookups_GatewayOnlyIsNotFoundAtOnce(t *testing.T) {
	svc := db.Service{ID: "svc-1", Subdomain: "abc123", AccessMode: "open", GatewayOnly: true}
	st := &fakeStoreSubdomain{svc: svc, geoErr: errors.New("db down")}
	srv := &fakeHTTPTunnelLookup{tn: &server.Tunnel{ServiceID: "svc-1", LocalAddr: "127.0.0.1:3000"}, ok: true}
	a := proxyDialerAdapter{st: st, srv: srv}

	if _, err := a.Lookup(context.Background(), "abc123"); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("Lookup err = %v, want ErrNotFound", err)
	}
	if st.geoCalls != 0 {
		t.Errorf("GetServiceIPGeo called %d times after the flag was seen", st.geoCalls)
	}
	// The AI gateway shares LookupByServiceID: it must still resolve, flagged,
	// and the proxy's custom-domain door refuses it by the flag.
	st.geoErr = nil
	if res, err := a.LookupByServiceID(context.Background(), "svc-1"); err != nil || !res.GatewayOnly {
		t.Fatalf("LookupByServiceID: %+v %v", res, err)
	}
	st.geoErr = errors.New("db down")

	// Without the flag the same service resolves.
	st.svc.GatewayOnly = false
	st.geoErr = nil
	if res, err := a.Lookup(context.Background(), "abc123"); err != nil || res.GatewayOnly {
		t.Fatalf("plain Lookup: %+v %v", res, err)
	}
}
