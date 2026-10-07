//go:build postgres

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// PostgresBackend is the v0.5.0 entry point for PostgreSQL. It opens and
// migrates the database using the postgres migration ladder, then wraps it
// in a type that satisfies Backend.
//
// Only compiled when the "postgres" build tag is set.
type PostgresBackend struct{ db *sql.DB }

// OpenPostgres opens a connection to the Postgres database at url (e.g.
// "postgres://user:pass@host/dbname"), pings it, sets a sensible connection
// limit, and runs the embedded postgres migration ladder.
//
// v0.5.1 P1.1: opens via the "pgx-rewrite" driver, which is a thin wrapper
// over pgx/stdlib that rewrites SQLite-style "?" placeholders to "$N"
// Postgres positional parameters at PrepareContext / QueryContext /
// ExecContext time. This lets the 44 v0.5.0 production SQL files
// (~180 placeholder sites) keep their SQLite-style query strings while
// running unmodified under Postgres. The corpus + state-machine details
// live in postgres_rewriter.go / postgres_rewriter_test.go.
func OpenPostgres(url string) (*PostgresBackend, error) {
	return openPostgres(url, pgMigrationLockWait, slog.Default())
}

// openPostgres is OpenPostgres with the time it may wait for the migration
// lock and the logger that says when it waits.
func openPostgres(url string, lockWait time.Duration, log *slog.Logger) (*PostgresBackend, error) {
	d, err := sql.Open("pgx-rewrite", url)
	if err != nil {
		return nil, fmt.Errorf("OpenPostgres open: %w", err)
	}
	if err := d.Ping(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("OpenPostgres ping: %w", err)
	}
	d.SetMaxOpenConns(10)
	if err := migratePostgresLocked(d, lockWait, log); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("OpenPostgres migrate: %w", err)
	}
	return &PostgresBackend{db: d}, nil
}

// pgMigrationLockKey is the advisory-lock key every Burrow process takes
// while it migrates a Postgres database. The value is arbitrary and fixed
// ("burrow" read as a number); it must never change between versions.
const pgMigrationLockKey int64 = 0x627572726f77

const (
	// pgMigrationLockWait is how long a start waits for another instance to
	// finish migrating before it gives up.
	pgMigrationLockWait = 60 * time.Second
	// pgMigrationLockPoll is how often it asks again.
	pgMigrationLockPoll = 200 * time.Millisecond
)

// migratePostgresLocked runs the migration ladder under a session-level
// advisory lock, so that two processes starting against the same database
// (two relays, or two test binaries) migrate one after the other: the second
// waits, then finds everything applied. The lock lives on one dedicated
// connection and is released explicitly; closing that connection would
// release it too, so a crash cannot leave it held.
//
// The lock is asked for with pg_try_advisory_lock until wait has passed, so
// an instance that holds it and hangs does not make every later start hang
// with it: the start fails with an error that names the lock. The first time
// it has to wait it says so in the log.
func migratePostgresLocked(d *sql.DB, wait time.Duration, log *slog.Logger) error {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration lock connection: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(wait)
	for waited := false; ; {
		var got bool
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := conn.QueryRowContext(qctx, `SELECT pg_try_advisory_lock($1)`, pgMigrationLockKey).Scan(&got)
		cancel()
		if err != nil {
			return fmt.Errorf("take migration lock (advisory lock %d): %w", pgMigrationLockKey, err)
		}
		if got {
			break
		}
		if !waited {
			waited = true
			log.Info("postgres: waiting for the migration lock; another Burrow instance is migrating this database",
				slog.Int64("advisory_lock", pgMigrationLockKey), slog.Duration("wait_at_most", wait))
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("the migration lock (advisory lock %d) is still held by another session after %s: "+
				"another Burrow instance is migrating this database or hangs; see pg_locks (locktype 'advisory') for its pid",
				pgMigrationLockKey, wait)
		}
		time.Sleep(min(left, pgMigrationLockPoll))
	}
	// A failed unlock is not fatal for this process, but the connection must
	// not go back to the pool still holding the lock: discard it.
	defer func() {
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, pgMigrationLockKey); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return MigrateForDriver(d, "postgres")
}

// DB implements Backend.
func (b *PostgresBackend) DB() *sql.DB { return b.db }

// Driver implements Backend.
func (b *PostgresBackend) Driver() string { return "postgres" }

// Now implements Backend. Postgres uses now() for the current timestamp.
func (b *PostgresBackend) Now() string { return "now()" }

