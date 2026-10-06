//go:build postgres

package db

import (
	"context"
	"database/sql"
)

// clientLoginStartLockKey is the advisory-lock key of a sign-in start. Any
// constant no other code locks will do; this one spells "burrowcl".
const clientLoginStartLockKey int64 = 0x627572726f77636c

// lockClientLoginStart makes the starts of sign-in requests wait for each
// other on Postgres, so that the caps counted by InsertClientLogin hold
// exactly: the lock is taken in the insert's transaction and released when it
// ends. A build with the postgres tag still runs SQLite by default; there the
// single writer does the same job and nothing is locked.
func lockClientLoginStart(ctx context.Context, d *sql.DB, tx *sql.Tx) error {
	if _, ok := d.Driver().(*rewriteDriver); !ok {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, clientLoginStartLockKey)
	return err
}
