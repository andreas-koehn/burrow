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
//
// The transaction is set to READ COMMITTED first, whatever the database's
// default is. The counts must see the rows of the starts this one waited
// for; under REPEATABLE READ or SERIALIZABLE the snapshot would be the one of
// the lock statement, taken before the lock was granted, and the caps could
// be passed. It must be the first statement of the transaction.
func lockClientLoginStart(ctx context.Context, d *sql.DB, tx *sql.Tx) error {
	if _, ok := d.Driver().(*rewriteDriver); !ok {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, clientLoginStartLockKey)
	return err
}