// Placeholder implements Backend. Postgres uses $N positional parameters.
func (b *PostgresBackend) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }

// Close closes the underlying database connection.
func (b *PostgresBackend) Close() error { return b.db.Close() }

// compile-time assertion: *PostgresBackend satisfies Backend.
var _ Backend = (*PostgresBackend)(nil)

// ---------------------------------------------------------------------------
// pgx-rewrite driver — wraps pgx/stdlib and rewrites ? → $N at query time.
// ---------------------------------------------------------------------------

func init() {
	sql.Register("pgx-rewrite", &rewriteDriver{base: &stdlib.Driver{}})
}

// rewriteDriver delegates Open to the real pgx/stdlib driver and wraps the
// returned driver.Conn so that PrepareContext / QueryContext / ExecContext
// run the query string through rewriteQuestionMarks first.
type rewriteDriver struct {
	base driver.Driver
}

func (d *rewriteDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &rewriteConn{base: c}, nil
}

// rewriteConn is the wrapper conn. We forward every interface pgx/stdlib's
// Conn implements and rewrite the query string for the four methods that
// take one: Prepare, PrepareContext, QueryContext, ExecContext. Plain
// (deprecated) Query/Exec on driver.Conn are intentionally NOT implemented
// because pgx/stdlib's Conn does not implement them either — database/sql
// dispatches via the *Context variants.
type rewriteConn struct {
	base driver.Conn
}

// Prepare satisfies driver.Conn. database/sql may call Prepare directly in
// older paths; we rewrite there as well for safety.
func (c *rewriteConn) Prepare(query string) (driver.Stmt, error) {
	return c.base.Prepare(rewriteQuestionMarks(query))
}

func (c *rewriteConn) Close() error { return c.base.Close() }

func (c *rewriteConn) Begin() (driver.Tx, error) { return c.base.Begin() }

// PrepareContext is the modern path — database/sql calls this whenever a
// caller uses ExecContext/QueryContext with a *sql.DB.
func (c *rewriteConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.base.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, rewriteQuestionMarks(query))
	}
	return c.base.Prepare(rewriteQuestionMarks(query))
}

// QueryContext implements driver.QueryerContext. database/sql skips the
// Prepare/Exec round-trip when the conn implements this directly, which
// pgx/stdlib does.
func (c *rewriteConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if qc, ok := c.base.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, rewriteQuestionMarks(query), args)
	}
	// Fallback: prepare + query the slow path.
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	if qc, ok := stmt.(driver.StmtQueryContext); ok {
		return qc.QueryContext(ctx, args)
	}
	return nil, driver.ErrSkip
}

// ExecContext implements driver.ExecerContext.
func (c *rewriteConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if ec, ok := c.base.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, rewriteQuestionMarks(query), args)
	}
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	if ec, ok := stmt.(driver.StmtExecContext); ok {
		return ec.ExecContext(ctx, args)
	}
	return nil, driver.ErrSkip
}

// BeginTx forwards to pgx/stdlib's transaction support.
func (c *rewriteConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.base.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	return c.base.Begin()
}

