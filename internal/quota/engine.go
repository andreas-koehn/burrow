// Package quota implements Burrow's byte-estimate rate-limit + quota engine
// (spec Part D). Currency is byte-estimate per minute (bytes/4), NOT
// tokenization. Two dimensions are supported: rpm (requests/minute) and bpm
// (estimated-bytes/minute). Scopes are api_key | gateway_key | model | role |
// service | global.
//
// The engine holds lazy-refill token buckets in process memory keyed by
// (scope, subject, dimension). On each Charge call the engine consults every
// configured limit that matches the caller's subjects, returning the *most
// restrictive* denial.
//
// Window=day limits are "quotas" in spec terminology (spec Part D.3); they
// reuse the same Engine + Limit struct but bypass the token-bucket runtime and
// instead query the usage_events table on demand (no separate counter table —
// spec Part D.3 PINNED).
//
// The engine is reloadable: handlers call Reload(ctx) after POST/PUT/DELETE
// to refresh the in-memory snapshot of configured limits.
package quota

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// Scope constants. The strings MUST match the DB column values.
const (
	ScopeAPIKey  = "api_key"
	ScopeRole    = "role"
	ScopeService = "service"
	ScopeGlobal  = "global"
	// ScopeGatewayKey limits one gateway key, named by its bare id.
	ScopeGatewayKey = "gateway_key"
	// ScopeModel limits a model, by the name clients ask for, across keys.
	ScopeModel = "model"
)

// Dimension constants. The strings MUST match the DB column values.
const (
	DimensionRPM = "rpm" // requests per window
	DimensionBPM = "bpm" // estimated-bytes per window (bytes/4)
)

// Window constants.
const (
	WindowMinute = "minute"
	WindowDay    = "day"
)

// Limit is one configured rate-limit row exposed to the rest of the
// codebase (handlers + chain). It mirrors db.RateLimit field-for-field but
// keeps the API package decoupled from the storage shape.
type Limit struct {
	ID        string
	Scope     string // api_key|gateway_key|model|role|service|global
	Subject   string
	Dimension string // rpm|bpm
	Limit     int
	Burst     int
	Window    string // minute|day
}

// Subjects identifies the caller for a Charge. Empty strings mean "scope
// not applicable" — e.g. an unauthenticated request has APIKeyID="" so
// api_key-scoped limits skip it. The role / service / global scopes follow
// the same rule: a global limit always applies (empty subject matches), a
// role limit applies when RoleName matches Limit.Subject, etc.
type Subjects struct {
	// APIKeyID is the per-key subject: a service key id, or
	// "gw:<gateway key id>" for a gateway key.
	APIKeyID  string
	RoleName  string
	ServiceID string
	// GatewayKeyID is the bare id of the gateway key that asked; "" for
	// other traffic. gateway_key limits match it.
	GatewayKeyID string
	// Model is the model name the client asked for; "" when the request
	// names none. model limits match it.
	Model string
}

// DailyUsageStore is the narrow read surface the engine needs for window=day
// quotas. *db.DB satisfies it; tests provide a fake. The bytes-* methods
// return byte-estimates (bytes/4) per spec Part D; the count-* methods
// return raw row counts. All return 0 when the subject is empty.
type DailyUsageStore interface {
	SumDailyUsageEventsByGatewayKey(ctx context.Context, gatewayKeyID string) (int64, error)
	CountDailyUsageEventsByGatewayKey(ctx context.Context, gatewayKeyID string) (int64, error)
	SumDailyUsageEventsByModel(ctx context.Context, model string) (int64, error)
	CountDailyUsageEventsByModel(ctx context.Context, model string) (int64, error)
	SumDailyUsageEventsByAPIKey(ctx context.Context, apiKeyID string) (int64, error)
	SumDailyUsageEventsByService(ctx context.Context, serviceID string) (int64, error)
	CountDailyUsageEventsByAPIKey(ctx context.Context, apiKeyID string) (int64, error)
	CountDailyUsageEventsByService(ctx context.Context, serviceID string) (int64, error)
}

// LimitStore is the narrow read surface the engine needs for Reload.
// *db.DB satisfies it; tests provide a fake.
type LimitStore interface {
	ListRateLimits(ctx context.Context) ([]db.RateLimit, error)
}

