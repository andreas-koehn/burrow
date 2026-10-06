package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
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

// clientLoginUnknownSource is the source of every caller whose address is
// not an address.
const clientLoginUnknownSource = "unknown"

// ClientLoginSourceKey says which source a caller's address counts as for
// the cap of one source. An IPv4 address is itself, also when it is written
// as an IPv6 one (::ffff:203.0.113.7). An IPv6 address is its /64: that is
// what one host or one home network is handed, so counting single addresses
// would let one machine take every place by changing its address. Text that
// is not an address is one source for all such callers ("unknown"): it can
// come from a forwarded header, and a caller who writes a new text each time
// must not get a new cap each time.
func ClientLoginSourceKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return clientLoginUnknownSource
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return p.String()
}

// InsertClientLogin stores a new pending request. inserted is false when
// maxPending live pending requests exist already, or maxPerIP of them came
// from the source of r.SourceIP (ClientLoginSourceKey: the address, its /64
// for IPv6, or one shared source for what is not an address): one source
// cannot use up the places of everybody else.
// r.SourceIP itself is stored as given.
//
// The count of the source and the insert with the count of all are two
// statements of one transaction, and no other start runs between them. On
// SQLite the pool holds one connection and the transaction keeps it. On
// Postgres the transaction first takes an advisory lock that makes starts
// wait for each other, and reads at READ COMMITTED, so that each statement
// sees what the starts before it committed (lockClientLoginStart). The cap of
// all is still part of the insert statement. r.CreatedAt is the time the
// request starts at.
func (x *DB) InsertClientLogin(ctx context.Context, r ClientLoginRequest, maxPending, maxPerIP int) (inserted bool, err error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("insert client login request: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockClientLoginStart(ctx, x.sqlDB, tx); err != nil {
		return false, fmt.Errorf("insert client login request: lock: %w", err)
	}
	created := r.CreatedAt.UTC()
	// At most maxPending rows are pending, so this reads a handful of rows.
	rows, err := tx.QueryContext(ctx,
		`SELECT source_ip FROM client_login_requests WHERE status='pending' AND expires_at > ?`, created)
	if err != nil {
		return false, fmt.Errorf("insert client login request: count source: %w", err)
	}
	key, fromSource := ClientLoginSourceKey(r.SourceIP), 0
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("insert client login request: count source: %w", err)
		}
		if ClientLoginSourceKey(ip) == key {
			fromSource++
		}
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("insert client login request: count source: %w", err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("insert client login request: count source: %w", err)
	}
	if fromSource >= maxPerIP {
		return false, nil
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO client_login_requests(device_code_hash, user_code, hostname, os, arch, client_version,
		   source_ip, status, token_name, created_at, expires_at)
		 SELECT ?,?,?,?,?,?,?,'pending',?,?,?
		 WHERE (SELECT COUNT(*) FROM client_login_requests
		         WHERE status='pending' AND expires_at > ?) < ?`,
		r.DeviceCodeHash, r.UserCode, r.Hostname, r.OS, r.Arch, r.ClientVersion,
		r.SourceIP, r.TokenName, created, r.ExpiresAt.UTC(),
		created, maxPending,
	)
	if err != nil {
		return false, fmt.Errorf("insert client login request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert client login request rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("insert client login request: commit: %w", err)
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
