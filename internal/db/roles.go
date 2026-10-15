package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrRoleBuiltin is returned by UpdateRole / DeleteRole when the target role
// is one of the built-in (admin/user) rows. Handlers map this to HTTP 409.
var ErrRoleBuiltin = errors.New("db: role is built-in")

// ErrRoleExists is returned by CreateRole when a row with the same primary
// key (name) already exists. Handlers map this to HTTP 409.
var ErrRoleExists = errors.New("db: role already exists")

// ListRoles returns all roles ordered by name, including the v0.4.0 columns
// (builtin, permissions JSON, default_for_new_users) added by migration 0005.
func (x *DB) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT name, description, created_at, builtin, permissions, default_for_new_users
		   FROM roles
		  ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		r, err := scanRoleRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list roles rows: %w", err)
	}
	return out, nil
}

// GetRole returns the role with the given name, or ErrNotFound. Reads the
// v0.4.0 columns (builtin, permissions JSON, default_for_new_users).
func (x *DB) GetRole(ctx context.Context, name string) (Role, error) {
	r, err := scanRoleRow(x.sqlDB.QueryRowContext(ctx,
		`SELECT name, description, created_at, builtin, permissions, default_for_new_users
		   FROM roles WHERE name=?`, name,
	).Scan)
	if err == sql.ErrNoRows {
		return Role{}, ErrNotFound
	}
	if err != nil {
		return Role{}, err
	}
	return r, nil
}

// scanRoleRow accepts a Scan function and assembles a Role from the
// migration-0005 column order:
//
//	name, description, created_at, builtin, permissions, default_for_new_users
//
// builtin and default_for_new_users are INTEGER 0/1 in SQLite and BOOLEAN in
// Postgres; both scan into bool. permissions is a TEXT JSON array of
// permission key strings. An empty or invalid JSON value yields an empty
// slice — never an error — so a freshly migrated row (default '[]')
// round-trips cleanly.
func scanRoleRow(scan func(dest ...any) error) (Role, error) {
	var (
		r         Role
		builtin   bool
		permsRaw  string
		isDefault bool
	)
	if err := scan(&r.Name, &r.Description, &r.CreatedAt, &builtin, &permsRaw, &isDefault); err != nil {
		if err == sql.ErrNoRows {
			return Role{}, err
		}
		return Role{}, fmt.Errorf("scan role: %w", err)
	}
	r.Builtin = builtin
	r.DefaultForNewUsers = isDefault
	if permsRaw != "" {
		_ = json.Unmarshal([]byte(permsRaw), &r.Permissions)
	}
	if r.Permissions == nil {
		r.Permissions = []string{}
	}
	return r, nil
}

