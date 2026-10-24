package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

type recordingCloser struct {
	mu  sync.Mutex
	ids map[string]int
}

func (c *recordingCloser) CloseByToken(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids[id]++
	return 1
}

func (c *recordingCloser) closed(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ids[id] > 0
}

// A token whose expiry passes while a session is open gets the session closed
// by the next sweep tick; a token that never expires, or has not yet, does not.
func TestExpiredTokenSweeper_ClosesSessionsOfExpiredTokens(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d, err := db.Open(filepath.Join(t.TempDir(), "sweep.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	if err := st.SeedAdmin(ctx, "admin@x", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(ctx, "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(300 * time.Millisecond)
	later := time.Now().Add(time.Hour)
	for name, exp := range map[string]*time.Time{"soon": &soon, "later": &later, "never": nil} {
		if _, err := st.IssueClientTokenUntil(ctx, u.ID, name, exp, ""); err != nil {
			t.Fatal(err)
		}
	}
	toks, _ := st.ListClientTokens(ctx, u.ID)
	id := map[string]string{}
	for _, tk := range toks {
		id[tk.Name] = tk.ID
	}

	closer := &recordingCloser{ids: map[string]int{}}
	var wg sync.WaitGroup
	runExpiredTokenSweeper(ctx, &wg, st, closer, slog.Default(), 20*time.Millisecond)

	time.Sleep(100 * time.Millisecond)
	if closer.closed(id["soon"]) {
		t.Fatal("a session was closed before its token expired")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !closer.closed(id["soon"]) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !closer.closed(id["soon"]) {
		t.Fatal("the session of the expired token was not closed")
	}
	if closer.closed(id["later"]) || closer.closed(id["never"]) {
		t.Fatalf("closed sessions of live tokens: %v", closer.ids)
	}
	cancel()
	wg.Wait()
}
