package quota

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// fakeLimitStore is an in-memory LimitStore for the engine tests.
type fakeLimitStore struct {
	mu   sync.Mutex
	rows []db.RateLimit
}

func (f *fakeLimitStore) ListRateLimits(_ context.Context) ([]db.RateLimit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.RateLimit, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// fakeDailyUsage is the test double for DailyUsageStore.
type fakeDailyUsage struct {
	bytesByKey map[string]int64 // api_key_id → daily byte-estimate
	bytesBySvc map[string]int64 // service_id → daily byte-estimate
	countByKey map[string]int64
	countBySvc map[string]int64
}

func (f *fakeDailyUsage) SumDailyUsageEventsByAPIKey(_ context.Context, k string) (int64, error) {
	return f.bytesByKey[k], nil
}
func (f *fakeDailyUsage) SumDailyUsageEventsByService(_ context.Context, s string) (int64, error) {
	return f.bytesBySvc[s], nil
}
func (f *fakeDailyUsage) CountDailyUsageEventsByAPIKey(_ context.Context, k string) (int64, error) {
	return f.countByKey[k], nil
}
func (f *fakeDailyUsage) CountDailyUsageEventsByService(_ context.Context, s string) (int64, error) {
	return f.countBySvc[s], nil
}

func newEngine(t *testing.T, rows []db.RateLimit) *Engine {
	t.Helper()
	store := &fakeLimitStore{rows: rows}
	usage := &fakeDailyUsage{
		bytesByKey: map[string]int64{}, bytesBySvc: map[string]int64{},
		countByKey: map[string]int64{}, countBySvc: map[string]int64{},
	}
	e := NewWithStores(store, usage)
	if err := e.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return e
}

// TestChargeAllowsUntilBurst — burst=5, charge 5 units → allowed; 6th
// charge → deny with scope and reset_seconds≈60 (full minute window).
func TestChargeAllowsUntilBurst(t *testing.T) {
	e := newEngine(t, []db.RateLimit{{
		ID: "rl1", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
		Lim: 5, Burst: 5, Window: WindowMinute,
	}})
	// Freeze the clock so the lazy refill cannot accidentally regenerate
	// tokens between charges.
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })

	who := Subjects{APIKeyID: "k1"}
	for i := 1; i <= 5; i++ {
		d := e.Charge(context.Background(), who, DimensionRPM, 1)
		if !d.Allow {
			t.Fatalf("charge %d: want allow, got deny scope=%s retry=%d", i, d.LimitingScope, d.RetryAfter)
		}
	}
	d := e.Charge(context.Background(), who, DimensionRPM, 1)
	if d.Allow {
		t.Fatalf("charge 6: want deny, got allow")
	}
	if d.LimitingScope != ScopeAPIKey {
		t.Errorf("LimitingScope = %q, want api_key", d.LimitingScope)
	}
	if d.RetryAfter < 1 || d.RetryAfter > 60 {
		t.Errorf("RetryAfter = %d, want 1..60", d.RetryAfter)
	}
	if d.Kind != "rate_limit" {
		t.Errorf("Kind = %q, want rate_limit", d.Kind)
	}
	if d.LimitingID != "rl1" {
		t.Errorf("LimitingID = %q, want rl1", d.LimitingID)
	}
}

// TestMostRestrictiveScopeWins — two limits in play (api_key rpm=5, role
// rpm=100); after 5 charges the 6th should deny with scope=api_key,
// because api_key is the more-restrictive bucket.
func TestMostRestrictiveScopeWins(t *testing.T) {
	e := newEngine(t, []db.RateLimit{
		{ID: "rk", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
			Lim: 5, Burst: 5, Window: WindowMinute},
		{ID: "rr", Scope: ScopeRole, Subject: "user", Dimension: DimensionRPM,
			Lim: 100, Burst: 100, Window: WindowMinute},
	})
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })

	who := Subjects{APIKeyID: "k1", RoleName: "user"}
	for i := 1; i <= 5; i++ {
		if d := e.Charge(context.Background(), who, DimensionRPM, 1); !d.Allow {
			t.Fatalf("charge %d: want allow, got deny scope=%s", i, d.LimitingScope)
		}
	}
	d := e.Charge(context.Background(), who, DimensionRPM, 1)
	if d.Allow {
		t.Fatalf("charge 6: want deny, got allow")
	}
	if d.LimitingScope != ScopeAPIKey {
		t.Errorf("LimitingScope = %q, want api_key (most restrictive)", d.LimitingScope)
	}
}

