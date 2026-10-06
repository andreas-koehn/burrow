package exact

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// testCache opens a fresh sqlite DB, applies all migrations (so the
// v0.4.0 cache_entries table from 0004 exists), and returns the wired
// Cache engine.
func testCache(t *testing.T) *Cache {
	t.Helper()
	raw, err := db.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(raw); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	d := db.Wrap(raw)
	t.Cleanup(func() { _ = d.Close() })
	return New(d, nil)
}

// TestCanonicaliseDeterministic asserts that two semantically-equal request
// bodies (whitespace differences + key reordering) collapse to the same
// canonical byte string. This is the property the cache key relies on.
func TestCanonicaliseDeterministic(t *testing.T) {
	in1 := CanonicaliseInput{
		Method:       "POST",
		Scheme:       "HTTPS",
		Host:         "api.example.com",
		Path:         "/v1/chat",
		APIKeyHeader: "Authorization",
		Headers: map[string][]string{
			"Content-Type":      {"application/json"},
			"Accept":            {"application/json"},
			"Anthropic-Version": {"2023-06-01"},
			"Authorization":     {"Bearer secret-1"},
			"X-Request-ID":      {"abc"}, // not allowlisted — dropped
		},
		Body: []byte(`{"model":"sonnet","messages":[{"role":"user","content":"hi"}],"stream":false,"n":1}`),
	}
	in2 := CanonicaliseInput{
		Method:       "post", // lower-cased
		Scheme:       "https",
		Host:         "API.example.com", // host casing differs
		Path:         "/v1/chat",
		APIKeyHeader: "Authorization",
		Headers: map[string][]string{
			"anthropic-version": {"2023-06-01"}, // header-name casing differs
			"accept":            {"application/json"},
			"content-type":      {"application/json"},
			"authorization":     {"Bearer secret-2"}, // different key — still excluded
			"x-trace":           {"xyz"},
		},
		// Same body but with: whitespace, key reordering, the excluded keys
		// dropped (`stream`, `n`), and the api-key-header name dropped as a
		// body key just in case.
		Body: []byte(`  {  "messages" : [ {"content":"hi","role":"user"} ] , "model":"sonnet"  }  `),
	}
	c1 := Canonicalise(in1)
	c2 := Canonicalise(in2)
	if string(c1) != string(c2) {
		t.Fatalf("Canonicalise not deterministic:\n  in1 → %q\n  in2 → %q", c1, c2)
	}

	// And: hashing yields the same hex.
	if HashKey(c1) != HashKey(c2) {
		t.Fatalf("HashKey mismatch: %s vs %s", HashKey(c1), HashKey(c2))
	}

	// Different body → different key.
	in3 := in1
	in3.Body = []byte(`{"model":"sonnet","messages":[{"role":"user","content":"different"}]}`)
	if HashKey(Canonicalise(in1)) == HashKey(Canonicalise(in3)) {
		t.Fatal("different payloads must yield different cache keys")
	}

	// The Authorization (api-key) header MUST NOT appear anywhere in the
	// canonical bytes — that's what lets applies_per: global share entries
	// across keys.
	if strings.Contains(string(c1), "secret-1") || strings.Contains(string(c1), "authorization") {
		t.Fatalf("api-key header leaked into canonical bytes:\n%s", c1)
	}
}

// TestLookupMissThenStoreHit walks the basic cache lifecycle: miss → store
// → hit (returns the same body) → ttl-expired → miss again.
func TestLookupMissThenStoreHit(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)

	key := "global:abcd1234"
	// Miss on empty cache.
	if _, hit, err := c.Lookup(ctx, key); err != nil || hit {
		t.Fatalf("empty Lookup: hit=%v err=%v want (false, nil)", hit, err)
	}

	// Store a 200 response.
	ent := Entry{
		Body:       []byte(`{"id":"resp-1"}`),
		Status:     200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		CreatedAt:  time.Now().UTC(),
		TTLSeconds: 3600,
	}
	if err := c.Store(ctx, key, ent); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Hit returns the same body + status.
	got, hit, err := c.Lookup(ctx, key)
	if err != nil || !hit {
		t.Fatalf("after Store: hit=%v err=%v want (true, nil)", hit, err)
	}
	if string(got.Body) != string(ent.Body) || got.Status != ent.Status {
		t.Fatalf("Lookup roundtrip mismatch: got %+v want %+v", got, ent)
	}
	if got.Headers["Content-Type"] != "application/json" {
		t.Fatalf("Lookup headers: %+v", got.Headers)
	}

	// Hit counter incremented.
	if c.hits.Load() != 1 || c.misses.Load() != 1 {
		t.Fatalf("counters: hits=%d misses=%d want 1,1", c.hits.Load(), c.misses.Load())
	}
}