// Decision is the return shape of Charge. Allow=true means proceed; on
// deny, RetryAfter and LimitingScope describe the denial — RetryAfter is in
// seconds, LimitingScope is the wire-format scope string of the row that
// triggered the denial.
type Decision struct {
	Allow         bool
	RetryAfter    int    // seconds until the bucket admits another request
	LimitingScope string // scope of the most-restrictive rule that denied
	LimitingID    string // the rate_limits.id of the deciding rule (debugging/audit)
	// Kind is "rate_limit" for window=minute denials and "quota" for window=day
	// denials. Callers use this to pick the right JSON body shape (the spec
	// uses different bodies for the two — see 429BodyRateLimit /
	// 429BodyQuota in the api handlers).
	Kind string
	// ResetAt is populated only for window=day (quota) denials, giving the
	// next UTC midnight. Zero for rate_limit denials.
	ResetAt time.Time
}

// bucketKey is the internal sync.Map key for one live bucket.
type bucketKey struct {
	scope, subject, dimension, window string
}

// bucket is one lazy-refill token bucket. tokens is int64-scaled so we can
// represent partial refills as integer math (we keep a millisecond clock and
// compute (elapsed_ms * limit) / (60_000) tokens since last refill).
// limit and burst are the configured values for this bucket; if the row is
// rewritten with a new (limit,burst) Reload swaps them in place. The
// id/limitingScope fields are stashed so deny decisions can report them
// without an extra map lookup.
type bucket struct {
	id            string
	limitingScope string

	limit int64 // tokens per minute (window=minute) — also used as cap for window=day
	burst int64

	mu        sync.Mutex // guards tokens + lastRefillMS; sync.Mutex chosen over atomics for correctness (refill+subtract is two-step)
	tokens    int64      // current available tokens (clamped 0..burst)
	lastRefMS int64      // unix milliseconds at last refill computation
}

// Engine is the rate-limit + quota engine. Construction loads every
// configured row into in-memory buckets; mutation handlers call Reload to
// refresh after a POST/PUT/DELETE. The engine is safe for concurrent use.
type Engine struct {
	store      LimitStore
	dailyUsage DailyUsageStore

	// limits is the current snapshot of all configured rows, indexed by
	// (scope, subject, dimension, window). Used by Charge to look up which
	// configured rows apply to a given Subjects+dimension call.
	mu      sync.RWMutex
	limits  map[bucketKey]Limit
	buckets sync.Map // bucketKey → *bucket
	// nowMS is the clock the engine reads on every Charge. Tests inject a
	// fake clock by setting a non-nil now() function via NewWithClock.
	now func() time.Time
}

// New constructs an Engine and loads the current limit set from the DB.
// When the LimitStore is nil the engine is empty (handlers and chain may
// still call Charge — every call allows). The dailyUsage parameter is
// optional: when nil, window=day limits are *skipped* (deny=false). This
// keeps the engine constructable in tests that only exercise minute-window
// buckets.
func New(d *db.DB) *Engine {
	if d == nil {
		return &Engine{
			limits: map[bucketKey]Limit{},
			now:    time.Now,
		}
	}
	e := &Engine{
		store:      d,
		dailyUsage: d,
		limits:     map[bucketKey]Limit{},
		now:        time.Now,
	}
	// Best-effort initial load — handlers can Reload() to recover from a
	// transient DB error. We do NOT propagate the error here because the
	// engine is otherwise correct (it just has no rules) and the wiring is
	// idempotent — a successful Reload later fills the map.
	_ = e.Reload(context.Background())
	return e
}

// NewWithStores is the test-friendly constructor. The dailyUsage parameter
// may be nil to skip window=day handling.
func NewWithStores(store LimitStore, dailyUsage DailyUsageStore) *Engine {
	return &Engine{
		store:      store,
		dailyUsage: dailyUsage,
		limits:     map[bucketKey]Limit{},
		now:        time.Now,
	}
}

// SetClock overrides the time source for deterministic tests. Production
// callers should not use this.
func (e *Engine) SetClock(now func() time.Time) {
	e.mu.Lock()
	e.now = now
	e.mu.Unlock()
}