// TestBPMByteCounting — bpm dimension subtracts the byte-count units. With
// limit=100000 and burst=100000, 50 charges of 2048 bytes (=102400) should
// drain the bucket and the next charge denies.
func TestBPMByteCounting(t *testing.T) {
	e := newEngine(t, []db.RateLimit{{
		ID: "rb", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionBPM,
		Lim: 100_000, Burst: 100_000, Window: WindowMinute,
	}})
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })

	who := Subjects{APIKeyID: "k1"}
	const unit = 2048
	// 100000 / 2048 = 48.8 → after 48 charges the bucket has 100000-48*2048
	// = 1696 tokens, still enough for one more (1696 < 2048 → next denies).
	allowed := 0
	for i := 0; i < 100; i++ {
		d := e.Charge(context.Background(), who, DimensionBPM, unit)
		if !d.Allow {
			break
		}
		allowed++
	}
	if allowed < 40 || allowed > 50 {
		t.Errorf("allowed=%d, want ~48 (100000/2048)", allowed)
	}
	d := e.Charge(context.Background(), who, DimensionBPM, unit)
	if d.Allow {
		t.Fatalf("want deny after bucket drained, got allow")
	}
	if d.LimitingScope != ScopeAPIKey {
		t.Errorf("scope = %q, want api_key", d.LimitingScope)
	}
}

// TestWindowDayQuota — window=day; charge until usage_events sum exceeds the
// configured cap and assert the deny carries ResetAt = next midnight UTC.
func TestWindowDayQuota(t *testing.T) {
	store := &fakeLimitStore{rows: []db.RateLimit{{
		ID: "rd", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionBPM,
		Lim: 1000, Burst: 1000, Window: WindowDay,
	}}}
	usage := &fakeDailyUsage{
		bytesByKey: map[string]int64{"k1": 999}, // one byte-estimate short of cap
		bytesBySvc: map[string]int64{},
		countByKey: map[string]int64{}, countBySvc: map[string]int64{},
	}
	e := NewWithStores(store, usage)
	if err := e.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Clock at 22:00 UTC → reset_seconds ≈ 2h = 7200s.
	frozen := time.Date(2026, 5, 20, 22, 0, 0, 0, time.UTC)
	e.SetClock(func() time.Time { return frozen })

	who := Subjects{APIKeyID: "k1"}

	// 1-unit charge brings used (999) + 1 = 1000 = cap → still allow.
	if d := e.Charge(context.Background(), who, DimensionBPM, 1); !d.Allow {
		t.Fatalf("charge at exactly cap: want allow, got deny retry=%d", d.RetryAfter)
	}
	// 2-unit charge brings used (999) + 2 = 1001 > 1000 → deny.
	d := e.Charge(context.Background(), who, DimensionBPM, 2)
	if d.Allow {
		t.Fatalf("over-cap charge: want deny, got allow")
	}
	if d.Kind != "quota" {
		t.Errorf("Kind = %q, want quota", d.Kind)
	}
	if d.LimitingScope != ScopeAPIKey {
		t.Errorf("LimitingScope = %q, want api_key", d.LimitingScope)
	}
	// ResetAt = next midnight UTC.
	wantReset := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	if !d.ResetAt.Equal(wantReset) {
		t.Errorf("ResetAt = %v, want %v", d.ResetAt, wantReset)
	}
	// RetryAfter ≈ 2 hours = 7200 seconds (one second slack for ceil).
	if d.RetryAfter < 7199 || d.RetryAfter > 7201 {
		t.Errorf("RetryAfter = %d, want ~7200", d.RetryAfter)
	}
}

// TestChargeWithNoConfiguredLimits — empty engine allows everything.
func TestChargeWithNoConfiguredLimits(t *testing.T) {
	e := newEngine(t, nil)
	d := e.Charge(context.Background(), Subjects{APIKeyID: "anything"}, DimensionRPM, 1)
	if !d.Allow {
		t.Fatalf("empty engine must allow; got deny")
	}
}

