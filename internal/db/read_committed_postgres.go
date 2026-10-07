//go:build postgres

package db

import (
	"context"
	"database/sql"
)

// ReadCommitted sets tx to READ COMMITTED on Postgres, whatever the
// database's or the session's default is. It is for a transaction that is
// written so two of them may meet (INSERT … ON CONFLICT DO NOTHING, a DELETE
// that may find its row gone): under REPEATABLE READ or SERIALIZABLE Postgres
// ends the later one with SQLSTATE 40001 instead of letting it see what the
// earlier one committed. It must be the first statement of the transaction.
// A build with the postgres tag still runs SQLite by default; nothing is
// done there.
func ReadCommitted(ctx context.Context, d *sql.DB, tx *sql.Tx) error {
	if _, ok := d.Driver().(*rewriteDriver); !ok {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`)
	return err
}
