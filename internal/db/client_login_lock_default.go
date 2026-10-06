//go:build !postgres

package db

import (
	"context"
	"database/sql"
)

// lockClientLoginStart is a no-op in the default build: SQLite has one
// writer, so starts cannot overlap. The postgres build has the real one.
func lockClientLoginStart(context.Context, *sql.DB, *sql.Tx) error { return nil }
