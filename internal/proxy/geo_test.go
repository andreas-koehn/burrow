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
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// copyFixture copies the test database to dir/name.
func copyFixture(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "GeoLite2-Country-Test.mmdb"))
	if err != nil {
		t.Skipf("geo fixture not present: %v", err)
	}
	return writeAtomic(t, dir, name, b)
}

// writeAtomic replaces dir/name by a new file, as geoipupdate does: the
// database in use is mapped into memory, so it must never be written to in
// place.
func writeAtomic(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	tmp := p + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// The file is loaded at start and again when it changes; a file that cannot
// be read keeps the database in use; a file that arrives later is picked up.
func TestGeoMMDB_Reload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "geo.mmdb")

	// Not there at start: the lookup is not enabled, the error is kept, and
	// Open still returns a lookup the watcher can fill.
	g, err := proxy.OpenGeoDB(path)
	if err == nil {
		t.Fatal("a missing database was opened")
	}
	m, ok := g.(*proxy.MMDBGeoLookup)
	if !ok || m.Enabled() || m.LastError() == "" || m.DBPath() != path {
		t.Fatalf("lookup after a failed open: %T enabled=%v err=%q", g, g.Enabled(), m.LastError())
	}
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan error, 8)
	go m.Watch(ctx, 20*time.Millisecond, func(reloaded bool, err error) {
		if reloaded {
			events <- nil
		} else {
			events <- err
		}
	})
	waitEvent := func(why string) error {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no event", why)
		}
		return nil
	}
	// The file appears.
	copyFixture(t, dir, "geo.mmdb")
	if err := waitEvent("file appears"); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !m.Enabled() || m.LastError() != "" {
		t.Fatalf("not loaded: enabled=%v err=%q", m.Enabled(), m.LastError())
	}
	if cc, _ := m.Country(net.ParseIP("89.160.20.112")); cc != "SE" {
		t.Fatalf("country %q", cc)
	}

	// A broken file (a download cut short): the database in use stays.
	writeAtomic(t, dir, "geo.mmdb", []byte("not an mmdb file, and a different size"))
	if err := waitEvent("broken file"); err == nil {
		t.Fatal("a broken file was loaded")
	}
	if !m.Enabled() {
		t.Fatal("a broken file turned the database off")
	}
	if cc, _ := m.Country(net.ParseIP("81.2.69.160")); cc != "GB" {
		t.Fatalf("country after a failed reload %q", cc)
	}
	if m.LastError() == "" {
		t.Error("the failed reload left no error")
	}

	// The good file comes back: reloaded, error cleared.
	copyFixture(t, dir, "geo.mmdb")
	if err := waitEvent("good file again"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m.LastError() != "" {
		t.Errorf("error after a good reload: %q", m.LastError())
	}
}

// Reload reports a change once and leaves an unchanged file alone.
func TestGeoMMDB_ReloadOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	path := copyFixture(t, dir, "geo.mmdb")
	m, err := proxy.OpenMMDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if changed, err := m.Reload(); changed || err != nil {
		t.Fatalf("unchanged file: changed=%v err=%v", changed, err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if changed, err := m.Reload(); !changed || err != nil {
		t.Fatalf("touched file: changed=%v err=%v", changed, err)
	}
}

// The http proxy enforces country rules with the real database: a block list
// denies its country, an allow list admits only its countries, and an
// address with no country record follows the list.
func TestProxy_IPGeoDenied_RealDatabase(t *testing.T) {
	path := copyFixture(t, t.TempDir(), "geo.mmdb")
	g, err := proxy.OpenGeoDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.(*proxy.MMDBGeoLookup).Close()
	p := proxy.New(deadDialer{}, openChecker{}, authDomain, testLog(), proxy.WithGeoLookup(g))

	denied := func(res *proxy.Resolved, addr string) bool {
		req := httptest.NewRequest("GET", "/x", nil)
		req.RemoteAddr = addr + ":1234"
		return p.IPGeoDenied(res, req)
	}
	const se, gb, none = "89.160.20.112", "81.2.69.160", "200.7.84.0"

	block := &proxy.Resolved{IPBlockCountries: []string{"SE"}}
	if !denied(block, se) || denied(block, gb) || denied(block, none) {
		t.Errorf("block SE: se=%v gb=%v unknown=%v, want true false false", denied(block, se), denied(block, gb), denied(block, none))
	}
	allow := &proxy.Resolved{IPAllowCountries: []string{"GB"}}
	if denied(allow, gb) || !denied(allow, se) || !denied(allow, none) {
		t.Errorf("allow GB: gb=%v se=%v unknown=%v, want false true true", denied(allow, gb), denied(allow, se), denied(allow, none))
	}

	// Without a database an allow list denies and a block list lets through.
	bare := proxy.New(deadDialer{}, openChecker{}, authDomain, testLog())
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = gb + ":1234"
	if !bare.IPGeoDenied(allow, req) {
		t.Error("an allow list of countries let a request through with no database")
	}
	if bare.IPGeoDenied(block, req) {
		t.Error("a block list of countries denied a request with no database")
	}
}
