package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Statuses of a client sign-in request.
const (
	ClientLoginPending  = "pending"
	ClientLoginApproved = "approved"
	ClientLoginDenied   = "denied"
)

// ClientLoginRequest is a row of client_login_requests: a client that asked
// to be signed in and waits for a dashboard user to approve it. The row holds
// the hash of the device code only, and never a client token.
type ClientLoginRequest struct {
	DeviceCodeHash string
	UserCode       string
	Hostname       string
	OS             string
	Arch           string
	ClientVersion  string
	SourceIP       string
	Status         string
	ApprovedBy     string // empty until approved
	TokenName      string // the client's suggestion while pending, the approver's choice afterwards
	LastPollAt     *time.Time
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

const clientLoginCols = `device_code_hash, user_code, hostname, os, arch, client_version, source_ip,
	status, COALESCE(approved_by,''), token_name, last_poll_at, created_at, expires_at`

func scanClientLogin(row *sql.Row) (ClientLoginRequest, error) {
	var r ClientLoginRequest
	var lastPoll sql.NullTime
	err := row.Scan(&r.DeviceCodeHash, &r.UserCode, &r.Hostname, &r.OS, &r.Arch, &r.ClientVersion, &r.SourceIP,
		&r.Status, &r.ApprovedBy, &r.TokenName, &lastPoll, &r.CreatedAt, &r.ExpiresAt)
	if err == sql.ErrNoRows {
		return ClientLoginRequest{}, ErrNotFound
	}
	if err != nil {
		return ClientLoginRequest{}, fmt.Errorf("scan client login request: %w", err)
	}
	if lastPoll.Valid {
		r.LastPollAt = &lastPoll.Time
	}
	return r, nil
}

// InsertClientLogin stores a new pending request unless maxPending live
// pending requests exist already; inserted is false then. The count and the
// insert are one statement, so the cap cannot be passed between a check and
// a write. r.CreatedAt is the time the request starts at.
func (x *DB) InsertClientLogin(ctx context.Context, r ClientLoginRequest, maxPending int) (inserted bool, err error) {
	created := r.CreatedAt.UTC()
	res, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO client_login_requests(device_code_hash, user_code, hostname, os, arch, client_version,
		   source_ip, status, token_name, created_at, expires_at)
		 SELECT ?,?,?,?,?,?,?,'pending',?,?,?
		 WHERE (SELECT COUNT(*) FROM client_login_requests WHERE status='pending' AND expires_at > ?) < ?`,
		r.DeviceCodeHash, r.UserCode, r.Hostname, r.OS, r.Arch, r.ClientVersion,
		r.SourceIP, r.TokenName, created, r.ExpiresAt.UTC(), created, maxPending,
	)
	if err != nil {
		return false, fmt.Errorf("insert client login request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert client login request rows affected: %w", err)
	}
	return n == 1, nil
}

// IsClientLoginDuplicate reports whether err is the unique-constraint error
// of InsertClientLogin: the user code (or the device code hash) is taken.
func IsClientLoginDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "23505")
}

// GetClientLoginByUserCode returns the request with this user code if it has
// not expired at now; otherwise ErrNotFound.
func (x *DB) GetClientLoginByUserCode(ctx context.Context, userCode string, now time.Time) (ClientLoginRequest, error) {
	return scanClientLogin(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+clientLoginCols+` FROM client_login_requests WHERE user_code=? AND expires_at > ?`,
		userCode, now.UTC()))
}

// GetClientLoginByDeviceHash returns the request with this device code hash,
// expired or not, or ErrNotFound.
func (x *DB) GetClientLoginByDeviceHash(ctx context.Context, hash string) (ClientLoginRequest, error) {
	return scanClientLogin(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+clientLoginCols+` FROM client_login_requests WHERE device_code_hash=?`, hash))
}

// DecideClientLogin moves a pending, unexpired request to status (approved or
// denied). decided is false when no such request exists: the state check is
// part of the statement, so a request is decided at most once.
// approvedBy is stored for an approval and must be empty for a denial;
// tokenName replaces the client's suggestion when it is not empty.
func (x *DB) DecideClientLogin(ctx context.Context, userCode, status, approvedBy, tokenName string, now time.Time) (decided bool, err error) {
	if status != ClientLoginApproved && status != ClientLoginDenied {
		return false, fmt.Errorf("decide client login request: unknown status %q", status)
	}
	var by any
	if approvedBy != "" {
		by = approvedBy
	}
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE client_login_requests
		    SET status=?, approved_by=?, token_name=CASE WHEN ?='' THEN token_name ELSE ? END
		  WHERE user_code=? AND status='pending' AND expires_at > ?`,
		status, by, tokenName, tokenName, userCode, now.UTC())
	if err != nil {
		return false, fmt.Errorf("decide client login request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("decide client login request rows affected: %w", err)
	}
	return n == 1, nil
}

// TouchClientLoginPoll records a poll at now unless the request was polled
// after notAfter; allowed is false then (or when the row is gone). One
// statement, so two polls at the same moment cannot both pass.
func (x *DB) TouchClientLoginPoll(ctx context.Context, hash string, now, notAfter time.Time) (allowed bool, err error) {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE client_login_requests SET last_poll_at=?
		  WHERE device_code_hash=? AND (last_poll_at IS NULL OR last_poll_at <= ?)`,
		now.UTC(), hash, notAfter.UTC())
	if err != nil {
		return false, fmt.Errorf("touch client login poll: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("touch client login poll rows affected: %w", err)
	}
	return n == 1, nil
}

// DeleteClientLogin removes the request with this device code hash.
func (x *DB) DeleteClientLogin(ctx context.Context, hash string) error {
	if _, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM client_login_requests WHERE device_code_hash=?`, hash); err != nil {
		return fmt.Errorf("delete client login request: %w", err)
	}
	return nil
}

// CollectClientLogin hands an approved request over: in one transaction it
// deletes the request and inserts the client token ct for the approver.
//
// The delete carries the state check (approved, approved by ct.UserID, not
// expired at now) and the transaction goes on only when it removed exactly
// one row. Of any number of concurrent calls for one request, one deletes the
// row and inserts its token; the others find nothing to delete and insert
// nothing. collected is false for them, and for a request that is not
// approved, has expired or is gone.
func (x *DB) CollectClientLogin(ctx context.Context, hash string, now time.Time, ct ClientToken) (collected bool, err error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("collect client login: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM client_login_requests
		  WHERE device_code_hash=? AND status='approved' AND approved_by=? AND expires_at > ?`,
		hash, ct.UserID, now.UTC())
	if err != nil {
		return false, fmt.Errorf("collect client login: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("collect client login: rows affected: %w", err)
	}
	if n != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO client_tokens(id, user_id, name, token_hash) VALUES(?,?,?,?)`,
		ct.ID, ct.UserID, ct.Name, ct.TokenHash); err != nil {
		return false, fmt.Errorf("collect client login: insert token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("collect client login: commit: %w", err)
	}
	return true, nil
}

// DeleteExpiredClientLogins removes every request that has expired at now,
// whatever its status, and reports how many.
func (x *DB) DeleteExpiredClientLogins(ctx context.Context, now time.Time) (int, error) {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM client_login_requests WHERE expires_at <= ?`, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired client login requests: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired client login requests rows affected: %w", err)
	}
	return int(n), nil
}
