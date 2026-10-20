package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CreateSession inserts a new session row. The expiry is written in UTC:
// SQLite stores it as text, and DeleteExpiredSessions compares that text. An
// empty Kind is a dashboard session; an empty ParentID is stored as NULL.
func (x *DB) CreateSession(ctx context.Context, s Session) error {
	if s.Kind == "" {
		s.Kind = SessionKindDashboard
	}
	var parent any
	if s.ParentID != "" {
		parent = s.ParentID
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, expires_at, user_agent, ip, kind, parent_id) VALUES(?,?,?,?,?,?,?)`,
		s.ID, s.UserID, s.ExpiresAt.UTC(), s.UserAgent, s.IP, s.Kind, parent,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession returns the session with the given ID, or ErrNotFound.
// Note: the returned session may be expired; callers must check ExpiresAt.
func (x *DB) GetSession(ctx context.Context, id string) (Session, error) {
	var s Session
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE id=?`, id,
	).Scan(&s.ID, &s.UserID, &s.ExpiresAt, &s.CreatedAt, &s.UserAgent, &s.IP, &s.Kind, &s.ParentID)
	if err == sql.ErrNoRows {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

// sessionCols are the columns a Session is scanned from, in its field order.
const sessionCols = `id, user_id, expires_at, created_at, COALESCE(user_agent,''), COALESCE(ip,''), kind, COALESCE(parent_id,'')`

// DeleteSession removes the session with the given ID and the services
// sessions it handed off.
func (x *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := x.DeleteSessionWithChildren(ctx, id)
	return err
}

// DeleteSessionWithChildren removes the session with the given ID and the
// services sessions it handed off, in one transaction, and returns the ids
// of those services sessions (for their audit rows).
func (x *DB) DeleteSessionWithChildren(ctx context.Context, id string) ([]string, error) {
	children, _, err := x.deleteSessionTree(ctx, id, "")
	return children, err
}

// deleteSessionTree deletes session id (scoped to userID when it is not "")
// and its services sessions, and returns the children's ids and whether the
// session itself was there.
func (x *DB) deleteSessionTree(ctx context.Context, id, userID string) ([]string, bool, error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin delete session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	scope, args := "", []any{id}
	if userID != "" {
		scope, args = " AND user_id=?", []any{id, userID}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`+scope, args...)
	if err != nil {
		return nil, false, fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("delete session rows affected: %w", err)
	}
	if n == 0 {
		// Not this caller's session (or gone): its children are left alone.
		return nil, false, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE parent_id=?`, id)
	if err != nil {
		return nil, false, fmt.Errorf("list services sessions: %w", err)
	}
	var children []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("scan services session: %w", err)
		}
		children = append(children, c)
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE parent_id=?`, id); err != nil {
		return nil, false, fmt.Errorf("delete services sessions: %w", err)
	}
	return children, true, tx.Commit()
}

// ListSessionsByUser returns the user's sessions, newest first.
func (x *DB) ListSessionsByUser(ctx context.Context, userID string) ([]Session, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT `+sessionCols+`
		   FROM sessions WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.ExpiresAt, &s.CreatedAt, &s.UserAgent, &s.IP, &s.Kind, &s.ParentID); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions rows: %w", err)
	}
	return out, nil
}

// DeleteSessionForUser deletes one session scoped to its owner, with the
// services sessions it handed off. Returns ErrNotFound if the session itself
// is missing or owned by another user.
func (x *DB) DeleteSessionForUser(ctx context.Context, id, userID string) error {
	_, err := x.DeleteSessionForUserWithChildren(ctx, id, userID)
	return err
}

// DeleteSessionForUserWithChildren is DeleteSessionForUser that also
// returns the ids of the services sessions it deleted with the session.
func (x *DB) DeleteSessionForUserWithChildren(ctx context.Context, id, userID string) ([]string, error) {
	children, found, err := x.deleteSessionTree(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return children, nil
}

// DeleteSessionsByUserExcept deletes all of the user's sessions except keepID
// and returns the number deleted ("sign out everywhere else").
func (x *DB) DeleteSessionsByUserExcept(ctx context.Context, userID, keepID string) (int64, error) {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id=? AND id<>?`, userID, keepID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions except: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete sessions except rows affected: %w", err)
	}
	return n, nil
}

// DeleteSessionsByUser deletes all of the user's sessions (used when an admin
// suspends an account) and returns the number deleted.
func (x *DB) DeleteSessionsByUser(ctx context.Context, userID string) (int64, error) {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id=?`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions by user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete sessions by user rows affected: %w", err)
	}
	return n, nil
}

// DeleteExpiredSessions removes all sessions whose expires_at is in the past
// and returns the number of rows deleted.
//
// The current time is taken in Go, in UTC, and bound: no date function of
// either database is used, so the statement runs on SQLite and Postgres.
// On Postgres expires_at is a timestamp and the comparison is one of
// instants. On SQLite both sides are the text modernc/sqlite writes for a
// time.Time ("YYYY-MM-DD HH:MM:SS.fffffffff +0000 UTC"), which sorts by time
// as long as both are UTC; CreateSession writes the expiry in UTC.
func (x *DB) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions rows affected: %w", err)
	}
	return n, nil
}
