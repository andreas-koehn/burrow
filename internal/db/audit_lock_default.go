//go:build !postgres

package db

import (
	"context"
	"database/sql"
)

// lockAuditChain is a no-op in the default build: SQLite has one writer, so
// two appends cannot overlap. The postgres build has the real one.
func lockAuditChain(context.Context, *sql.DB, *sql.Tx) error { return nil }
