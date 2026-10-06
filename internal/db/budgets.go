package db

// budgets.go — typed CRUD over the budgets table (migration 0009).
//
// The budgets table is defined by spec Part F (Cost & budgets). A row is one
// "spend this much per day before action_on_exceed fires" rule. current_usd +
// exceeded are computed live by the cost engine from usage_events × pricing
// table, so no live counters live in this table.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateBudget inserts a new budgets row. The caller provides the row's ID
// (typically uuid.NewString()). created_at is populated by SQLite's
// CURRENT_TIMESTAMP default.
func (x *DB) CreateBudget(ctx context.Context, b Budget) error {
	var awid any
	if b.AlertWebhookID != nil {
		awid = *b.AlertWebhookID
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO budgets(id, scope, subject_id, daily_usd, daily_tokens, action_on_exceed, alert_webhook_id)
		 VALUES(?,?,?,?,?,?,?)`,
		b.ID, b.Scope, b.SubjectID, b.DailyUSD, b.DailyTokens, b.ActionOnExceed, awid,
	)
	if err != nil {
		return fmt.Errorf("create budget: %w", err)
	}
	return nil
}

// GetBudget returns the row with the given id, or ErrNotFound.
func (x *DB) GetBudget(ctx context.Context, id string) (Budget, error) {
	var b Budget
	var awid sql.NullString
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT id, scope, subject_id, daily_usd, daily_tokens, action_on_exceed, alert_webhook_id, created_at
		   FROM budgets WHERE id=?`,
		id,
	).Scan(&b.ID, &b.Scope, &b.SubjectID, &b.DailyUSD, &b.DailyTokens, &b.ActionOnExceed, &awid, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Budget{}, ErrNotFound
	}
	if err != nil {
		return Budget{}, fmt.Errorf("get budget: %w", err)
	}
	if awid.Valid {
		s := awid.String
		b.AlertWebhookID = &s
	}
	return b, nil
}

