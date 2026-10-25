package customdomain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type countingDB struct {
	mu    sync.Mutex
	calls map[string]int
	rows  map[string]DomainRow
	fail  bool
}

func (c *countingDB) LookupCustomDomainByHostname(_ context.Context, h string) (DomainRow, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[h]++
	if c.fail {
		return DomainRow{}, false, errors.New("db down")
	}
	r, ok := c.rows[h]
	return r, ok, nil
}

func (c *countingDB) n(h string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[h]
}

// Host dispatch looks up every Host header that is no name of the relay's,
// so a miss is remembered for a few seconds (bounded), or arbitrary Host
// headers would each cost a database query. A custom-domain write clears
// the remembered misses; a failed lookup is not remembered.
func TestLookupBySNI_NegativeCache(t *testing.T) {
	db := &countingDB{calls: map[string]int{}, rows: map[string]DomainRow{}}
	s := New(db)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, ok, err := s.LookupBySNI(ctx, "Nobody.example.com"); ok || err != nil {
			t.Fatalf("miss: %v %v", ok, err)
		}
	}
	if n := db.n("nobody.example.com"); n != 1 {
		t.Fatalf("three misses asked the database %d times, want 1", n)
	}

	// Bound meanwhile: the write's invalidation clears the misses.
	db.mu.Lock()
	db.rows["nobody.example.com"] = DomainRow{Hostname: "nobody.example.com", ServiceID: "s1"}
	db.mu.Unlock()
	s.InvalidateHostname("other.example.com") // any write clears them (a wildcard covers other names)
	if c, ok, _ := s.LookupBySNI(ctx, "nobody.example.com"); !ok || c.ServiceID != "s1" {
		t.Fatalf("after a write the bound name is still a miss: %v %+v", ok, c)
	}

	// A miss read before a write is not kept.
	gen := s.gen
	s.InvalidateHostname("racing.example.com")
	s.rememberMiss("racing.example.com", gen)
	s.mu.RLock()
	_, kept := s.neg["racing.example.com"]
	s.mu.RUnlock()
	if kept {
		t.Fatal("a miss read before a write was remembered")
	}

	// Expiry.
	if _, ok, _ := s.LookupBySNI(ctx, "late.example.com"); ok {
		t.Fatal("late is not bound")
	}
	now = now.Add(negativeTTL + time.Second)
	_, _, _ = s.LookupBySNI(ctx, "late.example.com")
	if n := db.n("late.example.com"); n != 2 {
		t.Fatalf("an expired miss was not looked up again (%d calls)", n)
	}

	// Errors are not remembered.
	db.mu.Lock()
	db.fail = true
	db.mu.Unlock()
	for i := 0; i < 2; i++ {
		if _, _, err := s.LookupBySNI(ctx, "err.example.com"); err == nil {
			t.Fatal("want the lookup error")
		}
	}
	if n := db.n("err.example.com"); n != 2 {
		t.Fatalf("a failed lookup was remembered (%d calls)", n)
	}
	db.mu.Lock()
	db.fail = false
	db.mu.Unlock()

	// Bounded.
	for i := 0; i < negativeMax+500; i++ {
		_, _, _ = s.LookupBySNI(ctx, fmt.Sprintf("h%d.example.com", i))
	}
	s.mu.RLock()
	size := len(s.neg)
	s.mu.RUnlock()
	if size > negativeMax {
		t.Fatalf("%d remembered misses, want at most %d", size, negativeMax)
	}
}
