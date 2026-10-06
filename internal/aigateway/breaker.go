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
// it keeps per key is a fixed number of counters, whatever the traffic. Forget
// drops a key; closed keys without recent samples are dropped as new keys
// appear.
type Breaker struct {
	mu         sync.Mutex
	now        func() time.Time
	window     time.Duration // split into breakerBuckets equal buckets
	coolDown   time.Duration
	minSamples int
	failurePct int
	states     map[string]*breakerState
	trials     uint64 // the last trial handed out; a trial's token is never 0
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
	trial   uint64 // the token of that trial
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
// one caller as the trial (half-open) and refuses the rest until that
// caller's Report decides; a trial nobody reports on is granted again after
// another cool-down.
//
// trial is non-zero for the caller that was granted the trial. It hands the
// value back in Report; in half-open no other report counts. Ask once per
// request and key: a second question while the trial is under way is refused.
func (b *Breaker) Allow(key string) (allowed bool, trial uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[key]
	if s == nil {
		return true, 0
	}
	now := b.now()
	switch {
	case !s.trialAt.IsZero():
		if now.Sub(s.trialAt) < b.coolDown {
			return false, 0
		}
	case s.openedAt.IsZero():
		return true, 0
	case now.Sub(s.openedAt) < b.coolDown:
		return false, 0
	default:
		s.openedAt, s.buckets = time.Time{}, [breakerBuckets]breakerBucket{}
	}
	b.trials++
	s.trialAt, s.trial = now, b.trials
	return true, s.trial
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

// Forget drops what the breaker knows about key. It is called when a
// provider is deleted or renamed, so that the slug reads as closed and a
// provider that takes it later starts clean.
//
// A request that is under way may still report on key afterwards: that
// starts a fresh, closed entry with one sample, which dropIdle removes again.
func (b *Breaker) Forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.states, key)
}

// dropIdle removes every closed entry without a sample inside the window.
// Such an entry says the same as no entry. It runs when a new key appears,
// so the map holds no more keys than were seen within one window, plus the
// open ones. The caller holds mu.
func (b *Breaker) dropIdle(now time.Time) {
	oldest := now.UnixNano()/(int64(b.window)/breakerBuckets) - breakerBuckets
	for key, s := range b.states {
		if !s.openedAt.IsZero() || !s.trialAt.IsZero() {
			continue
		}
		idle := true
		for _, c := range s.buckets {
			if c.slice > oldest && c.ok+c.failed > 0 {
				idle = false
				break
			}
		}
		if idle {
			delete(b.states, key)
		}
	}
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
// client's fault is not reported at all. trial is what Allow returned for
// this request and key.
//
// In half-open only the trial's own report counts, and it decides. A result of
// a request admitted before the breaker opened (a long stream ending, a late
// timeout) says nothing about the provider now and changes nothing.
func (b *Breaker) Report(key string, ok bool, trial uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	s := b.states[key]
	if s == nil {
		b.dropIdle(now)
		s = &breakerState{}
		b.states[key] = s
	}
	if !s.trialAt.IsZero() {
		if trial == 0 || trial != s.trial {
			return
		}
		s.trialAt, s.trial = time.Time{}, 0
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
