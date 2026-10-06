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
// SumUsageTokens* returns (tokens_in, tokens_out) over the usage_events rows
// for a given window. The window string is interpreted by SQLite's
// datetime('now', ...) expression — callers pass one of "today" / "week" /
// "month" / "year" and the helper maps it to the matching boundary.
//
// These queries deliberately use SQLite's relative-date arithmetic
// ('start of day', '-7 days', etc.) so the comparison against the ts column
// works regardless of whether the column stored an RFC3339Nano string or a
// SQLite-native datetime — both compare lexicographically when ts is in
// "YYYY-MM-DD HH:MM:SS" or ISO 8601 form, which is the case for usage_events.

// UsageWindowBoundary returns the SQLite expression that names the start
// of the current window. Exposed so the cost engine can build queries that
// join by (window, model) without re-encoding the mapping.
func UsageWindowBoundary(window string) string {
	switch window {
	case "today":
		return "datetime('now', 'start of day')"
	case "week":
		return "datetime('now', '-7 days')"
	case "month":
		return "datetime('now', '-30 days')"
	case "year":
		return "datetime('now', '-365 days')"
	default:
		return "datetime('now', 'start of day')"
	}
}

// UsageRow is one row of the cost-engine aggregation query: model + observed
// token totals + observed byte totals + per-(service, api_key) labels. The
// engine adds what upstreams reported (ReportedUSD) to the price-table cost
// of the tokens no upstream put a price on (PricedTokensIn/Out).
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
}

// ListUsageForWindow returns one UsageRow per (service_id, api_key_id, kind)
// combination over the named window. The kind column is used as a coarse
// "model" proxy when the proxy chain hasn't yet plumbed the real model name
// onto usage_events (chain.go has a TODO for that). The cost engine treats
// kind as the lookup key; once chain.go adds a model column the engine will
// switch to that without changing this method.
func (x *DB) ListUsageForWindow(ctx context.Context, window string) ([]UsageRow, error) {
	q := fmt.Sprintf(`
		SELECT service_id, api_key_id, kind,
		       COALESCE(SUM(tokens_in), 0)  AS tokens_in,
		       COALESCE(SUM(tokens_out), 0) AS tokens_out,
		       COALESCE(SUM(bytes_in), 0)   AS bytes_in,
		       COALESCE(SUM(bytes_out), 0)  AS bytes_out,
		       COALESCE(SUM(cost_usd), 0)   AS reported_usd,
		       COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN tokens_in  ELSE 0 END), 0) AS priced_tokens_in,
		       COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN tokens_out ELSE 0 END), 0) AS priced_tokens_out
		  FROM usage_events
		 WHERE ts >= %s
		 GROUP BY service_id, api_key_id, kind`, UsageWindowBoundary(window))
	rows, err := x.sqlDB.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list usage for window %s: %w", window, err)
	}
	defer rows.Close()
	out := make([]UsageRow, 0)
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.ServiceID, &u.APIKeyID, &u.Kind,
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
// day. Used by Engine.CheckBudgets when the budget scope is api_key. Returns
// (0, 0) when apiKeyID is empty.
func (x *DB) SumDailyTokensByAPIKey(ctx context.Context, apiKeyID string) (int64, int64, error) {
	if apiKeyID == "" {
		return 0, 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		   FROM usage_events
		  WHERE api_key_id = ?
		    AND ts >= datetime('now', 'start of day')`,
		apiKeyID,
	)
	var in, out int64
	if err := row.Scan(&in, &out); err != nil {
		return 0, 0, fmt.Errorf("sum daily tokens by api_key: %w", err)
	}
	return in, out, nil
}

// SumDailyTokensByService is the service-scope variant of SumDailyTokensByAPIKey.
func (x *DB) SumDailyTokensByService(ctx context.Context, serviceID string) (int64, int64, error) {
	if serviceID == "" {
		return 0, 0, nil
	}
	row := x.sqlDB.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		   FROM usage_events
		  WHERE service_id = ?
		    AND ts >= datetime('now', 'start of day')`,
		serviceID,
	)
	var in, out int64
	if err := row.Scan(&in, &out); err != nil {
		return 0, 0, fmt.Errorf("sum daily tokens by service: %w", err)
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
