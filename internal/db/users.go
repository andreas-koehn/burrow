package db

import (
	"context"
	"database/sql"
	"fmt"
)

// CountUsers returns the total number of users in the database.
func (x *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := x.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// UpdateUserPassword sets a new password_hash for the given user ID.
// Returns ErrNotFound if no row matched.
func (x *DB) UpdateUserPassword(ctx context.Context, userID, newHash string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE users SET password_hash=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		newHash, userID,
	)
	if err != nil {
		return fmt.Errorf("update user password: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user password rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("update user password: %w", ErrNotFound)
	}
	return nil
}

// ListUsersPage returns a filtered, paginated page of users plus the total
// count of rows matching the filter (ignoring limit/offset). q is a
// case-insensitive email substring ("" matches all). limit<=0 defaults to 50
// and is capped at 200; offset<0 is treated as 0. Never returns password_hash.
func (x *DB) ListUsersPage(ctx context.Context, q string, limit, offset int) ([]User, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	like := "%" + q + "%"

	var total int
	if err := x.sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE email LIKE ? ESCAPE '\'`, like,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, email, role, status, last_login, created_at
		   FROM users
		  WHERE email LIKE ? ESCAPE '\'
		  ORDER BY created_at
		  LIMIT ? OFFSET ?`,
		like, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	out := make([]User, 0, limit)
	for rows.Next() {
		var u User
		var lastLogin sql.NullTime
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.Status, &lastLogin, &u.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		if lastLogin.Valid {
			u.LastLogin = &lastLogin.Time
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list users rows: %w", err)
	}
	return out, total, nil
}

// UpdateUserRole sets a user's role. Returns ErrNotFound if no row matched.
func (x *DB) UpdateUserRole(ctx context.Context, id, role string) error {
	return x.execAffectOne(ctx, "update user role",
		`UPDATE users SET role=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, role, id)
}

// UpdateUserStatus sets a user's status. Returns ErrNotFound if no row matched.
func (x *DB) UpdateUserStatus(ctx context.Context, id, status string) error {
	return x.execAffectOne(ctx, "update user status",
		`UPDATE users SET status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, id)
}

// TouchUserLastLogin sets last_login to the current timestamp (best-effort
// semantics for callers, but surfaces DB errors).
func (x *DB) TouchUserLastLogin(ctx context.Context, id string) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`UPDATE users SET last_login=CURRENT_TIMESTAMP WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("touch user last_login: %w", err)
	}
	return nil
}

// execAffectOne runs a statement and maps "0 rows affected" to ErrNotFound.
func (x *DB) execAffectOne(ctx context.Context, what, query string, args ...any) error {
	res, err := x.sqlDB.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", what, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes the user with the given ID.
// ON DELETE CASCADE removes all associated sessions, tokens, and tunnels.
// Returns ErrNotFound if no row matched. A user who owns the service behind a
// provider that a synthetic model targets stays: the error wraps
// ErrProviderInUse and names the models.
func (x *DB) DeleteUser(ctx context.Context, id string) error {
	res, err := x.sqlDB.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return x.providerInUseOr(ctx, fmt.Errorf("delete user: %w", err), `WHERE s.user_id=?`, id)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete user rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete user: %w", ErrNotFound)
	}
	return nil
}

// CreateUser inserts a new user row. Returns an error if the email already exists.
func (x *DB) CreateUser(ctx context.Context, u User) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO users(id, email, password_hash, role) VALUES(?,?,?,?)`,
		u.ID, u.Email, u.PasswordHash, u.Role,
	)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// GetUserByEmail returns the user with the given email, or ErrNotFound.
func (x *DB) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return x.scanUser(x.sqlDB.QueryRowContext(ctx,
		`SELECT id, email, password_hash, role, status, last_login, created_at, updated_at FROM users WHERE email=?`, email,
	))
}

// GetUserByID returns the user with the given ID, or ErrNotFound.
func (x *DB) GetUserByID(ctx context.Context, id string) (User, error) {
	return x.scanUser(x.sqlDB.QueryRowContext(ctx,
		`SELECT id, email, password_hash, role, status, last_login, created_at, updated_at FROM users WHERE id=?`, id,
	))
}

// scanUser scans a single user row from a QueryRow result.
func (x *DB) scanUser(row *sql.Row) (User, error) {
	var u User
	var lastLogin sql.NullTime
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Status, &lastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	if lastLogin.Valid {
		u.LastLogin = &lastLogin.Time
	}
	return u, nil
}
