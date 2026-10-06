package db

// ai_metrics.go — trailing-24h aggregation over usage_events for the AI-endpoint
// dashboard surfaces (GET /api/v1/ai/providers and .../{slug}/metrics). The proxy
// hot path (internal/aimeter SQLSink) writes one usage_events row per proxied
// request; these read-side aggregations turn that into the numbers the UI shows.
// usage_events has no latency column, so p95 latency is not derivable here.

import (
	"context"
	"fmt"
	"time"
)

// Every time boundary below is computed in Go, in UTC, and bound, and no
// date function of either database is used: the statements run on SQLite and
// Postgres alike. cache_hit is INTEGER 0/1 on SQLite and BOOLEAN on Postgres;
// "CASE WHEN cache_hit THEN 1 ELSE 0 END" counts it on both.

// AIEndpointCount is the per-service trailing-24h request + cache-hit summary
// used by the AI-endpoints list.
type AIEndpointCount struct {
	Requests  int
	CacheHits int
}

// AIEndpointKindTokens is a per-"kind" token subtotal for one service. kind is
// the cost-engine pricing-lookup key, so the handler can derive USD from these.
type AIEndpointKindTokens struct {
	Kind      string
	TokensIn  int64 // all rows
	TokensOut int64 // all rows
	// ReportedUSD is the sum of cost_usd over the rows that carry one;
	// PricedTokensIn/Out are the tokens of the rows without one, which the
	// price table applies to. Same split as UsageRow.
	ReportedUSD     float64
	PricedTokensIn  int64
	PricedTokensOut int64
}

// AIEndpointAgg is the per-service trailing-24h aggregate behind the endpoint
// detail metrics endpoint.
type AIEndpointAgg struct {
	Requests  int
	TokensIn  int64
	TokensOut int64
	CacheHits int
	ByKind    []AIEndpointKindTokens
	// PerMinute is requests-per-minute over the trailing hour, oldest (index 0)
	// → newest (index 59).
	PerMinute [60]int
}

// AIEndpointCounts24h returns per-service request + cache-hit counts over the
// trailing 24h, keyed by service_id.
func (x *DB) AIEndpointCounts24h(ctx context.Context) (map[string]AIEndpointCount, error) {
	rows, err := x.sqlDB.QueryContext(ctx, `
		SELECT service_id,
		       COUNT(*) AS requests,
		       CAST(COALESCE(SUM(CASE WHEN cache_hit THEN 1 ELSE 0 END), 0) AS BIGINT) AS cache_hits
		  FROM usage_events
		 WHERE ts >= ?
		 GROUP BY service_id`, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("ai endpoint counts 24h: %w", err)
	}
	defer rows.Close()
	out := map[string]AIEndpointCount{}
	for rows.Next() {
		var sid string
		var c AIEndpointCount
		if err := rows.Scan(&sid, &c.Requests, &c.CacheHits); err != nil {
			return nil, fmt.Errorf("scan ai endpoint count: %w", err)
		}
		out[sid] = c
	}
	return out, rows.Err()
}

// AIEndpointMetrics24h returns the trailing-24h aggregate for one service plus a
// 60-bucket requests-per-minute series over the trailing hour.
func (x *DB) AIEndpointMetrics24h(ctx context.Context, serviceID string) (AIEndpointAgg, error) {
	var agg AIEndpointAgg
	// One clock reading for the three queries. Whole seconds, as the minute
	// buckets are counted in.
	now := time.Now().UTC().Truncate(time.Second)
	dayAgo := now.Add(-24 * time.Hour)

	row := x.sqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       CAST(COALESCE(SUM(tokens_in), 0) AS BIGINT),
		       CAST(COALESCE(SUM(tokens_out), 0) AS BIGINT),
		       CAST(COALESCE(SUM(CASE WHEN cache_hit THEN 1 ELSE 0 END), 0) AS BIGINT)
		  FROM usage_events
		 WHERE service_id = ? AND ts >= ?`, serviceID, dayAgo)
	if err := row.Scan(&agg.Requests, &agg.TokensIn, &agg.TokensOut, &agg.CacheHits); err != nil {
		return agg, fmt.Errorf("ai endpoint metrics 24h: %w", err)
	}

	// Per-kind token subtotals (kind = pricing-lookup key for cost derivation).
	krows, err := x.sqlDB.QueryContext(ctx, `
		SELECT kind,
		       CAST(COALESCE(SUM(tokens_in), 0) AS BIGINT),
		       CAST(COALESCE(SUM(tokens_out), 0) AS BIGINT),
		       COALESCE(SUM(cost_usd), 0),
		       CAST(COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN tokens_in  ELSE 0 END), 0) AS BIGINT),
		       CAST(COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN tokens_out ELSE 0 END), 0) AS BIGINT)
		  FROM usage_events
		 WHERE service_id = ? AND ts >= ?
		 GROUP BY kind`, serviceID, dayAgo)
	if err != nil {
		return agg, fmt.Errorf("ai endpoint kind tokens: %w", err)
	}
	defer krows.Close()
	for krows.Next() {
		var k AIEndpointKindTokens
		if err := krows.Scan(&k.Kind, &k.TokensIn, &k.TokensOut,
			&k.ReportedUSD, &k.PricedTokensIn, &k.PricedTokensOut); err != nil {
			return agg, fmt.Errorf("scan kind tokens: %w", err)
		}
		agg.ByKind = append(agg.ByKind, k)
	}
	if err := krows.Err(); err != nil {
		return agg, err
	}

	// Requests-per-minute over the trailing hour. minsAgo: 0 = the last
	// minute … 59 = 59 minutes ago. Newest goes at PerMinute[59]. The rows of
	// the hour are bucketed here rather than in SQL: the two databases share
	// no expression for "minutes since a timestamp".
	mrows, err := x.sqlDB.QueryContext(ctx, `
		SELECT ts
		  FROM usage_events
		 WHERE service_id = ? AND ts >= ?`, serviceID, now.Add(-60*time.Minute))
	if err != nil {
		return agg, fmt.Errorf("ai endpoint per-minute: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var raw any
		if err := mrows.Scan(&raw); err != nil {
			return agg, fmt.Errorf("scan per-minute: %w", err)
		}
		ts, ok := usageTime(raw)
		if !ok {
			continue
		}
		// Whole seconds on both sides, then whole minutes.
		minsAgo := (now.Unix() - ts.Unix()) / 60
		if minsAgo >= 0 && minsAgo < 60 {
			agg.PerMinute[59-minsAgo]++
		}
	}
	return agg, mrows.Err()
}

// usageTime reads a usage_events.ts value as the driver returns it: a
// time.Time (Postgres, and SQLite when the driver recognises the text), or
// the text the SQLite driver stored for a time.Time, of which the leading
// "YYYY-MM-DD HH:MM:SS" is read as UTC (the sink writes UTC).
func usageTime(raw any) (time.Time, bool) {
	var text string
	switch v := raw.(type) {
	case time.Time:
		return v, true
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return time.Time{}, false
	}
	if len(text) < 19 {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, text[:19]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
