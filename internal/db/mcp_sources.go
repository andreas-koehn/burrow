package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrDuplicateMCPSource is returned when a source's slug is taken or its
// service already backs another source.
var ErrDuplicateMCPSource = errors.New("db: tool source slug or service already in use")

// ErrMCPSourceBusy is returned by ModifyMCPSource when other writers changed
// the row under every one of its attempts.
var ErrMCPSourceBusy = errors.New("db: tool source is being changed concurrently")

// ErrServiceHasMCPSource is wrapped by DeleteService when a tool source
// points at the service; the error names the source.
var ErrServiceHasMCPSource = errors.New("service backs a tool source")

// ErrUserHasMCPSource is wrapped by DeleteUser when the user owns the service
// behind a tool source; the error names the sources.
var ErrUserHasMCPSource = errors.New("user owns the service of a tool source")

const mcpSourceCols = `slug, name, kind, service_id, url, credential_slot, auth_header, auth_format, protocol_version, rpm, max_concurrent, timeout_s, enabled, last_sync_at, last_sync_error, created_at, updated_at,
  (SELECT COUNT(*) FROM mcp_tools t WHERE t.source_slug=mcp_sources.slug AND t.status<>'removed'),
  (SELECT COUNT(*) FROM mcp_tools t WHERE t.source_slug=mcp_sources.slug AND t.status='pending')`

func scanMCPSource(row interface{ Scan(...any) error }) (MCPSource, error) {
	var s MCPSource
	var last sql.NullTime
	err := row.Scan(&s.Slug, &s.Name, &s.Kind, &s.ServiceID, &s.URL, &s.CredentialSlot, &s.AuthHeader, &s.AuthFormat,
		&s.ProtocolVersion, &s.RPM, &s.MaxConcurrent, &s.TimeoutS, (*intBool)(&s.Enabled), &last, &s.LastSyncError,
		&s.CreatedAt, &s.UpdatedAt, &s.ToolCount, &s.PendingCount)
	if err != nil {
		return s, err
	}
	if last.Valid {
		t := last.Time
		s.LastSyncAt = &t
	}
	return s, nil
}

func insertMCPSource(ctx context.Context, q execer, s MCPSource) error {
	now := time.Now().UTC()
	_, err := q.ExecContext(ctx,
		`INSERT INTO mcp_sources(slug, name, kind, service_id, url, credential_slot, auth_header, auth_format, protocol_version, rpm, max_concurrent, timeout_s, enabled, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.Slug, s.Name, s.Kind, s.ServiceID, s.URL, s.CredentialSlot,
		orDefault(s.AuthHeader, "Authorization"), orDefault(s.AuthFormat, "Bearer {key}"), s.ProtocolVersion,
		s.RPM, s.MaxConcurrent, s.TimeoutS, boolToInt(s.Enabled), now, now)
	if err != nil {
		if isDuplicateServiceErr(err) {
			return ErrDuplicateMCPSource
		}
		return fmt.Errorf("create tool source: %w", err)
	}
	return nil
}

// CreateRemoteMCPSource inserts the hidden backing service row (type
// "direct", the same kind of row a direct AI provider has) and the source in
// one transaction. The service's type, the source's kind and the link between
// the two are set here, whatever the caller passed.
func (x *DB) CreateRemoteMCPSource(ctx context.Context, svc Service, s MCPSource) error {
	svc.Type, s.Kind, s.ServiceID = "direct", "remote", svc.ID
	if svc.CreatedAt.IsZero() {
		svc.CreatedAt = time.Now().UTC()
	}
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remote tool source tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(id, user_id, name, type, access_mode, api_key_header, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		svc.ID, svc.UserID, svc.Name, svc.Type, svc.AccessMode, orDefault(svc.APIKeyHeader, "Authorization"), svc.CreatedAt,
	); err != nil {
		_ = tx.Rollback()
		if isDuplicateServiceErr(err) {
			return ErrDuplicateMCPSource
		}
		return fmt.Errorf("create backing service: %w", err)
	}
	if err := insertMCPSource(ctx, tx, s); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remote tool source tx: %w", err)
	}
	return nil
}

// CreateTunnelMCPSource inserts a source bound to an existing http service
// and closes the service's direct doors (gateway-only) in the same
// transaction: the MCP server behind a tunnel must be reached through the
// gateway only.
func (x *DB) CreateTunnelMCPSource(ctx context.Context, s MCPSource) error {
	s.Kind = "tunnel"
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tunnel tool source tx: %w", err)
	}
	if err := insertMCPSource(ctx, tx, s); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE services SET gateway_only=1 WHERE id=? AND type='http'`, s.ServiceID)
	if err == nil {
		err = notFoundIfNoRows(res, "tunnel tool source: gateway-only")
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tunnel tool source tx: %w", err)
	}
	return nil
}

