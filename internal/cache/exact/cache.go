package exact

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/db"
)

// Settings is the global cache configuration (spec Part B.7 cache sub-object,
// also used for the per-service variant with the same shape). The wire-level
// JSON shape lives in api/cache_handlers.go; this struct is the Go-side
// canonical form the engine consumes.
type Settings struct {
	Enabled       bool
	AppliesPer    string // "global"|"per_endpoint"|"per_api_key"
	TTLSeconds    int
	MaxEntries    int
	MaxPerEntryKB int
}

// DefaultSettings is the v0.4.0 default applied when no row exists in
// settings yet (spec Part B.7 default-fill: cache.enabled=false,
// applies_per=global, ttl=3600, max_entries=10000, max_per_entry_kb=512).
var DefaultSettings = Settings{
	Enabled:       false,
	AppliesPer:    "global",
	TTLSeconds:    3600,
	MaxEntries:    10000,
	MaxPerEntryKB: 512,
}

// ValidAppliesPer is the closed enum the handler rejects unknown values
// against (spec Part B.3 PUT /cache/settings → 400 invalid setting).
var ValidAppliesPer = map[string]bool{
	"global":       true,
	"per_endpoint": true,
	"per_api_key":  true,
}

// Entry is the cached response. Body is opaque bytes the caller (proxy hot
// path, Task 10) writes back to the client on a Lookup hit; Headers is the
// subset of upstream response headers the cache replays (Content-Type at
// minimum); Status is the HTTP status code (always 2xx, since non-2xx are
// not cached); CreatedAt + TTLSeconds drive expiry.
type Entry struct {
	Body       []byte
	Status     int
	Headers    map[string]string
	CreatedAt  time.Time
	TTLSeconds int
}

// sqliteTimeFormat is the zone-less text earlier versions stored created_at
// as (always UTC). Store now binds a UTC time.Time; rows in this format are
// still read.
const sqliteTimeFormat = "2006-01-02 15:04:05.000"

// Cache is the exact-match prompt cache engine. Lookup/Store hit a single
// indexed SELECT/INSERT against cache_entries. LRU eviction runs in a
// background goroutine triggered on Store overflow.
//
// In-process hit/miss counters back the GET /cache/stats hit_rate_24h field;
// they reset on process restart (documented at the API).
//
// Caller-side invariants the engine cannot enforce:
//
//   - This engine does not inspect request headers. Callers MUST honour
//     `Burrow-Cache: bypass` (spec E.2) by skipping the Lookup call entirely.
//   - Streamed responses (SSE/chunked) MUST NOT be passed to Store; the
//     caller is responsible for filtering. v0.4.0 never caches streamed
//     responses.
type Cache struct {
	d   *db.DB
	log *slog.Logger

	// Atomic counters for hit_rate (in-process; reset on restart). The wire
	// field is called hit_rate_24h, but with in-process counters the 24h
	// window is best-effort — at start of process there's no window data.
	hits   atomic.Uint64
	misses atomic.Uint64

	// maxEntries is the LRU cap threaded in from Settings via SetMaxEntries.
	// Zero (the default) disables auto-eviction; the PUT /cache/settings
	// handler updates this whenever the settings row is saved. Stored as
	// atomic so Store can read it without taking a mutex on the hot path.
	maxEntries atomic.Int64

	// evictMu prevents two overlapping eviction goroutines (one running
	// trims, the other queued no-op).
	evictMu sync.Mutex

	// onMissMu guards onMissFn. Using a mutex (rather than atomic.Value) so
	// the function value is safely swappable without the nil-function type
	// assertion gymnastics that atomic.Value requires.
	onMissMu sync.RWMutex
	onMissFn func(ctx context.Context, key string, body []byte)
}

// New constructs a Cache over an open, migrated *db.DB. The logger is used
// for non-fatal background errors (eviction) and may be nil. The cap on
// entries starts at zero (no auto-eviction); call SetMaxEntries to enable.
func New(d *db.DB, log *slog.Logger) *Cache {
	if log == nil {
		log = slog.Default()
	}
	return &Cache{d: d, log: log}
}

