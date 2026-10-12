package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/metrics"
)

// The gauges a scrape reads: the backend, the pool, the custom domains, the
// audit chain; and the statement durations of the typed DB.
func TestRegisterGauges(t *testing.T) {
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "gauges.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	wrapped := db.Wrap(sqldb)
	rec := metrics.New()
	stop := registerGauges(rec, sqldb, "sqlite", discardLog())

	text := expo(t, rec)
	wantLines(t, text,
		`burrow_db_backend{driver="sqlite"} 1`,
		`burrow_db_pool_active 0`,
		`burrow_custom_domain_count 0`,
		`burrow_audit_chain_length 0`,
	)
	for _, name := range []string{"burrow_goroutines ", "burrow_db_pool_idle ", `burrow_db_query_duration_seconds_count{op="select"} `} {
		if !strings.Contains(text, "\n"+name) {
			t.Errorf("no sample of %s", strings.TrimSpace(name))
		}
	}
	if strings.Contains(text, "burrow_audit_chain_last_hash{") {
		t.Error("a last hash for an empty audit log")
	}

	// An audit entry: seen by a scrape once the read interval has passed.
	logger := audit.NewLogger(wrapped, nil, nil)
	if err := logger.Append(context.Background(), audit.Event{Action: audit.ActionBackupRun}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(expo(t, rec), "burrow_audit_chain_length 1") {
		t.Fatal("the database was read again within the interval")
	}

	// After stop no statement is timed any more.
	stop()
	before := grepBurrow(expo(t, rec), "burrow_db_query_duration_seconds_count")
	if _, err := wrapped.ListWebhooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := grepBurrow(expo(t, rec), "burrow_db_query_duration_seconds_count"); after != before {
		t.Fatalf("statements timed after stop:\n%s\n%s", before, after)
	}
}

func TestRegisterGauges_NilIsSafe(t *testing.T) {
	registerGauges(nil, nil, "sqlite", discardLog())()
	retentionCompacted(nil)(map[string]int{"audit_events": 1}, time.Now())
}

func TestRetentionCompacted(t *testing.T) {
	rec := metrics.New()
	at := time.Unix(1_760_000_000, 0)
	retentionCompacted(rec)(map[string]int{"audit_events": 3, "usage_events": 0}, at)
	wantLines(t, expo(t, rec),
		`burrow_retention_compact_rows_deleted_total{table="audit_events"} 3`,
		`burrow_retention_compact_rows_deleted_total{table="usage_events"} 0`,
		`burrow_retention_compact_last_run_seconds 1.76e+09`,
	)
}
