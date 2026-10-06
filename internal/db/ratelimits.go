package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CRITICAL — the rate_limits table's window column is created via migration
// 0009 as a double-quoted identifier:
//
//	"window" TEXT NOT NULL DEFAULT 'minute'
//
// `window` is a SQLite reserved word, so every read/write of the column MUST
// use the quoted form `"window"`. A bare `window` token fails to parse.

// CreateRateLimit inserts a new rate_limits row. The row's ID is the caller's
// responsibility (typically a uuid); created_at is populated by SQLite's
// CURRENT_TIMESTAMP default.
func (x *DB) CreateRateLimit(ctx context.Context, rl RateLimit) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO rate_limits(id, scope, subject, dimension, lim, burst, "window")
		 VALUES(?,?,?,?,?,?,?)`,
		rl.ID, rl.Scope, rl.Subject, rl.Dimension, rl.Lim, rl.Burst, rl.Window,
	)
	if err != nil {
		return fmt.Errorf("create rate_limit: %w", err)
	}
	return nil
}

// GetRateLimit returns the row with the given id, or ErrNotFound.
func (x *DB) GetRateLimit(ctx context.Context, id string) (RateLimit, error) {
	var rl RateLimit
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT id, scope, subject, dimension, lim, burst, "window", created_at
		 FROM rate_limits WHERE id=?`,
		id,
	).Scan(&rl.ID, &rl.Scope, &rl.Subject, &rl.Dimension, &rl.Lim, &rl.Burst, &rl.Window, &rl.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RateLimit{}, ErrNotFound
	}
	if err != nil {
		return RateLimit{}, fmt.Errorf("get rate_limit: %w", err)
	}
	return rl, nil
}

// ListRateLimits returns every rate_limits row ordered by (scope, subject,
// dimension). The list is always returned as a non-nil slice (possibly empty).
func (x *DB) ListRateLimits(ctx context.Context) ([]RateLimit, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, scope, subject, dimension, lim, burst, "window", created_at
		 FROM rate_limits ORDER BY scope, subject, dimension`,
	)
	if err != nil {
		return nil, fmt.Errorf("list rate_limits: %w", err)
	}
	defer rows.Close()
	out := make([]RateLimit, 0)
	for rows.Next() {
		var rl RateLimit
		if err := rows.Scan(&rl.ID, &rl.Scope, &rl.Subject, &rl.Dimension,
			&rl.Lim, &rl.Burst, &rl.Window, &rl.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan rate_limit: %w", err)
		}
		out = append(out, rl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rate_limits rows: %w", err)
	}
	return out, nil
}

// UpdateRateLimit replaces every mutable column on the row with the given id.
// Returns ErrNotFound when no row matches. The scope/subject/dimension/window
// keys are part of the bucket identity and may all be re-targeted.
func (x *DB) UpdateRateLimit(ctx context.Context, rl RateLimit) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE rate_limits
		   SET scope=?, subject=?, dimension=?, lim=?, burst=?, "window"=?
		 WHERE id=?`,
		rl.Scope, rl.Subject, rl.Dimension, rl.Lim, rl.Burst, rl.Window, rl.ID,
	)
	if err != nil {
		return fmt.Errorf("update rate_limit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update rate_limit rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRateLimit removes the row with the given id. Returns ErrNotFound when
// no row matches.
func (x *DB) DeleteRateLimit(ctx context.Context, id string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM rate_limits WHERE id=?`, id,
	)
	if err != nil {
		return fmt.Errorf("delete rate_limit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete rate_limit rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Day-window quota queries. Each reads the usage_events rows of one subject
// since the start of the current UTC day. The day boundary is computed in Go
// and bound (see UsageWindowStart), so the statements run on SQLite and
// Postgres alike. An empty subject is 0 without a query.
//
// The Sum* queries return the byte estimate (bytes_in+bytes_out)/4, the
// currency of rate limits (spec Part D); the Count* queries return the number
// of requests.

// GatewayKeySubjectPrefix marks a gateway key in a rate-limit subject:
// "gw:<gateway key id>". Such a key's usage rows carry its id in
// gateway_key_id and an empty api_key_id.
const GatewayKeySubjectPrefix = "gw:"

// usageKeyColumn maps a per-key subject to the usage_events column that
// holds it and the id to compare with. The column is one of two constants.
func usageKeyColumn(subject string) (column, id string) {
	if rest, ok := strings.CutPrefix(subject, GatewayKeySubjectPrefix); ok {
		return "gateway_key_id", rest
	}
	return "api_key_id", subject
}

// sumDailyUsageSQL sums the bytes of the rows whose column equals the first
// parameter since the second (a time). Each column it is called with has an
// index (column, ts): service_id, api_key_id, gateway_key_id, requested_model.
func sumDailyUsageSQL(column string) string {
	return `SELECT CAST(COALESCE(SUM(bytes_in), 0) + COALESCE(SUM(bytes_out), 0) AS BIGINT)
		   FROM usage_events
		  WHERE ` + column + ` = ?
		    AND ts >= ?`
}

// sumDailyUsage returns today's byte estimate of the rows whose column equals
// id. column is a constant of this file, never input.
func (x *DB) sumDailyUsage(ctx context.Context, column, id string) (int64, error) {
	if id == "" {
		return 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx, sumDailyUsageSQL(column), id, utcDayStart())
	var totalBytes int64
	if err := row.Scan(&totalBytes); err != nil {
		return 0, fmt.Errorf("sum daily usage by %s: %w", column, err)
	}
	return totalBytes / 4, nil
}

// countDailyUsage returns today's number of rows whose column equals id.
func (x *DB) countDailyUsage(ctx context.Context, column, id string) (int64, error) {
	if id == "" {
		return 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_events
		  WHERE `+column+` = ?
		    AND ts >= ?`,
		id, utcDayStart(),
	)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count daily usage by %s: %w", column, err)
	}
	return n, nil
}

