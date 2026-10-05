package client

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/testutil"
)

// probeLog collects what Probe reports.
type probeLog struct {
	mu   sync.Mutex
	got  []bool
	addr []string
}

func (p *probeLog) report(addr string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, ok)
	p.addr = append(p.addr, addr)
}

func (p *probeLog) results() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.got...)
}

func (p *probeLog) is(want ...bool) bool {
	got := p.results()
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// startProbe runs Probe until the test ends and returns when it has stopped.
func startProbe(t *testing.T, log *probeLog, interval time.Duration, targets ...string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); Probe(ctx, targets, interval, log.report) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestProbe_ReportsChangesOnly(t *testing.T) {
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ls.Addr().String()
	log := &probeLog{}
	startProbe(t, log, 10*time.Millisecond, addr)

	if !waitTrue(func() bool { return log.is(true) }, 2*time.Second) {
		t.Fatalf("reports = %v, want [true]", log.results())
	}
	time.Sleep(60 * time.Millisecond) // several probes, no new report
	if !log.is(true) {
		t.Fatalf("reports = %v, want [true] only", log.results())
	}

	_ = ls.Close()
	if !waitTrue(func() bool { return log.is(true, false) }, 2*time.Second) {
		t.Fatalf("reports = %v, want [true false]", log.results())
	}

	var again net.Listener
	if !waitTrue(func() bool { again, err = net.Listen("tcp", addr); return err == nil }, 2*time.Second) {
		t.Fatalf("cannot listen on %s again: %v", addr, err)
	}
	defer again.Close()
	if !waitTrue(func() bool { return log.is(true, false, true) }, 2*time.Second) {
		t.Fatalf("reports = %v, want [true false true]", log.results())
	}
	time.Sleep(60 * time.Millisecond)
	if !log.is(true, false, true) {
		t.Fatalf("reports = %v, want exactly [true false true]", log.results())
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, a := range log.addr {
		if a != addr {
			t.Fatalf("reported address %q, want %q", a, addr)
		}
	}
}

func TestProbe_DeadTargetIsReportedOnce(t *testing.T) {
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ls.Addr().String()
	_ = ls.Close()
	log := &probeLog{}
	startProbe(t, log, 10*time.Millisecond, addr)
	if !waitTrue(func() bool { return log.is(false) }, 2*time.Second) {
		t.Fatalf("reports = %v, want [false]", log.results())
	}
	time.Sleep(80 * time.Millisecond)
	if !log.is(false) {
		t.Fatalf("reports = %v, want [false] only", log.results())
	}
}

func TestProbe_SameTargetTwiceIsProbedOnce(t *testing.T) {
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	log := &probeLog{}
	startProbe(t, log, 10*time.Millisecond, ls.Addr().String(), ls.Addr().String(), "")
	if !waitTrue(func() bool { return len(log.results()) > 0 }, 2*time.Second) {
		t.Fatal("no report")
	}
	time.Sleep(50 * time.Millisecond)
	if !log.is(true) {
		t.Fatalf("reports = %v, want [true]", log.results())
	}
}

func TestProbe_CancelStopsItAndLeavesNoGoroutine(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	log := &probeLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// a long interval: only the cancellation can end it in time
	go func() {
		defer close(done)
		Probe(ctx, []string{ls.Addr().String(), "127.0.0.1:1"}, time.Hour, log.report)
	}()
	if !waitTrue(func() bool { return len(log.results()) == 2 }, 3*time.Second) {
		t.Fatalf("reports = %v", log.results())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Probe did not return after the context was cancelled")
	}
	n := len(log.results())
	time.Sleep(30 * time.Millisecond)
	if len(log.results()) != n {
		t.Fatal("a report arrived after Probe had returned")
	}
}

// closeSpy tells when the connection was closed.
type closeSpy struct {
	net.Conn
	closed chan struct{}
}

func (c closeSpy) Close() error { close(c.closed); return c.Conn.Close() }

func TestProbe_DialHasATimeoutAndTheConnectionIsClosedAtOnce(t *testing.T) {
	prev := probeDial
	t.Cleanup(func() { probeDial = prev })

	closed := make(chan struct{})
	deadlines := make(chan time.Duration, 1)
	probeDial = func(ctx context.Context, addr string) (net.Conn, error) {
		if dl, ok := ctx.Deadline(); ok {
			deadlines <- time.Until(dl)
		} else {
			deadlines <- 0
		}
		a, b := net.Pipe()
		go func() {
			// A probe that wrote anything to the local service would show up here.
			buf := make([]byte, 1)
			if n, _ := b.Read(buf); n > 0 {
				t.Error("the probe sent data to the local service")
			}
		}()
		return closeSpy{a, closed}, nil
	}
	log := &probeLog{}
	startProbe(t, log, time.Hour, "127.0.0.1:3000")
	select {
	case d := <-deadlines:
		if d <= 0 || d > time.Second {
			t.Fatalf("dial deadline in %v, want at most 1 s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no dial")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe connection was not closed")
	}
	if !waitTrue(func() bool { return log.is(true) }, time.Second) {
		t.Fatalf("reports = %v", log.results())
	}
}
