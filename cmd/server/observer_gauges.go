// observer_gauges.go — the metrics that are read rather than counted: gauges
// a collector sets when /metrics is scraped, the duration of database
// statements, and what the background jobs report.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/metrics"
)

const (
	// gaugeQueryTimeout bounds the database reads of one scrape.
	gaugeQueryTimeout = 2 * time.Second
	// gaugeQueryEvery is how often, at most, a scrape reads the database:
	// a scraper that asks more often gets the values of the last read.
	gaugeQueryEvery = 10 * time.Second
)

// registerGauges wires what the recorder cannot count as it happens:
//
//   - burrow_db_backend, set once;
//   - burrow_goroutines, burrow_db_pool_active, burrow_db_pool_idle, read on
//     every scrape (no query);
//   - burrow_custom_domain_count, burrow_audit_chain_length and
//     burrow_audit_chain_last_hash, read from the database on a scrape, at
//     most every gaugeQueryEvery;
//   - burrow_db_query_duration_seconds, fed by the typed DB.
//
// stop ends the feeding of the query durations; call it before the database
// closes.
func registerGauges(rec *metrics.Recorder, database *sql.DB, driver string, log *slog.Logger) (stop func()) {
	if rec == nil || database == nil {
		return func() {}
	}
	rec.SetDBBackend(driver)
	rec.AddCollector(func() {
		rec.SetGoroutines(runtime.NumGoroutine())
		st := database.Stats()
		rec.SetDBPoolActive(int64(st.InUse))
		rec.SetDBPoolIdle(int64(st.Idle))
	})

	wrapped := db.Wrap(database)
	var mu sync.Mutex
	var last time.Time
	rec.AddCollector(func() {
		mu.Lock()
		due := time.Since(last) >= gaugeQueryEvery
		if due {
			last = time.Now()
		}
		mu.Unlock()
		if !due {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), gaugeQueryTimeout)
		defer cancel()
		if n, err := wrapped.CountCustomDomains(ctx); err != nil {
			log.Warn("metrics: custom domain count", "err", err)
		} else {
			rec.SetCustomDomainCount(n)
		}
		if n, err := wrapped.CountAuditEvents(ctx); err != nil {
			log.Warn("metrics: audit chain length", "err", err)
		} else {
			rec.SetAuditChainLength(n)
		}
		if _, hash, ok, err := wrapped.LatestAuditRow(ctx, nil); err != nil {
			log.Warn("metrics: audit chain head", "err", err)
		} else if ok {
			rec.SetAuditChainLastHash(hash)
		}
	})

	return db.ObserveQueries(database, func(op string, d time.Duration) {
		rec.ObserveDBQueryDuration(op, d.Seconds())
	})
}

// retentionCompacted is the compactor's OnRun hook: rows deleted per table,
// and when the run ended.
func retentionCompacted(rec *metrics.Recorder) func(counts map[string]int, at time.Time) {
	return func(counts map[string]int, at time.Time) {
		if rec == nil {
			return
		}
		for table, n := range counts {
			rec.AddRetentionCompactRowsDeleted(table, int64(n))
		}
		rec.SetRetentionCompactLastRunSeconds(at.Unix())
	}
}
