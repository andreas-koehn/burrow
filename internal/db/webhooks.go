package db

// webhooks.go — typed CRUD over the webhooks + webhook_deliveries tables
// (migration 0008). The webhook delivery dispatcher (internal/webhook) is
// the only writer of webhook_deliveries; the JSON handlers in internal/api
// read both tables.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateWebhook inserts a new webhooks row. The caller pre-computes the
// secret hash (sha256 of plaintext) and supplies a non-empty events JSON
// array (the closed event-vocabulary check lives in internal/webhook /
// internal/api). created_at is populated by the SQLite default.
func (x *DB) CreateWebhook(ctx context.Context, w Webhook) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO webhooks(id, name, url, secret_hash, events, paused)
		 VALUES(?,?,?,?,?,?)`,
		w.ID, w.Name, w.URL, w.SecretHash, w.Events, boolToInt(w.Paused),
	)
	if err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}
	return nil
}

// GetWebhook returns the row with the given id, or ErrNotFound.
func (x *DB) GetWebhook(ctx context.Context, id string) (Webhook, error) {
	var w Webhook
	var paused int
	var firstFail sql.NullTime
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT id, name, url, secret_hash, events, paused,
		        consecutive_failures, first_failure_at, created_at
		   FROM webhooks WHERE id=?`,
		id,
	).Scan(&w.ID, &w.Name, &w.URL, &w.SecretHash, &w.Events, &paused,
		&w.ConsecutiveFailures, &firstFail, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("get webhook: %w", err)
	}
	w.Paused = paused != 0
	if firstFail.Valid {
		t := firstFail.Time
		w.FirstFailureAt = &t
	}
	return w, nil
}

// ListWebhooks returns every webhooks row ordered by created_at ascending.
// The returned slice is always non-nil (possibly empty).
func (x *DB) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, name, url, secret_hash, events, paused,
		        consecutive_failures, first_failure_at, created_at
		   FROM webhooks ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	defer rows.Close()
	out := make([]Webhook, 0)
	for rows.Next() {
		var w Webhook
		var paused int
		var firstFail sql.NullTime
		if err := rows.Scan(&w.ID, &w.Name, &w.URL, &w.SecretHash, &w.Events,
			&paused, &w.ConsecutiveFailures, &firstFail, &w.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan webhook: %w", err)
		}
		w.Paused = paused != 0
		if firstFail.Valid {
			t := firstFail.Time
			w.FirstFailureAt = &t
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webhooks rows: %w", err)
	}
	return out, nil
}

