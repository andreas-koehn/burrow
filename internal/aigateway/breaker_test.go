package aigateway

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestBreaker() (*Breaker, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	b := NewBreaker()
	b.now = func() time.Time { return now }
	return b, &now
}

// allowed asks for key and drops the trial token.
func allowed(b *Breaker, key string) bool {
	ok, _ := b.Allow(key)
	return ok
}

// trial opens key's breaker, waits out the cool-down and takes the trial.
func trial(t *testing.T, b *Breaker, now *time.Time, key string) uint64 {
	t.Helper()
	for i := 0; i < 5; i++ {
		b.Report(key, false, 0)
	}
	*now = now.Add(31 * time.Second)
	ok, token := b.Allow(key)
	if !ok || token == 0 || b.State(key) != BreakerHalfOpen {
		t.Fatalf("no trial after the cool-down: allowed %v token %d state %s", ok, token, b.State(key))
	}
	return token
}

// In half-open only the trial's own result decides. Results of requests that
// were admitted before the breaker opened change nothing.
func TestBreaker_OnlyTheTrialDecides(t *testing.T) {
	// A stale success does not close; the trial's failure then reopens.
	b, now := newTestBreaker()
	token := trial(t, b, now, "zai")
	b.Report("zai", true, 0)
	if b.State("zai") != BreakerHalfOpen || allowed(b, "zai") {
		t.Fatalf("a stale success decided the trial: state %s", b.State("zai"))
	}
	b.Report("zai", false, token)
	if b.State("zai") != BreakerOpen {
		t.Fatalf("the trial's failure did not reopen: %s", b.State("zai"))
	}

	// A stale failure does not reopen; the trial's success then closes.
	b, now = newTestBreaker()
	token = trial(t, b, now, "zai")
	b.Report("zai", false, 0)
	if b.State("zai") != BreakerHalfOpen {
		t.Fatalf("a stale failure decided the trial: state %s", b.State("zai"))
	}
	b.Report("zai", true, token)
	if b.State("zai") != BreakerClosed || !allowed(b, "zai") {
		t.Fatalf("the trial's success did not close: %s", b.State("zai"))
	}

	// The other order: the trial decides first, a stale result afterwards is
	// an ordinary sample of a closed breaker.
	b, now = newTestBreaker()
	token = trial(t, b, now, "zai")
	b.Report("zai", true, token)
	b.Report("zai", false, 0)
	if b.State("zai") != BreakerClosed {
		t.Fatalf("one failure after a successful trial: %s", b.State("zai"))
	}

	// The token of an abandoned trial does not decide the next one.
	b, now = newTestBreaker()
	old := trial(t, b, now, "zai")
	*now = now.Add(31 * time.Second)
	ok, token := b.Allow("zai")
	if !ok || token == 0 || token == old {
		t.Fatalf("abandoned trial not granted again: %v %d (old %d)", ok, token, old)
	}
	b.Report("zai", true, old)
	if b.State("zai") != BreakerHalfOpen {
		t.Fatalf("an abandoned trial's late result decided the new trial: %s", b.State("zai"))
	}
	b.Report("zai", true, token)
	if b.State("zai") != BreakerClosed {
		t.Fatalf("state %s", b.State("zai"))
	}
	// A closed breaker hands out no token.
	if ok, token := b.Allow("zai"); !ok || token != 0 {
		t.Fatalf("closed: allowed %v token %d", ok, token)
	}
}

