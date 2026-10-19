package db

// oauth.go — the tables of Burrow's OAuth 2.1 authorization server
// (migration 0036): pre-registered clients, authorization codes, grants and
// tokens. Codes and tokens are stored as sha256-hex only; the plaintext never
// reaches this layer. Lists are JSON arrays.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// OAuthClient is a pre-registered public client.
type OAuthClient struct {
	ClientID     string
	Name         string
	RedirectURIs []string
	CreatedBy    string // user id of the admin who registered it
	CreatedAt    time.Time
}

// OAuthCode is an authorization code waiting for its exchange, with the grant
// it will become.
type OAuthCode struct {
	Hash          string
	ClientID      string
	ClientName    string
	UserID        string
	RedirectURI   string
	CodeChallenge string // S256
	Resource      string
	Scopes        []string
	AllowedTools  []string
	ToolsReadOnly bool
	ExpiresAt     time.Time
	Used          bool
	GrantID       string // set by the exchange
	CreatedAt     time.Time
}

// OAuthGrant is one user's consent for one client at one resource.
type OAuthGrant struct {
	ID            string
	ClientID      string
	ClientName    string
	UserID        string
	Resource      string
	Scopes        []string
	AllowedTools  []string
	ToolsReadOnly bool
	CreatedAt     time.Time
	LastUsed      *time.Time
	RevokedAt     *time.Time
}

// OAuthToken is an access or refresh token of a grant.
type OAuthToken struct {
	Hash      string
	Kind      string // "access" or "refresh"
	GrantID   string
	FamilyID  string
	ExpiresAt time.Time
	UsedAt    *time.Time // refresh tokens: when it was exchanged
	CreatedAt time.Time
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseList(s string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []string{}
	}
	return out
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

// --- clients ---------------------------------------------------------------

// CreateOAuthClient inserts a pre-registered client.
func (x *DB) CreateOAuthClient(ctx context.Context, c OAuthClient) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO oauth_clients(client_id, name, redirect_uris, created_by, created_at) VALUES(?,?,?,?,?)`,
		c.ClientID, c.Name, jsonList(c.RedirectURIs), c.CreatedBy, c.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("create oauth client: %w", err)
	}
	return nil
}

const oauthClientCols = `client_id, name, redirect_uris, created_by, created_at`

func scanOAuthClient(row interface{ Scan(...any) error }) (OAuthClient, error) {
	var c OAuthClient
	var uris string
	if err := row.Scan(&c.ClientID, &c.Name, &uris, &c.CreatedBy, &c.CreatedAt); err != nil {
		return c, err
	}
	c.RedirectURIs = parseList(uris)
	c.CreatedAt = c.CreatedAt.UTC()
	return c, nil
}

// GetOAuthClient returns the pre-registered client, or ErrNotFound.
func (x *DB) GetOAuthClient(ctx context.Context, clientID string) (OAuthClient, error) {
	c, err := scanOAuthClient(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+oauthClientCols+` FROM oauth_clients WHERE client_id=?`, clientID))
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return OAuthClient{}, fmt.Errorf("get oauth client: %w", err)
	}
	return c, nil
}

// ListOAuthClients returns every pre-registered client, oldest first.
func (x *DB) ListOAuthClients(ctx context.Context) ([]OAuthClient, error) {
	rows, err := x.sqlDB.QueryContext(ctx, `SELECT `+oauthClientCols+` FROM oauth_clients ORDER BY created_at, client_id`)
	if err != nil {
		return nil, fmt.Errorf("list oauth clients: %w", err)
	}
	defer rows.Close()
	out := []OAuthClient{}
	for rows.Next() {
		c, err := scanOAuthClient(rows)
		if err != nil {
			return nil, fmt.Errorf("scan oauth client: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteOAuthClient removes a pre-registered client and ends every grant it
// holds (their tokens are deleted). ErrNotFound when there is no such client.
func (x *DB) DeleteOAuthClient(ctx context.Context, clientID string, now time.Time) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete oauth client: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM oauth_clients WHERE client_id=?`, clientID)
	if err != nil {
		return fmt.Errorf("delete oauth client: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM oauth_tokens WHERE grant_id IN (SELECT id FROM oauth_grants WHERE client_id=?)`, clientID); err != nil {
		return fmt.Errorf("delete oauth client tokens: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_grants SET revoked_at=? WHERE client_id=? AND revoked_at IS NULL`, now.UTC(), clientID); err != nil {
		return fmt.Errorf("revoke oauth client grants: %w", err)
	}
	return tx.Commit()
}

// --- codes -----------------------------------------------------------------

// CreateOAuthCode stores an authorization code by its hash.
func (x *DB) CreateOAuthCode(ctx context.Context, c OAuthCode) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO oauth_codes(code_hash, client_id, client_name, user_id, redirect_uri, code_challenge, resource,
		   scopes, allowed_tools, tools_read_only, expires_at, used, grant_id, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,0,'',?)`,
		c.Hash, c.ClientID, c.ClientName, c.UserID, c.RedirectURI, c.CodeChallenge, c.Resource,
		jsonList(c.Scopes), jsonList(c.AllowedTools), boolToInt(c.ToolsReadOnly), c.ExpiresAt.UTC(), c.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("create oauth code: %w", err)
	}
	return nil
}

// RedeemOAuthCode marks the code used and returns it. A code that was used
// before comes back with Used set and nothing changed; the caller refuses it
// (and revokes GrantID). ErrNotFound when no code has that hash. The caller
// checks expiry, client, redirect URI and verifier after this: a failed
// attempt burns the code too.
func (x *DB) RedeemOAuthCode(ctx context.Context, hash string) (OAuthCode, error) {
	var c OAuthCode
	var scopes, tools string
	var ro, used int
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT code_hash, client_id, client_name, user_id, redirect_uri, code_challenge, resource,
		        scopes, allowed_tools, tools_read_only, expires_at, used, grant_id, created_at
		   FROM oauth_codes WHERE code_hash=?`, hash).
		Scan(&c.Hash, &c.ClientID, &c.ClientName, &c.UserID, &c.RedirectURI, &c.CodeChallenge, &c.Resource,
			&scopes, &tools, &ro, &c.ExpiresAt, &used, &c.GrantID, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthCode{}, ErrNotFound
	}
	if err != nil {
		return OAuthCode{}, fmt.Errorf("get oauth code: %w", err)
	}
	c.Scopes, c.AllowedTools, c.ToolsReadOnly = parseList(scopes), parseList(tools), ro != 0
	c.ExpiresAt, c.CreatedAt = c.ExpiresAt.UTC(), c.CreatedAt.UTC()
	if used != 0 {
		c.Used = true
		return c, nil
	}
	res, err := x.sqlDB.ExecContext(ctx, `UPDATE oauth_codes SET used=1 WHERE code_hash=? AND used=0`, hash)
	if err != nil {
		return OAuthCode{}, fmt.Errorf("redeem oauth code: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Someone else redeemed it between the read and the update.
		c.Used = true
		if err := x.sqlDB.QueryRowContext(ctx, `SELECT grant_id FROM oauth_codes WHERE code_hash=?`, hash).Scan(&c.GrantID); err != nil {
			return OAuthCode{}, fmt.Errorf("reread oauth code: %w", err)
		}
	}
	return c, nil
}

