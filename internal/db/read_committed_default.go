//go:build !postgres

package db

import (
	"context"
	"database/sql"
)

// ReadCommitted is a no-op in the default build: SQLite has one writer and a
// transaction never fails for a concurrent one. The postgres build has the
// real one.
func ReadCommitted(context.Context, *sql.DB, *sql.Tx) error { return nil }