// UpdateWebhook replaces the mutable fields (name, url, events, paused)
// on the row with the given id. The secret_hash and counters are NOT
// touched here — secret rotation goes through a dedicated path (rotate
// = delete + create; the spec exposes secrets exactly once). Returns
// ErrNotFound when no row matches.
func (x *DB) UpdateWebhook(ctx context.Context, w Webhook) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE webhooks SET name=?, url=?, events=?, paused=? WHERE id=?`,
		w.Name, w.URL, w.Events, boolToInt(w.Paused), w.ID,
	)
	if err != nil {
		return fmt.Errorf("update webhook: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update webhook rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWebhook removes the row with the given id. ON DELETE CASCADE
// removes associated webhook_deliveries rows. Returns ErrNotFound when
// no row matches.
func (x *DB) DeleteWebhook(ctx context.Context, id string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM webhooks WHERE id=?`, id,
	)
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete webhook rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetWebhookPaused updates only the paused flag on the row with the given
// id. Used by the dispatcher's auto-pause logic and by the JSON pause/
// resume handlers. Returns ErrNotFound when no row matches.
func (x *DB) SetWebhookPaused(ctx context.Context, id string, paused bool) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE webhooks SET paused=? WHERE id=?`, boolToInt(paused), id,
	)
	if err != nil {
		return fmt.Errorf("set webhook paused: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set webhook paused rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetWebhookFailureCounters writes the dispatcher's accounting for the
// consecutive failure streak. Callers pass count=0 + firstFailureAt=nil
// to clear the streak (success after retries, or operator resume).
// Returns ErrNotFound when no row matches.
func (x *DB) SetWebhookFailureCounters(ctx context.Context, id string, count int, firstFailureAt *time.Time) error {
	var fa any
	if firstFailureAt != nil {
		fa = firstFailureAt.UTC()
	}
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE webhooks
		    SET consecutive_failures=?, first_failure_at=?
		  WHERE id=?`,
		count, fa, id,
	)
	if err != nil {
		return fmt.Errorf("set webhook failure counters: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set webhook failure counters rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- webhook_deliveries ------------------------------------------------------

// InsertWebhookDelivery appends one webhook_deliveries row. The dispatcher
// inserts one row per attempt (so a single Publish + 3 retries creates up to
// 3 rows under the same webhook_id with monotonically increasing attempt).
func (x *DB) InsertWebhookDelivery(ctx context.Context, d WebhookDelivery) error {
	var req, resp any
	if d.RequestPreview != nil {
		req = *d.RequestPreview
	}
	if d.ResponsePreview != nil {
		resp = *d.ResponsePreview
	}
	var ts any
	if !d.Ts.IsZero() {
		ts = d.Ts.UTC()
	} else {
		ts = time.Now().UTC()
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO webhook_deliveries(
			id, webhook_id, event, ts, status_code, attempt, latency_ms,
			request_preview, response_preview
		) VALUES(?,?,?,?,?,?,?,?,?)`,
		d.ID, d.WebhookID, d.Event, ts, d.StatusCode, d.Attempt, d.LatencyMs,
		req, resp,
	)
	if err != nil {
		return fmt.Errorf("insert webhook delivery: %w", err)
	}
	return nil
}

// WebhookDeliveryQuery is the filter shape for ListWebhookDeliveries.
type WebhookDeliveryQuery struct {
	WebhookID string // optional exact match
	Limit     int    // 0 → default 100, capped at 1000
}

// ListWebhookDeliveries returns the most recent webhook_deliveries rows
// ordered by ts DESC, optionally scoped to one webhook_id. Limit 0 maps
// to 100; limit > 1000 is capped at 1000 to bound the response.
func (x *DB) ListWebhookDeliveries(ctx context.Context, q WebhookDeliveryQuery) ([]WebhookDelivery, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var rows *sql.Rows
	var err error
	if q.WebhookID != "" {
		rows, err = x.sqlDB.QueryContext(ctx,
			`SELECT id, webhook_id, event, ts, status_code, attempt, latency_ms,
			        request_preview, response_preview
			   FROM webhook_deliveries WHERE webhook_id=? ORDER BY ts DESC LIMIT ?`,
			q.WebhookID, limit,
		)
	} else {
		rows, err = x.sqlDB.QueryContext(ctx,
			`SELECT id, webhook_id, event, ts, status_code, attempt, latency_ms,
			        request_preview, response_preview
			   FROM webhook_deliveries ORDER BY ts DESC LIMIT ?`,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list webhook deliveries: %w", err)
	}
	defer rows.Close()
	out := make([]WebhookDelivery, 0)
	for rows.Next() {
		var d WebhookDelivery
		var req, resp sql.NullString
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.Event, &d.Ts, &d.StatusCode,
			&d.Attempt, &d.LatencyMs, &req, &resp); err != nil {
			return nil, fmt.Errorf("scan webhook delivery: %w", err)
		}
		if req.Valid {
			s := req.String
			d.RequestPreview = &s
		}
		if resp.Valid {
			s := resp.String
			d.ResponsePreview = &s
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webhook deliveries rows: %w", err)
	}
	return out, nil
}

// CountConsecutive4xxSince returns the number of 4xx webhook_deliveries rows
// for the given webhook_id with ts >= since. Used by the dispatcher's
// auto-pause heuristic (10 consecutive 4xx in 1h → pause).
//
// "Consecutive" is bounded by the time window: any 2xx delivery after `since`
// implicitly clears the streak because the dispatcher already resets
// consecutive_failures to 0 on a 2xx. The 1h time window is the
// auto-pause guardrail per spec; this function only counts rows.
func (x *DB) CountConsecutive4xxSince(ctx context.Context, webhookID string, since time.Time) (int, error) {
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhook_deliveries
		  WHERE webhook_id=? AND status_code >= 400 AND status_code < 500
		    AND ts >= ?`,
		webhookID, since.UTC(),
	)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count 4xx webhook deliveries: %w", err)
	}
	return n, nil
}

// boolToInt converts a Go bool into the 0/1 integer of an INTEGER column
// (pgx will not bind a Go bool to an int column).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// intBool scans an INTEGER NOT NULL 0/1 column into a bool; both drivers hand
// such a column over as int64 (pgx will not scan an int4 into a *bool).
type intBool bool

func (b *intBool) Scan(src any) error {
	v, ok := src.(int64)
	if !ok {
		return fmt.Errorf("intBool: unsupported type %T", src)
	}
	*b = v != 0
	return nil
}
