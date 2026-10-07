package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AuditEventInsert is the input shape for InsertAuditEvent. The caller
// (internal/audit.Logger.Append) computes id, ts, prev_hash and hash before
// passing them in — this layer just executes the row insert.
type AuditEventInsert struct {
	ID, ActorID, ActorEmail, Action, SubjectID, SubjectLabel,
	Result, SourceIP, UserAgent, RequestID, Payload, PrevHash, Hash string
	Ts time.Time
}

// InsertAuditEvent appends one audit_events row inside an existing
// transaction. The caller (audit.Logger) holds the tx so the chain head
// read + this insert are atomic — without that, two concurrent appenders
// could both read the same prev_hash and produce a forked chain.
func (x *DB) InsertAuditEvent(ctx context.Context, tx *sql.Tx, e AuditEventInsert) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit_events(
			id, ts, actor_id, actor_email, action,
			subject_id, subject_label, result,
			source_ip, user_agent, request_id,
			payload, prev_hash, hash
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Ts.UTC(), e.ActorID, e.ActorEmail, e.Action,
		e.SubjectID, e.SubjectLabel, e.Result,
		e.SourceIP, e.UserAgent, e.RequestID,
		e.Payload, e.PrevHash, e.Hash,
	)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

// LatestAuditHash returns the hash of the most recent audit_events row in
// id order, or ("", false) when the table is empty (genesis case).
func (x *DB) LatestAuditHash(ctx context.Context, tx *sql.Tx) (string, bool, error) {
	var h string
	q := `SELECT hash FROM audit_events ORDER BY id DESC LIMIT 1`
	var err error
	if tx != nil {
		err = tx.QueryRowContext(ctx, q).Scan(&h)
	} else {
		err = x.sqlDB.QueryRowContext(ctx, q).Scan(&h)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("latest audit hash: %w", err)
	}
	return h, true, nil
}

// LatestAuditRow returns the id and the hash of the most recent
// audit_events row in id order: the row a new one chains to, and the id a
// new one has to sort after. ok is false when the table is empty.
func (x *DB) LatestAuditRow(ctx context.Context, tx *sql.Tx) (id, hash string, ok bool, err error) {
	q := `SELECT id, hash FROM audit_events ORDER BY id DESC LIMIT 1`
	if tx != nil {
		err = tx.QueryRowContext(ctx, q).Scan(&id, &hash)
	} else {
		err = x.sqlDB.QueryRowContext(ctx, q).Scan(&id, &hash)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("latest audit row: %w", err)
	}
	return id, hash, true, nil
}

// LockAuditChain makes the appends to the audit chain wait for each other
// until tx ends, across every process that shares the database. It must be
// the first statement of tx. See lockAuditChain.
func (x *DB) LockAuditChain(ctx context.Context, tx *sql.Tx) error {
	return lockAuditChain(ctx, x.sqlDB, tx)
}

// AuditQuery is the filter shape consumed by ListAuditEvents.
//
// Limit defaults to 100 (caller-applied); a zero Limit is treated as "no
// page cap" inside this function — handlers always pass an explicit limit.
type AuditQuery struct {
	Since    *time.Time
	Until    *time.Time
	Action   string // exact match
	Actor    string // matches actor_id OR actor_email exact
	Q        string // case-insensitive LIKE %q% on action || subject_label || actor_email
	BeforeID string // pagination cursor — return rows with id < BeforeID
	FromID   string // inclusive lower bound on id (verify/range queries)
	ToID     string // inclusive upper bound on id (verify/range queries)
	Limit    int    // 0 = no limit (use for export); handlers pass an explicit cap
}

