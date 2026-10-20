package mcpgw

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/ankoehn/burrow/internal/mcptest"
	"github.com/ankoehn/burrow/internal/store"
)

// The limits of a source are looked up and taken as one step: a Forget that
// runs in between must not hand two callers separate limits for one source
// (T06 review).
func TestAdmit_ForgetDuringLookupKeepsTheLimit(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	one := 1
	g := newGWFixture(t, up, func(in *store.MCPSourceInput) { in.MaxConcurrent = &one })
	src := g.source(t)
	req := CallRequest{Source: src}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				g.gw.Forget("gh")
			}
		}
	}()
	var held, bad atomic.Int32
	var workers sync.WaitGroup
	for w := 0; w < 8; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 3000; i++ {
				release, err := g.gw.admit(context.Background(), req)
				if err != nil {
					continue
				}
				if held.Add(1) > 1 {
					bad.Add(1)
				}
				held.Add(-1)
				release()
			}
		}()
	}
	workers.Wait()
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d times two calls held the one place of the source at once", bad.Load())
	}
}

// Final review: an OAuth grant has a rate limit of its own (GrantRPM calls
// per minute, a bucket refilled continuously; 0 = none), since no api_key
// limit can name it. Each grant has its own bucket; a gateway key's call is
// not charged to it.
func TestAdmit_GrantRateLimit(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	g := newGWFixture(t, up)
	src := g.source(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	g.gw.now = func() time.Time { return now }
	g.gw.opts.GrantRPM = 2
	call := func(c Caller) error {
		release, err := g.gw.admit(context.Background(), CallRequest{Source: src, Caller: c})
		if err == nil {
			release()
		}
		return err
	}
	one, two := Caller{GrantID: "g1"}, Caller{GrantID: "g2"}
	for i := 0; i < 2; i++ {
		if err := call(one); err != nil {
			t.Fatalf("g1 call %d: %v", i, err)
		}
	}
	err := call(one)
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != CodeLimitExceeded || !strings.Contains(string(je.Data), `"limit":"grant_rpm"`) {
		t.Fatalf("g1 third call: %v", err)
	}
	if err := call(two); err != nil {
		t.Fatalf("another grant: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := call(Caller{GatewayKeyID: "k1"}); err != nil {
			t.Fatalf("a key's call %d: %v", i, err)
		}
	}
	now = now.Add(30 * time.Second) // one call's worth at 2 per minute
	if err := call(one); err != nil {
		t.Fatalf("g1 after 30 s: %v", err)
	}
	g.gw.opts.GrantRPM = 0
	for i := 0; i < 5; i++ {
		if err := call(one); err != nil {
			t.Fatalf("no grant limit, call %d: %v", i, err)
		}
	}
}