// ListBudgets returns every budgets row ordered by (scope, subject_id). The
// returned slice is always non-nil (possibly empty).
func (x *DB) ListBudgets(ctx context.Context) ([]Budget, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, scope, subject_id, daily_usd, daily_tokens, action_on_exceed, alert_webhook_id, created_at
		   FROM budgets ORDER BY scope, subject_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	defer rows.Close()
	out := make([]Budget, 0)
	for rows.Next() {
		var b Budget
		var awid sql.NullString
		if err := rows.Scan(&b.ID, &b.Scope, &b.SubjectID, &b.DailyUSD, &b.DailyTokens,
			&b.ActionOnExceed, &awid, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan budget: %w", err)
		}
		if awid.Valid {
			s := awid.String
			b.AlertWebhookID = &s
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list budgets rows: %w", err)
	}
	return out, nil
}

// UpdateBudget replaces every mutable column on the row with the given id.
// Returns ErrNotFound when no row matches.
func (x *DB) UpdateBudget(ctx context.Context, b Budget) error {
	var awid any
	if b.AlertWebhookID != nil {
		awid = *b.AlertWebhookID
	}
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE budgets
		    SET scope=?, subject_id=?, daily_usd=?, daily_tokens=?, action_on_exceed=?, alert_webhook_id=?
		  WHERE id=?`,
		b.Scope, b.SubjectID, b.DailyUSD, b.DailyTokens, b.ActionOnExceed, awid, b.ID,
	)
	if err != nil {
		return fmt.Errorf("update budget: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update budget rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteBudget removes the row with the given id. Returns ErrNotFound when no
// row matches.
func (x *DB) DeleteBudget(ctx context.Context, id string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM budgets WHERE id=?`, id,
	)
	if err != nil {
		return fmt.Errorf("delete budget: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete budget rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- usage_events aggregation for the cost engine ---------------------------
//
// Every time boundary is computed in Go, in UTC, and bound as a parameter:
// neither database's date functions appear in a query, so the same statement
// runs on SQLite and Postgres. usage_events.ts is written as a UTC time.Time
// by the usage sink, and a bound UTC time.Time compares correctly with it on
// both (as text on SQLite, as a timestamp on Postgres).

// UsageWindowStart returns the first instant of the named window, in UTC:
// "today" is the start of the current UTC day (budgets and day quotas are per
// UTC day), "week", "month" and "year" are the last 7, 30 and 365 days. An
// unknown window is "today".
func UsageWindowStart(window string, now time.Time) time.Time {
	now = now.UTC()
	switch window {
	case "week":
		return now.AddDate(0, 0, -7)
	case "month":
		return now.AddDate(0, 0, -30)
	case "year":
		return now.AddDate(0, 0, -365)
	}
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// utcDayStart is the start of the current UTC day.
func utcDayStart() time.Time { return UsageWindowStart("today", time.Now()) }

// UsageRow is one row of the cost-engine aggregation query: the identity of
// a group of requests (who asked, for which model, who answered) with its
// token and byte totals. The engine adds what upstreams reported
// (ReportedUSD) to the price-table cost of the tokens no upstream put a price
// on (PricedTokensIn/Out).
type UsageRow struct {
	ServiceID string
	APIKeyID  string
	Kind      string
	TokensIn  int64 // all rows
	TokensOut int64 // all rows
	BytesIn   int64
	BytesOut  int64
	// ReportedUSD is the sum of cost_usd over the rows that carry one.
	ReportedUSD float64
	// PricedTokensIn/Out are the tokens of the rows without a reported cost.
	PricedTokensIn  int64
	PricedTokensOut int64
	// How the gateway routed the requests; all "" for traffic that did not
	// come through a gateway key. RequestedModel is the name the client
	// asked for, ProviderSlug and TargetModel are who answered.
	GatewayKeyID   string
	Dialect        string
	ProviderSlug   string
	RequestedModel string
	TargetModel    string
	// Requests is the number of usage rows in the group.
	Requests int64
}

// listUsageForWindowSQL is the window aggregation; its one parameter is the
// start of the window. Index idx_usage_events_ts serves the ts range.
const listUsageForWindowSQL = `
		SELECT service_id, api_key_id, kind,
		       gateway_key_id, dialect, provider_slug, requested_model, target_model,
		       COUNT(*) AS requests,
		       CAST(COALESCE(SUM(tokens_in), 0)  AS BIGINT) AS tokens_in,
		       CAST(COALESCE(SUM(tokens_out), 0) AS BIGINT) AS tokens_out,
		       CAST(COALESCE(SUM(bytes_in), 0)   AS BIGINT) AS bytes_in,
		       CAST(COALESCE(SUM(bytes_out), 0)  AS BIGINT) AS bytes_out,
		       COALESCE(SUM(cost_usd), 0)   AS reported_usd,
		       CAST(COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN tokens_in  ELSE 0 END), 0) AS BIGINT) AS priced_tokens_in,
		       CAST(COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN tokens_out ELSE 0 END), 0) AS BIGINT) AS priced_tokens_out
		  FROM usage_events
		 WHERE ts >= ?
		 GROUP BY service_id, api_key_id, kind,
		          gateway_key_id, dialect, provider_slug, requested_model, target_model`

// ListUsageForWindow returns one UsageRow per (service_id, api_key_id, kind,
// gateway_key_id, dialect, provider_slug, requested_model, target_model)
// combination over the named window (see UsageWindowStart).
func (x *DB) ListUsageForWindow(ctx context.Context, window string) ([]UsageRow, error) {
	rows, err := x.sqlDB.QueryContext(ctx, listUsageForWindowSQL,
		UsageWindowStart(window, time.Now()))
	if err != nil {
		return nil, fmt.Errorf("list usage for window %s: %w", window, err)
	}
	defer rows.Close()
	out := make([]UsageRow, 0)
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.ServiceID, &u.APIKeyID, &u.Kind,
			&u.GatewayKeyID, &u.Dialect, &u.ProviderSlug, &u.RequestedModel, &u.TargetModel,
			&u.Requests,
			&u.TokensIn, &u.TokensOut, &u.BytesIn, &u.BytesOut,
			&u.ReportedUSD, &u.PricedTokensIn, &u.PricedTokensOut); err != nil {
			return nil, fmt.Errorf("scan usage row: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list usage rows: %w", err)
	}
	return out, nil
}

// SumDailyTokensByAPIKey returns (tokens_in, tokens_out) summed over the
// usage_events rows for the given api_key since the start of the current UTC
// day. Returns (0, 0) when apiKeyID is empty.
func (x *DB) SumDailyTokensByAPIKey(ctx context.Context, apiKeyID string) (int64, int64, error) {
	return x.sumDailyTokens(ctx, "api_key_id", apiKeyID)
}

// SumDailyTokensByService is the service-scope variant of SumDailyTokensByAPIKey.
func (x *DB) SumDailyTokensByService(ctx context.Context, serviceID string) (int64, int64, error) {
	return x.sumDailyTokens(ctx, "service_id", serviceID)
}

// sumDailyTokens sums today's tokens of the rows whose column equals id.
// column is one of the constants the callers above pass, never input.
func (x *DB) sumDailyTokens(ctx context.Context, column, id string) (int64, int64, error) {
	if id == "" {
		return 0, 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT CAST(COALESCE(SUM(tokens_in), 0) AS BIGINT), CAST(COALESCE(SUM(tokens_out), 0) AS BIGINT)
		   FROM usage_events
		  WHERE `+column+` = ?
		    AND ts >= ?`,
		id, utcDayStart(),
	)
	var in, out int64
	if err := row.Scan(&in, &out); err != nil {
		return 0, 0, fmt.Errorf("sum daily tokens by %s: %w", column, err)
	}
	return in, out, nil
}

// LookupServiceAPIKey returns the (id, service_id) of an api_key row, used by
// Engine.CheckBudgets when an api_key-scoped budget exceeds with action
// disable_key — the engine needs the service_id to call DeleteServiceAPIKey.
// Returns ErrNotFound when no row matches.
func (x *DB) LookupServiceAPIKey(ctx context.Context, apiKeyID string) (id, serviceID string, err error) {
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT id, service_id FROM service_api_keys WHERE id=?`, apiKeyID,
	)
	if err := row.Scan(&id, &serviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("lookup service api key: %w", err)
	}
	return id, serviceID, nil
}
