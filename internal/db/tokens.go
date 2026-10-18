package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

const clientTokenCols = `id, user_id, name, token_hash, last_used, created_at,
	last_seen, last_os, last_arch, last_client_version, last_remote_addr, last_served, expires_at`

func scanClientToken(row interface{ Scan(...any) error }) (ClientToken, error) {
	var ct ClientToken
	var lastUsed, lastSeen, expires sql.NullTime
	var served string
	if err := row.Scan(&ct.ID, &ct.UserID, &ct.Name, &ct.TokenHash, &lastUsed, &ct.CreatedAt,
		&lastSeen, &ct.LastOS, &ct.LastArch, &ct.LastClientVersion, &ct.LastRemoteAddr, &served, &expires); err != nil {
		return ClientToken{}, err
	}
	if lastUsed.Valid {
		ct.LastUsed = &lastUsed.Time
	}
	if lastSeen.Valid {
		ct.LastSeen = &lastSeen.Time
	}
	if expires.Valid {
		ct.ExpiresAt = &expires.Time
	}
	ct.LastServed = []string{}
	if served != "" {
		ct.LastServed = strings.Split(served, servedSep)
	}
	return ct, nil
}

// CreateClientToken inserts a new client token row.
func (x *DB) CreateClientToken(ctx context.Context, ct ClientToken) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO client_tokens(id, user_id, name, token_hash, expires_at) VALUES(?,?,?,?,?)`,
		ct.ID, ct.UserID, ct.Name, ct.TokenHash, nullableTime(ct.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("create client token: %w", err)
	}
	return nil
}

// GetClientTokenByHash returns the token whose token_hash matches, or ErrNotFound.
func (x *DB) GetClientTokenByHash(ctx context.Context, hash string) (ClientToken, error) {
	ct, err := scanClientToken(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+clientTokenCols+` FROM client_tokens WHERE token_hash=?`, hash))
	if err == sql.ErrNoRows {
		return ClientToken{}, ErrNotFound
	}
	if err != nil {
		return ClientToken{}, fmt.Errorf("get client token by hash: %w", err)
	}
	return ct, nil
}

// ListClientTokensByUser returns all tokens belonging to the given user.
func (x *DB) ListClientTokensByUser(ctx context.Context, userID string) ([]ClientToken, error) {
	return x.queryClientTokens(ctx,
		`SELECT `+clientTokenCols+` FROM client_tokens WHERE user_id=? ORDER BY created_at`, userID)
}

// ListClientTokens returns the tokens of every user, by name and then by age.
// It is for callers that may see all of them (the clients overview of an
// admin).
func (x *DB) ListClientTokens(ctx context.Context) ([]ClientToken, error) {
	return x.queryClientTokens(ctx,
		`SELECT `+clientTokenCols+` FROM client_tokens ORDER BY name, created_at, id`)
}

func (x *DB) queryClientTokens(ctx context.Context, query string, args ...any) ([]ClientToken, error) {
	rows, err := x.sqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list client tokens: %w", err)
	}
	defer rows.Close()

	var out []ClientToken
	for rows.Next() {
		ct, err := scanClientToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan client token: %w", err)
		}
		out = append(out, ct)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list client tokens rows: %w", err)
	}
	return out, nil
}

// Limits of what a client session leaves on its token. The values come from
// the client and are display text only.
const (
	maxSessionText = 64  // OS, architecture, client version (characters)
	maxSessionAddr = 128 // remote address
	maxServedName  = 128 // one service name; a service title has at most 120
	maxServedNames = 50
	servedSep      = "\n"
)

// sessionText makes a value sent by a client fit to store and to show:
// control characters are dropped, the rest is cut to max characters.
func sessionText(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max]))
	}
	return s
}

// servedNames returns the names fit to store: cleaned like every session
// text, without empty names and duplicates, sorted, at most maxServedNames.
func servedNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = sessionText(n, maxServedName); n != "" {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > maxServedNames {
		out = out[:maxServedNames]
	}
	return out
}

// RecordClientSession stores what the session that logged in with the token
// looks like now and sets last_seen to the current time. It replaces the
// record of the session before. Returns ErrNotFound when no token has the id
// (it was revoked meanwhile).
//
// last_served holds the names joined by a newline. A service name is free
// text (the API only limits a title to 120 characters, and a client names its
// tunnels itself), so the names are not trusted to be free of newlines:
// sessionText drops every control character first.
func (x *DB) RecordClientSession(ctx context.Context, tokenID string, rec ClientSessionRecord) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE client_tokens SET last_seen=?, last_os=?, last_arch=?, last_client_version=?, last_remote_addr=?, last_served=? WHERE id=?`,
		time.Now().UTC(),
		sessionText(rec.OS, maxSessionText), sessionText(rec.Arch, maxSessionText),
		sessionText(rec.ClientVersion, maxSessionText), sessionText(rec.RemoteAddr, maxSessionAddr),
		strings.Join(servedNames(rec.Served), servedSep), tokenID,
	)
	if err != nil {
		return fmt.Errorf("record client session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record client session rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("record client session: %w", ErrNotFound)
	}
	return nil
}

// TouchClientTokenLastUsed sets last_used to the current timestamp for the given token ID.
func (x *DB) TouchClientTokenLastUsed(ctx context.Context, id string) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`UPDATE client_tokens SET last_used=CURRENT_TIMESTAMP WHERE id=?`, id,
	)
	if err != nil {
		return fmt.Errorf("touch client token last_used: %w", err)
	}
	return nil
}

// DeleteClientToken removes the token with the given ID, scoped to the owning user.
// Returns ErrNotFound if no row matched (token does not exist or belongs to another user).
func (x *DB) DeleteClientToken(ctx context.Context, id, userID string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM client_tokens WHERE id=? AND user_id=?`, id, userID,
	)
	if err != nil {
		return fmt.Errorf("delete client token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete client token rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete client token: %w", ErrNotFound)
	}
	return nil
}

// nullableTime is t as an instant in UTC, or NULL for nil.
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
