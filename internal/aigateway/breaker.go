package aigateway

import (
	"sync"
	"time"
)

// Breaker is a passive circuit breaker keyed by provider slug. It learns only
// from real traffic: hosted providers are never probed, so a health check
// never spends the operator's quota.
//
// One breaker serves every request of the relay; all state is behind mu. What
// it keeps per key is a fixed number of counters, whatever the traffic.
type Breaker struct {
	mu         sync.Mutex
	now        func() time.Time
	window     time.Duration // split into breakerBuckets equal buckets
	coolDown   time.Duration
	minSamples int
	failurePct int
	states     map[string]*breakerState
}

// BreakerState is what a breaker says about one key.
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"    // the provider is used
	BreakerOpen     BreakerState = "open"      // the provider is skipped until the cool-down ends
	BreakerHalfOpen BreakerState = "half_open" // the cool-down is over; one trial is let through and its result decides
)

const breakerBuckets = 60

type breakerState struct {
	buckets  [breakerBuckets]breakerBucket
	openedAt time.Time // non-zero = open since then
	// trialAt is non-zero in half-open: when the one trial of this period
	// was let through. The first Report decides; if none comes within a
	// cool-down (the client left, the provider answered a 4xx), the next
	// Allow is the trial of a new period.
	trialAt time.Time
}

// breakerBucket counts the results of one slice of the window.
type breakerBucket struct {
	slice      int64 // which slice of time the counts belong to
	ok, failed int
}

// NewBreaker returns a breaker with the default thresholds: it opens when at
// least half of the last minute's requests (minimum five) failed, and after 30
// seconds lets one trial request through; its result decides.
func NewBreaker() *Breaker {
	return &Breaker{
		now: time.Now, window: 60 * time.Second, coolDown: 30 * time.Second,
		minSamples: 5, failurePct: 50, states: map[string]*breakerState{},
	}
}

// Allow reports whether key may be tried now. After the cool-down it admits
// one caller as the trial (half-open) and refuses the rest until a Report
// decides; a trial nobody reports on is granted again after another cool-down.
func (b *Breaker) Allow(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[key]
	if s == nil {
		return true
	}
	now := b.now()
	switch {
	case !s.trialAt.IsZero():
		if now.Sub(s.trialAt) < b.coolDown {
			return false
		}
		s.trialAt = now
		return true
	case s.openedAt.IsZero():
		return true
	case now.Sub(s.openedAt) < b.coolDown:
		return false
	}
	s.openedAt, s.trialAt, s.buckets = time.Time{}, now, [breakerBuckets]breakerBucket{}
	return true
}

// Open reports whether key is currently refused: it is open, or half-open
// with its trial under way. It does not start a trial.
func (b *Breaker) Open(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refused(b.states[key])
}

// State returns the state of key for display. It does not start a trial: a
// breaker whose cool-down is over reads as half-open.
func (b *Breaker) State(key string) BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[key]
	switch {
	case s == nil:
		return BreakerClosed
	case !s.trialAt.IsZero():
		return BreakerHalfOpen
	case s.openedAt.IsZero():
		return BreakerClosed
	case b.now().Sub(s.openedAt) < b.coolDown:
		return BreakerOpen
	}
	return BreakerHalfOpen
}

func (b *Breaker) refused(s *breakerState) bool {
	if s == nil {
		return false
	}
	now := b.now()
	if !s.trialAt.IsZero() {
		return now.Sub(s.trialAt) < b.coolDown
	}
	return !s.openedAt.IsZero() && now.Sub(s.openedAt) < b.coolDown
}

// Report records the outcome of one attempt against key. Only a failure of
// the provider is reported as ok=false; a request the provider refused as the
// client's fault is not reported at all.
func (b *Breaker) Report(key string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[key]
	if s == nil {
		s = &breakerState{}
		b.states[key] = s
	}
	now := b.now()
	if !s.trialAt.IsZero() {
		s.trialAt = time.Time{}
		if !ok {
			s.openedAt = now
			return
		}
		s.buckets = [breakerBuckets]breakerBucket{}
	}
	width := int64(b.window) / breakerBuckets
	slice := now.UnixNano() / width
	bk := &s.buckets[int(slice%breakerBuckets)]
	if bk.slice != slice {
		*bk = breakerBucket{slice: slice}
	}
	if ok {
		bk.ok++
	} else {
		bk.failed++
	}
	if !s.openedAt.IsZero() {
		// Already open: a result of a request that was under way neither
		// closes it nor starts the cool-down again.
		return
	}
	total, failed := 0, 0
	for _, c := range s.buckets {
		if c.slice > slice-breakerBuckets {
			total += c.ok + c.failed
			failed += c.failed
		}
	}
	if total >= b.minSamples && failed*100 >= b.failurePct*total {
		s.openedAt = now
	}
}
