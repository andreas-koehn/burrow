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

// SumDailyUsageEvents returns the sum of (bytes_in+bytes_out)/4 over the
// usage_events rows for the given subject since the start of the current UTC
// day. The byte-estimate currency is bytes/4 per spec Part D (rate-limit
// currency = byte-estimate per minute, same shape applies to day windows).
//
// Subjects of type api_key match api_key_id; service matches service_id;
// other scopes (role, global) return 0 because usage_events does not record
// a role label and a global aggregate is the caller's job. This is the path
// used by the quota engine's window=day check.
func (x *DB) SumDailyUsageEventsByAPIKey(ctx context.Context, apiKeyID string) (int64, error) {
	column, id := usageKeyColumn(apiKeyID)
	if id == "" {
		return 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes_in)+SUM(bytes_out), 0)
		   FROM usage_events
		  WHERE `+column+` = ?
		    AND ts >= datetime('now', 'start of day')`,
		id,
	)
	var totalBytes int64
	if err := row.Scan(&totalBytes); err != nil {
		return 0, fmt.Errorf("sum daily usage by api_key: %w", err)
	}
	return totalBytes / 4, nil
}

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

// SumDailyUsageEventsByService is the service-scope variant. See
// SumDailyUsageEventsByAPIKey.
func (x *DB) SumDailyUsageEventsByService(ctx context.Context, serviceID string) (int64, error) {
	if serviceID == "" {
		return 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes_in)+SUM(bytes_out), 0)
		   FROM usage_events
		  WHERE service_id = ?
		    AND ts >= datetime('now', 'start of day')`,
		serviceID,
	)
	var totalBytes int64
	if err := row.Scan(&totalBytes); err != nil {
		return 0, fmt.Errorf("sum daily usage by service: %w", err)
	}
	return totalBytes / 4, nil
}

// CountDailyUsageEventsByAPIKey returns the number of usage_events rows for
// the given api_key since UTC midnight. Used by quota window=day +
// dimension=rpm (request-count daily cap).
func (x *DB) CountDailyUsageEventsByAPIKey(ctx context.Context, apiKeyID string) (int64, error) {
	column, id := usageKeyColumn(apiKeyID)
	if id == "" {
		return 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_events
		  WHERE `+column+` = ?
		    AND ts >= datetime('now', 'start of day')`,
		id,
	)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count daily usage by api_key: %w", err)
	}
	return n, nil
}

// CountDailyUsageEventsByService is the service-scope variant of the daily
// request-count query.
func (x *DB) CountDailyUsageEventsByService(ctx context.Context, serviceID string) (int64, error) {
	if serviceID == "" {
		return 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_events
		  WHERE service_id = ?
		    AND ts >= datetime('now', 'start of day')`,
		serviceID,
	)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count daily usage by service: %w", err)
	}
	return n, nil
}
