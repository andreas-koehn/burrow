package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AIGatewayKey is a key that belongs to the AI gateway, not to one service.
type AIGatewayKey struct {
	ID            string
	Name          string
	KeyHash       string
	KeyPrefix     string // first 8 characters of the plaintext, for display
	UserID        string
	AllowedModels []string // never nil after a read; empty = all models
	SystemText    string   // put in front of the app's system content; empty = none
	// AllowedTools holds "<source>__<tool>" and "<source>__*" entries; never
	// nil after a read. Empty = no tools.
	AllowedTools []string
	// ToolsReadOnly limits the key to tools classed read-only.
	ToolsReadOnly bool
	LastUsed      *time.Time
	CreatedAt     time.Time
	RevokedAt     *time.Time
}

const aiGatewayKeyCols = `id, name, key_hash, key_prefix, user_id, allowed_models, system_text, allowed_tools, tools_read_only, last_used, created_at, revoked_at`

func scanAIGatewayKey(row interface{ Scan(...any) error }) (AIGatewayKey, error) {
	var k AIGatewayKey
	var allowed, tools string
	var lastUsed, revoked sql.NullTime
	if err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.UserID, &allowed, &k.SystemText, &tools, &k.ToolsReadOnly, &lastUsed, &k.CreatedAt, &revoked); err != nil {
		return k, err
	}
	k.AllowedModels = []string{}
	if allowed != "" {
		if err := json.Unmarshal([]byte(allowed), &k.AllowedModels); err != nil {
			return k, fmt.Errorf("ai gateway key %s: allowed_models: %w", k.ID, err)
		}
		if k.AllowedModels == nil {
			k.AllowedModels = []string{}
		}
	}
	k.AllowedTools = []string{}
	if tools != "" {
		if err := json.Unmarshal([]byte(tools), &k.AllowedTools); err != nil {
			return k, fmt.Errorf("ai gateway key %s: allowed_tools: %w", k.ID, err)
		}
		if k.AllowedTools == nil {
			k.AllowedTools = []string{}
		}
	}
	if lastUsed.Valid {
		t := lastUsed.Time
		k.LastUsed = &t
	}
	if revoked.Valid {
		t := revoked.Time
		k.RevokedAt = &t
	}
	return k, nil
}