// Reload re-reads every rate_limits row from the store and rebuilds the
// in-memory configuration snapshot. Existing buckets for unchanged keys are
// preserved so a Reload during traffic does not reset every counter; buckets
// whose key disappeared are removed from the sync.Map; buckets whose
// (limit, burst) changed are updated in place (tokens clamped to the new
// burst).
func (e *Engine) Reload(ctx context.Context) error {
	if e.store == nil {
		return nil
	}
	rows, err := e.store.ListRateLimits(ctx)
	if err != nil {
		return fmt.Errorf("quota: list rate_limits: %w", err)
	}

	newLimits := make(map[bucketKey]Limit, len(rows))
	for _, r := range rows {
		key := bucketKey{
			scope: r.Scope, subject: r.Subject,
			dimension: r.Dimension, window: r.Window,
		}
		newLimits[key] = Limit{
			ID: r.ID, Scope: r.Scope, Subject: r.Subject,
			Dimension: r.Dimension, Limit: int(r.Lim), Burst: int(r.Burst),
			Window: r.Window,
		}
	}

	e.mu.Lock()
	e.limits = newLimits
	e.mu.Unlock()

	// Drop buckets that no longer have a matching config; update buckets that
	// still exist but whose (limit, burst) changed.
	e.buckets.Range(func(k, v any) bool {
		key := k.(bucketKey)
		if _, ok := newLimits[key]; !ok {
			e.buckets.Delete(key)
			return true
		}
		b := v.(*bucket)
		lim := newLimits[key]
		b.mu.Lock()
		b.limit = int64(lim.Limit)
		// Clamp tokens to new burst.
		if b.tokens > int64(lim.Burst) {
			b.tokens = int64(lim.Burst)
		}
		b.burst = int64(lim.Burst)
		b.id = lim.ID
		b.limitingScope = lim.Scope
		b.mu.Unlock()
		return true
	})

	return nil
}

// DropBucket removes the in-memory token bucket for a specific rule shape
// (scope, subject, dimension, window) without touching the DB or the limits
// map. This is called from the DELETE handler as a belt-and-suspenders
// safeguard: Reload() already removes stale buckets, but if Reload() fails
// (transient DB error) the saturated bucket would survive and keep denying
// requests even though the rule row is gone. Calling DropBucket directly
// guarantees the bucket is evicted regardless of Reload()'s success.
func (e *Engine) DropBucket(scope, subject, dimension, window string) {
	key := bucketKey{scope: scope, subject: subject, dimension: dimension, window: window}
	e.buckets.Delete(key)
}

// Limits returns a snapshot of every configured limit. The returned slice is
// safe to mutate by the caller.
func (e *Engine) Limits() []Limit {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Limit, 0, len(e.limits))
	for _, l := range e.limits {
		out = append(out, l)
	}
	return out
}

// matchingSubject returns the Subjects field that corresponds to the given
// scope, or "" + true for ScopeGlobal (which always applies). The second
// return value reports whether the scope is recognised; an unknown scope
// causes the engine to skip that limit (defensive — should never happen with
// a constrained DB enum, but cheap to guard).
func matchingSubject(scope string, who Subjects) (string, bool) {
	switch scope {
	case ScopeAPIKey:
		return who.APIKeyID, true
	case ScopeGatewayKey:
		return who.GatewayKeyID, true
	case ScopeModel:
		return who.Model, true
	case ScopeRole:
		return who.RoleName, true
	case ScopeService:
		return who.ServiceID, true
	case ScopeGlobal:
		return "", true
	}
	return "", false
}