// TestTTLExpiry asserts that an entry whose created_at + ttl < now is not
// returned (Lookup → miss). SQLite-side datetime arithmetic is what we're
// validating end-to-end.
func TestTTLExpiry(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)

	key := "global:expiring"
	// Store with a created_at well in the past + tiny TTL so it is
	// guaranteed-expired on the next Lookup, deterministically.
	if err := c.Store(ctx, key, Entry{
		Body:       []byte(`{"old":"resp"}`),
		Status:     200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		CreatedAt:  time.Now().UTC().Add(-2 * time.Hour),
		TTLSeconds: 60, // 1 minute → expired ~119 minutes ago
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	if _, hit, err := c.Lookup(ctx, key); err != nil || hit {
		t.Fatalf("expired Lookup: hit=%v err=%v want (false, nil)", hit, err)
	}
}

// TestClearScope asserts that Clear("") wipes everything and Clear("scope")
// only deletes rows whose scope_key matches the prefix.
func TestClearScope(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)
	mustStore := func(key string) {
		if err := c.Store(ctx, key, Entry{
			Body: []byte("x"), Status: 200,
			Headers:   map[string]string{"Content-Type": "application/json"},
			CreatedAt: time.Now().UTC(), TTLSeconds: 3600,
		}); err != nil {
			t.Fatalf("Store %s: %v", key, err)
		}
	}
	mustStore("global:a")
	mustStore("endpoint:svc1:/foo:b")
	mustStore("endpoint:svc1:/bar:c")
	mustStore("endpoint:svc2:/baz:d")

	// Targeted clear on one service's endpoint scope.
	if err := c.Clear(ctx, "endpoint:svc1:"); err != nil {
		t.Fatalf("Clear scope: %v", err)
	}
	// global:a survives.
	if _, hit, _ := c.Lookup(ctx, "global:a"); !hit {
		t.Fatal("global:a wrongly cleared")
	}
	// endpoint:svc2 survives.
	if _, hit, _ := c.Lookup(ctx, "endpoint:svc2:/baz:d"); !hit {
		t.Fatal("endpoint:svc2 wrongly cleared")
	}
	// endpoint:svc1 entries gone.
	if _, hit, _ := c.Lookup(ctx, "endpoint:svc1:/foo:b"); hit {
		t.Fatal("endpoint:svc1:/foo not cleared")
	}

	// Now full clear.
	if err := c.Clear(ctx, ""); err != nil {
		t.Fatalf("Clear all: %v", err)
	}
	entries, _, _, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if entries != 0 {
		t.Fatalf("after Clear(\"\"): entries=%d want 0", entries)
	}
}

// TestSettingsFromJSONRejectsUnknownAppliesPer asserts the engine-level
// validation the handler reuses.
func TestSettingsFromJSONRejectsUnknownAppliesPer(t *testing.T) {
	_, err := SettingsFromJSON([]byte(`{"enabled":true,"applies_per":"per_user","ttl_seconds":60,"max_entries":10,"max_per_entry_kb":10}`))
	if err == nil {
		t.Fatal("SettingsFromJSON accepted unknown applies_per")
	}
	if !strings.Contains(err.Error(), "applies_per") {
		t.Fatalf("error should mention applies_per: %v", err)
	}
	// And valid values pass.
	for _, v := range []string{"global", "per_endpoint", "per_api_key"} {
		body := []byte(`{"enabled":true,"applies_per":"` + v + `","ttl_seconds":60,"max_entries":10,"max_per_entry_kb":10}`)
		if _, err := SettingsFromJSON(body); err != nil {
			t.Fatalf("SettingsFromJSON(%q): %v", v, err)
		}
	}
}

