package aigateway

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// entries is the number of keys the limiter holds state for.
func (l *Limiter) entries() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.state)
}

// waitFor polls cond; it fails the test when cond never holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Microsecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("never happened: %s", what)
}

func TestLimiter_BoundsAndReleases(t *testing.T) {
	l := NewLimiter()
	ctx := context.Background()
	r1, ok1 := l.Acquire(ctx, "ollama", 2)
	r2, ok2 := l.Acquire(ctx, "ollama", 2)
	if !ok1 || !ok2 || l.InUse("ollama") != 2 {
		t.Fatalf("two places must be free: %v %v in use %d", ok1, ok2, l.InUse("ollama"))
	}
	// A third caller waits and gives up when its context ends.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, ok := l.Acquire(short, "ollama", 2); ok {
		t.Fatal("a third request got a place")
	}
	if l.Waiting("ollama") != 0 {
		t.Fatal("a waiter that gave up is still queued")
	}
	// Other providers are independent.
	if r, ok := l.Acquire(ctx, "zai", 1); !ok {
		t.Fatal("limits are per key")
	} else {
		r()
	}
	// A waiter gets the place as soon as one is released.
	got := make(chan bool, 1)
	go func() {
		r, ok := l.Acquire(ctx, "ollama", 2)
		if ok {
			r() // before the test hears of it: it counts the places afterwards
		}
		got <- ok
	}()
	waitFor(t, "the waiter queues", func() bool { return l.Waiting("ollama") == 1 })
	r1()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the waiter was refused")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never got the released place")
	}
	r2()
	if n := l.InUse("ollama"); n != 0 {
		t.Fatalf("in use after all releases = %d", n)
	}
	if n := l.entries(); n != 0 {
		t.Fatalf("%d entries left behind by idle keys", n)
	}
}

func TestLimiter_Unlimited(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 100; i++ {
		if _, ok := l.Acquire(context.Background(), "x", 0); !ok {
			t.Fatal("max 0 must never refuse")
		}
	}
	if l.InUse("x") != 0 || l.entries() != 0 {
		t.Fatal("unlimited keys are not counted")
	}
	if _, ok := l.Acquire(context.Background(), "x", -1); !ok {
		t.Fatal("a negative limit is no limit")
	}
	// Nothing to wait for, so nothing a context could end: a provider without
	// a limit is never "busy".
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := l.Acquire(gone, "x", 0); !ok {
		t.Fatal("no limit, and still refused")
	}
}

