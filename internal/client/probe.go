package client

import (
	"context"
	"net"
	"sync"
	"time"
)

// probeTimeout is how long one probe waits for the local service.
const probeTimeout = time.Second

// probeDial opens the connection of one probe. Tests replace it.
var probeDial = func(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// Probe finds out whether something listens on each local target: it connects
// to each one now and then every interval, and closes the connection at once
// without sending anything. report is called with the first result for a
// target and after that only when the result changes; with more than one
// target it is called from more than one goroutine.
//
// Probe returns when ctx ends, and report is not called after that. To stop
// probing one target earlier, as the client does once a visitor connection
// has reached it, run Probe for that target with a context of its own.
func Probe(ctx context.Context, targets []string, interval time.Duration, report func(addr string, ok bool)) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for _, addr := range targets {
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			probeTarget(ctx, addr, interval, report)
		}()
	}
	wg.Wait()
}

func probeTarget(ctx context.Context, addr string, interval time.Duration, report func(string, bool)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	known, last := false, false
	for {
		ok := probeOnce(ctx, addr)
		if ctx.Err() != nil {
			return // a dial that was cut short says nothing about the target
		}
		if !known || ok != last {
			known, last = true, ok
			report(addr, ok)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func probeOnce(ctx context.Context, addr string) bool {
	dctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := probeDial(dctx, addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
