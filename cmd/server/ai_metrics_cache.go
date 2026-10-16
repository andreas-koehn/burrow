// ai_metrics_cache.go — the usage figures of the AI provider pages, as the
// API reads them. The request and cache-hit counts are read from the
// database each time; the p95 latency is computed at most once a minute.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

const (
	// aiLatencyTTL is how long a computed p95 latency is served before it is
	// computed again. The read behind it walks every usage row of the last
	// 24 hours; on SQLite it holds the one connection meanwhile and every
	// other statement waits, so it must not run per request of a dashboard
	// that polls. The figure is a percentile over a day: a minute's delay
	// changes little of it.
	aiLatencyTTL = time.Minute
	// aiLatencyTimeout bounds one computation, whoever asked for it. A
	// caller's own deadline bounds it too (see compute).
	aiLatencyTimeout = 30 * time.Second
)

// aiMetricsSource is *db.DB as aiMetricsStore needs it.
type aiMetricsSource interface {
	AIEndpointRequests24h(ctx context.Context) (map[string]db.AIEndpointCount, error)
	AIEndpointLatencyP95(ctx context.Context) (map[string]int, error)
	AIEndpointMetrics24h(ctx context.Context, serviceID string) (db.AIEndpointAgg, error)
}

// aiMetricsStore is the relay's api.AIMetricsStore. It answers like *db.DB,
// except that the p95 latency of all services together is kept for
// aiLatencyTTL:
//
//   - One computation at a time. A caller that finds the figure too old
//     computes it and waits for the result; callers that arrive meanwhile get
//     the previous figure, or wait for that computation when there is none
//     yet. Nobody starts a second read.
//   - The computation does not end when its caller hangs up: it runs on a
//     context detached from the request's cancellation, so that work
//     everybody after it would repeat is not thrown away. It keeps the
//     request's deadline, though: no handler runs longer for it than the API
//     lets a handler run, which the shutdown grace of main relies on.
//   - A computation that fails keeps the previous figure and is tried again
//     after aiLatencyTTL, not at once: a read that fails slowly must not run
//     per request either. The failure is logged once, until one succeeds.
type aiMetricsStore struct {
	src aiMetricsSource
	log *slog.Logger
	now func() time.Time

	mu sync.Mutex
	// p95 is the last figure computed, service id → ms; nil until one
	// computation succeeded. The map is never written after it is stored
	// here: callers share it.
	p95 map[string]int
	// at is when the last computation started, tried whether there was one.
	at    time.Time
	tried bool
	// running is closed when the computation in flight is over; nil when
	// none is.
	running chan struct{}
	failing bool
}

// newAIMetricsStore wraps src. log may be nil.
func newAIMetricsStore(src aiMetricsSource, log *slog.Logger) *aiMetricsStore {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &aiMetricsStore{src: src, log: log, now: time.Now}
}

// AIEndpointCounts24h returns the request and cache-hit counts of the last
// 24 hours as they are now, with the p95 latency as of at most aiLatencyTTL
// ago. Without a latency figure (none computed yet and the computation
// failed) the counts are returned without one.
func (m *aiMetricsStore) AIEndpointCounts24h(ctx context.Context) (map[string]db.AIEndpointCount, error) {
	counts, err := m.src.AIEndpointRequests24h(ctx)
	if err != nil {
		return nil, err
	}
	for sid, ms := range m.latency(ctx) {
		c := counts[sid]
		c.LatencyP95Ms = ms
		counts[sid] = c
	}
	return counts, nil
}

// AIEndpointMetrics24h is not kept: it reads one service.
func (m *aiMetricsStore) AIEndpointMetrics24h(ctx context.Context, serviceID string) (db.AIEndpointAgg, error) {
	return m.src.AIEndpointMetrics24h(ctx, serviceID)
}

// latency returns the p95 latency per service: the kept figure while it is
// fresh, else a new one (see aiMetricsStore). The result may be nil and must
// not be written to.
func (m *aiMetricsStore) latency(ctx context.Context) map[string]int {
	m.mu.Lock()
	if wait := m.running; wait != nil {
		prev := m.p95
		m.mu.Unlock()
		if prev != nil {
			return prev
		}
		// Nothing to serve yet: wait for the computation in flight, as long
		// as this caller is there.
		select {
		case <-wait:
		case <-ctx.Done():
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.p95
	}
	if m.tried && m.now().Sub(m.at) < aiLatencyTTL {
		defer m.mu.Unlock()
		return m.p95
	}
	done := make(chan struct{})
	m.running = done
	m.at, m.tried = m.now(), true
	m.mu.Unlock()
	return m.compute(ctx, done)
}

// latencyNoWait is latency for the data plane: it never waits for a
// computation. It returns the last figure (nil on a cold start or when none
// succeeded yet) and, when that is older than aiLatencyTTL and no computation
// runs, starts one in the background. The providers page keeps the blocking
// latency.
func (m *aiMetricsStore) latencyNoWait() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil && (!m.tried || m.now().Sub(m.at) >= aiLatencyTTL) {
		done := make(chan struct{})
		m.running = done
		m.at, m.tried = m.now(), true
		go m.compute(context.Background(), done)
	}
	return m.p95
}

// serviceLatencies is the aigateway.LatencySource: the p95 per service,
// without waiting.
func (m *aiMetricsStore) serviceLatencies(context.Context) map[string]int {
	return m.latencyNoWait()
}

// errLatencyPanicked is what a computation that panicked is logged with.
var errLatencyPanicked = errors.New("the read panicked")

// compute runs the one computation, stores its result and returns the figure
// to serve. done is closed when it is over, also when the source panics: a
// computation that never ends would leave every later caller waiting or
// without a fresh figure for good.
func (m *aiMetricsStore) compute(ctx context.Context, done chan struct{}) (figure map[string]int) {
	var p95 map[string]int
	err := errLatencyPanicked // replaced unless the source panics
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.running = nil
		close(done)
		switch {
		case err == nil:
			m.p95, m.failing = p95, false
		case !m.failing:
			m.failing = true
			m.log.Warn("ai provider latency: computation failed, the previous figure is kept",
				"err", err, "retry_after", aiLatencyTTL.String())
		}
		figure = m.p95
	}()
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), aiLatencyTimeout)
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		var cancelAtDeadline context.CancelFunc
		qctx, cancelAtDeadline = context.WithDeadline(qctx, deadline)
		defer cancelAtDeadline()
	}
	p95, err = m.src.AIEndpointLatencyP95(qctx)
	if err == nil && p95 == nil {
		p95 = map[string]int{}
	}
	return nil // set by the deferred function
}