// TestStoreTriggersEviction verifies that Store auto-invokes EvictIfOverflow
// when SetMaxEntries has been called with a positive value, trimming the
// oldest-by-last_hit_at entries down to the cap. The eviction goroutine is
// async, so we poll for the row count to converge. last_hit_at is set
// explicitly on each row (via a direct UPDATE) so the trim ordering is
// deterministic — without that, all rows would share NULL last_hit_at and
// COALESCE would fall back to created_at which can tie on identical
// timestamps.
func TestStoreTriggersEviction(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)
	const maxEntries = 5
	c.SetMaxEntries(maxEntries)

	// Insert maxEntries+2 entries with monotonically-increasing last_hit_at.
	// Row i's last_hit_at = 2000-01-01 00:00:0i — so row 0 is oldest, row 6
	// newest. After eviction we expect rows 0 and 1 to be gone.
	const total = maxEntries + 2
	keys := make([]string, total)
	for i := 0; i < total; i++ {
		keys[i] = fmt.Sprintf("global:k%02d", i)
		if err := c.Store(ctx, keys[i], Entry{
			Body:       []byte(fmt.Sprintf(`{"i":%d}`, i)),
			Status:     200,
			Headers:    map[string]string{"Content-Type": "application/json"},
			CreatedAt:  time.Now().UTC(),
			TTLSeconds: 3600,
		}); err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
		// Pin last_hit_at deterministically so eviction picks the lowest
		// indices first. Format must match sqliteTimeFormat so julianday()
		// parses cleanly if the engine ever inspects it (here it doesn't).
		stamp := fmt.Sprintf("2000-01-01 00:00:%02d.000", i)
		if _, err := c.d.DB().ExecContext(ctx,
			`UPDATE cache_entries SET last_hit_at = ? WHERE key_hash = ?`,
			stamp, keys[i],
		); err != nil {
			t.Fatalf("UPDATE last_hit_at row %d: %v", i, err)
		}
	}

	// Eviction is async. Poll the row count for up to ~2s.
	var entries int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		entries, _, _, err = c.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if entries == maxEntries {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if entries != maxEntries {
		t.Fatalf("post-eviction entries=%d want %d", entries, maxEntries)
	}

	// The trimmed rows MUST be the oldest-by-last_hit_at ones — rows 0 and
	// 1. Lookup on key 0/1 must miss; lookup on the remaining keys must hit.
	for i := 0; i < total; i++ {
		_, hit, err := c.Lookup(ctx, keys[i])
		if err != nil {
			t.Fatalf("Lookup row %d: %v", i, err)
		}
		wantHit := i >= 2
		if hit != wantHit {
			t.Fatalf("row %d (%s): hit=%v want %v (oldest 2 should be evicted)",
				i, keys[i], hit, wantHit)
		}
	}
}

// TestSetMaxEntriesZeroDisablesEviction asserts that the default (no
// SetMaxEntries call, or SetMaxEntries(0)) leaves the cache unbounded —
// Store never triggers a trim. This is the safe-default contract; the
// caller (PUT /cache/settings handler) opts in by writing a positive value.
func TestSetMaxEntriesZeroDisablesEviction(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)
	// Explicitly set to zero (also the default) to be unambiguous.
	c.SetMaxEntries(0)

	for i := 0; i < 20; i++ {
		if err := c.Store(ctx, fmt.Sprintf("global:k%02d", i), Entry{
			Body: []byte("x"), Status: 200,
			Headers:   map[string]string{"Content-Type": "application/json"},
			CreatedAt: time.Now().UTC(), TTLSeconds: 3600,
		}); err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}
	// Give any (incorrect) background trim a chance to run.
	time.Sleep(50 * time.Millisecond)
	entries, _, _, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if entries != 20 {
		t.Fatalf("entries=%d want 20 (eviction must not run when cap=0)", entries)
	}
}

