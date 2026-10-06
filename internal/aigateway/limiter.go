package aigateway

import (
	"context"
	"sync"
)

// Limiter bounds how many requests each provider serves at once. The limit is
// passed on every call, so a changed provider setting applies from the next
// request on without rebuilding anything: the most recent limit a key was
// asked with is the one its queue is served by. Places taken under an earlier
// limit stay counted until they are given back.
//
// Places are handed out in the order requests arrived. A key has an entry
// only while a request holds a place or waits for one, so a provider that is
// renamed or deleted leaves nothing behind once its requests have ended.
//
// Three limits of this design are accepted; do not "fix" them in passing:
//   - Nothing watches the setting. A raised or lifted limit lets waiters in at
//     the next Acquire or release for that key, not at the moment it is saved.
//   - Requests admitted while a key had no limit are not counted. When a limit
//     is set later they run on beside it until they end.
//   - The key is the provider's slug. After a rename the requests admitted or
//     queued under the old slug drain under the old key, next to the new one's.
type Limiter struct {
	mu    sync.Mutex
	state map[string]*limitState
}

type limitState struct {
	inUse   int
	max     int           // the limit of the most recent Acquire; <= 0 = none
	waiters []*slotWaiter // in order of arrival
}

type slotWaiter struct {
	ready   chan struct{} // closed when the place is handed over
	granted bool          // under Limiter.mu: the place is this waiter's
}

// NewLimiter returns an empty limiter.
func NewLimiter() *Limiter { return &Limiter{state: map[string]*limitState{}} }

// Acquire waits for a place at key, at most until ctx ends. max <= 0 means
// unlimited: it returns ok at once, whatever ctx says, and the request is not
// counted. Under a limit a request whose ctx has ended gets no place, free or
// not. When ok, release must be called when the request is over; calling it
// again does nothing.
func (l *Limiter) Acquire(ctx context.Context, key string, max int) (release func(), ok bool) {
	if max <= 0 {
		// The limit was lifted: whoever still waits under the old one goes in.
		l.mu.Lock()
		if s := l.state[key]; s != nil {
			s.max = 0
			s.grant()
		}
		l.mu.Unlock()
		return func() {}, true
	}
	if ctx.Err() != nil {
		return nil, false
	}
	l.mu.Lock()
	s := l.state[key]
	if s == nil {
		s = &limitState{}
		l.state[key] = s
	}
	// A raised limit lets waiters in before this request, which came later.
	s.max = max
	s.grant()
	if len(s.waiters) == 0 && s.inUse < max {
		s.inUse++
		l.mu.Unlock()
		return l.releaser(key), true
	}
	w := &slotWaiter{ready: make(chan struct{})}
	s.waiters = append(s.waiters, w)
	l.mu.Unlock()

	select {
	case <-w.ready:
		if ctx.Err() != nil {
			// Handed a place at the moment it gave up: pass it on.
			l.release(key)
			return nil, false
		}
		return l.releaser(key), true
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted {
			l.mu.Unlock()
			l.release(key)
			return nil, false
		}
		for i, q := range s.waiters {
			if q == w {
				s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
				break
			}
		}
		l.drop(key, s)
		l.mu.Unlock()
		return nil, false
	}
}

// grant hands free places to waiters, in order. Called with Limiter.mu held.
func (s *limitState) grant() {
	for len(s.waiters) > 0 && (s.max <= 0 || s.inUse < s.max) {
		w := s.waiters[0]
		s.waiters[0] = nil
		s.waiters = s.waiters[1:]
		s.inUse++
		w.granted = true
		close(w.ready)
	}
}

// drop forgets a key nobody uses or waits for. Called with l.mu held.
func (l *Limiter) drop(key string, s *limitState) {
	if s.inUse == 0 && len(s.waiters) == 0 && l.state[key] == s {
		delete(l.state, key)
	}
}

// releaser returns the function that gives one place at key back, once.
func (l *Limiter) releaser(key string) func() {
	var once sync.Once
	return func() { once.Do(func() { l.release(key) }) }
}

func (l *Limiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state[key]
	if s == nil || s.inUse == 0 {
		return
	}
	s.inUse--
	s.grant()
	l.drop(key, s)
}

// InUse reports how many places at key are taken now. Requests admitted while
// the key had no limit are not counted.
func (l *Limiter) InUse(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.state[key]; s != nil {
		return s.inUse
	}
	return 0
}
