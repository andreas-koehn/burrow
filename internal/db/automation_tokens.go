package db

// automation_tokens.go — typed CRUD over the automation_tokens table
// (migration 0010). Plaintext bearer tokens are NEVER persisted; only the
// sha256-hex hash (TokenHash) of the plaintext is stored. The permissions
// column is a JSON array.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CreateAutomationToken inserts a new automation_tokens row. The caller
// pre-computes the token hash (sha256-hex of plaintext) and supplies the
// JSON-encoded permissions array. created_at is populated by the SQLite
// default.
func (x *DB) CreateAutomationToken(ctx context.Context, t AutomationToken) error {
	var expires interface{}
	if t.ExpiresAt != nil {
		expires = t.ExpiresAt.UTC()
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO automation_tokens(id, name, prefix, user_id, role_at_mint, token_hash, permissions, expires_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Prefix, t.UserID, t.RoleAtMint, t.TokenHash, t.Permissions, expires,
	)
	if err != nil {
		return fmt.Errorf("create automation token: %w", err)
	}
	return nil
}

// GetAutomationToken returns the row with the given id, or ErrNotFound.
func (x *DB) GetAutomationToken(ctx context.Context, id string) (AutomationToken, error) {
	return x.scanAutomationToken(x.sqlDB.QueryRowContext(ctx,
		`SELECT id, name, prefix, user_id, role_at_mint, token_hash, permissions,
		        expires_at, last_used, created_at
		   FROM automation_tokens WHERE id=?`, id))
}

// GetAutomationTokenByHash returns the row whose token_hash matches the
// given sha256-hex digest, or ErrNotFound. Used by the bearer middleware
// for the single per-request lookup.
func (x *DB) GetAutomationTokenByHash(ctx context.Context, hash string) (AutomationToken, error) {
	return x.scanAutomationToken(x.sqlDB.QueryRowContext(ctx,
		`SELECT id, name, prefix, user_id, role_at_mint, token_hash, permissions,
		        expires_at, last_used, created_at
		   FROM automation_tokens WHERE token_hash=?`, hash))
}

// ListAutomationTokens returns every row, ordered by created_at ascending.
// The returned slice is always non-nil (possibly empty).
func (x *DB) ListAutomationTokens(ctx context.Context) ([]AutomationToken, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, name, prefix, user_id, role_at_mint, token_hash, permissions,
		        expires_at, last_used, created_at
		   FROM automation_tokens ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list automation tokens: %w", err)
	}
	defer rows.Close()
	return scanAutomationTokens(rows)
}

// ListAutomationTokensByUser returns every row owned by the given user,
// ordered by created_at ascending. The slice is always non-nil.
func (x *DB) ListAutomationTokensByUser(ctx context.Context, userID string) ([]AutomationToken, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, name, prefix, user_id, role_at_mint, token_hash, permissions,
		        expires_at, last_used, created_at
		   FROM automation_tokens WHERE user_id=? ORDER BY created_at`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list automation tokens by user: %w", err)
	}
	defer rows.Close()
	return scanAutomationTokens(rows)
}

// DeleteAutomationToken removes the row with the given id. Returns
// ErrNotFound if no row matched.
func (x *DB) DeleteAutomationToken(ctx context.Context, id string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM automation_tokens WHERE id=?`, id,
	)
	if err != nil {
		return fmt.Errorf("delete automation token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete automation token rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete automation token: %w", ErrNotFound)
	}
	return nil
}

// TouchAutomationTokenLastUsed sets last_used to the current timestamp.
// Best-effort: the bearer middleware ignores errors from this call.
func (x *DB) TouchAutomationTokenLastUsed(ctx context.Context, id string) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`UPDATE automation_tokens SET last_used=CURRENT_TIMESTAMP WHERE id=?`, id,
	)
	if err != nil {
		return fmt.Errorf("touch automation token last_used: %w", err)
	}
	return nil
}

func (x *DB) scanAutomationToken(row *sql.Row) (AutomationToken, error) {
	var t AutomationToken
	var expires, lastUsed sql.NullTime
	err := row.Scan(&t.ID, &t.Name, &t.Prefix, &t.UserID, &t.RoleAtMint,
		&t.TokenHash, &t.Permissions, &expires, &lastUsed, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AutomationToken{}, ErrNotFound
	}
	if err != nil {
		return AutomationToken{}, fmt.Errorf("get automation token: %w", err)
	}
	if expires.Valid {
		ts := expires.Time.UTC()
		t.ExpiresAt = &ts
	}
	if lastUsed.Valid {
		ts := lastUsed.Time.UTC()
		t.LastUsed = &ts
	}
	return t, nil
}

func scanAutomationTokens(rows *sql.Rows) ([]AutomationToken, error) {
	out := make([]AutomationToken, 0)
	for rows.Next() {
		var t AutomationToken
		var expires, lastUsed sql.NullTime
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.UserID, &t.RoleAtMint,
			&t.TokenHash, &t.Permissions, &expires, &lastUsed, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan automation token: %w", err)
		}
		if expires.Valid {
			ts := expires.Time.UTC()
			t.ExpiresAt = &ts
		}
		if lastUsed.Valid {
			ts := lastUsed.Time.UTC()
			t.LastUsed = &ts
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list automation tokens rows: %w", err)
	}
	return out, nil
}