// --- grants and tokens -----------------------------------------------------

func insertOAuthToken(ctx context.Context, q execer, t OAuthToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO oauth_tokens(token_hash, kind, grant_id, family_id, expires_at, created_at) VALUES(?,?,?,?,?,?)`,
		t.Hash, t.Kind, t.GrantID, t.FamilyID, t.ExpiresAt.UTC(), t.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert oauth token: %w", err)
	}
	return nil
}

// CreateOAuthGrant inserts the grant and its first tokens in one transaction
// and records the grant on the code it was exchanged for (codeHash; empty:
// none).
func (x *DB) CreateOAuthGrant(ctx context.Context, g OAuthGrant, codeHash string, tokens ...OAuthToken) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin oauth grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oauth_grants(id, client_id, client_name, user_id, resource, scopes, allowed_tools, tools_read_only, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		g.ID, g.ClientID, g.ClientName, g.UserID, g.Resource, jsonList(g.Scopes), jsonList(g.AllowedTools),
		boolToInt(g.ToolsReadOnly), g.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("insert oauth grant: %w", err)
	}
	for _, t := range tokens {
		if err := insertOAuthToken(ctx, tx, t); err != nil {
			return err
		}
	}
	if codeHash != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET grant_id=? WHERE code_hash=?`, g.ID, codeHash); err != nil {
			return fmt.Errorf("link oauth code: %w", err)
		}
	}
	return tx.Commit()
}

const oauthGrantCols = `g.id, g.client_id, g.client_name, g.user_id, g.resource, g.scopes, g.allowed_tools, g.tools_read_only, g.created_at, g.last_used, g.revoked_at`

func scanOAuthGrant(row interface{ Scan(...any) error }, extra ...any) (OAuthGrant, error) {
	var g OAuthGrant
	var scopes, tools string
	var ro int
	var last, revoked sql.NullTime
	dest := append([]any{&g.ID, &g.ClientID, &g.ClientName, &g.UserID, &g.Resource, &scopes, &tools, &ro,
		&g.CreatedAt, &last, &revoked}, extra...)
	if err := row.Scan(dest...); err != nil {
		return g, err
	}
	g.Scopes, g.AllowedTools, g.ToolsReadOnly = parseList(scopes), parseList(tools), ro != 0
	g.CreatedAt = g.CreatedAt.UTC()
	g.LastUsed, g.RevokedAt = nullTimePtr(last), nullTimePtr(revoked)
	return g, nil
}

