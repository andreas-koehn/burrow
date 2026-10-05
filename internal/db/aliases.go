package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrAliasExists is returned by CreateModelAlias when a row with the same
// alias primary key already exists. Handlers map this to HTTP 409.
var ErrAliasExists = errors.New("db: model alias already exists")

// CreateModelAlias inserts a new model_aliases row.
//
// Returns ErrAliasExists when a row with the same alias already exists
// (SQLite UNIQUE constraint violation on the PRIMARY KEY). The created_at
// column is populated by SQLite's CURRENT_TIMESTAMP default, so the caller
// does not pass a timestamp.
//
// service_id MUST reference an existing services row (FK enforced via the
// PRAGMA foreign_keys=ON pragma set in Open); an unknown service_id surfaces
// as a generic insert error.
//
// v0.5.0: m.Provider and m.Priority are persisted; Priority defaults to 100
// when zero-valued (matching the schema DEFAULT 100).
func (x *DB) CreateModelAlias(ctx context.Context, m ModelAlias) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO model_aliases(alias, concrete_model, service_id, provider, priority) VALUES(?,?,?,?,?)`,
		m.Alias, m.ConcreteModel, m.ServiceID, m.Provider, m.Priority,
	)
	if err != nil {
		// modernc.org/sqlite reports UNIQUE violations via the error string;
		// the simplest stable detection is to check for the canonical phrase.
		if isSQLiteUnique(err) {
			return ErrAliasExists
		}
		return fmt.Errorf("create model alias: %w", err)
	}
	return nil
}

// GetModelAlias returns the row with the given alias, or ErrNotFound.
// v0.5.0: includes provider and priority columns.
func (x *DB) GetModelAlias(ctx context.Context, alias string) (ModelAlias, error) {
	var m ModelAlias
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT alias, concrete_model, service_id, created_at, provider, priority FROM model_aliases WHERE alias=?`,
		alias,
	).Scan(&m.Alias, &m.ConcreteModel, &m.ServiceID, &m.CreatedAt, &m.Provider, &m.Priority)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelAlias{}, ErrNotFound
	}
	if err != nil {
		return ModelAlias{}, fmt.Errorf("get model alias: %w", err)
	}
	return m, nil
}

// ListModelAliases returns every model_aliases row ordered by alias.
// The list is always returned as a non-nil slice (possibly empty).
// v0.5.0: includes provider and priority columns.
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

// GetAliasesByPriority returns all model_aliases rows matching the given alias
// name, ordered by priority ASC. alias is the table's primary key, so at most
// one row matches and the order needs no tie-breaker (SQLite's rowid, used
// here before, does not exist on Postgres). This is the multi_provider
// routing query — the idx_model_aliases_alias_priority index covers it.
// v0.5.0 addition. The result is always a non-nil slice (possibly empty).
func (x *DB) GetAliasesByPriority(ctx context.Context, alias string) ([]ModelAlias, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT alias, concrete_model, service_id, created_at, provider, priority
		   FROM model_aliases
		  WHERE alias=?
		  ORDER BY priority ASC`,
		alias,
	)
	if err != nil {
		return nil, fmt.Errorf("get aliases by priority: %w", err)
	}
	defer rows.Close()
	out := make([]ModelAlias, 0)
	for rows.Next() {
		var m ModelAlias
		if err := rows.Scan(&m.Alias, &m.ConcreteModel, &m.ServiceID, &m.CreatedAt, &m.Provider, &m.Priority); err != nil {
			return nil, fmt.Errorf("scan alias by priority: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get aliases by priority rows: %w", err)
	}
	return out, nil
}

// UpdateModelAlias replaces (concrete_model, service_id, provider, priority)
// for the row with the given alias. Returns ErrNotFound when no row matches.
// v0.5.0: provider and priority are now part of the mutable payload.
func (x *DB) UpdateModelAlias(ctx context.Context, alias, concreteModel, serviceID string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE model_aliases SET concrete_model=?, service_id=? WHERE alias=?`,
		concreteModel, serviceID, alias,
	)
	if err != nil {
		return fmt.Errorf("update model alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update model alias rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateModelAliasFull replaces all mutable columns (concrete_model, service_id,
// provider, priority) for the row with the given alias. Used by the PUT handler
// when provider/priority are supplied (v0.5.0). Returns ErrNotFound when no row
// matches.
func (x *DB) UpdateModelAliasFull(ctx context.Context, alias, concreteModel, serviceID, provider string, priority int) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE model_aliases SET concrete_model=?, service_id=?, provider=?, priority=? WHERE alias=?`,
		concreteModel, serviceID, provider, priority, alias,
	)
	if err != nil {
		return fmt.Errorf("update model alias full: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update model alias full rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteModelAlias removes the row with the given alias. Returns ErrNotFound
// when no row matches.
func (x *DB) DeleteModelAlias(ctx context.Context, alias string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM model_aliases WHERE alias=?`, alias,
	)
	if err != nil {
		return fmt.Errorf("delete model alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete model alias rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// isSQLiteUnique reports whether err is a modernc.org/sqlite UNIQUE
// constraint violation. The driver does not expose a typed error for this so
// we string-match the canonical phrase the C-port emits. This is robust
// across versions; if it ever changes the test TestModelAliasesCreateConflict
// will fail loudly.
func isSQLiteUnique(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "constraint failed: UNIQUE")
}