// TestStats reports the entries + on_disk_bytes from the DB and computes
// hit_rate from in-process counters.
func TestStats(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)

	// Empty: 0 entries, 0 bytes, 0.0 rate.
	entries, bytes, rate, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats empty: %v", err)
	}
	if entries != 0 || bytes != 0 || rate != 0 {
		t.Fatalf("empty stats: entries=%d bytes=%d rate=%f", entries, bytes, rate)
	}

	body := []byte(`{"data":"some response"}`)
	if err := c.Store(ctx, "global:s1", Entry{
		Body: body, Status: 200,
		Headers:   map[string]string{"Content-Type": "application/json"},
		CreatedAt: time.Now().UTC(), TTLSeconds: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	// Two lookups → one hit, one miss → 0.5
	_, _, _ = c.Lookup(ctx, "global:s1")
	_, _, _ = c.Lookup(ctx, "global:missing")

	entries, bytes, rate, err = c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if entries != 1 {
		t.Fatalf("entries=%d want 1", entries)
	}
	if bytes != int64(len(body)) {
		t.Fatalf("bytes=%d want %d", bytes, len(body))
	}
	if rate < 0.49 || rate > 0.51 {
		t.Fatalf("rate=%f want ~0.5", rate)
	}
}

// TestOnMissHookFiredAfterStore verifies that the OnMiss callback is invoked
// with the correct key and body after a successful Store. Uses an atomic
// counter to assert the goroutine fires without data races.
func TestOnMissHookFiredAfterStore(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)

	var called atomic.Int32
	var gotKey string
	var gotBody []byte

	c.SetOnMiss(func(_ context.Context, key string, body []byte) {
		gotKey = key
		gotBody = body
		called.Add(1)
	})

	key := "global:onmiss"
	body := []byte(`{"result":"42"}`)
	if err := c.Store(ctx, key, Entry{
		Body:       body,
		Status:     200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		CreatedAt:  time.Now().UTC(),
		TTLSeconds: 3600,
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// The callback fires in a goroutine — poll for up to 500 ms.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if called.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if called.Load() == 0 {
		t.Fatal("OnMiss callback was not called after Store")
	}
	if gotKey != key {
		t.Errorf("OnMiss key: got %q want %q", gotKey, key)
	}
	if string(gotBody) != string(body) {
		t.Errorf("OnMiss body: got %q want %q", gotBody, body)
	}
}

// TestOnMissNilSafe asserts that a nil OnMiss callback (default) does not
// panic on Store — the hook must be nil-safe.
func TestOnMissNilSafe(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)
	// No SetOnMiss call — callback is nil by default.

	if err := c.Store(ctx, "global:nilsafe", Entry{
		Body:       []byte(`{"x":1}`),
		Status:     200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		CreatedAt:  time.Now().UTC(),
		TTLSeconds: 3600,
	}); err != nil {
		t.Fatalf("Store with nil OnMiss: %v", err)
	}
	// No panic = pass. Give any goroutine a moment to not-fire.
	time.Sleep(20 * time.Millisecond)
}

