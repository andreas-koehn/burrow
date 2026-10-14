package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQueryOp(t *testing.T) {
	for query, want := range map[string]string{
		"SELECT 1":                             "select",
		"  \n\tselect id FROM users":           "select",
		"INSERT INTO t(a) VALUES(?)":           "insert",
		"Update t SET a=?":                     "update",
		"DELETE FROM t":                        "delete",
		"WITH x AS (SELECT 1) SELECT * FROM x": "other",
		"PRAGMA foreign_keys=ON":               "other",
		"":                                     "other",
		"sel":                                  "other",
		"selects":                              "select", // the first six letters decide
	} {
		if got := queryOp(query); got != want {
			t.Errorf("queryOp(%q) = %q, want %q", query, got, want)
		}
	}
}

// Every Wrap of a database reports to the database's observer; another
// database is not observed; after stop nothing is reported.
func TestObserveQueries(t *testing.T) {
	open := func(name string) *DB {
		d, err := Open(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		if err := Migrate(d); err != nil {
			t.Fatal(err)
		}
		return Wrap(d)
	}
	x, other := open("a.db"), open("b.db")

	var mu sync.Mutex
	ops := map[string]int{}
	stop := ObserveQueries(x.DB(), func(op string, d time.Duration) {
		if d < 0 {
			t.Errorf("negative duration for %s", op)
		}
		mu.Lock()
		ops[op]++
		mu.Unlock()
	})
	ctx := context.Background()

	// A second wrapper of the same database, as cmd/server makes them: its
	// typed methods report too.
	again := Wrap(x.DB())
	if _, err := again.ListWebhooks(ctx); err != nil { // a select
		t.Fatal(err)
	}
	if _, err := x.ObservedExec(ctx, `INSERT INTO settings(key, value) VALUES(?,?)`, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.ObservedExec(ctx, `DELETE FROM settings WHERE key=?`, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ListWebhooks(ctx); err != nil { // another database: not observed
		t.Fatal(err)
	}
	mu.Lock()
	got := map[string]int{"select": ops["select"], "insert": ops["insert"], "delete": ops["delete"]}
	mu.Unlock()
	if got["select"] != 1 || got["insert"] != 1 || got["delete"] != 1 {
		t.Fatalf("observed %v, want one select, one insert, one delete", got)
	}

	stop()
	stop() // twice is harmless
	if _, err := x.ListWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ops["select"] != 1 {
		t.Fatalf("observed after stop: %v", ops)
	}
}

// A second observer for the same database replaces the first; the first one's
// stop must not remove the second.
func TestObserveQueries_StopRemovesOnlyItsOwn(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := Migrate(d); err != nil {
		t.Fatal(err)
	}
	x := Wrap(d)
	var first, second int
	stop1 := ObserveQueries(d, func(string, time.Duration) { first++ })
	stop2 := ObserveQueries(d, func(string, time.Duration) { second++ })
	stop1() // not the registered one: no effect
	if _, err := x.ListWebhooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first != 0 || second != 1 {
		t.Fatalf("first %d, second %d; want 0 and 1", first, second)
	}
	stop2()
	if _, err := x.ListWebhooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second != 1 || queryObserversN.Load() != 0 {
		t.Fatalf("second %d after its stop, %d observers left", second, queryObserversN.Load())
	}
}

// An observer that panics fails no query, and the first panic is logged
// (once per process) without the statement's text or arguments.
func TestObserveQueries_PanicIsContainedAndLoggedOnce(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := Migrate(d); err != nil {
		t.Fatal(err)
	}
	var logged []string
	var mu sync.Mutex
	observerPanicLog = func(msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, msg+" "+fmt.Sprint(args...))
	}
	observerPanicOnce = sync.Once{}
	t.Cleanup(func() { observerPanicLog = defaultObserverPanicLog; observerPanicOnce = sync.Once{} })

	stop := ObserveQueries(d, func(string, time.Duration) { panic("boom") })
	defer stop()
	x := Wrap(d)
	for i := 0; i < 3; i++ {
		if _, err := x.ObservedExec(context.Background(), `INSERT INTO settings(key, value) VALUES(?,?)`, "secret-key"+fmt.Sprint(i), "secret-value"); err != nil {
			t.Fatalf("a panicking observer failed the statement: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 {
		t.Fatalf("logged %d times, want once: %v", len(logged), logged)
	}
	if strings.Contains(logged[0], "secret") || strings.Contains(strings.ToLower(logged[0]), "insert") {
		t.Fatalf("the log line carries the statement: %s", logged[0])
	}
}
