//go:build postgres

package db

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// pgForeignKeyViolation is SQLSTATE 23503.
	pgForeignKeyViolation = "23503"
	// pgTargetProviderFK is the name Postgres gives the foreign key that
	// migration 0023 declares inline on ai_model_targets.provider_slug.
	pgTargetProviderFK = "ai_model_targets_provider_slug_fkey"
)

func init() {
	pgTargetProviderFKViolation = func(err error) bool {
		var pe *pgconn.PgError
		return errors.As(err, &pe) && pe.Code == pgForeignKeyViolation && pe.ConstraintName == pgTargetProviderFK
	}
}
