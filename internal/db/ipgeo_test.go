package db

import (
	"context"
	"errors"
	"testing"
)

// TestIPGeoConfigRoundTrip verifies that GetServiceIPGeo before any write
// returns the empty default, and that Set→Get round-trips every field.
func TestIPGeoConfigRoundTrip(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc-a")

	// Empty default: no row → zero config with non-nil empty slices.
	got, err := x.GetServiceIPGeo(ctx, svc)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if got.Enabled {
		t.Errorf("want enabled=false, got true")
	}
	for name, sl := range map[string][]string{
		"AllowCIDRs":     got.AllowCIDRs,
		"BlockCIDRs":     got.BlockCIDRs,
		"AllowCountries": got.AllowCountries,
		"BlockCountries": got.BlockCountries,
	} {
		if sl == nil || len(sl) != 0 {
			t.Errorf("%s: want empty non-nil slice, got %#v", name, sl)
		}
	}

	// Write + read back.
	in := ServiceIPGeoConfig{
		ServiceID:      svc,
		Enabled:        true,
		AllowCIDRs:     []string{"203.0.113.0/24", "2001:db8::/32"},
		BlockCIDRs:     []string{"198.51.100.0/24"},
		AllowCountries: []string{"US", "DE"},
		BlockCountries: []string{"KP"},
	}
	if err := x.SetServiceIPGeo(ctx, in); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = x.GetServiceIPGeo(ctx, svc)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Enabled {
		t.Errorf("enabled lost: %+v", got)
	}
	if len(got.AllowCIDRs) != 2 || got.AllowCIDRs[0] != "203.0.113.0/24" {
		t.Errorf("allow_cidrs round-trip: %+v", got.AllowCIDRs)
	}
	if len(got.BlockCIDRs) != 1 || got.BlockCIDRs[0] != "198.51.100.0/24" {
		t.Errorf("block_cidrs round-trip: %+v", got.BlockCIDRs)
	}
	if len(got.AllowCountries) != 2 || got.AllowCountries[1] != "DE" {
		t.Errorf("allow_countries round-trip: %+v", got.AllowCountries)
	}
	if len(got.BlockCountries) != 1 || got.BlockCountries[0] != "KP" {
		t.Errorf("block_countries round-trip: %+v", got.BlockCountries)
	}

	// Update (upsert) overwrites every column.
	in.Enabled = false
	in.AllowCIDRs = []string{}
	in.BlockCIDRs = []string{"10.0.0.0/8"}
	in.AllowCountries = nil // nil → empty []
	in.BlockCountries = []string{}
	if err := x.SetServiceIPGeo(ctx, in); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	got, _ = x.GetServiceIPGeo(ctx, svc)
	if got.Enabled {
		t.Errorf("enabled not cleared")
	}
	if len(got.AllowCIDRs) != 0 || got.AllowCIDRs == nil {
		t.Errorf("allow_cidrs cleared poorly: %#v", got.AllowCIDRs)
	}
	if len(got.BlockCIDRs) != 1 || got.BlockCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("block_cidrs round-trip 2: %+v", got.BlockCIDRs)
	}
	if got.AllowCountries == nil || len(got.AllowCountries) != 0 {
		t.Errorf("allow_countries nil→[] failed: %#v", got.AllowCountries)
	}
}

// TestIPGeoConfigCascadeOnServiceDelete verifies that deleting the parent
// service removes the ip-geo row (ON DELETE CASCADE in migration 0009).
func TestIPGeoConfigCascadeOnServiceDelete(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc-a")
	if err := x.SetServiceIPGeo(ctx, ServiceIPGeoConfig{
		ServiceID:  svc,
		Enabled:    true,
		AllowCIDRs: []string{"10.0.0.0/8"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := x.DeleteService(ctx, svc); err != nil {
		t.Fatalf("delete svc: %v", err)
	}
	// After cascade: no row → empty default (not ErrNotFound).
	cfg, err := x.GetServiceIPGeo(ctx, svc)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if cfg.Enabled || len(cfg.AllowCIDRs) != 0 {
		t.Errorf("cascade did not clear row: %+v", cfg)
	}
}

// TestServiceMTLSCAPEMRoundTrip verifies Set/Get/Clear on the mtls_ca_pem column.
func TestServiceMTLSCAPEMRoundTrip(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	svc := seedSvc(t, x, "u1", "svc-a")

	// Unset: empty pem.
	pem, err := x.GetServiceMTLSCAPEM(ctx, svc)
	if err != nil {
		t.Fatalf("get unset: %v", err)
	}
	if pem != "" {
		t.Errorf("want empty pem, got %q", pem)
	}

	// Set.
	in := "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----\n"
	if err := x.SetServiceMTLSCAPEM(ctx, svc, in); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := x.GetServiceMTLSCAPEM(ctx, svc)
	if got != in {
		t.Errorf("round-trip mismatch")
	}

	// Clear (empty pem → NULL).
	if err := x.SetServiceMTLSCAPEM(ctx, svc, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = x.GetServiceMTLSCAPEM(ctx, svc)
	if got != "" {
		t.Errorf("clear did not null the column: got %q", got)
	}

	// Set on unknown service → ErrNotFound.
	if err := x.SetServiceMTLSCAPEM(ctx, "no-such-svc", in); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound on unknown service, got %v", err)
	}

	// Get on unknown service → ErrNotFound.
	_, err = x.GetServiceMTLSCAPEM(ctx, "no-such-svc")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound on unknown service, got %v", err)
	}
}

// TestIPGeoDecodeMalformedJSON guards against a corrupt row (manually
// written outside the typed CRUD) by exercising the decoder path.
func TestIPGeoDecodeMalformedJSON(t *testing.T) {
	var out []string
	if err := decodeJSONArray("", &out); err != nil {
		t.Errorf("empty: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Errorf("empty input should yield []")
	}
	out = nil
	if err := decodeJSONArray("[]", &out); err != nil {
		t.Errorf("empty array: %v", err)
	}
	out = nil
	if err := decodeJSONArray(`["a","b"]`, &out); err != nil {
		t.Errorf("ok input: %v", err)
	}
	out = nil
	// Just ensure the error surfaces; the wording belongs to encoding/json.
	if err := decodeJSONArray("not json", &out); err == nil {
		t.Errorf("want error on garbage, got nil")
	}
}
