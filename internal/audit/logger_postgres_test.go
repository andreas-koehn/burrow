//go:build postgres

package audit

import (
	"context"
	"os"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// Two relay instances appending to one Postgres at once keep one chain: the
// previous hash is read and the row inserted under an advisory lock, and each
// id sorts after the row it chains to.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestVerifyAfterConcurrentAppends_TwoInstancesOnPostgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres audit chain check")
	}
	open := func() *db.DB {
		b, err := db.OpenPostgres(pgURL)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		x := db.Wrap(b.DB())
		t.Cleanup(func() { _ = x.Close() })
		return x
	}
	xa, xb := open(), open()
	la, lb := newTestLogger(t, xa), newTestLogger(t, xb)
	ctx := context.Background()

	// Other packages may have written to this database: verify from here on.
	if err := la.Append(ctx, Event{Action: ActionUserCreate, SubjectID: "marker"}); err != nil {
		t.Fatal(err)
	}
	from, _, ok, err := xa.LatestAuditRow(ctx, nil)
	if err != nil || !ok {
		t.Fatalf("latest row: ok=%v err=%v", ok, err)
	}
	appendConcurrently(t, []*Logger{la, lb}, 16, 60)
	n := 0
	if err := xa.IterAuditEventsAsc(ctx, from, "", func(db.AuditEvent) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n < 961 {
		t.Fatalf("%d rows from the marker on, want at least 961", n)
	}
	if ok, bad, err := lb.Verify(ctx, from, ""); err != nil || !ok {
		t.Fatalf("verify after two instances appended at once: ok=%v mismatched=%q err=%v", ok, bad, err)
	}
}
