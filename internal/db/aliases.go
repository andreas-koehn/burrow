package db

import (
	"context"
	"fmt"
	"strings"
)

// The model_aliases table is what earlier versions kept model aliases in.
// Aliases became synthetic models: nothing writes the table any more, and
// the one-time import (store.ImportModelAliases) reads it.

// ListModelAliases returns every model_aliases row ordered by alias.
// The list is always returned as a non-nil slice (possibly empty).
func (x *DB) ListModelAliases(ctx context.Context) ([]ModelAlias, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT alias, concrete_model, service_id, created_at, provider, priority FROM model_aliases ORDER BY alias`,
	)
	if err != nil {
		return nil, fmt.Errorf("list model aliases: %w", err)
	}
	defer rows.Close()
	out := make([]ModelAlias, 0)
	for rows.Next() {
		var m ModelAlias
		if err := rows.Scan(&m.Alias, &m.ConcreteModel, &m.ServiceID, &m.CreatedAt, &m.Provider, &m.Priority); err != nil {
			return nil, fmt.Errorf("scan model alias: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list model aliases rows: %w", err)
	}
	return out, nil
}

// isSQLiteUnique reports whether err is a modernc.org/sqlite UNIQUE
// constraint violation. The driver does not expose a typed error for this so
// we string-match the canonical phrase the C-port emits. This is robust
// across versions.
func isSQLiteUnique(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "constraint failed: UNIQUE")
}