// GetMCPSource returns the source with the given slug, or ErrNotFound.
func (x *DB) GetMCPSource(ctx context.Context, slug string) (MCPSource, error) {
	s, err := scanMCPSource(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+mcpSourceCols+` FROM mcp_sources WHERE slug=?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPSource{}, ErrNotFound
	}
	if err != nil {
		return MCPSource{}, fmt.Errorf("get tool source: %w", err)
	}
	return s, nil
}

// GetMCPSourceByService returns the source backed by serviceID, or ErrNotFound.
func (x *DB) GetMCPSourceByService(ctx context.Context, serviceID string) (MCPSource, error) {
	s, err := scanMCPSource(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+mcpSourceCols+` FROM mcp_sources WHERE service_id=?`, serviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPSource{}, ErrNotFound
	}
	if err != nil {
		return MCPSource{}, fmt.Errorf("get tool source by service: %w", err)
	}
	return s, nil
}

// ListMCPSources returns every source ordered by slug (never nil).
func (x *DB) ListMCPSources(ctx context.Context) ([]MCPSource, error) {
	rows, err := x.sqlDB.QueryContext(ctx, `SELECT `+mcpSourceCols+` FROM mcp_sources ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list tool sources: %w", err)
	}
	defer rows.Close()
	out := make([]MCPSource, 0)
	for rows.Next() {
		s, err := scanMCPSource(rows)
		if err != nil {
			return nil, fmt.Errorf("list tool sources: scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ModifyMCPSource reads the source, hands it to modify and stores the
// editable fields of the result, as one step: the UPDATE only applies while
// the row still holds the values that were read, and modify runs again on the
// fresh row when another writer got in between. So two concurrent updates
// cannot lose one another's fields. modify's error stops the update and is
// returned as is. ErrNotFound when no source has the slug; ErrMCPSourceBusy
// when the row changed under every attempt.
//
// An approval was given for one upstream and one credential: when the update
// changes url or credential_slot, every approved tool of the source becomes
// pending again in the same transaction, and every tool loses its approved
// hash and definition, so that neither a later sync nor a removed tool that
// comes back is approved by the old hash. repending is how many tools were
// approved before.
func (x *DB) ModifyMCPSource(ctx context.Context, slug string, modify func(MCPSource) (MCPSource, error)) (repending int, err error) {
	for i := 0; i < modifyUpstreamAttempts; i++ {
		old, err := x.GetMCPSource(ctx, slug)
		if err != nil {
			return 0, err
		}
		s, err := modify(old)
		if err != nil {
			return 0, err
		}
		n, done, err := x.modifyMCPSourceOnce(ctx, slug, old, s)
		if err != nil {
			return 0, err
		}
		if done {
			return n, nil
		}
	}
	return 0, fmt.Errorf("modify tool source %s: %w", slug, ErrMCPSourceBusy)
}

// modifyMCPSourceOnce stores s over old in one transaction; done is false
// when the row no longer held old.
func (x *DB) modifyMCPSourceOnce(ctx context.Context, slug string, old, s MCPSource) (repending int, done bool, err error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin modify tool source tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx,
		`UPDATE mcp_sources SET name=?, url=?, credential_slot=?, auth_header=?, auth_format=?, protocol_version=?, rpm=?, max_concurrent=?, timeout_s=?, enabled=?, updated_at=?
		  WHERE slug=? AND name=? AND url=? AND credential_slot=? AND auth_header=? AND auth_format=? AND protocol_version=? AND rpm=? AND max_concurrent=? AND timeout_s=? AND enabled=?`,
		s.Name, s.URL, s.CredentialSlot, orDefault(s.AuthHeader, "Authorization"), orDefault(s.AuthFormat, "Bearer {key}"),
		s.ProtocolVersion, s.RPM, s.MaxConcurrent, s.TimeoutS, boolToInt(s.Enabled), now,
		slug, old.Name, old.URL, old.CredentialSlot, old.AuthHeader, old.AuthFormat, old.ProtocolVersion,
		old.RPM, old.MaxConcurrent, old.TimeoutS, boolToInt(old.Enabled))
	if err != nil {
		return 0, false, fmt.Errorf("modify tool source: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("modify tool source rows affected: %w", err)
	}
	if n == 0 {
		return 0, false, nil
	}
	if s.URL != old.URL || s.CredentialSlot != old.CredentialSlot {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_tools WHERE source_slug=? AND status=?`, slug, MCPToolApproved).Scan(&repending); err != nil {
			return 0, false, fmt.Errorf("modify tool source: count approved tools: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mcp_tools SET status = CASE WHEN status=? THEN ? ELSE status END,
			   updated_at = CASE WHEN status=? THEN ? ELSE updated_at END,
			   approved_hash='', approved_definition=''
			 WHERE source_slug=? AND (approved_hash<>'' OR status=?)`,
			MCPToolApproved, MCPToolPending, MCPToolApproved, now, slug, MCPToolApproved); err != nil {
			return 0, false, fmt.Errorf("modify tool source: tools back to pending: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit modify tool source tx: %w", err)
	}
	return repending, true, nil
}

// DeleteMCPSourceAndBacking deletes the source with its tools. A remote
// source's hidden backing service goes with it (and everything that hangs on
// that row); a tunnel source's service is only unbound and stays, with its
// gateway-only flag as it is. ErrNotFound when no source has the slug.
func (x *DB) DeleteMCPSourceAndBacking(ctx context.Context, slug string) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete tool source tx: %w", err)
	}
	var kind, serviceID string
	err = tx.QueryRowContext(ctx, `SELECT kind, service_id FROM mcp_sources WHERE slug=?`, slug).Scan(&kind, &serviceID)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return ErrNotFound
	}
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete tool source: %w", err)
	}
	// The tools are removed explicitly as well, so that no engine has to
	// carry the cascade for it.
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_tools WHERE source_slug=?`, slug); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete tool source: tools: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_sources WHERE slug=?`, slug); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete tool source: %w", err)
	}
	if kind == "remote" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id=? AND type='direct'`, serviceID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete tool source: backing service: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete tool source tx: %w", err)
	}
	return nil
}
