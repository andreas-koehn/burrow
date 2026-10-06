package db

import (
	"errors"

	"modernc.org/sqlite"
)

// sqliteConstraintForeignKey is SQLITE_CONSTRAINT_FOREIGNKEY, the extended
// result code of a failed foreign key.
const sqliteConstraintForeignKey = 787

// pgTargetProviderFKViolation reports whether err is Postgres refusing a
// statement over the foreign key of ai_model_targets.provider_slug. The pgx
// driver is compiled in only with the "postgres" build tag, whose file
// replaces this default.
var pgTargetProviderFKViolation = func(error) bool { return false }

// isTargetProviderFKViolation reports whether err is a foreign-key violation
// that a model target on a provider can cause. SQLite does not say which
// foreign key failed, so any is accepted there; Postgres names the constraint
// and only that of ai_model_targets.provider_slug counts. The error's type is
// examined, not its text.
func isTargetProviderFKViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqliteConstraintForeignKey
	}
	return pgTargetProviderFKViolation(err)
}