// TestSubjectMismatchSkipsLimit — a limit on api_key=k1 does not apply when
// the caller's APIKeyID is k2.
func TestSubjectMismatchSkipsLimit(t *testing.T) {
	e := newEngine(t, []db.RateLimit{{
		ID: "r", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
		Lim: 1, Burst: 1, Window: WindowMinute,
	}})
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })
	// Drain the k1 bucket so any errant match would deny.
	if d := e.Charge(context.Background(), Subjects{APIKeyID: "k1"}, DimensionRPM, 1); !d.Allow {
		t.Fatalf("first k1 charge: %+v", d)
	}
	if d := e.Charge(context.Background(), Subjects{APIKeyID: "k1"}, DimensionRPM, 1); d.Allow {
		t.Fatalf("second k1 charge: want deny")
	}
	// Now charge as k2 — must allow (not the same subject).
	if d := e.Charge(context.Background(), Subjects{APIKeyID: "k2"}, DimensionRPM, 1); !d.Allow {
		t.Fatalf("k2 charge: want allow, got deny scope=%s", d.LimitingScope)
	}
}

// TestGlobalScopeMatchesEverySubject — global-scoped limits apply to all
// callers regardless of subject.
func TestGlobalScopeMatchesEverySubject(t *testing.T) {
	e := newEngine(t, []db.RateLimit{{
		ID: "g", Scope: ScopeGlobal, Subject: "", Dimension: DimensionRPM,
		Lim: 2, Burst: 2, Window: WindowMinute,
	}})
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })

	if d := e.Charge(context.Background(), Subjects{APIKeyID: "k1"}, DimensionRPM, 1); !d.Allow {
		t.Fatalf("k1 #1: deny %+v", d)
	}
	if d := e.Charge(context.Background(), Subjects{APIKeyID: "k2"}, DimensionRPM, 1); !d.Allow {
		t.Fatalf("k2 #1: deny %+v", d)
	}
	// Bucket is now empty (global limit=2).
	d := e.Charge(context.Background(), Subjects{APIKeyID: "k3"}, DimensionRPM, 1)
	if d.Allow {
		t.Fatalf("global cap should deny k3, got allow")
	}
	if d.LimitingScope != ScopeGlobal {
		t.Errorf("scope = %q, want global", d.LimitingScope)
	}
}

// TestReloadPreservesExistingBuckets — after Reload, an unchanged limit
// keeps its current token count (denies don't reset just because the admin
// re-pushed the same config).
func TestReloadPreservesExistingBuckets(t *testing.T) {
	store := &fakeLimitStore{rows: []db.RateLimit{{
		ID: "r", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
		Lim: 2, Burst: 2, Window: WindowMinute,
	}}}
	e := NewWithStores(store, nil)
	if err := e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })

	who := Subjects{APIKeyID: "k1"}
	_ = e.Charge(context.Background(), who, DimensionRPM, 1)
	_ = e.Charge(context.Background(), who, DimensionRPM, 1)
	// Bucket empty.
	if d := e.Charge(context.Background(), who, DimensionRPM, 1); d.Allow {
		t.Fatalf("pre-reload third charge should deny")
	}

	// Reload with the same config — bucket must remain empty.
	if err := e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := e.Charge(context.Background(), who, DimensionRPM, 1); d.Allow {
		t.Fatalf("post-reload charge should still deny; bucket reset?")
	}
}

// TestReloadDropsRemovedLimits — when a limit is removed from the store,
// Reload removes the bucket and Charge allows freely.
func TestReloadDropsRemovedLimits(t *testing.T) {
	store := &fakeLimitStore{rows: []db.RateLimit{{
		ID: "r", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
		Lim: 1, Burst: 1, Window: WindowMinute,
	}}}
	e := NewWithStores(store, nil)
	if err := e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })

	who := Subjects{APIKeyID: "k1"}
	if d := e.Charge(context.Background(), who, DimensionRPM, 1); !d.Allow {
		t.Fatal("first charge should allow")
	}
	if d := e.Charge(context.Background(), who, DimensionRPM, 1); d.Allow {
		t.Fatal("second charge should deny")
	}

	// Remove the limit + Reload.
	store.mu.Lock()
	store.rows = nil
	store.mu.Unlock()
	if err := e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Now even repeated charges are allowed.
	for i := 0; i < 10; i++ {
		if d := e.Charge(context.Background(), who, DimensionRPM, 1); !d.Allow {
			t.Fatalf("after delete: charge %d denied", i)
		}
	}
}