// Charge consults every configured rate-limit + quota that matches the
// caller's subjects+dimension and either grants the charge (allow=true) or
// returns the most-restrictive denial. units is the size of the charge in
// the dimension's currency:
//
//   - dimension=rpm  → units is typically 1 (one request)
//   - dimension=bpm  → units is the byte-estimate (req+resp bytes / 4)
//
// On allow, the matching minute-window buckets are debited by units atomically.
// On deny, no bucket is mutated (token-bucket invariant: a denied request
// pays nothing).
//
// Window=day "quotas" do NOT debit anything on Charge — they are computed
// from usage_events at decision time (spec Part D.3 PINNED — no separate
// counter table). Their "allow" outcome means the caller has not yet
// exceeded the daily cap; on deny we report the next UTC midnight in
// Decision.ResetAt.
func (e *Engine) Charge(ctx context.Context, who Subjects, dim string, units int) Decision {
	if units < 0 {
		units = 0
	}
	e.mu.RLock()
	cfg := make([]Limit, 0, len(e.limits))
	for _, l := range e.limits {
		if l.Dimension != dim {
			continue
		}
		// Does this limit's (scope, subject) match the caller?
		subj, ok := matchingSubject(l.Scope, who)
		if !ok {
			continue
		}
		if l.Scope != ScopeGlobal && subj != l.Subject {
			continue
		}
		cfg = append(cfg, l)
	}
	e.mu.RUnlock()

	if len(cfg) == 0 {
		// No configured limits match → unbounded.
		return Decision{Allow: true}
	}

	// Two-phase: check all minute-window buckets first to find any denial
	// (without debiting), then check window=day quotas. If everything would
	// allow, debit the minute buckets and return Allow=true. This keeps the
	// "denied request pays nothing" invariant.
	type matched struct {
		lim Limit
		b   *bucket
	}
	minuteMatches := make([]matched, 0, len(cfg))
	dayMatches := make([]Limit, 0, len(cfg))
	for _, l := range cfg {
		switch l.Window {
		case WindowMinute, "":
			b := e.bucketFor(l)
			minuteMatches = append(minuteMatches, matched{l, b})
		case WindowDay:
			dayMatches = append(dayMatches, l)
		}
	}

	// Phase 1: window=minute pre-check (no debit).
	var denied *Decision
	for _, m := range minuteMatches {
		if d := preCheckMinute(m.b, int64(units), e.now); !d.Allow {
			d.LimitingScope = m.lim.Scope
			d.LimitingID = m.lim.ID
			d.Kind = "rate_limit"
			// Most restrictive = longest RetryAfter wins; ties broken by
			// scope precedence (api_key > service > role > global) to keep
			// the wire-visible "scope" stable when two rules deny at once.
			if denied == nil || d.RetryAfter > denied.RetryAfter ||
				(d.RetryAfter == denied.RetryAfter &&
					scopeRank(m.lim.Scope) > scopeRank(denied.LimitingScope)) {
				dc := d
				denied = &dc
			}
		}
	}
	if denied != nil {
		return *denied
	}

	// Phase 2: window=day quotas.
	for _, l := range dayMatches {
		d := e.checkDayQuota(ctx, l, who, units)
		if !d.Allow {
			d.LimitingScope = l.Scope
			d.LimitingID = l.ID
			d.Kind = "quota"
			if denied == nil || d.RetryAfter > denied.RetryAfter {
				dc := d
				denied = &dc
			}
		}
	}
	if denied != nil {
		return *denied
	}

	// Phase 3: commit debits to the minute buckets.
	for _, m := range minuteMatches {
		debitMinute(m.b, int64(units), e.now)
	}
	return Decision{Allow: true}
}

// bucketFor returns the (lazy-created) live bucket for the given configured
// limit. The bucket is keyed by (scope, subject, dimension, window) so two
// configurations with the same shape share a counter, but a re-configure
// via Reload preserves the existing tokens (clamped to the new burst).
func (e *Engine) bucketFor(l Limit) *bucket {
	key := bucketKey{
		scope: l.Scope, subject: l.Subject,
		dimension: l.Dimension, window: l.Window,
	}
	if v, ok := e.buckets.Load(key); ok {
		b := v.(*bucket)
		// Refresh limit/burst lazily in case Reload happened after the
		// bucket was created but before this Charge.
		b.mu.Lock()
		b.limit = int64(l.Limit)
		b.burst = int64(l.Burst)
		b.id = l.ID
		b.limitingScope = l.Scope
		b.mu.Unlock()
		return b
	}
	b := &bucket{
		id:            l.ID,
		limitingScope: l.Scope,
		limit:         int64(l.Limit),
		burst:         int64(l.Burst),
		tokens:        int64(l.Burst), // start full
		lastRefMS:     e.now().UnixMilli(),
	}
	v, _ := e.buckets.LoadOrStore(key, b)
	return v.(*bucket)
}

