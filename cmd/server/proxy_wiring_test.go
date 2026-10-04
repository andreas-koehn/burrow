package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/db"
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
	svc db.Service
	err error
}

// ServiceForSubdomain returns svc for its current slug only. A fake whose svc
// carries no slug answers for any slug (the older tests rely on that).
func (f *fakeStoreSubdomain) ServiceForSubdomain(_ context.Context, sub string) (db.Service, error) {
	if f.err != nil {
		return db.Service{}, f.err
	}
	if f.svc.Subdomain != "" && f.svc.Subdomain != sub {
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
	tn *server.Tunnel
	ok bool
}

// LookupHTTPTunnel matches on the slug the tunnel registered with, when the
// fake tunnel carries one.
func (f *fakeHTTPTunnelLookup) LookupHTTPTunnel(sub string) (*server.Tunnel, bool) {
	if f.tn != nil && f.tn.Subdomain != "" && f.tn.Subdomain != sub {
		return nil, false
	}
	return f.tn, f.ok
}

// LookupHTTPTunnelByServiceID matches on the tunnel's service id, when the
// fake tunnel carries one.
func (f *fakeHTTPTunnelLookup) LookupHTTPTunnelByServiceID(serviceID string) (*server.Tunnel, bool) {
	if f.tn != nil && f.tn.ServiceID != "" && f.tn.ServiceID != serviceID {
		return nil, false
	}
	return f.tn, f.ok
}

func (f *fakeHTTPTunnelLookup) OpenTunnelStream(_ context.Context, _ *server.Tunnel) (net.Conn, error) {
	// Not used in Lookup tests; DialTunnelStream is tested separately.
	return nil, errors.New("not implemented in fake")
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
		AccessMode:   "api_key",
		APIKeyHeader: "X-Api-Key",
	}
	tn := &server.Tunnel{LocalAddr: "127.0.0.1:3000"}

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
		st:  &fakeStoreSubdomain{svc: db.Service{ID: "svc-1"}},
		srv: &fakeHTTPTunnelLookup{tn: nil, ok: false},
	}
	_, err := a.Lookup(context.Background(), "abc123")
	if !errors.Is(err, proxy.ErrNotFound) {
		t.Errorf("expected proxy.ErrNotFound when tunnel gone, got %v", err)
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
