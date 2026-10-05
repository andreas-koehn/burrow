package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrDuplicateProvider is returned when a provider's slug is taken or its
// service already backs another provider.
var ErrDuplicateProvider = errors.New("db: provider slug or service already in use")

const aiProviderCols = `slug, name, kind, service_id, api_format, created_at`

func scanAIProvider(row interface{ Scan(...any) error }) (AIProvider, error) {
	var p AIProvider
	err := row.Scan(&p.Slug, &p.Name, &p.Kind, &p.ServiceID, &p.APIFormat, &p.CreatedAt)
	return p, err
}

// CreateAIProvider inserts a provider row.
func (x *DB) CreateAIProvider(ctx context.Context, p AIProvider) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO ai_providers(slug, name, kind, service_id, api_format) VALUES(?,?,?,?,?)`,
		p.Slug, p.Name, p.Kind, p.ServiceID, p.APIFormat,
	)
	if err != nil {
		if isDuplicateServiceErr(err) {
			return ErrDuplicateProvider
		}
		return fmt.Errorf("create ai provider: %w", err)
	}
	return nil
}

// GetAIProvider returns the provider with the given slug, or ErrNotFound.
func (x *DB) GetAIProvider(ctx context.Context, slug string) (AIProvider, error) {
	p, err := scanAIProvider(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+aiProviderCols+` FROM ai_providers WHERE slug=?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return AIProvider{}, ErrNotFound
	}
	if err != nil {
		return AIProvider{}, fmt.Errorf("get ai provider: %w", err)
	}
	return p, nil
}

// GetAIProviderByService returns the provider backed by serviceID, or ErrNotFound.
func (x *DB) GetAIProviderByService(ctx context.Context, serviceID string) (AIProvider, error) {
	p, err := scanAIProvider(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+aiProviderCols+` FROM ai_providers WHERE service_id=?`, serviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return AIProvider{}, ErrNotFound
	}
	if err != nil {
		return AIProvider{}, fmt.Errorf("get ai provider by service: %w", err)
	}
	return p, nil
}

// ListAIProviders returns every provider ordered by slug (never nil).
func (x *DB) ListAIProviders(ctx context.Context) ([]AIProvider, error) {
	rows, err := x.sqlDB.QueryContext(ctx, `SELECT `+aiProviderCols+` FROM ai_providers ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list ai providers: %w", err)
	}
	defer rows.Close()
	out := make([]AIProvider, 0)
	for rows.Next() {
		p, err := scanAIProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("list ai providers: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateAIProvider renames a provider and/or changes its display name.
func (x *DB) UpdateAIProvider(ctx context.Context, slug, newSlug, name string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE ai_providers SET slug=?, name=? WHERE slug=?`, newSlug, name, slug)
	if err != nil {
		if isDuplicateServiceErr(err) {
			return ErrDuplicateProvider
		}
		return fmt.Errorf("update ai provider: %w", err)
	}
	return notFoundIfNoRows(res, "update ai provider")
}

// DeleteAIProvider removes a provider row. The backing service is untouched.
func (x *DB) DeleteAIProvider(ctx context.Context, slug string) error {
	res, err := x.sqlDB.ExecContext(ctx, `DELETE FROM ai_providers WHERE slug=?`, slug)
	if err != nil {
		return fmt.Errorf("delete ai provider: %w", err)
	}
	return notFoundIfNoRows(res, "delete ai provider")
}

func notFoundIfNoRows(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", op, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
