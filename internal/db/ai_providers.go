package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrDuplicateProvider is returned when a provider's slug is taken or its
// service already backs another provider.
var ErrDuplicateProvider = errors.New("db: provider slug or service already in use")

const aiProviderCols = `slug, name, kind, service_id, api_format, base_url, credential_slot, auth_header, auth_format, extra_headers, billing, created_at`

func scanAIProvider(row interface{ Scan(...any) error }) (AIProvider, error) {
	var p AIProvider
	var headers string
	err := row.Scan(&p.Slug, &p.Name, &p.Kind, &p.ServiceID, &p.APIFormat, &p.BaseURL, &p.CredentialSlot,
		&p.AuthHeader, &p.AuthFormat, &headers, &p.Billing, &p.CreatedAt)
	if err != nil {
		return p, err
	}
	p.ExtraHeaders = map[string]string{}
	if headers != "" {
		if err := json.Unmarshal([]byte(headers), &p.ExtraHeaders); err != nil {
			return p, fmt.Errorf("ai provider %s: extra_headers: %w", p.Slug, err)
		}
	}
	return p, nil
}

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertAIProvider writes p through q.
func insertAIProvider(ctx context.Context, q execer, p AIProvider) error {
	headers, err := json.Marshal(nonNilHeaders(p.ExtraHeaders))
	if err != nil {
		return fmt.Errorf("create ai provider: extra_headers: %w", err)
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO ai_providers(slug, name, kind, service_id, api_format, base_url, credential_slot, auth_header, auth_format, extra_headers, billing)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		p.Slug, p.Name, p.Kind, p.ServiceID, p.APIFormat, p.BaseURL, p.CredentialSlot,
		orDefault(p.AuthHeader, "Authorization"), orDefault(p.AuthFormat, "Bearer {key}"), string(headers), orDefault(p.Billing, "metered"),
	)
	if err != nil {
		if isDuplicateServiceErr(err) {
			return ErrDuplicateProvider
		}
		return fmt.Errorf("create ai provider: %w", err)
	}
	return nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func nonNilHeaders(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}

// CreateAIProvider inserts a provider row.
func (x *DB) CreateAIProvider(ctx context.Context, p AIProvider) error {
	return insertAIProvider(ctx, x.sqlDB, p)
}

// CreateDirectAIProvider inserts the backing service row (type "direct") and
// the provider in one transaction. The service's type, the provider's kind
// and the link between the two are set here, whatever the caller passed.
func (x *DB) CreateDirectAIProvider(ctx context.Context, svc Service, p AIProvider) error {
	svc.Type, p.Kind, p.ServiceID = "direct", "direct", svc.ID
	if svc.CreatedAt.IsZero() {
		svc.CreatedAt = time.Now().UTC()
	}
	header := svc.APIKeyHeader
	if header == "" {
		header = "Authorization"
	}
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin direct provider tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(id, user_id, name, type, access_mode, api_key_header, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		svc.ID, svc.UserID, svc.Name, svc.Type, svc.AccessMode, header, svc.CreatedAt,
	); err != nil {
		_ = tx.Rollback()
		if isDuplicateServiceErr(err) {
			return ErrDuplicateProvider
		}
		return fmt.Errorf("create backing service: %w", err)
	}
	if err := insertAIProvider(ctx, tx, p); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit direct provider tx: %w", err)
	}
	return nil
}

// UpdateAIProviderUpstream replaces the upstream settings of the provider p.Slug.
func (x *DB) UpdateAIProviderUpstream(ctx context.Context, p AIProvider) error {
	headers, err := json.Marshal(nonNilHeaders(p.ExtraHeaders))
	if err != nil {
		return fmt.Errorf("update ai provider upstream: extra_headers: %w", err)
	}
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE ai_providers SET base_url=?, credential_slot=?, auth_header=?, auth_format=?, extra_headers=?, billing=?, api_format=? WHERE slug=?`,
		p.BaseURL, p.CredentialSlot, orDefault(p.AuthHeader, "Authorization"), orDefault(p.AuthFormat, "Bearer {key}"),
		string(headers), orDefault(p.Billing, "metered"), p.APIFormat, p.Slug)
	if err != nil {
		return fmt.Errorf("update ai provider upstream: %w", err)
	}
	return notFoundIfNoRows(res, "update ai provider upstream")
}

// DeleteAIProviderAndBacking deletes the provider and, for kind "direct", its
// backing service, in one transaction.
func (x *DB) DeleteAIProviderAndBacking(ctx context.Context, slug string) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete provider tx: %w", err)
	}
	var kind, serviceID string
	err = tx.QueryRowContext(ctx, `SELECT kind, service_id FROM ai_providers WHERE slug=?`, slug).Scan(&kind, &serviceID)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return ErrNotFound
	}
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete ai provider: %w", err)
	}
	deleted := int64(0)
	if kind == "direct" {
		// The provider row goes with the service by cascade.
		res, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id=? AND type='direct'`, serviceID)
		if err == nil {
			deleted, err = res.RowsAffected()
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete ai provider: backing service: %w", err)
		}
	}
	if deleted == 0 {
		// A tunnel provider, or a direct one whose service is not a backing
		// row: that service is someone's tunnel and stays.
		if _, err := tx.ExecContext(ctx, `DELETE FROM ai_providers WHERE slug=?`, slug); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete ai provider: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete provider tx: %w", err)
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
