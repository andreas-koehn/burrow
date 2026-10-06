package aigateway

import (
	"sync"
	"testing"
	"time"
)

func newTestBreaker() (*Breaker, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	return b, &now
}

func TestBreaker_OpensOnFailureRateAndRecovers(t *testing.T) {
	b, now := newTestBreaker()
	if !b.Allow("zai") {
		t.Fatal("a fresh key must be allowed")
	}
	for i := 0; i < 4; i++ {
		b.Report("zai", false)
	}
	if !b.Allow("zai") {
		t.Fatal("4 samples are below the minimum; must still be closed")
	}
	b.Report("zai", false)
	if b.Allow("zai") || !b.Open("zai") || b.State("zai") != BreakerOpen {
		t.Fatal("5 failures of 5 must open the breaker")
	}
	if !b.Allow("openrouter") || b.State("openrouter") != BreakerClosed {
		t.Fatal("breakers are per key")
	}

	*now = now.Add(29 * time.Second)
	if b.Allow("zai") {
		t.Fatal("still inside the cool-down")
	}
	*now = now.Add(2 * time.Second)
	if b.Open("zai") {
		t.Fatal("Open must report a breaker past its cool-down as not refusing")
	}
	if !b.Allow("zai") || b.State("zai") != BreakerHalfOpen {
		t.Fatal("after the cool-down one trial request must be allowed")
	}
	// Half-open: one failure reopens at once, without waiting for 5 samples.
	b.Report("zai", false)
	if b.Allow("zai") {
		t.Fatal("a failed trial must reopen the breaker")
	}
	*now = now.Add(31 * time.Second)
	_ = b.Allow("zai")
	b.Report("zai", true)
	if !b.Allow("zai") || b.Open("zai") || b.State("zai") != BreakerClosed {
		t.Fatal("a successful trial must close the breaker")
	}
	// The failures from before the trial are forgotten: four more do not open it.
	for i := 0; i < 3; i++ {
		b.Report("zai", false)
	}
	if !b.Allow("zai") {
		t.Fatal("old failures counted after a successful trial")
	}
}

func TestBreaker_MixedTrafficBelowThresholdStaysClosed(t *testing.T) {
	b, _ := newTestBreaker()
	for i := 0; i < 10; i++ {
		b.Report("zai", i%3 == 0) // 4 ok, 6 failed = 60 % → opens
	}
	if b.Allow("zai") {
		t.Fatal("60 % failures must open")
	}
	b2, _ := newTestBreaker()
	for i := 0; i < 10; i++ {
		b2.Report("zai", i%2 == 0 || i == 1) // 6 ok, 4 failed = 40 %
	}
	if !b2.Allow("zai") {
		t.Fatal("40 % failures must stay closed")
	}
}

func TestBreaker_OldSamplesExpire(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 4; i++ {
		b.Report("zai", false)
	}
	*now = now.Add(61 * time.Second)
	b.Report("zai", false) // the four old failures are outside the window
	if !b.Allow("zai") {
		t.Fatal("expired samples must not count")
	}
}

// Results that arrive while the breaker is open (requests already under way)
// neither close it nor push its cool-down out.
func TestBreaker_ReportsWhileOpenDoNotMoveTheCoolDown(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 5; i++ {
		b.Report("zai", false)
	}
	*now = now.Add(20 * time.Second)
	b.Report("zai", false)
	b.Report("zai", true)
	if b.Allow("zai") {
		t.Fatal("a result reported while open closed the breaker")
	}
	*now = now.Add(11 * time.Second) // 31 s after it opened
	if !b.Allow("zai") {
		t.Fatal("a failure reported while open pushed the cool-down out")
	}
}

// Memory per key does not grow with traffic.
func TestBreaker_ManySamplesStayBounded(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 100_000; i++ {
		if i%1000 == 0 {
			*now = now.Add(time.Second)
		}
		b.Report("zai", true)
	}
	b.Report("zai", false)
	if !b.Allow("zai") {
		t.Fatal("one failure among thousands of successes opened the breaker")
	}
}

func TestBreaker_Concurrent(t *testing.T) {
	b := NewBreaker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Report("k", i%2 == 0)
			_ = b.Allow("k")
			_ = b.Open("k")
			_ = b.State("k")
		}(i)
	}
	wg.Wait()
}
