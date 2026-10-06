package db

import (
	"context"
	"fmt"
	"time"
)

// UsageAttempt is one upstream attempt of a gateway request.
type UsageAttempt struct {
	RequestID    string
	Position     int
	Ts           time.Time
	ProviderSlug string
	TargetModel  string
	Status       int    // upstream HTTP status, 0 when none was received
	ErrorCode    string // "" on success
	DurationMs   int64
}

// InsertUsageAttempts writes the attempts in one transaction; a no-op for an
// empty slice. A zero Ts is stored as now.
func (x *DB) InsertUsageAttempts(ctx context.Context, attempts []UsageAttempt) error {
	if len(attempts) == 0 {
		return nil
	}
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage attempts tx: %w", err)
	}
	for _, a := range attempts {
		ts := a.Ts
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO usage_attempts(request_id, position, ts, provider_slug, target_model, status, error_code, duration_ms)
			 VALUES(?,?,?,?,?,?,?,?)`,
			a.RequestID, a.Position, ts, a.ProviderSlug, a.TargetModel, a.Status, a.ErrorCode, a.DurationMs); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert usage attempt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage attempts tx: %w", err)
	}
	return nil
}

// ListUsageAttempts returns the request's attempts by position (never nil).
func (x *DB) ListUsageAttempts(ctx context.Context, requestID string) ([]UsageAttempt, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT request_id, position, ts, provider_slug, target_model, status, error_code, duration_ms
		   FROM usage_attempts WHERE request_id=? ORDER BY position`, requestID)
	if err != nil {
		return nil, fmt.Errorf("list usage attempts: %w", err)
	}
	defer rows.Close()
	out := make([]UsageAttempt, 0)
	for rows.Next() {
		var a UsageAttempt
		if err := rows.Scan(&a.RequestID, &a.Position, &a.Ts, &a.ProviderSlug, &a.TargetModel, &a.Status, &a.ErrorCode, &a.DurationMs); err != nil {
			return nil, fmt.Errorf("list usage attempts: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