// TestSetOnMissClearCallback asserts that SetOnMiss(nil) disables the hook
// after it was previously set.
func TestSetOnMissClearCallback(t *testing.T) {
	ctx := context.Background()
	c := testCache(t)

	var called atomic.Int32
	c.SetOnMiss(func(_ context.Context, _ string, _ []byte) {
		called.Add(1)
	})
	// Immediately clear the hook.
	c.SetOnMiss(nil)

	if err := c.Store(ctx, "global:cleared", Entry{
		Body:       []byte(`{"x":2}`),
		Status:     200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		CreatedAt:  time.Now().UTC(),
		TTLSeconds: 3600,
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if called.Load() != 0 {
		t.Errorf("OnMiss fired after being cleared: called %d times", called.Load())
	}
}

// TestCacheLifecycle_Shared runs the check the Postgres test runs too.
func TestCacheLifecycle_Shared(t *testing.T) {
	checkCacheLifecycle(t, testCache(t), "shared")
}

// checkCacheLifecycle: store, hit, expiry by each entry's own ttl, stats and
// clear, on whatever database the cache sits on. Its keys carry prefix, so it
// can run again against the same database.
func checkCacheLifecycle(t *testing.T, c *Cache, prefix string) {
	t.Helper()
	ctx := context.Background()
	scope := "endpoint:" + prefix + ":/v1/chat/completions"
	key := func(name string) string { return scope + ":" + name }
	_ = c.Clear(ctx, scope)
	t.Cleanup(func() { _ = c.Clear(ctx, scope) })
	hits, misses := c.hits.Load(), c.misses.Load()

	if _, hit, err := c.Lookup(ctx, key("fresh")); err != nil || hit {
		t.Fatalf("empty Lookup: hit=%v err=%v", hit, err)
	}
	now := time.Now().UTC()
	for name, e := range map[string]Entry{
		"fresh":       {CreatedAt: now, TTLSeconds: 3600},
		"almost-gone": {CreatedAt: now.Add(-50 * time.Second), TTLSeconds: 60},
		"just-gone":   {CreatedAt: now.Add(-61 * time.Second), TTLSeconds: 60},
		"long-gone":   {CreatedAt: now.Add(-2 * time.Hour), TTLSeconds: 60},
		"long-lived":  {CreatedAt: now.Add(-2 * time.Hour), TTLSeconds: 3 * 3600},
		"zero-ttl":    {CreatedAt: now, TTLSeconds: 0},
	} {
		e.Body, e.Status, e.Headers = []byte(`{"id":"`+name+`"}`), 201, map[string]string{"Content-Type": "application/json"}
		if err := c.Store(ctx, key(name), e); err != nil {
			t.Fatalf("Store %s: %v", name, err)
		}
	}
	for name, want := range map[string]bool{
		"fresh": true, "almost-gone": true, "long-lived": true,
		"just-gone": false, "long-gone": false, "zero-ttl": false,
	} {
		got, hit, err := c.Lookup(ctx, key(name))
		if err != nil || hit != want {
			t.Errorf("%s: hit=%v err=%v, want hit=%v", name, hit, err, want)
			continue
		}
		if hit && (string(got.Body) != `{"id":"`+name+`"}` || got.Status != 201 || got.Headers["Content-Type"] != "application/json") {
			t.Errorf("%s: entry %+v", name, got)
		}
	}
	// A hit carries when the entry was stored (to the millisecond) and its ttl.
	got, _, _ := c.Lookup(ctx, key("almost-gone"))
	if d := got.CreatedAt.Sub(now.Add(-50 * time.Second)); d < -time.Millisecond || d > time.Millisecond || got.TTLSeconds != 60 {
		t.Errorf("almost-gone: created_at off by %v, ttl %d", d, got.TTLSeconds)
	}
	if h, m := c.hits.Load()-hits, c.misses.Load()-misses; h != 4 || m != 4 {
		t.Errorf("counters: %d hits, %d misses, want 4/4", h, m)
	}
	// A second store under the same key keeps the first entry.
	if err := c.Store(ctx, key("fresh"), Entry{Body: []byte(`other`), Status: 200, CreatedAt: now, TTLSeconds: 60}); err != nil {
		t.Fatalf("second Store: %v", err)
	}
	if got, hit, _ := c.Lookup(ctx, key("fresh")); !hit || string(got.Body) != `{"id":"fresh"}` {
		t.Errorf("second Store replaced the entry: %+v", got)
	}
	if n, bytes, _, err := c.Stats(ctx); err != nil || n < 6 || bytes <= 0 {
		t.Errorf("Stats: %d entries, %d bytes (%v)", n, bytes, err)
	}
	if err := c.Clear(ctx, scope); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, hit, err := c.Lookup(ctx, key("fresh")); err != nil || hit {
		t.Errorf("after Clear: hit=%v err=%v", hit, err)
	}
}