// preCheckMinute computes whether a debit of units would succeed against the
// bucket WITHOUT mutating it (other than the bookkeeping refill). Returns a
// Decision with Allow=true and RetryAfter=0 on success, or Allow=false plus
// the seconds until the next charge of `units` could succeed.
func preCheckMinute(b *bucket, units int64, now func() time.Time) Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	refillLocked(b, now)
	if b.tokens >= units {
		return Decision{Allow: true}
	}
	// Need `units - b.tokens` more tokens; refill rate is `limit/60` per second.
	need := units - b.tokens
	if b.limit <= 0 {
		// Configured limit of 0 means "always deny"; cap retry at 60s window.
		return Decision{Allow: false, RetryAfter: 60}
	}
	seconds := int(math.Ceil(float64(need) * 60 / float64(b.limit)))
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 60 {
		seconds = 60
	}
	return Decision{Allow: false, RetryAfter: seconds}
}

// debitMinute subtracts units from a bucket whose preCheck already passed.
// We re-acquire the lock and re-check; a concurrent Charge can have stolen
// the tokens between preCheck and debit, in which case the debit still
// proceeds (tokens go negative). Negative tokens are clamped on the next
// preCheck via refill — the worst case is one extra over-budget request per
// concurrent Charge race, which is acceptable for an in-process token
// bucket (cf. golang.org/x/time/rate which has the same property).
func debitMinute(b *bucket, units int64, now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	refillLocked(b, now)
	b.tokens -= units
}

// refillLocked tops up the bucket using the lazy-refill formula:
//
//	tokens += elapsed_ms * limit / 60_000
//
// clamped to [0, burst]. Caller must hold b.mu.
func refillLocked(b *bucket, now func() time.Time) {
	cur := now().UnixMilli()
	if cur <= b.lastRefMS {
		return
	}
	if b.limit > 0 {
		elapsed := cur - b.lastRefMS
		// elapsed_ms * limit / 60_000 — int64 math: limit can be up to a
		// few million, elapsed up to a day → safe well within int64.
		add := (elapsed * b.limit) / 60_000
		if add > 0 {
			b.tokens += add
			if b.tokens > b.burst {
				b.tokens = b.burst
			}
			b.lastRefMS = cur
		}
	} else {
		b.lastRefMS = cur
	}
}

// scopeRank returns the precedence used to break ties when two deny
// decisions have equal RetryAfter. Higher = wins. api_key > gateway_key >
// model > service > role > global keeps the most-specific scope visible in
// the wire body.
func scopeRank(scope string) int {
	switch scope {
	case ScopeAPIKey:
		return 6
	case ScopeGatewayKey:
		return 5
	case ScopeModel:
		return 4
	case ScopeService:
		return 3
	case ScopeRole:
		return 2
	case ScopeGlobal:
		return 1
	}
	return 0
}

// checkDayQuota consults usage_events for the day so far against the given
// limit row. units is the prospective charge; the limit is enforced AFTER
// admit: if used+units > limit the engine denies. This matches the
// pre-charge semantics of the minute-window path.
//
// When dailyUsage is nil, day quotas are skipped (allow). This keeps
// minute-only callers (tests, early wiring) functional.
func (e *Engine) checkDayQuota(ctx context.Context, l Limit, who Subjects, units int) Decision {
	if e.dailyUsage == nil {
		return Decision{Allow: true}
	}
	used, ok, err := e.dailyUsed(ctx, l, who)
	if !ok {
		// role / global day-quotas are not computable from usage_events
		// alone (no role label on usage_events; global = all rows is a
		// future enhancement). Skip — allow.
		return Decision{Allow: true}
	}
	if err != nil {
		// Fail-open on DB error so a transient read failure does not block
		// every request: the day quota is not enforced while the read fails.
		slog.Default().Warn("quota: daily usage read failed; day quota not enforced",
			slog.String("limit_id", l.ID), slog.String("scope", l.Scope), slog.String("err", err.Error()))
		return Decision{Allow: true}
	}
	if used+int64(units) > int64(l.Limit) {
		nextMidnight := nextUTCMidnight(e.now())
		retry := int(math.Ceil(nextMidnight.Sub(e.now()).Seconds()))
		if retry < 1 {
			retry = 1
		}
		return Decision{
			Allow:      false,
			RetryAfter: retry,
			ResetAt:    nextMidnight,
		}
	}
	return Decision{Allow: true}
}