// SetMaxEntries updates the LRU cap the auto-eviction path in Store uses.
// The JSON-API PUT /cache/settings handler calls this after a successful
// save so settings changes take effect without a process restart. Values
// <= 0 disable auto-eviction (unbounded cache); positive values trigger a
// background trim on the next Store overflow.
func (c *Cache) SetMaxEntries(n int) {
	if c == nil {
		return
	}
	if n < 0 {
		n = 0
	}
	c.maxEntries.Store(int64(n))
}

// SetOnMiss registers a callback that is invoked AFTER a successful Store
// completes — i.e. the proxy hot path has just inserted a new exact-cache row
// for a previously-missed key. The semantic tier (Task 16) wires this to its
// Promote() call so the new row is indexed for vector search.
//
// The callback runs in a detached goroutine (context.Background()) so that
// caller-context cancellation (e.g. the visitor's stream closing) cannot kill
// the promote step. Nil is safe: passing nil disables the hook.
//
// Calling SetOnMiss concurrently with Store is safe.
func (c *Cache) SetOnMiss(fn func(ctx context.Context, key string, body []byte)) {
	if c == nil {
		return
	}
	c.onMissMu.Lock()
	c.onMissFn = fn
	c.onMissMu.Unlock()
}

// Lookup checks for a cached entry under the given fully-prefixed key. On
// hit, last_hit_at is best-effort touched (errors logged, not returned —
// the caller already has its entry). The row is read by its unique key and
// its age is checked here against its own ttl_seconds: created_at +
// ttl_seconds > now.
//
// Returns (entry, true, nil) on hit; (zero, false, nil) on miss or expiry;
// non-nil error only on real I/O failure.
func (c *Cache) Lookup(ctx context.Context, key string) (Entry, bool, error) {
	if c == nil || c.d == nil {
		return Entry{}, false, errors.New("exact.Cache: not initialised")
	}
	// Two reads. The first takes only what says whether the entry is still
	// within its own ttl, so an expired entry's body is never fetched; that
	// is decided here and not in SQL, because SQLite and Postgres share no
	// expression for the age of a timestamp. key_hash is unique: one row at
	// most. An entry whose created_at cannot be read counts as expired.
	var (
		id         string
		createdRaw any
		ttl        int
	)
	err := c.d.DB().QueryRowContext(ctx,
		`SELECT id, created_at, ttl_seconds FROM cache_entries WHERE key_hash = ?`, key,
	).Scan(&id, &createdRaw, &ttl)
	if errors.Is(err, sql.ErrNoRows) {
		c.misses.Add(1)
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("cache lookup: %w", err)
	}
	createdAt, live := entryLive(createdRaw, ttl, time.Now())
	if !live {
		c.misses.Add(1)
		return Entry{}, false, nil
	}
	var (
		status     int
		headersStr string
		body       []byte
	)
	err = c.d.DB().QueryRowContext(ctx,
		`SELECT status, headers, body FROM cache_entries WHERE id = ?`, id,
	).Scan(&status, &headersStr, &body)
	if errors.Is(err, sql.ErrNoRows) {
		// Cleared or evicted between the two reads.
		c.misses.Add(1)
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("cache lookup: %w", err)
	}
	headers := map[string]string{}
	if headersStr != "" {
		if jerr := json.Unmarshal([]byte(headersStr), &headers); jerr != nil {
			// Corrupt headers JSON: still serve the body — log + continue.
			c.log.Warn("exact.Cache: headers JSON decode failed",
				slog.String("entry_id", id), slog.String("err", jerr.Error()))
			headers = map[string]string{}
		}
	}
	// Best-effort touch — never block a hit on this.
	if _, terr := c.d.DB().ExecContext(ctx,
		`UPDATE cache_entries SET last_hit_at = CURRENT_TIMESTAMP WHERE id = ?`, id,
	); terr != nil {
		c.log.Warn("exact.Cache: touch last_hit_at failed",
			slog.String("entry_id", id), slog.String("err", terr.Error()))
	}
	c.hits.Add(1)
	return Entry{
		Body:       body,
		Status:     status,
		Headers:    headers,
		CreatedAt:  createdAt,
		TTLSeconds: ttl,
	}, true, nil
}

