package proxy

// geo.go — mmdb-backed GeoLookup (E5 of the tcp-complete round).
//
// Wraps github.com/oschwald/maxminddb-golang to resolve a visitor IP to its
// ISO-3166-1 alpha-2 country code from a MaxMind-format database file
// (GeoLite2-Country or commercial GeoIP2-Country). The file is named by
// BURROW_GEOIP_DB (legacy: BURROW_GEO_DB_PATH). Burrow never downloads it:
// MaxMind's terms require the operator to accept the EULA and run
// geoipupdate.
//
// The file is loaded at start and reloaded when it changes (Watch polls its
// mtime and size, about once a minute). A reload that fails keeps the
// database that is loaded, so a half-written download never turns the rules
// off. Without a database (Enabled false) the country rules cannot be
// enforced; see IPGeoEngine.Allow and the tcp policy for what that means.
//
// Thread-safety: Country is safe for concurrent use; a reload swaps the
// reader under the write lock.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

// GeoReloadInterval is how often Watch looks at the database file.
const GeoReloadInterval = time.Minute

// MMDBGeoLookup is the GeoLookup backed by MaxMind's mmdb format.
type MMDBGeoLookup struct {
	mu       sync.RWMutex
	db       *maxminddb.Reader // nil until a load succeeds
	path     string
	openedAt time.Time
	modTime  time.Time // of the file as loaded
	size     int64
	lastErr  string // of the last load that failed, "" after a good one
}

// OpenMMDB opens the mmdb file at path and returns a lookup ready for
// Country(). It fails when the file cannot be read as an mmdb.
func OpenMMDB(path string) (*MMDBGeoLookup, error) {
	if path == "" {
		return nil, errors.New("mmdb path is empty")
	}
	m := &MMDBGeoLookup{path: path}
	if _, err := m.Reload(); err != nil {
		return nil, err
	}
	return m, nil
}

// OpenGeoDB is the factory cmd/server calls. An empty path gives the no-op
// lookup. Otherwise it always returns a lookup that Watch can keep trying:
// when the first load fails the lookup is not Enabled and the error is
// returned (and kept, see LastError), so a file that arrives later is picked
// up.
func OpenGeoDB(path string) (GeoLookup, error) {
	if path == "" {
		return NoopGeoLookup(), nil
	}
	m := &MMDBGeoLookup{path: path}
	_, err := m.Reload()
	return m, err
}

// GeoBuildTagEnabled is kept for callers of the old build tag: the reader is
// always compiled in now.
func GeoBuildTagEnabled() bool { return true }

// Reload loads the file when it is not loaded or has changed (mtime or size)
// since it was. It reports whether a new database is now in use. A failure
// leaves the database that is loaded in place.
func (m *MMDBGeoLookup) Reload() (bool, error) {
	if m == nil {
		return false, nil
	}
	fi, err := os.Stat(m.path)
	if err != nil {
		m.setErr(err)
		return false, fmt.Errorf("geoip database: %w", err)
	}
	m.mu.RLock()
	same := m.db != nil && fi.ModTime().Equal(m.modTime) && fi.Size() == m.size
	m.mu.RUnlock()
	if same {
		return false, nil
	}
	db, err := maxminddb.Open(m.path)
	if err != nil {
		m.setErr(err)
		return false, fmt.Errorf("geoip database: %w", err)
	}
	m.mu.Lock()
	old := m.db
	m.db, m.openedAt, m.modTime, m.size, m.lastErr = db, time.Now(), fi.ModTime(), fi.Size(), ""
	m.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return true, nil
}

func (m *MMDBGeoLookup) setErr(err error) {
	m.mu.Lock()
	m.lastErr = err.Error()
	m.mu.Unlock()
}

// Watch reloads the file when it changes, every interval (GeoReloadInterval
// when zero), until ctx ends. onChange, when not nil, is told of each reload
// and of each new failure.
func (m *MMDBGeoLookup) Watch(ctx context.Context, interval time.Duration, onChange func(reloaded bool, err error)) {
	if m == nil {
		return
	}
	if interval <= 0 {
		interval = GeoReloadInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	lastErr := m.LastError()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		changed, err := m.Reload()
		switch {
		case err != nil && err.Error() != lastErr:
			lastErr = err.Error()
			if onChange != nil {
				onChange(false, err)
			}
		case err == nil:
			lastErr = ""
			if changed && onChange != nil {
				onChange(true, nil)
			}
		}
	}
}

// LastError is why the last load failed, "" when it succeeded.
func (m *MMDBGeoLookup) LastError() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastErr
}

// Country resolves ip to its ISO-3166-1 alpha-2 country code. Returns
// ("", nil) when the IP is in the database but has no country record, or
// when no database is loaded, and ("", err) when the lookup fails.
func (m *MMDBGeoLookup) Country(ip net.IP) (string, error) {
	if m == nil {
		return "", nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.db == nil {
		return "", nil
	}
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	if err := m.db.Lookup(ip, &rec); err != nil {
		return "", err
	}
	return rec.Country.ISOCode, nil
}

// Enabled reports whether a database is loaded.
func (m *MMDBGeoLookup) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.db != nil
}

// DBPath returns the configured path of the mmdb file, loaded or not.
func (m *MMDBGeoLookup) DBPath() string {
	if m == nil {
		return ""
	}
	return m.path
}

// DBAgeSeconds returns the seconds since the loaded database was opened.
func (m *MMDBGeoLookup) DBAgeSeconds() int64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.openedAt.IsZero() {
		return 0
	}
	return int64(time.Since(m.openedAt).Seconds())
}

// Close releases the underlying mmdb reader. Safe to call multiple times;
// Country then returns ("", nil).
func (m *MMDBGeoLookup) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db == nil {
		return nil
	}
	err := m.db.Close()
	m.db = nil
	return err
}