// SumDailyUsageEventsByAPIKey is the byte estimate of a per-key subject: a
// service key id, or "gw:<gateway key id>".
func (x *DB) SumDailyUsageEventsByAPIKey(ctx context.Context, apiKeyID string) (int64, error) {
	column, id := usageKeyColumn(apiKeyID)
	return x.sumDailyUsage(ctx, column, id)
}

// CountDailyUsageEventsByAPIKey is the request count of a per-key subject.
func (x *DB) CountDailyUsageEventsByAPIKey(ctx context.Context, apiKeyID string) (int64, error) {
	column, id := usageKeyColumn(apiKeyID)
	return x.countDailyUsage(ctx, column, id)
}

// SumDailyUsageEventsByService is the byte estimate of a service.
func (x *DB) SumDailyUsageEventsByService(ctx context.Context, serviceID string) (int64, error) {
	return x.sumDailyUsage(ctx, "service_id", serviceID)
}

// CountDailyUsageEventsByService is the request count of a service.
func (x *DB) CountDailyUsageEventsByService(ctx context.Context, serviceID string) (int64, error) {
	return x.countDailyUsage(ctx, "service_id", serviceID)
}

// SumDailyUsageEventsByGatewayKey is the byte estimate of a gateway key,
// named by its bare id.
func (x *DB) SumDailyUsageEventsByGatewayKey(ctx context.Context, gatewayKeyID string) (int64, error) {
	return x.sumDailyUsage(ctx, "gateway_key_id", gatewayKeyID)
}

// CountDailyUsageEventsByGatewayKey is the request count of a gateway key.
func (x *DB) CountDailyUsageEventsByGatewayKey(ctx context.Context, gatewayKeyID string) (int64, error) {
	return x.countDailyUsage(ctx, "gateway_key_id", gatewayKeyID)
}

// SumDailyUsageEventsByModel is the byte estimate of the requests that asked
// for the model name (usage_events.requested_model), whoever answered.
func (x *DB) SumDailyUsageEventsByModel(ctx context.Context, model string) (int64, error) {
	return x.sumDailyUsage(ctx, "requested_model", model)
}

// CountDailyUsageEventsByModel is the request count of a requested model.
func (x *DB) CountDailyUsageEventsByModel(ctx context.Context, model string) (int64, error) {
	return x.countDailyUsage(ctx, "requested_model", model)
}
