package mcpgw

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

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
