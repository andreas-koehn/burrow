package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReaper counts DeleteExpiredSessions and PruneOAuth calls for testing.
type fakeReaper struct {
	calls  atomic.Int64
	prunes atomic.Int64
}

func (f *fakeReaper) PruneOAuth(context.Context, time.Time) error {
	f.prunes.Add(1)
	return nil
}

func (f *fakeReaper) DeleteExpiredSessions(_ context.Context) (int64, error) {
	f.calls.Add(1)
	return 0, nil
}

// TestRunSessionReaper_StopsOnCancel verifies that the reaper goroutine:
//   - calls DeleteExpiredSessions at least once (startup run)
//   - exits promptly when ctx is cancelled
//   - the WaitGroup unblocks after cancellation (no goroutine leak)
func TestRunSessionReaper_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	r := &fakeReaper{}
	logger := slog.Default()

	// Use a large interval so only the startup call fires before we cancel.
	runSessionReaper(ctx, &wg, r, logger, 24*time.Hour)

	// Give the startup purge a moment to run.
	deadline := time.Now().Add(2 * time.Second)
	for r.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.calls.Load() == 0 {
		t.Fatal("DeleteExpiredSessions was never called at startup")
	}

	// Cancel and confirm the WaitGroup unblocks within 1s (goroutine stops).
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		// ok
	case <-time.After(time.Second):
		t.Fatal("reaper goroutine did not stop within 1s after ctx cancel")
	}
}

// TestRunSessionReaper_TickerFires verifies that the reaper calls
// DeleteExpiredSessions again on each ticker tick (not just at startup).
func TestRunSessionReaper_TickerFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	r := &fakeReaper{}
	logger := slog.Default()

	// Short interval: 50ms so we get a few ticks quickly.
	runSessionReaper(ctx, &wg, r, logger, 50*time.Millisecond)

	// Wait for at least 3 calls (1 startup + 2 ticks).
	deadline := time.Now().Add(2 * time.Second)
	for r.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := r.calls.Load(); n < 3 {
		t.Fatalf("want >= 3 DeleteExpiredSessions calls, got %d", n)
	}
}

// Final review: the OAuth tables are pruned on the same tick (expired codes
// and tokens, grants revoked more than 30 days ago), at startup and on every
// tick after, and not only when someone approves a consent.
func TestRunSessionReaper_PrunesOAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	r := &fakeReaper{}
	runSessionReaper(ctx, &wg, r, slog.Default(), 50*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for r.prunes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := r.prunes.Load(); n < 2 {
		t.Fatalf("want >= 2 PruneOAuth calls (startup and a tick), got %d", n)
	}
	cancel()
	wg.Wait()
}
