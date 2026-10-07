package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CreateSession inserts a new session row. The expiry is written in UTC:
// SQLite stores it as text, and DeleteExpiredSessions compares that text.
func (x *DB) CreateSession(ctx context.Context, s Session) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, expires_at, user_agent, ip) VALUES(?,?,?,?,?)`,
		s.ID, s.UserID, s.ExpiresAt.UTC(), s.UserAgent, s.IP,
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
		`SELECT id, user_id, expires_at, created_at, COALESCE(user_agent,''), COALESCE(ip,'') FROM sessions WHERE id=?`, id,
	).Scan(&s.ID, &s.UserID, &s.ExpiresAt, &s.CreatedAt, &s.UserAgent, &s.IP)
	if err == sql.ErrNoRows {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

// DeleteSession removes the session with the given ID.
func (x *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := x.sqlDB.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// ListSessionsByUser returns the user's sessions, newest first.
func (x *DB) ListSessionsByUser(ctx context.Context, userID string) ([]Session, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, user_id, expires_at, created_at, COALESCE(user_agent,''), COALESCE(ip,'')
		   FROM sessions WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.ExpiresAt, &s.CreatedAt, &s.UserAgent, &s.IP); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions rows: %w", err)
	}
	return out, nil
}

// DeleteSessionForUser deletes one session scoped to its owner.
// Returns ErrNotFound if no row matched (missing or owned by another user).
func (x *DB) DeleteSessionForUser(ctx context.Context, id, userID string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM sessions WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete session for user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete session for user rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
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
