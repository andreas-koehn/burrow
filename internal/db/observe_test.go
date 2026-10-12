package db

import (
	"context"
	"path/filepath"
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
