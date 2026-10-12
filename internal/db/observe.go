package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// QueryObserver is told how long one statement took. For ExecContext that is
// the whole statement, including the wait for a pool connection. For QueryContext and QueryRowContext it is the time
// until the call returns: it includes the wait for a pool connection and the
// execution up to the first row, and excludes iterating the rows and Scan. op
// is the statement's verb: "select", "insert", "update", "delete", or "other" for everything
// else (a pragma, a WITH, DDL). It is called on the goroutine of the query
// and must return at once.
type QueryObserver func(op string, d time.Duration)

// queryObservers maps a *sql.DB to its observer. Wrap is called wherever a
// typed DB is wanted, many times for one database, so the observer belongs to
// the database and not to one wrapper of it.
var (
	queryObservers  sync.Map // *sql.DB → *observerEntry
	queryObserversN atomic.Int32
)

// observerEntry gives each registration an identity, so a stop removes only
// the registration it belongs to.
type observerEntry struct{ fn QueryObserver }

// ObserveQueries registers o for the statements the typed DB (every Wrap of
// d) runs outside a transaction, and for those run through ObservedExec.
// stop removes it again; call it before d is closed. A second observer for
// the same database replaces the first.
func ObserveQueries(d *sql.DB, o QueryObserver) (stop func()) {
	if d == nil || o == nil {
		return func() {}
	}
	e := &observerEntry{fn: o}
	if _, had := queryObservers.Swap(d, e); !had {
		queryObserversN.Add(1)
	}
	return func() {
		if queryObservers.CompareAndDelete(d, e) {
			queryObserversN.Add(-1)
		}
	}
}

// timedDB is the *sql.DB of a typed DB with the three statement methods the
// package uses reporting to the database's observer. Everything else (BeginTx,
// Close, Stats, …) is the embedded *sql.DB's own. Statements inside a
// transaction go through the *sql.Tx and are not timed.
type timedDB struct{ *sql.DB }

// observer returns the database's observer, nil when it has none. Without
// any observer in the process this is one atomic load.
func (t *timedDB) observer() QueryObserver {
	if queryObserversN.Load() == 0 {
		return nil
	}
	if o, ok := queryObservers.Load(t.DB); ok {
		return o.(*observerEntry).fn
	}
	return nil
}

// ExecContext is (*sql.DB).ExecContext, timed.
func (t *timedDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	o := t.observer()
	if o == nil {
		return t.DB.ExecContext(ctx, query, args...)
	}
	start := time.Now()
	res, err := t.DB.ExecContext(ctx, query, args...)
	report(o, query, time.Since(start))
	return res, err
}

// QueryContext is (*sql.DB).QueryContext, timed until the call returns (pool
// wait and execution up to the rows; not their iteration).
func (t *timedDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	o := t.observer()
	if o == nil {
		return t.DB.QueryContext(ctx, query, args...)
	}
	start := time.Now()
	rows, err := t.DB.QueryContext(ctx, query, args...)
	report(o, query, time.Since(start))
	return rows, err
}

// QueryRowContext is (*sql.DB).QueryRowContext, timed until the call returns;
// Scan is not included.
func (t *timedDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	o := t.observer()
	if o == nil {
		return t.DB.QueryRowContext(ctx, query, args...)
	}
	start := time.Now()
	row := t.DB.QueryRowContext(ctx, query, args...)
	report(o, query, time.Since(start))
	return row
}

// observerPanicLog and observerPanicOnce let the first panic of an observer
// be logged once per process (variables for the test).
var (
	observerPanicOnce sync.Once
	observerPanicLog  = defaultObserverPanicLog
)

func defaultObserverPanicLog(msg string, args ...any) { slog.Error(msg, args...) }

// report tells the observer; a panicking observer must not fail the query.
// The first panic is logged, once per process, with the panic value and
// nothing of the statement.
func report(o QueryObserver, query string, d time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			observerPanicOnce.Do(func() {
				observerPanicLog("db: query observer panicked; further panics are not logged", "panic", fmt.Sprint(r))
			})
		}
	}()
	o(queryOp(query), d)
}

// queryOp is the verb of a statement, lower case, from a fixed set. It reads
// the first word and allocates nothing.
func queryOp(query string) string {
	i := 0
	for i < len(query) && (query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r') {
		i++
	}
	if len(query)-i < 6 {
		return "other"
	}
	var w [6]byte
	for j := 0; j < 6; j++ {
		c := query[i+j]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		w[j] = c
	}
	switch w {
	case [6]byte{'s', 'e', 'l', 'e', 'c', 't'}:
		return "select"
	case [6]byte{'i', 'n', 's', 'e', 'r', 't'}:
		return "insert"
	case [6]byte{'u', 'p', 'd', 'a', 't', 'e'}:
		return "update"
	case [6]byte{'d', 'e', 'l', 'e', 't', 'e'}:
		return "delete"
	}
	return "other"
}

// ObservedExec runs a statement on the database and reports its duration to
// the database's observer, like the typed methods do. For the packages that
// write their own SQL on the request path (the usage sink, the connection
// log).
func (x *DB) ObservedExec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return x.sqlDB.ExecContext(ctx, query, args...)
}

// CountCustomDomains returns the number of custom domains bound to services.
func (x *DB) CountCustomDomains(ctx context.Context) (int64, error) {
	var n int64
	err := x.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM service_custom_domains`).Scan(&n)
	return n, err
}

// CountAuditEvents returns the number of rows in the audit log.
func (x *DB) CountAuditEvents(ctx context.Context) (int64, error) {
	var n int64
	err := x.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&n)
	return n, err
}