// dailyUsed reads what the subject of a day-window limit has used since UTC
// midnight, in the limit's dimension. ok is false for a scope usage_events
// cannot answer (role, global).
func (e *Engine) dailyUsed(ctx context.Context, l Limit, who Subjects) (used int64, ok bool, err error) {
	bpm := l.Dimension == DimensionBPM
	switch l.Scope {
	case ScopeAPIKey:
		if bpm {
			used, err = e.dailyUsage.SumDailyUsageEventsByAPIKey(ctx, who.APIKeyID)
		} else {
			used, err = e.dailyUsage.CountDailyUsageEventsByAPIKey(ctx, who.APIKeyID)
		}
	case ScopeService:
		if bpm {
			used, err = e.dailyUsage.SumDailyUsageEventsByService(ctx, who.ServiceID)
		} else {
			used, err = e.dailyUsage.CountDailyUsageEventsByService(ctx, who.ServiceID)
		}
	case ScopeGatewayKey:
		if bpm {
			used, err = e.dailyUsage.SumDailyUsageEventsByGatewayKey(ctx, who.GatewayKeyID)
		} else {
			used, err = e.dailyUsage.CountDailyUsageEventsByGatewayKey(ctx, who.GatewayKeyID)
		}
	case ScopeModel:
		if bpm {
			used, err = e.dailyUsage.SumDailyUsageEventsByModel(ctx, who.Model)
		} else {
			used, err = e.dailyUsage.CountDailyUsageEventsByModel(ctx, who.Model)
		}
	default:
		return 0, false, nil
	}
	return used, true, err
}

// nextUTCMidnight returns the next 00:00 UTC strictly after t.
func nextUTCMidnight(t time.Time) time.Time {
	tu := t.UTC()
	return time.Date(tu.Year(), tu.Month(), tu.Day()+1, 0, 0, 0, 0, time.UTC)
}

// Usage is a snapshot of a single limit's current state, returned by the
// /rate-limits/usage endpoint. Used is the current debit (burst - tokens
// for minute windows; usage_events sum/count for day windows). ResetSeconds
// is the same value Charge would return on deny.
type Usage struct {
	Limit
	Used         int64
	ResetSeconds int
}

// UsageFor returns one Usage row per configured limit that matches the given
// Subjects. The "snapshot" semantics here are advisory — Used reflects the
// state at call time and may change concurrently. Day-window rows are
// computed by querying usage_events on demand.
func (e *Engine) UsageFor(ctx context.Context, who Subjects) []Usage {
	e.mu.RLock()
	cfg := make([]Limit, 0, len(e.limits))
	for _, l := range e.limits {
		subj, ok := matchingSubject(l.Scope, who)
		if !ok {
			continue
		}
		if l.Scope != ScopeGlobal && subj != l.Subject {
			continue
		}
		cfg = append(cfg, l)
	}
	e.mu.RUnlock()

	out := make([]Usage, 0, len(cfg))
	for _, l := range cfg {
		u := Usage{Limit: l}
		switch l.Window {
		case WindowMinute, "":
			b := e.bucketFor(l)
			b.mu.Lock()
			refillLocked(b, e.now)
			u.Used = b.burst - b.tokens
			if u.Used < 0 {
				u.Used = 0
			}
			// ResetSeconds: how long until the bucket is fully refilled.
			missing := b.burst - b.tokens
			if missing <= 0 || b.limit <= 0 {
				u.ResetSeconds = 0
			} else {
				secs := int(math.Ceil(float64(missing) * 60 / float64(b.limit)))
				if secs > 60 {
					secs = 60
				}
				u.ResetSeconds = secs
			}
			b.mu.Unlock()
		case WindowDay:
			if e.dailyUsage != nil {
				u.Used, _, _ = e.dailyUsed(ctx, l, who)
			}
			nextMidnight := nextUTCMidnight(e.now())
			u.ResetSeconds = int(math.Ceil(nextMidnight.Sub(e.now()).Seconds()))
		}
		out = append(out, u)
	}
	return out
}

// Compile-time interface assertion: *db.DB satisfies the engine's narrow
// surfaces. If a future db.DB rename breaks this, the build will fail at
// the engine boundary, which is exactly where we want the friction.
var (
	_ LimitStore      = (*db.DB)(nil)
	_ DailyUsageStore = (*db.DB)(nil)
)

// atomicBool is unused — left here as a placeholder for a future
// "engine paused" toggle that would be more efficient than locking the
// whole map on every Charge. Kept as `_` to satisfy go-vet/golangci.
var _ atomic.Bool
