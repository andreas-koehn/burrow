package audit

import (
	"sync"
	"testing"
	"time"
)

// An id never sorts before the one minted before it, whatever the clock
// says: a caller that read an older time (it was overtaken on its way to the
// lock, or the clock stepped back) still gets a later id.
func TestNewULID_NeverGoesBackwards(t *testing.T) {
	ulidMu.Lock()
	savedNow, savedMs, savedB := ulidNow, ulidLastMs, ulidLastB
	ulidMu.Unlock()
	t.Cleanup(func() {
		ulidMu.Lock()
		ulidNow, ulidLastMs, ulidLastB = savedNow, savedMs, savedB
		ulidMu.Unlock()
	})

	base := time.Now().Add(time.Hour) // ahead of every id minted so far
	// Forward, back by a little, back by a lot, the same again, forward.
	steps := []time.Duration{0, 5 * time.Millisecond, 3 * time.Millisecond, -time.Minute, -time.Minute, 4 * time.Millisecond, 20 * time.Millisecond, 19 * time.Millisecond}
	i := 0
	ulidMu.Lock()
	ulidNow = func() time.Time { return base.Add(steps[i]) }
	ulidMu.Unlock()

	var prev string
	for i = range steps {
		id, err := NewULID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 26 {
			t.Fatalf("id %q has %d characters", id, len(id))
		}
		if prev != "" && id <= prev {
			t.Fatalf("step %d (clock %+v): id %s does not sort after %s", i, steps[i], id, prev)
		}
		prev = id
	}
}

// Minted from many goroutines, ids are unique and each goroutine sees its own
// in rising order.
func TestNewULID_Concurrent(t *testing.T) {
	const workers, each = 16, 500
	out := make([][]string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := make([]string, 0, each)
			for n := 0; n < each; n++ {
				id, err := NewULID()
				if err != nil {
					t.Error(err)
					return
				}
				ids = append(ids, id)
			}
			out[w] = ids
		}()
	}
	wg.Wait()
	seen := make(map[string]bool, workers*each)
	for w, ids := range out {
		for n, id := range ids {
			if seen[id] {
				t.Fatalf("id %s minted twice", id)
			}
			seen[id] = true
			if n > 0 && id <= ids[n-1] {
				t.Fatalf("worker %d: id %s does not sort after %s", w, id, ids[n-1])
			}
		}
	}
}
