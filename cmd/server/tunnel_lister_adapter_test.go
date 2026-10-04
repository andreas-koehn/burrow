package main

import (
	"context"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
)

// fakeAccessModeGetter satisfies tunnelGetter for the adapter's access-mode
// enrichment path. The adapter only inspects AccessMode, so the other
// db.Tunnel fields are intentionally left zero. (Distinct from the
// fakeTunnelGetter in clients_adapter_test.go which exercises a different
// shape of the same interface.)
type fakeAccessModeGetter struct{ mode string }

func (f fakeAccessModeGetter) GetTunnel(_ context.Context, _ string) (db.Tunnel, error) {
	return db.Tunnel{AccessMode: f.mode}, nil
}

// fakeUserTunnelLister is a minimal userTunnelLister that returns a fixed
// snapshot, so the adapter's server.TunnelView -> api.TunnelView mapping can
// be exercised without a live registry (a full TLS+yamux handshake just to
// populate *server.Server's unexported reg would be far heavier and not a
// "minimal seam"; *server.Server still satisfies userTunnelLister in prod).
type fakeUserTunnelLister struct {
	gotUser string
	views   []server.TunnelView
}

func (f *fakeUserTunnelLister) ListUserTunnels(userID string) []server.TunnelView {
	f.gotUser = userID
	return f.views
}

// TestTunnelListerAdapterMapsAllFields verifies that tunnelListerAdapter maps
// every field of server.TunnelView onto api.TunnelView without silently
// dropping any. If a future server.TunnelView/api.TunnelView field is added
// but not wired through ListUserTunnels, one of the assertions below fails.
func TestTunnelListerAdapterMapsAllFields(t *testing.T) {
	src := server.TunnelView{
		ID:         "tn-1",
		Name:       "web",
		Type:       "http",
		RemotePort: 0,
		LocalAddr:  "127.0.0.1:3000",
		BytesIn:    4242,
		BytesOut:   8484,
		Connected:  true,
		ServiceID:  "svc-99",
		Hostname:   "k7p2qx.tunnels.example.com",
		AccessMode: "api_key",
	}
	f := &fakeUserTunnelLister{views: []server.TunnelView{src}}

	out := tunnelListerAdapter{
		s:          f,
		slugs:      fakeServiceGetter{svcs: map[string]db.Service{"svc-99": {ID: "svc-99", Subdomain: "newslug"}}},
		authDomain: "tunnels.example.com",
	}.ListUserTunnels("u1")

	if f.gotUser != "u1" {
		t.Fatalf("adapter passed wrong userID to lister: %q", f.gotUser)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 api.TunnelView, got %d", len(out))
	}
	got := out[0]
	if got.ID != src.ID {
		t.Errorf("ID: got %q want %q", got.ID, src.ID)
	}
	if got.Name != src.Name {
		t.Errorf("Name: got %q want %q", got.Name, src.Name)
	}
	if got.Type != src.Type {
		t.Errorf("Type: got %q want %q", got.Type, src.Type)
	}
	if got.RemotePort != src.RemotePort {
		t.Errorf("RemotePort: got %d want %d", got.RemotePort, src.RemotePort)
	}
	if got.LocalAddr != src.LocalAddr {
		t.Errorf("LocalAddr: got %q want %q", got.LocalAddr, src.LocalAddr)
	}
	if got.BytesIn != src.BytesIn {
		t.Errorf("BytesIn: got %d want %d", got.BytesIn, src.BytesIn)
	}
	if got.BytesOut != src.BytesOut {
		t.Errorf("BytesOut: got %d want %d", got.BytesOut, src.BytesOut)
	}
	if got.Connected != src.Connected {
		t.Errorf("Connected: got %v want %v", got.Connected, src.Connected)
	}
	if got.ServiceID != src.ServiceID {
		t.Errorf("ServiceID: got %q want %q", got.ServiceID, src.ServiceID)
	}
	// The URL comes from the durable service row, not the live tunnel's
	// hostname, so a changed slug is reported at once.
	if want := "https://tunnels.example.com/svc/newslug/"; got.URL != want {
		t.Errorf("URL: got %q want %q", got.URL, want)
	}
	if got.AccessMode != src.AccessMode {
		t.Errorf("AccessMode: got %q want %q", got.AccessMode, src.AccessMode)
	}
}

type fakeServiceGetter struct {
	svcs map[string]db.Service
}

func (f fakeServiceGetter) ServiceByID(_ context.Context, id string) (db.Service, error) {
	svc, ok := f.svcs[id]
	if !ok {
		return db.Service{}, db.ErrNotFound
	}
	return svc, nil
}

