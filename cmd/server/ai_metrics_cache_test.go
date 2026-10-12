package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/db"
)

// The relay's store is what the API takes.
var _ api.AIMetricsStore = (*aiMetricsStore)(nil)

// countingMetrics is a usage database that counts its latency reads and
// answers them with what the test set. hold, when set, makes each latency
// read wait until it is closed.
type countingMetrics struct {
	mu       sync.Mutex
	requests map[string]db.AIEndpointCount
	p95      map[string]int
	err      error
	reqErr   error

	latencyReads atomic.Int32
	entered      chan struct{} // one value per latency read that began
	hold         chan struct{}
	// What the context of the last latency read was like when it began.
	ctxErr  error
	ctxLeft time.Duration // to its deadline; 0 without one
}

func (c *countingMetrics) set(p95 map[string]int, err error) {
	c.mu.Lock()
	c.p95, c.err = p95, err
	c.mu.Unlock()
}

func (c *countingMetrics) AIEndpointRequests24h(context.Context) (map[string]db.AIEndpointCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reqErr != nil {
		return nil, c.reqErr
	}
	out := map[string]db.AIEndpointCount{}
	for k, v := range c.requests {
		out[k] = v
	}
	return out, nil
}

func (c *countingMetrics) AIEndpointLatencyP95(ctx context.Context) (map[string]int, error) {
	c.latencyReads.Add(1)
	c.mu.Lock()
	c.ctxErr, c.ctxLeft = ctx.Err(), 0
	if dl, ok := ctx.Deadline(); ok {
		c.ctxLeft = time.Until(dl)
	}
	hold, entered := c.hold, c.entered
	c.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if hold != nil {
		<-hold
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	out := map[string]int{}
	for k, v := range c.p95 {
		out[k] = v
	}
	return out, nil
}

func (c *countingMetrics) AIEndpointMetrics24h(_ context.Context, serviceID string) (db.AIEndpointAgg, error) {
	return db.AIEndpointAgg{Requests: len(serviceID)}, nil
}

// testAIMetrics returns a store over src with a clock the test moves.
func testAIMetrics(src aiMetricsSource, log *slog.Logger) (*aiMetricsStore, *time.Time) {
	clock := time.Unix(1_700_000_000, 0)
	m := newAIMetricsStore(src, log)
	m.now = func() time.Time { return clock }
	return m, &clock
}

func p95Of(t *testing.T, m *aiMetricsStore, service string) int {
	t.Helper()
	counts, err := m.AIEndpointCounts24h(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return counts[service].LatencyP95Ms
}

// The latency is read once per minute, however often it is asked for; the
// counts beside it are read every time.
func TestAIMetricsStore_LatencyIsKeptForAMinute(t *testing.T) {
	src := &countingMetrics{
		requests: map[string]db.AIEndpointCount{"svc": {Requests: 3, CacheHits: 1}},
		p95:      map[string]int{"svc": 840},
	}
	m, clock := testAIMetrics(src, nil)

	counts, err := m.AIEndpointCounts24h(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := counts["svc"]; got != (db.AIEndpointCount{Requests: 3, CacheHits: 1, LatencyP95Ms: 840}) {
		t.Fatalf("first call: %+v", got)
	}

	// Within the window: the figure the database has by now is not read, the
	// counts are.
	src.set(map[string]int{"svc": 999}, nil)
	src.mu.Lock()
	src.requests["svc"] = db.AIEndpointCount{Requests: 4, CacheHits: 1}
	src.mu.Unlock()
	*clock = clock.Add(aiLatencyTTL - time.Second)
	counts, err = m.AIEndpointCounts24h(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := counts["svc"]; got != (db.AIEndpointCount{Requests: 4, CacheHits: 1, LatencyP95Ms: 840}) {
		t.Fatalf("second call within the window: %+v, want the new count and the kept latency", got)
	}
	if n := src.latencyReads.Load(); n != 1 {
		t.Fatalf("%d latency reads for two calls within the window, want 1", n)
	}

	// After the window: read again.
	*clock = clock.Add(time.Second)
	if got := p95Of(t, m, "svc"); got != 999 {
		t.Fatalf("p95 after the window: %d, want the new 999", got)
	}
	if n := src.latencyReads.Load(); n != 2 {
		t.Fatalf("%d latency reads after the window passed once, want 2", n)
	}

	// A service that is in the latency figure and no longer in the counts
	// still shows its latency, and one read of one service passes through.
	src.mu.Lock()
	delete(src.requests, "svc")
	src.mu.Unlock()
	if got := p95Of(t, m, "svc"); got != 999 {
		t.Fatalf("p95 of a service without a count: %d", got)
	}
	if agg, _ := m.AIEndpointMetrics24h(context.Background(), "four"); agg.Requests != 4 {
		t.Fatalf("AIEndpointMetrics24h did not pass through: %+v", agg)
	}
}

// However many callers arrive while the latency is being computed, one read
// runs. With nothing to serve yet they wait for it; with a previous figure
// they get that at once.
func TestAIMetricsStore_OneComputationAtATime(t *testing.T) {
	src := &countingMetrics{
		p95:     map[string]int{"svc": 100},
		hold:    make(chan struct{}),
		entered: make(chan struct{}, 64),
	}
	m, clock := testAIMetrics(src, nil)

	const callers = 16
	results := make(chan int, callers)
	for range callers {
		go func() {
			counts, err := m.AIEndpointCounts24h(context.Background())
			if err != nil {
				results <- -1
				return
			}
			results <- counts["svc"].LatencyP95Ms
		}()
	}
	<-src.entered // the one read has begun
	select {
	case got := <-results:
		t.Fatalf("a caller returned %d before the first figure existed", got)
	case <-time.After(100 * time.Millisecond):
	}
	// A caller that leaves while it waits gets no figure, and no read of its own.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if counts, err := m.AIEndpointCounts24h(gone); err != nil || counts["svc"].LatencyP95Ms != 0 {
		t.Fatalf("a caller that left: %+v, %v", counts, err)
	}
	close(src.hold)
	for range callers {
		select {
		case got := <-results:
			if got != 100 {
				t.Fatalf("a caller got %d, want the 100 of the one read", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a caller never got the figure")
		}
	}
	if n := src.latencyReads.Load(); n != 1 {
		t.Fatalf("%d latency reads for %d concurrent callers, want 1", n, callers+1)
	}

	// The window passes; the next read is slow. Its caller waits for it,
	// everybody else gets the previous figure without waiting.
	hold := make(chan struct{})
	src.mu.Lock()
	src.hold, src.p95 = hold, map[string]int{"svc": 200}
	src.mu.Unlock()
	*clock = clock.Add(aiLatencyTTL)
	// For a goroutine: -1 instead of failing the test from it.
	latency := func() int {
		counts, err := m.AIEndpointCounts24h(context.Background())
		if err != nil {
			return -1
		}
		return counts["svc"].LatencyP95Ms
	}
	refreshed := make(chan int, 1)
	go func() { refreshed <- latency() }()
	<-src.entered
	for range callers {
		done := make(chan int, 1)
		go func() { done <- latency() }()
		select {
		case got := <-done:
			if got != 100 {
				t.Fatalf("during the refresh a caller got %d, want the previous 100", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a caller waited for a refresh although a previous figure exists")
		}
	}
	close(hold)
	if got := <-refreshed; got != 200 {
		t.Fatalf("the caller that refreshed got %d, want the new 200", got)
	}
	if n := src.latencyReads.Load(); n != 2 {
		t.Fatalf("%d latency reads, want 2: one per window", n)
	}
}

// A computation that fails keeps the previous figure, is not repeated before
// the next window, and is logged once per run of failures.
func TestAIMetricsStore_FailedComputationKeepsThePreviousFigure(t *testing.T) {
	logs := &syncBuffer{}
	src := &countingMetrics{p95: map[string]int{"svc": 100}}
	m, clock := testAIMetrics(src, slog.New(slog.NewTextHandler(logs, nil)))
	lines := func() int { return strings.Count(logs.String(), "computation failed") }

	if got := p95Of(t, m, "svc"); got != 100 {
		t.Fatalf("first figure: %d", got)
	}
	src.set(nil, errors.New("database is locked"))
	for window := 1; window <= 3; window++ {
		*clock = clock.Add(aiLatencyTTL)
		for range 5 {
			if got := p95Of(t, m, "svc"); got != 100 {
				t.Fatalf("window %d: p95 %d after a failed computation, want the previous 100", window, got)
			}
		}
		if n := src.latencyReads.Load(); n != int32(1+window) {
			t.Fatalf("window %d: %d latency reads, want one per window (%d)", window, n, 1+window)
		}
	}
	if n := lines(); n != 1 {
		t.Fatalf("%d log lines for three failed computations in a row, want 1:\n%s", n, logs.String())
	}

	// It recovers, and a later failure is a new one.
	src.set(map[string]int{"svc": 300}, nil)
	*clock = clock.Add(aiLatencyTTL)
	if got := p95Of(t, m, "svc"); got != 300 {
		t.Fatalf("p95 after the database answered again: %d, want 300", got)
	}
	src.set(nil, errors.New("database is locked"))
	*clock = clock.Add(aiLatencyTTL)
	if got := p95Of(t, m, "svc"); got != 300 {
		t.Fatalf("p95 %d, want the kept 300", got)
	}
	if n := lines(); n != 2 {
		t.Fatalf("%d log lines, want 2: one per run of failures", n)
	}

	// Without any figure yet, a failure gives the counts without a latency
	// and no error: the provider list still renders.
	fresh := &countingMetrics{
		requests: map[string]db.AIEndpointCount{"svc": {Requests: 2}},
		err:      errors.New("database is locked"),
	}
	fm, _ := testAIMetrics(fresh, nil)
	counts, err := fm.AIEndpointCounts24h(context.Background())
	if err != nil || counts["svc"] != (db.AIEndpointCount{Requests: 2}) {
		t.Fatalf("counts without a latency figure: %+v, %v", counts, err)
	}
	// The counts' own failure is the caller's.
	fresh.mu.Lock()
	fresh.reqErr = errors.New("no such table")
	fresh.mu.Unlock()
	if _, err := fm.AIEndpointCounts24h(context.Background()); err == nil {
		t.Fatal("a failed count read returned no error")
	}
}

// The computation outlives a caller that hangs up, within a bound of its
// own, and never runs past the deadline its caller has.
func TestAIMetricsStore_ComputationIsDetachedFromItsCaller(t *testing.T) {
	src := &countingMetrics{p95: map[string]int{"svc": 100}}
	m, _ := testAIMetrics(src, nil)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	counts, err := m.AIEndpointCounts24h(gone)
	if err != nil || counts["svc"].LatencyP95Ms != 100 {
		t.Fatalf("with a cancelled caller: %+v, %v", counts, err)
	}
	src.mu.Lock()
	ctxErr, left := src.ctxErr, src.ctxLeft
	src.mu.Unlock()
	if ctxErr != nil {
		t.Fatalf("the read ran on a context that is over: %v", ctxErr)
	}
	if left <= 0 || left > aiLatencyTimeout {
		t.Fatalf("the read had %s to its deadline, want within (0, %s]", left, aiLatencyTimeout)
	}

	// A caller with two seconds left that then hangs up: the read goes on,
	// for no longer than those two seconds.
	hurried := &countingMetrics{p95: map[string]int{"svc": 100}}
	hm, _ := testAIMetrics(hurried, nil)
	short, cancelShort := context.WithTimeout(context.Background(), 2*time.Second)
	cancelShort()
	if _, err := hm.AIEndpointCounts24h(short); err != nil {
		t.Fatal(err)
	}
	hurried.mu.Lock()
	defer hurried.mu.Unlock()
	if hurried.ctxErr != nil {
		t.Fatalf("the read ran on a context that is over: %v", hurried.ctxErr)
	}
	if hurried.ctxLeft <= 0 || hurried.ctxLeft > 2*time.Second {
		t.Fatalf("the read had %s to its deadline, want within its caller's 2s", hurried.ctxLeft)
	}
}

// panickyMetrics panics in its first latency read.
type panickyMetrics struct {
	countingMetrics
	panicked atomic.Bool
}

func (p *panickyMetrics) AIEndpointLatencyP95(ctx context.Context) (map[string]int, error) {
	if p.panicked.CompareAndSwap(false, true) {
		panic("driver bug")
	}
	return p.countingMetrics.AIEndpointLatencyP95(ctx)
}

// A read that panics does not leave the store computing for ever.
func TestAIMetricsStore_PanicEndsTheComputation(t *testing.T) {
	src := &panickyMetrics{countingMetrics: countingMetrics{p95: map[string]int{"svc": 100}}}
	m, clock := testAIMetrics(src, nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic of the source was swallowed")
			}
		}()
		_, _ = m.AIEndpointCounts24h(context.Background())
	}()
	*clock = clock.Add(aiLatencyTTL)
	done := make(chan int, 1)
	go func() {
		counts, err := m.AIEndpointCounts24h(context.Background())
		if err != nil {
			done <- -1
			return
		}
		done <- counts["svc"].LatencyP95Ms
	}()
	select {
	case got := <-done:
		if got != 100 {
			t.Fatalf("p95 after the panic: %d, want 100", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("after a panicking read the store still waits for it")
	}
}

// Through the store the figures are the database's own, uncached read.
func TestAIMetricsStore_EqualsTheDatabase(t *testing.T) {
	_, x := realAuditLogger(t)
	ctx := context.Background()
	if err := x.CreateUser(ctx, db.User{ID: "u-aim", Email: "u-aim@test.invalid", PasswordHash: "h", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	row := 0
	for s, n := range []int{1, 20, 57, 101} {
		svc, err := x.GetOrCreateService(ctx, "u-aim", fmt.Sprintf("svc-aim-%d", s), "http")
		if err != nil {
			t.Fatal(err)
		}
		for i := range n {
			row++
			status, cacheHit := 200, i%9 == 0
			if i%13 == 5 {
				status = 502
			}
			if _, err := x.DB().ExecContext(ctx,
				`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, streamed, cache_hit, latency_ms, upstream_status)
				 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				fmt.Sprintf("aim-%d", row), svc.ID, "k1", now.Add(-time.Duration(i)*time.Minute), "openai", 1, 1, false, cacheHit,
				int64(50+(i*7919)%900), status); err != nil {
				t.Fatal(err)
			}
		}
	}
	want, err := x.AIEndpointCounts24h(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 4 {
		t.Fatalf("the database reports %d services, want 4: %v", len(want), want)
	}
	withLatency := 0
	for _, c := range want {
		if c.LatencyP95Ms > 0 {
			withLatency++
		}
	}
	if withLatency < 3 {
		t.Fatalf("only %d services have a latency; the comparison would show nothing: %v", withLatency, want)
	}
	m := newAIMetricsStore(x, nil)
	for call := 1; call <= 2; call++ { // computed, then kept
		got, err := m.AIEndpointCounts24h(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("call %d through the store: %v\nthe database: %v", call, got, want)
		}
	}
}