// CreateAIGatewayKey inserts a key row (hash only).
func (x *DB) CreateAIGatewayKey(ctx context.Context, k AIGatewayKey) error {
	allowed, err := json.Marshal(append([]string{}, k.AllowedModels...))
	if err != nil {
		return fmt.Errorf("create ai gateway key: %w", err)
	}
	tools, err := json.Marshal(append([]string{}, k.AllowedTools...))
	if err != nil {
		return fmt.Errorf("create ai gateway key: %w", err)
	}
	if _, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO ai_gateway_keys(id, name, key_hash, key_prefix, user_id, allowed_models, system_text, allowed_tools, tools_read_only) VALUES(?,?,?,?,?,?,?,?,?)`,
		k.ID, k.Name, k.KeyHash, k.KeyPrefix, k.UserID, string(allowed), k.SystemText, string(tools), k.ToolsReadOnly); err != nil {
		return fmt.Errorf("create ai gateway key: %w", err)
	}
	return nil
}

func (x *DB) getAIGatewayKeyBy(ctx context.Context, col, val string) (AIGatewayKey, error) {
	k, err := scanAIGatewayKey(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+aiGatewayKeyCols+` FROM ai_gateway_keys WHERE `+col+`=?`, val))
	if errors.Is(err, sql.ErrNoRows) {
		return AIGatewayKey{}, ErrNotFound
	}
	if err != nil {
		return AIGatewayKey{}, fmt.Errorf("get ai gateway key: %w", err)
	}
	return k, nil
}

// GetAIGatewayKeyByHash returns the key, revoked or not, or ErrNotFound.
func (x *DB) GetAIGatewayKeyByHash(ctx context.Context, hash string) (AIGatewayKey, error) {
	return x.getAIGatewayKeyBy(ctx, "key_hash", hash)
}

// GetAIGatewayKey returns the key with the id, or ErrNotFound.
func (x *DB) GetAIGatewayKey(ctx context.Context, id string) (AIGatewayKey, error) {
	return x.getAIGatewayKeyBy(ctx, "id", id)
}

// ListAIGatewayKeys returns the user's keys, newest first ("" = all users).
func (x *DB) ListAIGatewayKeys(ctx context.Context, userID string) ([]AIGatewayKey, error) {
	q := `SELECT ` + aiGatewayKeyCols + ` FROM ai_gateway_keys`
	var args []any
	if userID != "" {
		q += ` WHERE user_id=?`
		args = append(args, userID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := x.sqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list ai gateway keys: %w", err)
	}
	defer rows.Close()
	out := make([]AIGatewayKey, 0)
	for rows.Next() {
		k, err := scanAIGatewayKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list ai gateway keys: scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AIGatewayKeyPatch is a change to the editable fields of a key: each field
// that is set replaces the stored value, a nil one leaves it as it is.
type AIGatewayKeyPatch struct {
	Name, SystemText            *string
	AllowedModels, AllowedTools *[]string
	ToolsReadOnly               *bool
}

// UpdateAIGatewayKey changes the fields of the key that p sets (name, system
// text, the allow-lists, the read-only switch), revoked or not; ErrNotFound
// when there is no such key. It is one UPDATE that leaves every other field
// to the row as it stands: two partial updates at the same time do not undo
// each other, as a read-modify-write would.
func (x *DB) UpdateAIGatewayKey(ctx context.Context, id string, p AIGatewayKeyPatch) error {
	var name, text, allowed, tools, readOnly any // nil: NULL, the stored value stays
	if p.Name != nil {
		name = *p.Name
	}
	if p.SystemText != nil {
		text = *p.SystemText
	}
	for _, f := range []struct {
		list *[]string
		dst  *any
	}{{p.AllowedModels, &allowed}, {p.AllowedTools, &tools}} {
		if f.list == nil {
			continue
		}
		b, err := json.Marshal(append([]string{}, (*f.list)...))
		if err != nil {
			return fmt.Errorf("update ai gateway key: %w", err)
		}
		*f.dst = string(b)
	}
	if p.ToolsReadOnly != nil {
		readOnly = *p.ToolsReadOnly
	}
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE ai_gateway_keys SET
		   name            = COALESCE(?, name),
		   system_text     = COALESCE(?, system_text),
		   allowed_models  = COALESCE(?, allowed_models),
		   allowed_tools   = COALESCE(?, allowed_tools),
		   tools_read_only = COALESCE(?, tools_read_only)
		 WHERE id=?`,
		name, text, allowed, tools, readOnly, id)
	if err != nil {
		return fmt.Errorf("update ai gateway key: %w", err)
	}
	return notFoundIfNoRows(res, "update ai gateway key")
}

// RevokeAIGatewayKey sets revoked_at once; a second call keeps the first time.
func (x *DB) RevokeAIGatewayKey(ctx context.Context, id string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE ai_gateway_keys SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP) WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("revoke ai gateway key: %w", err)
	}
	return notFoundIfNoRows(res, "revoke ai gateway key")
}

// RevokeAIGatewayKeyIfActive revokes the key only if it is not revoked yet
// and reports whether it changed anything: false for a key that was revoked
// before, by anyone, and for one that does not exist. Of several callers at
// the same moment exactly one gets true.
func (x *DB) RevokeAIGatewayKeyIfActive(ctx context.Context, id string) (bool, error) {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE ai_gateway_keys SET revoked_at = CURRENT_TIMESTAMP WHERE id=? AND revoked_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("revoke ai gateway key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke ai gateway key: %w", err)
	}
	return n > 0, nil
}

// TouchAIGatewayKey records a use of the key.
func (x *DB) TouchAIGatewayKey(ctx context.Context, id string) error {
	if _, err := x.sqlDB.ExecContext(ctx,
		`UPDATE ai_gateway_keys SET last_used=CURRENT_TIMESTAMP WHERE id=?`, id); err != nil {
		return fmt.Errorf("touch ai gateway key: %w", err)
	}
	return nil
}