// ListAuditEvents returns matching rows in DESCENDING id order (newest
// first) — the natural shape for the UI list and the cursor pagination
// (?before_id=X is "older than X").
//
// Export and Verify also use this with Limit=0 + reverse iteration handled
// by the caller (they want ascending order). See the audit package.
func (x *DB) ListAuditEvents(ctx context.Context, q AuditQuery) ([]AuditEvent, error) {
	var where []string
	var args []any
	if q.Since != nil {
		where = append(where, "ts >= ?")
		args = append(args, q.Since.UTC())
	}
	if q.Until != nil {
		where = append(where, "ts <= ?")
		args = append(args, q.Until.UTC())
	}
	if q.Action != "" {
		where = append(where, "action = ?")
		args = append(args, q.Action)
	}
	if q.Actor != "" {
		where = append(where, "(actor_id = ? OR actor_email = ?)")
		args = append(args, q.Actor, q.Actor)
	}
	if q.Q != "" {
		like := "%" + strings.ToLower(q.Q) + "%"
		where = append(where, "(lower(action) LIKE ? OR lower(subject_label) LIKE ? OR lower(actor_email) LIKE ?)")
		args = append(args, like, like, like)
	}
	if q.BeforeID != "" {
		where = append(where, "id < ?")
		args = append(args, q.BeforeID)
	}
	if q.FromID != "" {
		where = append(where, "id >= ?")
		args = append(args, q.FromID)
	}
	if q.ToID != "" {
		where = append(where, "id <= ?")
		args = append(args, q.ToID)
	}
	sql := `SELECT id, ts, actor_id, actor_email, action,
			subject_id, subject_label, result,
			source_ip, user_agent, request_id,
			payload, prev_hash, hash
		FROM audit_events`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY id DESC"
	if q.Limit > 0 {
		sql += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := x.sqlDB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	out := make([]AuditEvent, 0)
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(
			&e.ID, &e.Ts, &e.ActorID, &e.ActorEmail, &e.Action,
			&e.SubjectID, &e.SubjectLabel, &e.Result,
			&e.SourceIP, &e.UserAgent, &e.RequestID,
			&e.Payload, &e.PrevHash, &e.Hash,
		); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit events rows: %w", err)
	}
	return out, nil
}

// IterAuditEventsAsc walks every audit_events row in ASCENDING id order
// (oldest first) and calls visit for each. The walk respects an optional
// (fromID, toID) inclusive range — both may be empty for "the whole chain".
//
// Verify and ExportNDJSON use this so the chain is replayed in original
// append order without buffering the whole table in memory.
func (x *DB) IterAuditEventsAsc(
	ctx context.Context, fromID, toID string,
	visit func(AuditEvent) error,
) error {
	var where []string
	var args []any
	if fromID != "" {
		where = append(where, "id >= ?")
		args = append(args, fromID)
	}
	if toID != "" {
		where = append(where, "id <= ?")
		args = append(args, toID)
	}
	sql := `SELECT id, ts, actor_id, actor_email, action,
			subject_id, subject_label, result,
			source_ip, user_agent, request_id,
			payload, prev_hash, hash
		FROM audit_events`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY id ASC"
	rows, err := x.sqlDB.QueryContext(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("iter audit events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(
			&e.ID, &e.Ts, &e.ActorID, &e.ActorEmail, &e.Action,
			&e.SubjectID, &e.SubjectLabel, &e.Result,
			&e.SourceIP, &e.UserAgent, &e.RequestID,
			&e.Payload, &e.PrevHash, &e.Hash,
		); err != nil {
			return fmt.Errorf("scan audit event: %w", err)
		}
		if err := visit(e); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iter audit events rows: %w", err)
	}
	return nil
}

// TamperAuditPayload is a TEST-ONLY helper that overwrites an existing
// row's payload column WITHOUT recomputing the hash. The audit verifier's
// tamper-detection test uses this to simulate a malicious in-place edit;
// production code MUST never call it (and the function lives outside any
// test build tag so external callers can wire it from cmd/server tests too,
// but it is reviewed as a deliberate seam).
func (x *DB) TamperAuditPayload(ctx context.Context, id, newPayload string) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`UPDATE audit_events SET payload=? WHERE id=?`, newPayload, id)
	if err != nil {
		return fmt.Errorf("tamper audit payload: %w", err)
	}
	return nil
}