func TestBreaker_OpensOnFailureRateAndRecovers(t *testing.T) {
	b, now := newTestBreaker()
	if !allowed(b, "zai") {
		t.Fatal("a fresh key must be allowed")
	}
	for i := 0; i < 4; i++ {
		b.Report("zai", false, 0)
	}
	if !allowed(b, "zai") {
		t.Fatal("4 samples are below the minimum; must still be closed")
	}
	b.Report("zai", false, 0)
	if allowed(b, "zai") || !b.Open("zai") || b.State("zai") != BreakerOpen {
		t.Fatal("5 failures of 5 must open the breaker")
	}
	if !allowed(b, "openrouter") || b.State("openrouter") != BreakerClosed {
		t.Fatal("breakers are per key")
	}

	*now = now.Add(29 * time.Second)
	if allowed(b, "zai") {
		t.Fatal("still inside the cool-down")
	}
	*now = now.Add(2 * time.Second)
	if b.Open("zai") {
		t.Fatal("Open must report a breaker past its cool-down as not refusing")
	}
	ok, token := b.Allow("zai")
	if !ok || token == 0 || b.State("zai") != BreakerHalfOpen {
		t.Fatal("after the cool-down one trial request must be allowed")
	}
	// Half-open: the trial's failure reopens at once, without waiting for 5 samples.
	b.Report("zai", false, token)
	if allowed(b, "zai") || b.State("zai") != BreakerOpen {
		t.Fatal("a failed trial must reopen the breaker")
	}
	*now = now.Add(31 * time.Second)
	_, token = b.Allow("zai")
	b.Report("zai", true, token)
	if !allowed(b, "zai") || b.Open("zai") || b.State("zai") != BreakerClosed {
		t.Fatal("a successful trial must close the breaker")
	}
	// The failures from before the trial are forgotten: four more do not open it.
	for i := 0; i < 3; i++ {
		b.Report("zai", false, 0)
	}
	if !allowed(b, "zai") {
		t.Fatal("old failures counted after a successful trial")
	}
}

func TestBreaker_MixedTrafficBelowThresholdStaysClosed(t *testing.T) {
	b, _ := newTestBreaker()
	for i := 0; i < 10; i++ {
		b.Report("zai", i%3 == 0, 0) // 4 ok, 6 failed = 60 % → opens
	}
	if allowed(b, "zai") {
		t.Fatal("60 % failures must open")
	}
	b2, _ := newTestBreaker()
	for i := 0; i < 10; i++ {
		b2.Report("zai", i%2 == 0 || i == 1, 0) // 6 ok, 4 failed = 40 %
	}
	if !allowed(b2, "zai") {
		t.Fatal("40 % failures must stay closed")
	}
}

func TestBreaker_OldSamplesExpire(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 4; i++ {
		b.Report("zai", false, 0)
	}
	*now = now.Add(61 * time.Second)
	b.Report("zai", false, 0) // the four old failures are outside the window
	if !allowed(b, "zai") {
		t.Fatal("expired samples must not count")
	}
}

// Results that arrive while the breaker is open (requests already under way)
// neither close it nor push its cool-down out.
func TestBreaker_ReportsWhileOpenDoNotMoveTheCoolDown(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 5; i++ {
		b.Report("zai", false, 0)
	}
	*now = now.Add(20 * time.Second)
	b.Report("zai", false, 0)
	b.Report("zai", true, 0)
	if allowed(b, "zai") {
		t.Fatal("a result reported while open closed the breaker")
	}
	*now = now.Add(11 * time.Second) // 31 s after it opened
	if !allowed(b, "zai") {
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
		b.Report("zai", true, 0)
	}
	b.Report("zai", false, 0)
	if !allowed(b, "zai") {
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
			b.Report("k", i%2 == 0, 0)
			_ = allowed(b, "k")
			_ = b.Open("k")
			_ = b.State("k")
		}(i)
	}
	wg.Wait()
}