func TestLimiter_ReleaseIsIdempotent(t *testing.T) {
	l := NewLimiter()
	r, _ := l.Acquire(context.Background(), "k", 1)
	r()
	r() // a second call must not free a place that another request holds
	r1, ok1 := l.Acquire(context.Background(), "k", 1)
	r() // nor a third, now that another request holds it
	if l.InUse("k") != 1 {
		t.Fatalf("in use = %d after a stale release", l.InUse("k"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, ok2 := l.Acquire(ctx, "k", 1)
	if !ok1 || ok2 {
		t.Fatalf("double release corrupted the count: %v %v", ok1, ok2)
	}
	r1()
	if l.InUse("k") != 0 {
		t.Fatalf("in use = %d", l.InUse("k"))
	}
}

// The limit can change while requests are in flight.
func TestLimiter_LimitChanges(t *testing.T) {
	l := NewLimiter()
	ctx := context.Background()
	r1, _ := l.Acquire(ctx, "k", 1)
	// raised to 2: a second request gets in although the first still runs
	r2, ok := l.Acquire(ctx, "k", 2)
	if !ok {
		t.Fatal("raising the limit must admit more")
	}
	// lowered to 1: nobody new gets in until both are back
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, ok := l.Acquire(short, "k", 1); ok {
		t.Fatal("lowering the limit must hold new requests")
	}
	if l.InUse("k") != 2 {
		t.Fatalf("places held under the old limit were lost: %d", l.InUse("k"))
	}
	r1()
	short2, cancel2 := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel2()
	if _, ok := l.Acquire(short2, "k", 1); ok {
		t.Fatal("one of two places is back; the limit of 1 is still reached")
	}
	r2()
	r3, ok := l.Acquire(ctx, "k", 1)
	if !ok || l.InUse("k") != 1 {
		t.Fatalf("after both are back one place is free: %v, in use %d", ok, l.InUse("k"))
	}
	r3()
	if l.InUse("k") != 0 || l.entries() != 0 {
		t.Fatalf("in use %d, entries %d", l.InUse("k"), l.entries())
	}
}

// Waiters queued under a limit get in when a later request brings a higher
// limit or none, without waiting for a release.
func TestLimiter_RaisedLimitAdmitsWaiters(t *testing.T) {
	for _, raised := range []int{4, 0} {
		l := NewLimiter()
		ctx := context.Background()
		r1, _ := l.Acquire(ctx, "k", 1)
		got := make(chan func(), 2)
		for i := 0; i < 2; i++ {
			go func() {
				r, ok := l.Acquire(ctx, "k", 1)
				if !ok {
					r = nil
				}
				got <- r
			}()
		}
		waitFor(t, "two waiters queue", func() bool { return l.Waiting("k") == 2 })
		rNew, ok := l.Acquire(ctx, "k", raised)
		if !ok {
			t.Fatalf("limit %d: the newcomer was refused", raised)
		}
		var rs []func()
		for i := 0; i < 2; i++ {
			select {
			case r := <-got:
				if r == nil {
					t.Fatalf("limit %d: a waiter was refused", raised)
				}
				rs = append(rs, r)
			case <-time.After(10 * time.Second):
				t.Fatalf("limit %d: a waiter stayed queued", raised)
			}
		}
		r1()
		rNew()
		for _, r := range rs {
			r()
		}
		if l.InUse("k") != 0 || l.Waiting("k") != 0 || l.entries() != 0 {
			t.Fatalf("limit %d: in use %d waiting %d entries %d", raised, l.InUse("k"), l.Waiting("k"), l.entries())
		}
	}
}

func TestLimiter_AlreadyCancelledContext(t *testing.T) {
	l := NewLimiter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A request whose caller is gone must not take a place, free or not.
	if r, ok := l.Acquire(ctx, "k", 1); ok {
		r()
		t.Fatal("a cancelled request took a place")
	}
	if l.InUse("k") != 0 || l.entries() != 0 {
		t.Fatalf("in use %d entries %d", l.InUse("k"), l.entries())
	}
}

// A request cancelled while it waits leaves the queue and never takes a place
// later; the place goes to the waiter behind it.
func TestLimiter_CancelledWaiterLeavesTheQueue(t *testing.T) {
	l := NewLimiter()
	bg := context.Background()
	r1, _ := l.Acquire(bg, "k", 1)

	ctx, cancel := context.WithCancel(bg)
	first := make(chan bool, 1)
	go func() {
		r, ok := l.Acquire(ctx, "k", 1)
		if ok {
			r()
		}
		first <- ok
	}()
	waitFor(t, "first waiter queues", func() bool { return l.Waiting("k") == 1 })
	second := make(chan func(), 1)
	go func() {
		r, _ := l.Acquire(bg, "k", 1)
		second <- r
	}()
	waitFor(t, "second waiter queues", func() bool { return l.Waiting("k") == 2 })

	cancel()
	if ok := <-first; ok {
		t.Fatal("a cancelled waiter got a place")
	}
	if l.Waiting("k") != 1 || l.InUse("k") != 1 {
		t.Fatalf("waiting %d in use %d", l.Waiting("k"), l.InUse("k"))
	}
	r1()
	select {
	case r := <-second:
		if l.InUse("k") != 1 {
			t.Fatalf("in use = %d: the cancelled waiter took a place after all", l.InUse("k"))
		}
		r()
	case <-time.After(10 * time.Second):
		t.Fatal("the place was lost with the cancelled waiter")
	}
	if l.InUse("k") != 0 || l.entries() != 0 {
		t.Fatalf("in use %d entries %d", l.InUse("k"), l.entries())
	}
}

// Cancellation racing a release: whoever wins, no place is lost and none is
// handed out twice.
func TestLimiter_CancelRacesRelease(t *testing.T) {
	l := NewLimiter()
	bg := context.Background()
	for i := 0; i < 300; i++ {
		r1, _ := l.Acquire(bg, "k", 1)
		ctx, cancel := context.WithCancel(bg)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if r, ok := l.Acquire(ctx, "k", 1); ok {
				r()
			}
		}()
		waitFor(t, "waiter queues", func() bool { return l.Waiting("k") == 1 })
		go cancel()
		r1()
		<-done
		// The place must be free again for a fresh request.
		short, stop := context.WithTimeout(bg, 5*time.Second)
		r, ok := l.Acquire(short, "k", 1)
		stop()
		if !ok {
			t.Fatalf("round %d: the place was lost (in use %d, waiting %d)", i, l.InUse("k"), l.Waiting("k"))
		}
		r()
		if l.InUse("k") != 0 || l.entries() != 0 {
			t.Fatalf("round %d: in use %d entries %d", i, l.InUse("k"), l.entries())
		}
	}
}

// Review Focus 7: a burst over a limit never has more in flight than the
// limit, everybody is served, and nothing is left behind.
func TestLimiter_Concurrent(t *testing.T) {
	const n, k = 50, 3
	before := runtime.NumGoroutine()
	l := NewLimiter()
	var peak, cur, served int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, ok := l.Acquire(context.Background(), "k", k)
			if !ok {
				return
			}
			c := atomic.AddInt32(&cur, 1)
			if in := l.InUse("k"); in > k {
				t.Errorf("limiter counts %d in use over a limit of %d", in, k)
			}
			for {
				p := atomic.LoadInt32(&peak)
				if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&cur, -1)
			atomic.AddInt32(&served, 1)
			r()
			r()
		}()
	}
	wg.Wait()
	if peak > k {
		t.Fatalf("peak concurrency %d exceeded the limit %d", peak, k)
	}
	if served != n {
		t.Fatalf("%d of %d were served", served, n)
	}
	if l.InUse("k") != 0 || l.Waiting("k") != 0 || l.entries() != 0 {
		t.Fatalf("in use %d waiting %d entries %d", l.InUse("k"), l.Waiting("k"), l.entries())
	}
	waitFor(t, "goroutines end", func() bool { return runtime.NumGoroutine() <= before })
}

// The same burst with half of the callers giving up while they wait.
func TestLimiter_ConcurrentWithCancellation(t *testing.T) {
	const n, k = 60, 2
	l := NewLimiter()
	var peak, cur int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%2 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(i%7)*time.Millisecond)
				defer cancel()
			}
			r, ok := l.Acquire(ctx, "k", k)
			if !ok {
				return
			}
			c := atomic.AddInt32(&cur, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&cur, -1)
			r()
		}(i)
	}
	wg.Wait()
	if peak > k {
		t.Fatalf("peak concurrency %d exceeded the limit %d", peak, k)
	}
	if l.InUse("k") != 0 || l.Waiting("k") != 0 || l.entries() != 0 {
		t.Fatalf("in use %d waiting %d entries %d", l.InUse("k"), l.Waiting("k"), l.entries())
	}
}
