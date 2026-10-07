//go:build postgres

package db

import (
	"context"
	"database/sql"
)

// auditChainLockKey is the advisory-lock key of an append to the audit
// chain. Any constant no other code locks will do; this one spells
// "burrowau". It must never change between versions: two versions running
// side by side during an upgrade have to wait for each other.
const auditChainLockKey int64 = 0x627572726f776175

// lockAuditChain makes the appends to the audit chain wait for each other on
// Postgres, so that reading the chain's last row and inserting the next one
// cannot interleave between two relay instances: the lock is taken in the
// append's transaction and released when it ends. A build with the postgres
// tag still runs SQLite by default; there the single writer does the same
// job and nothing is locked.
//
// The transaction is set to READ COMMITTED first, whatever the database's
// default is: the read of the last row must see what the appends this one
// waited for committed. Under REPEATABLE READ or SERIALIZABLE the snapshot
// would be the one of the lock statement, taken before the lock was granted.
// It must be the first statement of the transaction.
func lockAuditChain(ctx context.Context, d *sql.DB, tx *sql.Tx) error {
	if _, ok := d.Driver().(*rewriteDriver); !ok {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, auditChainLockKey)
	return err
}