// GetOAuthGrant returns the grant, revoked or not, or ErrNotFound.
func (x *DB) GetOAuthGrant(ctx context.Context, id string) (OAuthGrant, error) {
	g, err := scanOAuthGrant(x.sqlDB.QueryRowContext(ctx, `SELECT `+oauthGrantCols+` FROM oauth_grants g WHERE g.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthGrant{}, ErrNotFound
	}
	if err != nil {
		return OAuthGrant{}, fmt.Errorf("get oauth grant: %w", err)
	}
	return g, nil
}

// ListOAuthGrants returns the grants that are not revoked, oldest first: the
// user's, or everyone's when userID is empty.
func (x *DB) ListOAuthGrants(ctx context.Context, userID string) ([]OAuthGrant, error) {
	q := `SELECT ` + oauthGrantCols + ` FROM oauth_grants g WHERE g.revoked_at IS NULL`
	args := []any{}
	if userID != "" {
		q += ` AND g.user_id=?`
		args = append(args, userID)
	}
	rows, err := x.sqlDB.QueryContext(ctx, q+` ORDER BY g.created_at, g.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list oauth grants: %w", err)
	}
	defer rows.Close()
	out := []OAuthGrant{}
	for rows.Next() {
		g, err := scanOAuthGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("scan oauth grant: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeOAuthGrant ends the grant and deletes its tokens. Revoking a revoked
// grant changes nothing and is no error; ErrNotFound when there is no grant.
func (x *DB) RevokeOAuthGrant(ctx context.Context, id string, now time.Time) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin revoke oauth grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var seen string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM oauth_grants WHERE id=?`, id).Scan(&seen); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("find oauth grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now.UTC(), id); err != nil {
		return fmt.Errorf("revoke oauth grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id=?`, id); err != nil {
		return fmt.Errorf("delete oauth grant tokens: %w", err)
	}
	return tx.Commit()
}

// TouchOAuthGrant sets last_used, at most once a minute per grant.
func (x *DB) TouchOAuthGrant(ctx context.Context, id string, now time.Time) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`UPDATE oauth_grants SET last_used=? WHERE id=? AND (last_used IS NULL OR last_used < ?)`,
		now.UTC(), id, now.Add(-time.Minute).UTC())
	if err != nil {
		return fmt.Errorf("touch oauth grant: %w", err)
	}
	return nil
}

// GetOAuthToken returns the token with its grant, or ErrNotFound. Neither
// expiry nor revocation is checked here.
func (x *DB) GetOAuthToken(ctx context.Context, hash string) (OAuthToken, OAuthGrant, error) {
	var t OAuthToken
	var used sql.NullTime
	g, err := scanOAuthGrant(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+oauthGrantCols+`, t.token_hash, t.kind, t.grant_id, t.family_id, t.expires_at, t.used_at, t.created_at
		   FROM oauth_tokens t JOIN oauth_grants g ON g.id=t.grant_id WHERE t.token_hash=?`, hash),
		&t.Hash, &t.Kind, &t.GrantID, &t.FamilyID, &t.ExpiresAt, &used, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthToken{}, OAuthGrant{}, ErrNotFound
	}
	if err != nil {
		return OAuthToken{}, OAuthGrant{}, fmt.Errorf("get oauth token: %w", err)
	}
	t.ExpiresAt, t.CreatedAt, t.UsedAt = t.ExpiresAt.UTC(), t.CreatedAt.UTC(), nullTimePtr(used)
	return t, g, nil
}

// RotateOAuthRefresh marks the refresh token used and inserts its successors
// in one transaction. false (and nothing inserted) when the token was used
// already or is gone: the caller treats that as reuse.
func (x *DB) RotateOAuthRefresh(ctx context.Context, oldHash string, now time.Time, tokens ...OAuthToken) (bool, error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin rotate oauth refresh: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE oauth_tokens SET used_at=? WHERE token_hash=? AND kind='refresh' AND used_at IS NULL`, now.UTC(), oldHash)
	if err != nil {
		return false, fmt.Errorf("use oauth refresh: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	for _, t := range tokens {
		if err := insertOAuthToken(ctx, tx, t); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// DeleteOAuthToken removes one token (an access token revoked by its client).
func (x *DB) DeleteOAuthToken(ctx context.Context, hash string) error {
	if _, err := x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE token_hash=?`, hash); err != nil {
		return fmt.Errorf("delete oauth token: %w", err)
	}
	return nil
}

// PruneOAuth deletes codes and tokens past their expiry. Codes are kept ten
// minutes longer, so that a late second use is still recognised as one.
func (x *DB) PruneOAuth(ctx context.Context, now time.Time) error {
	if _, err := x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_codes WHERE expires_at < ?`, now.Add(-10*time.Minute).UTC()); err != nil {
		return fmt.Errorf("prune oauth codes: %w", err)
	}
	if _, err := x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE expires_at < ?`, now.UTC()); err != nil {
		return fmt.Errorf("prune oauth tokens: %w", err)
	}
	return nil
}