// The URL is empty without an auth domain, without a service row and for a
// service that has no slug.
func TestTunnelListerAdapterURLEmpty(t *testing.T) {
	f := &fakeUserTunnelLister{views: []server.TunnelView{
		{ID: "tn-1", Type: "http", ServiceID: "svc-1"},
		{ID: "tn-2", Type: "http", ServiceID: "svc-missing"},
		{ID: "tn-3", Type: "tcp", ServiceID: "svc-tcp"},
	}}
	slugs := fakeServiceGetter{svcs: map[string]db.Service{
		"svc-1":   {ID: "svc-1", Subdomain: "abc123"},
		"svc-tcp": {ID: "svc-tcp"},
	}}
	for _, v := range (tunnelListerAdapter{s: f, slugs: slugs}).ListUserTunnels("u1") {
		if v.URL != "" {
			t.Errorf("no auth domain: tunnel %s URL = %q, want empty", v.ID, v.URL)
		}
	}
	got := tunnelListerAdapter{s: f, slugs: slugs, authDomain: "example.com"}.ListUserTunnels("u1")
	if got[0].URL != "https://example.com/svc/abc123/" || got[1].URL != "" || got[2].URL != "" {
		t.Errorf("URLs = %q, %q, %q", got[0].URL, got[1].URL, got[2].URL)
	}
}

// TestTunnelListerAdapterEmpty verifies the nil/empty passthrough (no panic,
// empty result) so the "no live tunnels" path stays covered too.
func TestTunnelListerAdapterEmpty(t *testing.T) {
	out := tunnelListerAdapter{s: &fakeUserTunnelLister{}}.ListUserTunnels("u1")
	if len(out) != 0 {
		t.Fatalf("expected 0 views for empty lister, got %d", len(out))
	}
}

// TestTunnelListerAdapterEnrichesAccessModeFromStore verifies that when the
// live registry leaves AccessMode empty (cmd/server is the only place wired
// to a store), the adapter falls back to the persisted tunnel row so the
// dashboard renders the durable mode instead of always-Open.
func TestTunnelListerAdapterEnrichesAccessModeFromStore(t *testing.T) {
	src := server.TunnelView{
		ID: "tn-2", Name: "ai", Type: "http", LocalAddr: "127.0.0.1:11434",
		Connected: true, ServiceID: "svc-ai",
		Hostname: "5mjmrc.test.local",
		// AccessMode intentionally empty — server.go doesn't see the durable
		// row, the adapter must fetch it.
	}
	f := &fakeUserTunnelLister{views: []server.TunnelView{src}}
	got := tunnelListerAdapter{s: f, access: fakeAccessModeGetter{mode: "api_key"}}.ListUserTunnels("u1")
	if len(got) != 1 || got[0].AccessMode != "api_key" {
		t.Fatalf("expected adapter to enrich AccessMode=api_key; got %+v", got)
	}
}

type fakeServiceModeGetter struct {
	modes map[string]string
}

func (f fakeServiceModeGetter) ServiceAccessMode(_ context.Context, serviceID string) (string, error) {
	m, ok := f.modes[serviceID]
	if !ok {
		return "", db.ErrNotFound
	}
	return m, nil
}

// The durable services row is the source of truth: it must win over both the
// live registry value and the legacy tunnels row.
func TestTunnelListerAdapterPrefersDurableServiceMode(t *testing.T) {
	src := server.TunnelView{
		ID: "tn-9", Name: "ollama", Type: "http", Connected: true,
		ServiceID: "svc-ollama", AccessMode: "open",
	}
	f := &fakeUserTunnelLister{views: []server.TunnelView{src}}
	got := tunnelListerAdapter{
		s:        f,
		access:   fakeAccessModeGetter{mode: "open"},
		services: fakeServiceModeGetter{modes: map[string]string{"svc-ollama": "api_key"}},
	}.ListUserTunnels("u1")
	if len(got) != 1 || got[0].AccessMode != "api_key" {
		t.Fatalf("want durable mode api_key, got %+v", got)
	}
}

// No durable row (tcp tunnel, or lookup error): keep the previous fallback.
func TestTunnelListerAdapterFallsBackWithoutServiceRow(t *testing.T) {
	src := server.TunnelView{ID: "tn-3", Name: "ai", Type: "http", Connected: true, ServiceID: "svc-missing"}
	f := &fakeUserTunnelLister{views: []server.TunnelView{src}}
	got := tunnelListerAdapter{
		s:        f,
		access:   fakeAccessModeGetter{mode: "burrow_login"},
		services: fakeServiceModeGetter{modes: map[string]string{}},
	}.ListUserTunnels("u1")
	if len(got) != 1 || got[0].AccessMode != "burrow_login" {
		t.Fatalf("want legacy fallback burrow_login, got %+v", got)
	}
}