// TestRefillRegeneratesTokens — after the clock advances, the bucket
// refills proportionally to the elapsed time.
func TestRefillRegeneratesTokens(t *testing.T) {
	e := newEngine(t, []db.RateLimit{{
		ID: "r", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
		Lim: 60, Burst: 60, Window: WindowMinute,
	}})
	t0 := time.Unix(1_700_000_000, 0).UTC()
	cur := t0
	e.SetClock(func() time.Time { return cur })

	who := Subjects{APIKeyID: "k1"}
	// Drain.
	for i := 0; i < 60; i++ {
		if d := e.Charge(context.Background(), who, DimensionRPM, 1); !d.Allow {
			t.Fatalf("drain %d: %+v", i, d)
		}
	}
	if d := e.Charge(context.Background(), who, DimensionRPM, 1); d.Allow {
		t.Fatal("post-drain charge should deny")
	}
	// Advance 1 second → 60 limit / 60s = 1 token refilled.
	cur = t0.Add(time.Second)
	if d := e.Charge(context.Background(), who, DimensionRPM, 1); !d.Allow {
		t.Fatalf("after 1s refill: want allow, got deny retry=%d", d.RetryAfter)
	}
	// Bucket now has 0 again.
	if d := e.Charge(context.Background(), who, DimensionRPM, 1); d.Allow {
		t.Fatal("post-refill drained: charge should deny")
	}
}

// TestUsageForReturnsCurrentDebit — UsageFor reports the burst-tokens debit
// and a reset_seconds estimate.
func TestUsageForReturnsCurrentDebit(t *testing.T) {
	e := newEngine(t, []db.RateLimit{{
		ID: "r", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
		Lim: 60, Burst: 60, Window: WindowMinute,
	}})
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })

	who := Subjects{APIKeyID: "k1"}
	for i := 0; i < 10; i++ {
		_ = e.Charge(context.Background(), who, DimensionRPM, 1)
	}
	rows := e.UsageFor(context.Background(), who)
	if len(rows) != 1 {
		t.Fatalf("UsageFor len = %d, want 1", len(rows))
	}
	if rows[0].Used != 10 {
		t.Errorf("Used = %d, want 10", rows[0].Used)
	}
	if rows[0].ResetSeconds <= 0 || rows[0].ResetSeconds > 60 {
		t.Errorf("ResetSeconds = %d, want 1..60", rows[0].ResetSeconds)
	}
}

// TestDayQuotaWithoutUsageStoreAllows — if the engine has no dailyUsage
// hookup, window=day limits are skipped (allow). Keeps minute-only callers
// safe.
func TestDayQuotaWithoutUsageStoreAllows(t *testing.T) {
	store := &fakeLimitStore{rows: []db.RateLimit{{
		ID: "r", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
		Lim: 5, Burst: 5, Window: WindowDay,
	}}}
	e := NewWithStores(store, nil) // no daily-usage store
	_ = e.Reload(context.Background())
	d := e.Charge(context.Background(), Subjects{APIKeyID: "k1"}, DimensionRPM, 1)
	if !d.Allow {
		t.Fatalf("no-usage-store day quota must allow; got deny")
	}
}

// TestDeniedRequestPaysNothing — a denied charge does NOT debit any
// secondary bucket that would otherwise have allowed.
func TestDeniedRequestPaysNothing(t *testing.T) {
	e := newEngine(t, []db.RateLimit{
		{ID: "tight", Scope: ScopeAPIKey, Subject: "k1", Dimension: DimensionRPM,
			Lim: 1, Burst: 1, Window: WindowMinute},
		{ID: "loose", Scope: ScopeRole, Subject: "user", Dimension: DimensionRPM,
			Lim: 100, Burst: 100, Window: WindowMinute},
	})
	frozen := time.Unix(1_700_000_000, 0).UTC()
	e.SetClock(func() time.Time { return frozen })

	who := Subjects{APIKeyID: "k1", RoleName: "user"}
	_ = e.Charge(context.Background(), who, DimensionRPM, 1) // drains k1; role:user now used=1
	if d := e.Charge(context.Background(), who, DimensionRPM, 1); d.Allow {
		t.Fatal("k1 should deny on second charge")
	}
	// 50 additional denies (api_key bucket empty) must NOT debit the role
	// bucket on top of the legitimate 1 used by the first allow above.
	for i := 0; i < 50; i++ {
		_ = e.Charge(context.Background(), who, DimensionRPM, 1)
	}
	rows := e.UsageFor(context.Background(), who)
	var roleUsed int64 = -1
	for _, r := range rows {
		if r.Scope == ScopeRole {
			roleUsed = r.Used
			break
		}
	}
	// role bucket should reflect ONLY the single allowed charge; the 50
	// subsequent denies (api_key empty) must leave the role counter alone.
	if roleUsed != 1 {
		t.Errorf("role bucket used = %d, want 1 (denied charges must not debit)", roleUsed)
	}
}
