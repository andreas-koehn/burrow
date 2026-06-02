//go:build geo

package proxy_test

// geo_test.go — exercises the mmdb-backed GeoLookup (Task 17). Built
// only with `-tags geo`. The default build path never compiles this
// file (so the gate `go test ./...` stays free of the mmdb dep).
//
// Fixture: internal/proxy/testdata/GeoLite2-Country-Test.mmdb is the
// upstream MaxMind test database (Apache-2.0-equivalent license,
// committed verbatim from
// https://github.com/maxmind/MaxMind-DB/tree/main/test-data). If the
// fixture is missing the test SKIPs — making the suite robust to
// vendor-fixture-purge in CI mirrors.

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/ankoehn/burrow/internal/proxy"
)

func TestGeoMMDBLookup_OpenAndCountry(t *testing.T) {
	fixture := filepath.Join("testdata", "GeoLite2-Country-Test.mmdb")
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("geo fixture %s not present: %v", fixture, err)
	}

	db, err := proxy.OpenMMDB(fixture)
	if err != nil {
		t.Fatalf("OpenMMDB(%s): %v", fixture, err)
	}
	t.Cleanup(func() {
		if cerr := db.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	})

	if !db.Enabled() {
		t.Error("Enabled() = false; want true after successful Open")
	}
	if db.DBPath() != fixture {
		t.Errorf("DBPath() = %q; want %q", db.DBPath(), fixture)
	}
	// DBAgeSeconds is fresh (<5s); just verify it doesn't panic and is
	// non-negative.
	if age := db.DBAgeSeconds(); age < 0 {
		t.Errorf("DBAgeSeconds() = %d; want >= 0", age)
	}

	// IP -> ISO-3166-1 alpha-2 mappings probed against the upstream
	// MaxMind test fixture. If MaxMind updates the fixture and these
	// drift, the test is the source of truth for what we believe is
	// shipped at this commit.
	tests := []struct {
		ip   string
		want string
	}{
		{"81.2.69.142", "GB"},
		{"81.2.69.160", "GB"},
		{"89.160.20.112", "SE"},
		{"202.196.224.0", "PH"},
		{"2.125.160.216", "GB"},
		{"2a02:cf40::", "NO"},
	}
	for _, tc := range tests {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Errorf("net.ParseIP(%q) = nil", tc.ip)
			continue
		}
		got, err := db.Country(ip)
		if err != nil {
			t.Errorf("Country(%s): %v", tc.ip, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Country(%s) = %q; want %q", tc.ip, got, tc.want)
		}
	}
}

func TestGeoMMDBLookup_LookupIPInDBWithoutCountry(t *testing.T) {
	fixture := filepath.Join("testdata", "GeoLite2-Country-Test.mmdb")
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("geo fixture %s not present: %v", fixture, err)
	}
	db, err := proxy.OpenMMDB(fixture)
	if err != nil {
		t.Fatalf("OpenMMDB: %v", err)
	}
	defer db.Close()
	// 200.7.84.0 is a known fixture network with no country record —
	// Lookup() succeeds (err=nil) but country.iso_code is empty.
	got, err := db.Country(net.ParseIP("200.7.84.0"))
	if err != nil {
		t.Errorf("Country(200.7.84.0): %v", err)
	}
	if got != "" {
		t.Errorf("Country(200.7.84.0) = %q; want \"\" (no country record)", got)
	}
}

func TestGeoMMDBLookup_OpenMissingFile(t *testing.T) {
	_, err := proxy.OpenMMDB(filepath.Join("testdata", "does-not-exist.mmdb"))
	if err == nil {
		t.Fatal("OpenMMDB(nonexistent) = nil; want error")
	}
}

func TestGeoMMDBLookup_OpenEmptyPath(t *testing.T) {
	_, err := proxy.OpenMMDB("")
	if err == nil {
		t.Fatal("OpenMMDB(\"\") = nil; want error")
	}
}

func TestGeoMMDBLookup_NilReceiverSafe(t *testing.T) {
	var m *proxy.MMDBGeoLookup
	if m.Enabled() {
		t.Error("nil.Enabled() = true; want false")
	}
	if got, err := m.Country(net.ParseIP("81.2.69.142")); got != "" || err != nil {
		t.Errorf("nil.Country() = (%q, %v); want (\"\", nil)", got, err)
	}
	if m.DBPath() != "" {
		t.Errorf("nil.DBPath() = %q; want \"\"", m.DBPath())
	}
	if m.DBAgeSeconds() != 0 {
		t.Errorf("nil.DBAgeSeconds() = %d; want 0", m.DBAgeSeconds())
	}
	if err := m.Close(); err != nil {
		t.Errorf("nil.Close() = %v; want nil", err)
	}
}

func TestGeoMMDBLookup_OpenGeoDBFactory(t *testing.T) {
	fixture := filepath.Join("testdata", "GeoLite2-Country-Test.mmdb")
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("geo fixture %s not present: %v", fixture, err)
	}
	lookup, err := proxy.OpenGeoDB(fixture)
	if err != nil {
		t.Fatalf("OpenGeoDB(%s): %v", fixture, err)
	}
	if !lookup.Enabled() {
		t.Error("OpenGeoDB returned a disabled lookup; want enabled")
	}
	got, err := lookup.Country(net.ParseIP("81.2.69.142"))
	if err != nil {
		t.Fatalf("Country: %v", err)
	}
	if got != "GB" {
		t.Errorf("Country(81.2.69.142) = %q; want %q", got, "GB")
	}
	if !proxy.GeoBuildTagEnabled() {
		t.Error("GeoBuildTagEnabled() = false in -tags geo build; want true")
	}
}