// Ping forwards to pgx/stdlib's Pinger implementation (so *sql.DB.Ping works).
func (c *rewriteConn) Ping(ctx context.Context) error {
	if p, ok := c.base.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// ResetSession forwards to pgx/stdlib for connection pooling correctness.
func (c *rewriteConn) ResetSession(ctx context.Context) error {
	if rs, ok := c.base.(driver.SessionResetter); ok {
		return rs.ResetSession(ctx)
	}
	return nil
}

// IsValid forwards to pgx/stdlib so the pool can drop dead connections.
func (c *rewriteConn) IsValid() bool {
	if v, ok := c.base.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// compile-time assertions for the optional interfaces we surface.
var (
	_ driver.Conn               = (*rewriteConn)(nil)
	_ driver.ConnPrepareContext = (*rewriteConn)(nil)
	_ driver.QueryerContext     = (*rewriteConn)(nil)
	_ driver.ExecerContext      = (*rewriteConn)(nil)
	_ driver.ConnBeginTx        = (*rewriteConn)(nil)
	_ driver.Pinger             = (*rewriteConn)(nil)
	_ driver.SessionResetter    = (*rewriteConn)(nil)
	_ driver.Validator          = (*rewriteConn)(nil)
)

// rewriteQuestionMarks scans query and replaces every "?" that occurs OUTSIDE
// any of the following regions with $1, $2, … in left-to-right order:
//
//   - single-quoted string literals '…' (with ” as the embedded quote escape;
//     backslash escapes are NOT recognised — Postgres standard_conforming_strings
//     is on by default, so backslash is literal)
//   - double-quoted identifiers "…" (with "" as the embedded quote escape)
//   - dollar-quoted strings $$…$$ and $tag$…$tag$ where tag is a valid Postgres
//     identifier (letters / digits / underscore, starting with non-digit). The
//     opening tag determines the matching closing tag.
//   - line comments -- … to the next newline (or EOF)
//   - block comments /* … */; NOT nested (Postgres allows nesting, but no v0.5.0
//     production SQL string nests block comments — keeping the lexer simple)
//
// "?" inside any of those regions is left untouched. The function is pure and
// allocates one strings.Builder. The corpus test in postgres_rewriter_test.go
// pins the contract.
func rewriteQuestionMarks(query string) string {
	// Fast path: no '?' present → return as-is, no allocation.
	hasQ := false
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			hasQ = true
			break
		}
	}
	if !hasQ {
		return query
	}

	out := make([]byte, 0, len(query)+8)
	i := 0
	n := len(query)
	placeholderIdx := 0

	for i < n {
		ch := query[i]
		switch {
		case ch == '\'':
			// Single-quoted string literal. Consume until the matching unescaped
			// '. '' inside is a literal apostrophe (SQL standard) — we treat
			// it as part of the string and keep scanning.
			out = append(out, ch)
			i++
			for i < n {
				if query[i] == '\'' {
					// '' = embedded quote → stay in the string.
					if i+1 < n && query[i+1] == '\'' {
						out = append(out, '\'', '\'')
						i += 2
						continue
					}
					out = append(out, '\'')
					i++
					break
				}
				out = append(out, query[i])
				i++
			}
		case ch == '"':
			// Double-quoted identifier. Same escape rule ("").
			out = append(out, ch)
			i++
			for i < n {
				if query[i] == '"' {
					if i+1 < n && query[i+1] == '"' {
						out = append(out, '"', '"')
						i += 2
						continue
					}
					out = append(out, '"')
					i++
					break
				}
				out = append(out, query[i])
				i++
			}
		case ch == '-' && i+1 < n && query[i+1] == '-':
			// Line comment to newline (or EOF).
			out = append(out, '-', '-')
			i += 2
			for i < n && query[i] != '\n' {
				out = append(out, query[i])
				i++
			}
			// Newline (if any) is consumed in the next loop iteration.
		case ch == '/' && i+1 < n && query[i+1] == '*':
			// Block comment to matching */ (non-nested).
			out = append(out, '/', '*')
			i += 2
			for i < n {
				if query[i] == '*' && i+1 < n && query[i+1] == '/' {
					out = append(out, '*', '/')
					i += 2
					break
				}
				out = append(out, query[i])
				i++
			}
		case ch == '$':
			// Possible dollar-quoted string. Look ahead for [tag]$.
			tagEnd := i + 1
			for tagEnd < n && isDollarTagByte(query[tagEnd], tagEnd == i+1) {
				tagEnd++
			}
			if tagEnd < n && query[tagEnd] == '$' {
				// Found opener $tag$ (tag may be empty for $$…$$).
				tag := query[i : tagEnd+1] // includes both $ markers
				out = append(out, tag...)
				i = tagEnd + 1
				// Consume until matching closer.
				for i < n {
					if query[i] == '$' && i+len(tag) <= n && query[i:i+len(tag)] == tag {
						out = append(out, tag...)
						i += len(tag)
						break
					}
					out = append(out, query[i])
					i++
				}
			} else {
				// Bare $ that is NOT an opener (e.g. literal $5 inside an
				// already-handled string is impossible here; this is a $ in
				// open SQL). Emit and advance.
				out = append(out, '$')
				i++
			}
		case ch == '?':
			placeholderIdx++
			out = append(out, '$')
			out = strconv.AppendInt(out, int64(placeholderIdx), 10)
			i++
		default:
			out = append(out, ch)
			i++
		}
	}
	return string(out)
}

// isDollarTagByte reports whether b is valid inside a Postgres dollar-quote
// tag. The first byte must be a letter or underscore; subsequent bytes may
// also be digits. Empty tag (i.e. $$…$$) is allowed and handled by the caller
// (it sees query[i+1] == '$' immediately).
func isDollarTagByte(b byte, first bool) bool {
	if b == '_' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') {
		return true
	}
	if !first && b >= '0' && b <= '9' {
		return true
	}
	return false
}
