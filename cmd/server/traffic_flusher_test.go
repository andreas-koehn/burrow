package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/server"
)

// liveTunnels is a control server with one session whose tunnels the test
// sets.
type liveTunnels struct {
	mu      sync.Mutex
	tunnels []server.TunnelView
}

func (l *liveTunnels) set(tunnels ...server.TunnelView) {
	l.mu.Lock()
	l.tunnels = tunnels
	l.mu.Unlock()
}

func (l *liveTunnels) SnapshotSessions() []server.SessionSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return []server.SessionSnapshot{{SessionID: "s1", Tunnels: append([]server.TunnelView(nil), l.tunnels...)}}
}

// tunnelTotalsStore keeps the totals like the database does: a write on a
// context that is over fails, as database/sql refuses it.
type tunnelTotalsStore struct {
	mu        sync.Mutex
	in, out   map[string]int64
	deadlines []time.Duration // how far away the deadline of each call was
	calls     chan struct{}
}

func newTunnelTotalsStore() *tunnelTotalsStore {
	return &tunnelTotalsStore{in: map[string]int64{}, out: map[string]int64{}, calls: make(chan struct{}, 64)}
}

func (s *tunnelTotalsStore) FlushTunnelTotals(ctx context.Context, id string, addIn, addOut int64) error {
	defer func() { s.calls <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in[id] += addIn
	s.out[id] += addOut
	if dl, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, time.Until(dl))
	} else {
		s.deadlines = append(s.deadlines, -1)
	}
	return nil
}

func (s *tunnelTotalsStore) totals(id string) (in, out int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.in[id], s.out[id]
}

// The last flush happens after the relay's context is cancelled. It must
// still be written: what the tunnels carried since the last tick would
// otherwise be lost at every shutdown.
func TestRunTrafficFlusher_FinalFlushAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	live, st := &liveTunnels{}, newTunnelTotalsStore()
	logs := &syncBuffer{}
	live.set(server.TunnelView{ID: "t1", BytesIn: 100, BytesOut: 200})
	// An interval no test reaches: only the flush at shutdown runs.
	runTrafficFlusher(ctx, &wg, live, st, slog.New(slog.NewTextHandler(logs, nil)), 24*time.Hour)

	live.set(server.TunnelView{ID: "t1", BytesIn: 150, BytesOut: 260}, server.TunnelView{ID: "t2", BytesIn: 7, BytesOut: 9})
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the flusher did not stop after its context was cancelled")
	}

	if in, out := st.totals("t1"); in != 150 || out != 260 {
		t.Fatalf("t1 totals after shutdown: %d in, %d out, want 150 and 260", in, out)
	}
	if in, out := st.totals("t2"); in != 7 || out != 9 {
		t.Fatalf("t2 totals after shutdown: %d in, %d out, want 7 and 9", in, out)
	}
	if out := logs.String(); strings.Contains(out, "traffic flush") {
		t.Fatalf("the final flush logged a failure:\n%s", out)
	}
	// Detached from the cancelled context, and bounded.
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, left := range st.deadlines {
		if left <= 0 || left > trafficFinalFlushTimeout {
			t.Fatalf("the final flush ran with %s to its deadline, want within (0, %s]", left, trafficFinalFlushTimeout)
		}
	}
}

// Between shutdowns the flusher writes what changed since its last write.
func TestRunTrafficFlusher_WritesDeltas(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	live, st := &liveTunnels{}, newTunnelTotalsStore()
	live.set(server.TunnelView{ID: "t1", BytesIn: 10, BytesOut: 20})
	runTrafficFlusher(ctx, &wg, live, st, slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Millisecond)

	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting until %s", what)
			}
			select {
			case <-st.calls:
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	wait("the first totals are written", func() bool { in, out := st.totals("t1"); return in == 10 && out == 20 })
	live.set(server.TunnelView{ID: "t1", BytesIn: 25, BytesOut: 20})
	wait("the difference is added", func() bool { in, out := st.totals("t1"); return in == 25 && out == 20 })
	// A tunnel that came back counts from zero again: its bytes are added whole.
	live.set(server.TunnelView{ID: "t1", BytesIn: 5, BytesOut: 1})
	wait("a counter that restarted is added whole", func() bool { in, out := st.totals("t1"); return in == 30 && out == 21 })
}

// syncBuffer is a log sink for a goroutine and the test at once.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