// Half-open admits one trial, not everyone who asks.
func TestBreaker_HalfOpenAdmitsExactlyOne(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 5; i++ {
		b.Report("zai", false, 0)
	}
	*now = now.Add(31 * time.Second)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allowed(b, "zai") {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := admitted.Load(); n != 1 {
		t.Fatalf("%d of 200 admitted in half-open, want exactly 1", n)
	}
	if !b.Open("zai") || b.State("zai") != BreakerHalfOpen {
		t.Fatalf("while the trial is under way: Open %v State %s", b.Open("zai"), b.State("zai"))
	}
}

// A trial nobody reports on (the client left, the provider answered 4xx) does
// not leave the breaker half-open for good: after another cool-down one more
// trial is admitted, and not before.
func TestBreaker_AbandonedTrialIsGrantedAgain(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 5; i++ {
		b.Report("zai", false, 0)
	}
	*now = now.Add(31 * time.Second)
	if !allowed(b, "zai") {
		t.Fatal("no trial after the cool-down")
	}
	*now = now.Add(29 * time.Second)
	if allowed(b, "zai") {
		t.Fatal("a second trial before the first one's cool-down was over")
	}
	*now = now.Add(2 * time.Second)
	if b.Open("zai") || b.State("zai") != BreakerHalfOpen {
		t.Fatalf("after the abandoned trial's cool-down: Open %v State %s", b.Open("zai"), b.State("zai"))
	}
	ok, token := b.Allow("zai")
	if !ok || token == 0 || allowed(b, "zai") {
		t.Fatal("an abandoned trial must be granted again, once")
	}
	// The second trial succeeds: closed, for everyone.
	b.Report("zai", true, token)
	if !allowed(b, "zai") || !allowed(b, "zai") || b.State("zai") != BreakerClosed {
		t.Fatal("a successful trial did not close the breaker")
	}
	// And a failed trial reopens for a full cool-down.
	for i := 0; i < 5; i++ {
		b.Report("zai", false, 0)
	}
	*now = now.Add(31 * time.Second)
	_, token = b.Allow("zai")
	b.Report("zai", false, token)
	*now = now.Add(29 * time.Second)
	if allowed(b, "zai") || b.State("zai") != BreakerOpen {
		t.Fatal("a failed trial did not reopen the breaker")
	}
}

// A deleted or renamed provider's state is dropped: the slug reads as closed
// and a provider that takes the slug later starts clean.
func TestBreaker_Forget(t *testing.T) {
	b, _ := newTestBreaker()
	for i := 0; i < 5; i++ {
		b.Report("gone", false, 0)
	}
	if !b.Open("gone") {
		t.Fatal("breaker did not open")
	}
	b.Forget("gone")
	b.Forget("never-seen")
	if b.Open("gone") || b.State("gone") != BreakerClosed {
		t.Fatalf("forgotten key is still refused: state %s", b.State("gone"))
	}
	if len(b.states) != 0 {
		t.Fatalf("states kept: %d", len(b.states))
	}

	// A request that was under way reports after the provider is gone: dropped.
	b.Report("gone", false, 0)
	if b.Open("gone") || len(b.states) != 0 {
		t.Fatalf("a late report recreated the entry: %d states", len(b.states))
	}
}

// A closed entry without a sample inside the window goes when a new key appears.
func TestBreaker_IdleSweepDropsStaleClosed(t *testing.T) {
	b, now := newTestBreaker()
	b.Report("quiet", false, 0)
	*now = now.Add(2 * time.Minute)
	b.Report("other", true, 0)
	if _, kept := b.states["quiet"]; kept || len(b.states) != 1 {
		t.Fatalf("stale closed entry kept: %d states", len(b.states))
	}
}

// Late failures of requests that were under way when a provider was deleted
// do not bring its entry back: no other key has to appear first, and a
// provider that takes the slug starts closed and is judged on its own results.
func TestBreaker_ForgetThenLateFailures(t *testing.T) {
	b, _ := newTestBreaker()
	for i := 0; i < 5; i++ {
		b.Report("gone", false, 0)
	}
	if !b.Open("gone") {
		t.Fatal("breaker did not open: the test proves nothing")
	}
	b.Forget("gone")
	for i := 0; i < 5; i++ {
		b.Report("gone", false, 0)
	}
	if b.Open("gone") || b.State("gone") != BreakerClosed {
		t.Fatalf("late failures after Forget opened the slug again: state %s", b.State("gone"))
	}
	if len(b.states) != 0 {
		t.Fatalf("late reports recreated an entry: %d states", len(b.states))
	}

	// The slug is in use again: admitted as closed, without a trial token.
	ok, token := b.Allow("gone")
	if !ok || token != 0 {
		t.Fatalf("new provider under the slug: allowed %v token %d, want true 0", ok, token)
	}
	if _, kept := b.forgotten["gone"]; kept {
		t.Fatal("Allow did not clear the tombstone")
	}
	for i := 0; i < 4; i++ {
		b.Report("gone", false, 0)
	}
	if b.Open("gone") {
		t.Fatal("opened below the sample minimum")
	}
	b.Report("gone", false, 0)
	if !b.Open("gone") || b.State("gone") != BreakerOpen {
		t.Fatalf("five fresh failures did not open the new provider's breaker: state %s", b.State("gone"))
	}
}

// A late report that carries the token of a trial granted before Forget is
// dropped like any other.
func TestBreaker_ForgetDropsLateTrialReport(t *testing.T) {
	b, now := newTestBreaker()
	token := trial(t, b, now, "p")
	b.Forget("p")
	b.Report("p", false, token)
	if b.Open("p") || b.State("p") != BreakerClosed || len(b.states) != 0 {
		t.Fatalf("a forgotten trial's report left state behind: %s, %d states", b.State("p"), len(b.states))
	}
}

// Tombstones do not pile up: one older than the window goes when Forget runs
// again and when a new key appears, and no longer swallows reports.
func TestBreaker_TombstonesExpire(t *testing.T) {
	b, now := newTestBreaker()
	b.Forget("a")
	b.Forget("b")
	*now = now.Add(b.window + time.Second)
	b.Forget("c")
	if len(b.forgotten) != 1 {
		t.Fatalf("Forget kept expired tombstones: %v", b.forgotten)
	}
	*now = now.Add(b.window + time.Second)
	b.Report("new", true, 0)
	if len(b.forgotten) != 0 {
		t.Fatalf("the sweep kept an expired tombstone: %v", b.forgotten)
	}

	// An expired tombstone that was not swept yet no longer drops reports.
	b.Forget("d")
	*now = now.Add(b.window + time.Second)
	for i := 0; i < 5; i++ {
		b.Report("d", false, 0)
	}
	if !b.Open("d") {
		t.Fatal("reports long after Forget were still dropped")
	}
}

// A trial under way is never dropped, however old its entry is: its report
// must still find the trial it belongs to.
func TestBreaker_IdleSweepKeepsTrialUnderWay(t *testing.T) {
	b, now := newTestBreaker()
	token := trial(t, b, now, "p")
	*now = now.Add(b.coolDown + b.window + time.Hour)
	b.Report("other", true, 0)
	if b.State("p") != BreakerHalfOpen {
		t.Fatalf("entry with a trial under way was dropped: state %s", b.State("p"))
	}
	b.Report("p", false, token)
	if b.State("p") != BreakerOpen {
		t.Fatalf("the trial's failure did not reopen: state %s", b.State("p"))
	}
}

// An open breaker within its cool-down and window, and a key with fresh
// samples, are never dropped as idle.
func TestBreaker_IdleSweepKeepsLiveStates(t *testing.T) {
	b, now := newTestBreaker()
	for i := 0; i < 5; i++ {
		b.Report("down", false, 0)
	}
	*now = now.Add(b.coolDown + b.window - time.Second)
	b.Report("fresh", false, 0)
	b.Report("new", true, 0)
	if b.State("down") != BreakerHalfOpen {
		t.Errorf("open breaker was dropped: state %s", b.State("down"))
	}
	if _, kept := b.states["fresh"]; !kept {
		t.Error("a key with a sample inside the window was dropped")
	}
}

// Forget races with requests that ask and report (run with -race).
func TestBreaker_ForgetConcurrent(t *testing.T) {
	b := NewBreaker()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				if i%4 == 0 {
					b.Forget("p")
					continue
				}
				ok, trial := b.Allow("p")
				b.Open("p")
				if ok {
					b.Report("p", j%2 == 0, trial)
				}
			}
		}(i)
	}
	wg.Wait()
	b.Forget("p")
	b.Report("p", false, 0)
	if b.Open("p") || len(b.states) != 0 {
		t.Fatalf("state left after Forget: %d", len(b.states))
	}
}