// entryLive reports whether an entry created at raw (as the driver returned
// created_at) is still within its ttl at now, and when it was created.
func entryLive(raw any, ttlSeconds int, now time.Time) (time.Time, bool) {
	createdAt, ok := entryTime(raw)
	if !ok || now.Sub(createdAt).Seconds() >= float64(ttlSeconds) {
		return createdAt, false
	}
	return createdAt, true
}

// goTimeText is how the SQLite driver stores a bound time.Time.
const goTimeText = "2006-01-02 15:04:05.999999999 -0700 MST"

// entryTime reads a cache_entries.created_at value as the driver returns it:
// a time.Time (always on Postgres, where the column is a timestamp with time
// zone and so an instant whatever the session's time zone is), or text: what
// the SQLite driver stores for a time.Time, RFC 3339, the zone-less format
// earlier versions wrote, or SQLite's CURRENT_TIMESTAMP. Zone-less text is
// UTC.
func entryTime(raw any) (time.Time, bool) {
	var text string
	switch v := raw.(type) {
	case time.Time:
		return v.UTC(), true
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return time.Time{}, false
	}
	for _, layout := range []string{goTimeText, time.RFC3339Nano, sqliteTimeFormat, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Store inserts a new cache entry under the given fully-prefixed key. The
// caller (Task 10 wiring) computes the key (scope prefix + sha256 hex) and
// is responsible for ensuring the response is cacheable (2xx, not streamed,
// not body-too-large per per-entry size cap).
//
// Streamed responses (SSE/chunked) MUST NOT be passed to Store; the caller
// is responsible for filtering. v0.4.0 never caches streamed responses.
//
// If the configured maxEntries (see SetMaxEntries) is > 0 and the total row
// count now exceeds it, a background goroutine trims the (count - maxEntries)
// oldest by last_hit_at (with NULL last_hit_at sorted oldest, so never-hit
// entries are evicted first). Store returns as soon as the INSERT commits —
// eviction never blocks the caller path.
//
// While a live entry holds the key, a second Store is swallowed (the first
// writer wins; the second's payload is the same value for the same canonical
// request anyway). An entry that has outlived its ttl is replaced.
func (c *Cache) Store(ctx context.Context, key string, e Entry) error {
	if c == nil || c.d == nil {
		return errors.New("exact.Cache: not initialised")
	}
	headersJSON, err := json.Marshal(e.Headers)
	if err != nil {
		return fmt.Errorf("cache store: marshal headers: %w", err)
	}
	createdAt := e.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	// The scope_key column stores the human-readable prefix (everything left
	// of the last ':' in key — e.g. "global", "endpoint:<svc>:<path>",
	// "apikey:<id>"). Clear scope uses this column to bulk-delete a slice
	// without re-hashing every body.
	scope := scopeFromKey(key)
	// created_at is bound as a UTC time.Time, never as text: Postgres would
	// read zone-less text in the session's time zone, and the entry's age
	// would be off by that zone's offset. Milliseconds are enough.
	createdAt = createdAt.UTC().Truncate(time.Millisecond)
	if err := c.insert(ctx, key, scope, e, string(headersJSON), createdAt); err != nil {
		return err
	}

	// Fire the OnMiss callback in a detached goroutine so the semantic tier
	// can index the new entry without blocking the caller (proxy hot path).
	// Uses context.Background() so the callback outlives the caller's request.
	c.onMissMu.RLock()
	cb := c.onMissFn
	c.onMissMu.RUnlock()
	if cb != nil {
		bodyCopy := make([]byte, len(e.Body))
		copy(bodyCopy, e.Body)
		go cb(context.Background(), key, bodyCopy)
	}

	// Trigger LRU eviction when the configured cap is non-zero. Eviction
	// itself is async (EvictIfOverflow spawns a goroutine), so this never
	// blocks the caller path.
	if limit := c.maxEntries.Load(); limit > 0 {
		c.EvictIfOverflow(int(limit))
	}
	return nil
}

// insert writes the entry unless a live one already holds the key. An entry
// that has outlived its ttl does not keep its key: it is deleted in the same
// transaction, so the new answer is cached. Whether it has expired is decided
// here, as in Lookup. When two writers meet, the first wins (ON CONFLICT DO
// NOTHING), under every isolation level the database may default to.
func (c *Cache) insert(ctx context.Context, key, scope string, e Entry, headersJSON string, createdAt time.Time) error {
	tx, err := c.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cache store: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Two writers of one key may meet here, and the statements below are
	// written for that. Under REPEATABLE READ or SERIALIZABLE Postgres would
	// end the later one with SQLSTATE 40001 instead: pin READ COMMITTED.
	if err := db.ReadCommitted(ctx, c.d.DB(), tx); err != nil {
		return fmt.Errorf("cache store: %w", err)
	}
	var (
		oldID  string
		oldRaw any
		oldTTL int
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, created_at, ttl_seconds FROM cache_entries WHERE key_hash = ?`, key,
	).Scan(&oldID, &oldRaw, &oldTTL)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("cache store: %w", err)
	default:
		if _, live := entryLive(oldRaw, oldTTL, time.Now()); live {
			return nil // first writer wins
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cache_entries WHERE id = ?`, oldID); err != nil {
			return fmt.Errorf("cache store: drop expired entry: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO cache_entries
		  (id, scope_key, key_hash, status, headers, body, created_at, ttl_seconds)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(key_hash) DO NOTHING`,
		uuid.NewString(), scope, key, e.Status, headersJSON, e.Body, createdAt, e.TTLSeconds,
	); err != nil {
		return fmt.Errorf("cache store: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cache store: %w", err)
	}
	return nil
}

// EvictIfOverflow trims the cache down to maxEntries oldest-first by
// last_hit_at (NULLs treated as oldest), running in a background goroutine
// so the caller never waits. The mutex guarantees only one trim is in
// flight at a time. maxEntries <= 0 disables eviction (unbounded cache).
func (c *Cache) EvictIfOverflow(maxEntries int) {
	if c == nil || c.d == nil || maxEntries <= 0 {
		return
	}
	go func() {
		// Detach from the caller context (the proxy request may already be
		// done by the time we run); use a fresh timeout for the eviction
		// itself so a stuck DB doesn't leak goroutines.
		c.evictMu.Lock()
		defer c.evictMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var count int
		if err := c.d.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM cache_entries`).Scan(&count); err != nil {
			c.log.Warn("exact.Cache: eviction count failed", slog.String("err", err.Error()))
			return
		}
		if count <= maxEntries {
			return
		}
		trim := count - maxEntries
		// COALESCE(last_hit_at, created_at) ensures never-hit entries fall
		// back to their creation time for ordering — otherwise a sea of
		// NULLs would all tie at "oldest" and the LIMIT picks an arbitrary
		// subset.
		if _, err := c.d.DB().ExecContext(ctx, `
			DELETE FROM cache_entries
			 WHERE id IN (
				SELECT id FROM cache_entries
				 ORDER BY COALESCE(last_hit_at, created_at) ASC
				 LIMIT ?
			 )`, trim,
		); err != nil {
			c.log.Warn("exact.Cache: eviction delete failed",
				slog.Int("trim", trim), slog.String("err", err.Error()))
		}
	}()
}

// Clear bulk-deletes cache entries under the given scope prefix. Empty
// scope deletes everything (used by DELETE /api/v1/cache/entries). A scope
// like "endpoint:<svc>:" is matched as a LIKE prefix so DELETE
// /api/v1/services/{id}/cache/entries can wipe one service's entries
// without re-hashing.
func (c *Cache) Clear(ctx context.Context, scope string) error {
	if c == nil || c.d == nil {
		return errors.New("exact.Cache: not initialised")
	}
	if scope == "" {
		_, err := c.d.DB().ExecContext(ctx, `DELETE FROM cache_entries`)
		if err != nil {
			return fmt.Errorf("cache clear all: %w", err)
		}
		return nil
	}
	// scope_key is stored exact (without the trailing hex hash) so an "="
	// match clears one global / one endpoint / one apikey scope cleanly.
	// For service-wide endpoint clears (LIKE "endpoint:<svc>:%") the caller
	// passes the LIKE pattern in; we just pass it straight to SQLite.
	_, err := c.d.DB().ExecContext(ctx,
		`DELETE FROM cache_entries WHERE scope_key = ? OR scope_key LIKE ?`,
		scope, scope+"%",
	)
	if err != nil {
		return fmt.Errorf("cache clear scope: %w", err)
	}
	return nil
}

// Stats returns the (entries, on_disk_bytes, hit_rate_24h) snapshot the
// GET /api/v1/cache/stats handler returns. hit_rate is hits / (hits + misses)
// from the in-process atomic counters; on a brand-new process before any
// lookups have run the value is 0.0 (documented limitation: counters reset
// on restart, so the field is "since-start" not literally 24h).
func (c *Cache) Stats(ctx context.Context) (entries int, onDiskBytes int64, hitRate float64, err error) {
	if c == nil || c.d == nil {
		return 0, 0, 0, errors.New("exact.Cache: not initialised")
	}
	row := c.d.DB().QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(length(body)), 0) FROM cache_entries`)
	if err = row.Scan(&entries, &onDiskBytes); err != nil {
		return 0, 0, 0, fmt.Errorf("cache stats: %w", err)
	}
	h := c.hits.Load()
	m := c.misses.Load()
	if h+m > 0 {
		hitRate = float64(h) / float64(h+m)
	}
	return entries, onDiskBytes, hitRate, nil
}

// scopeFromKey returns the scope prefix portion of a fully-qualified cache
// key. The format is "<scope>:<hex>" where <hex> never contains ':' (it's
// sha256 hex), so the prefix is everything before the LAST ':'.
//
// Examples:
//
//	global:abc123…              → "global"
//	endpoint:svc1:/foo:abc123…  → "endpoint:svc1:/foo"
//	apikey:k1:abc123…           → "apikey:k1"
//
// On a malformed key (no ':'), the whole key is returned as the scope.
func scopeFromKey(key string) string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == ':' {
			return key[:i]
		}
	}
	return key
}

// SettingsToJSON marshals Settings into the wire JSON form. Lives on the
// engine side so the JSON shape stays close to the Settings struct.
func (s Settings) ToJSON() ([]byte, error) {
	return json.Marshal(struct {
		Enabled       bool   `json:"enabled"`
		AppliesPer    string `json:"applies_per"`
		TTLSeconds    int    `json:"ttl_seconds"`
		MaxEntries    int    `json:"max_entries"`
		MaxPerEntryKB int    `json:"max_per_entry_kb"`
	}{s.Enabled, s.AppliesPer, s.TTLSeconds, s.MaxEntries, s.MaxPerEntryKB})
}

// SettingsFromJSON parses the wire JSON form. Unknown applies_per values
// are rejected with an error (caller maps to 400 invalid setting).
func SettingsFromJSON(raw []byte) (Settings, error) {
	var w struct {
		Enabled       bool   `json:"enabled"`
		AppliesPer    string `json:"applies_per"`
		TTLSeconds    int    `json:"ttl_seconds"`
		MaxEntries    int    `json:"max_entries"`
		MaxPerEntryKB int    `json:"max_per_entry_kb"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return Settings{}, fmt.Errorf("cache settings: invalid json: %w", err)
	}
	if w.AppliesPer == "" {
		w.AppliesPer = DefaultSettings.AppliesPer
	}
	if !ValidAppliesPer[w.AppliesPer] {
		return Settings{}, fmt.Errorf("cache settings: invalid applies_per %q", w.AppliesPer)
	}
	return Settings{
		Enabled:       w.Enabled,
		AppliesPer:    w.AppliesPer,
		TTLSeconds:    w.TTLSeconds,
		MaxEntries:    w.MaxEntries,
		MaxPerEntryKB: w.MaxPerEntryKB,
	}, nil
}