// CreateRole inserts a new non-builtin role. permissions is encoded to JSON.
// If defaultForNewUsers is true the prior default is cleared in the same
// transaction so exactly one role is the default at any moment. Returns
// ErrRoleExists on a UNIQUE-violation (name primary key).
func (x *DB) CreateRole(ctx context.Context, name, description string, permissions []string, defaultForNewUsers bool) error {
	if permissions == nil {
		permissions = []string{}
	}
	permsJSON, err := json.Marshal(permissions)
	if err != nil {
		return fmt.Errorf("encode permissions: %w", err)
	}
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort cleanup
	if defaultForNewUsers {
		if _, err := tx.ExecContext(ctx,
			`UPDATE roles SET default_for_new_users=false WHERE default_for_new_users=true`,
		); err != nil {
			return fmt.Errorf("clear prior default: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO roles(name, description, builtin, permissions, default_for_new_users)
		 VALUES(?,?,false,?,?)`,
		name, description, string(permsJSON), defaultForNewUsers,
	); err != nil {
		if isUniqueViolation(err) {
			return ErrRoleExists
		}
		return fmt.Errorf("insert role: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create role: %w", err)
	}
	return nil
}

// RoleUpdate carries the optional fields of UpdateRole. A nil pointer means
// "leave this column unchanged"; an explicit empty slice (non-nil) for
// Permissions persists an empty perm set (rare but legal).
type RoleUpdate struct {
	Description        *string
	Permissions        *[]string
	DefaultForNewUsers *bool
}

// UpdateRole patches a non-builtin role. Refuses to touch a builtin row
// (ErrRoleBuiltin → 409). If u.DefaultForNewUsers is true the prior default
// is cleared in the same transaction. Returns ErrNotFound when no row matches.
func (x *DB) UpdateRole(ctx context.Context, name string, u RoleUpdate) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var builtin bool
	if err := tx.QueryRowContext(ctx,
		`SELECT builtin FROM roles WHERE name=?`, name,
	).Scan(&builtin); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("lookup role: %w", err)
	}
	if builtin {
		return ErrRoleBuiltin
	}

	// Optional default-for-new-users swap (single tx). When transitioning
	// from false→true we first clear any prior default; when explicitly
	// setting false we simply persist false on this row and leave any other
	// default alone.
	if u.DefaultForNewUsers != nil && *u.DefaultForNewUsers {
		if _, err := tx.ExecContext(ctx,
			`UPDATE roles SET default_for_new_users=false WHERE default_for_new_users=true`,
		); err != nil {
			return fmt.Errorf("clear prior default: %w", err)
		}
	}

	// Build the SET clause from the supplied optional fields. We always run
	// at least one UPDATE so the row's row-version semantics are exercised.
	set := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if u.Description != nil {
		set = append(set, "description=?")
		args = append(args, *u.Description)
	}
	if u.Permissions != nil {
		perms := *u.Permissions
		if perms == nil {
			perms = []string{}
		}
		permsJSON, err := json.Marshal(perms)
		if err != nil {
			return fmt.Errorf("encode permissions: %w", err)
		}
		set = append(set, "permissions=?")
		args = append(args, string(permsJSON))
	}
	if u.DefaultForNewUsers != nil {
		set = append(set, "default_for_new_users=?")
		args = append(args, *u.DefaultForNewUsers)
	}
	if len(set) > 0 {
		args = append(args, name)
		q := "UPDATE roles SET " + joinComma(set) + " WHERE name=?"
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("update role: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit update role: %w", err)
	}
	return nil
}

// joinComma is a tiny helper so this file doesn't import strings just for
// the SET clause assembly.
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// DeleteRole removes a non-builtin role in one transaction. Users whose
// role matches the deleted name are re-assigned to fallbackRole (the current
// default-for-new-users row, resolved by the caller). The list of affected
// user IDs is returned so the caller (handler / store) can emit one audit
// event per re-assigned user. Refuses to touch a builtin row.
func (x *DB) DeleteRole(ctx context.Context, name, fallbackRole string) (affected []string, err error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var builtin bool
	if err := tx.QueryRowContext(ctx,
		`SELECT builtin FROM roles WHERE name=?`, name,
	).Scan(&builtin); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lookup role: %w", err)
	}
	if builtin {
		return nil, ErrRoleBuiltin
	}

	// Collect the affected users BEFORE the UPDATE so we can hand the
	// list back to the caller. The query is harmless when no rows match.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM users WHERE role=?`, name)
	if err != nil {
		return nil, fmt.Errorf("list affected users: %w", err)
	}
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan affected user: %w", scanErr)
		}
		affected = append(affected, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list affected users rows: %w", err)
	}

	if len(affected) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET role=?, updated_at=CURRENT_TIMESTAMP WHERE role=?`,
			fallbackRole, name,
		); err != nil {
			return nil, fmt.Errorf("reassign users: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM roles WHERE name=?`, name); err != nil {
		return nil, fmt.Errorf("delete role: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delete role: %w", err)
	}
	return affected, nil
}

// DefaultRoleName returns the name of the role whose default_for_new_users
// flag is set. ErrNotFound when no row is marked default — this is a hard
// invariant the store must enforce (always exactly one default at any time,
// established by migration 0005 + the single-tx swaps in CreateRole /
// UpdateRole).
func (x *DB) DefaultRoleName(ctx context.Context) (string, error) {
	var name string
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT name FROM roles WHERE default_for_new_users=true LIMIT 1`,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("default role: %w", err)
	}
	return name, nil
}
