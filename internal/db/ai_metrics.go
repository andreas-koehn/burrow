package db

// ai_metrics.go — trailing-24h aggregation over usage_events for the AI-endpoint
// dashboard surfaces (GET /api/v1/ai/providers and .../{slug}/metrics). The proxy
// hot path (internal/aimeter SQLSink) writes one usage_events row per proxied
// request; these read-side aggregations turn that into the numbers the UI shows.
// usage_events has no latency column, so p95 latency is not derivable here.

import (
	"context"
	"fmt"
)

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
	TokensIn  int64
	TokensOut int64
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
		       COUNT(*)                    AS requests,
		       COALESCE(SUM(cache_hit), 0) AS cache_hits
		  FROM usage_events
		 WHERE ts >= datetime('now', '-1 day')
		 GROUP BY service_id`)
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

	row := x.sqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(tokens_in), 0),
		       COALESCE(SUM(tokens_out), 0),
		       COALESCE(SUM(cache_hit), 0)
		  FROM usage_events
		 WHERE service_id = ? AND ts >= datetime('now', '-1 day')`, serviceID)
	if err := row.Scan(&agg.Requests, &agg.TokensIn, &agg.TokensOut, &agg.CacheHits); err != nil {
		return agg, fmt.Errorf("ai endpoint metrics 24h: %w", err)
	}

	// Per-kind token subtotals (kind = pricing-lookup key for cost derivation).
	krows, err := x.sqlDB.QueryContext(ctx, `
		SELECT kind,
		       COALESCE(SUM(tokens_in), 0),
		       COALESCE(SUM(tokens_out), 0)
		  FROM usage_events
		 WHERE service_id = ? AND ts >= datetime('now', '-1 day')
		 GROUP BY kind`, serviceID)
	if err != nil {
		return agg, fmt.Errorf("ai endpoint kind tokens: %w", err)
	}
	defer krows.Close()
	for krows.Next() {
		var k AIEndpointKindTokens
		if err := krows.Scan(&k.Kind, &k.TokensIn, &k.TokensOut); err != nil {
			return agg, fmt.Errorf("scan kind tokens: %w", err)
		}
		agg.ByKind = append(agg.ByKind, k)
	}
	if err := krows.Err(); err != nil {
		return agg, err
	}

	// Requests-per-minute over the trailing hour. mins_ago: 0 = current minute
	// … 59 = 59 minutes ago. Newest goes at PerMinute[59].
	//
	// ts is stored in Go's time.Time string form (e.g.
	// "2026-05-29 10:44:34.127276661 +0000 UTC") which strftime cannot parse —
	// so we feed it only the leading "YYYY-MM-DD HH:MM:SS" via substr(ts,1,19).
	// (The window filter uses a plain lexical >= comparison, which the existing
	// usage queries also rely on, so it needs no normalization.)
	mrows, err := x.sqlDB.QueryContext(ctx, `
		SELECT CAST((strftime('%s','now') - strftime('%s', substr(ts, 1, 19))) / 60 AS INTEGER) AS mins_ago,
		       COUNT(*)
		  FROM usage_events
		 WHERE service_id = ? AND ts >= datetime('now', '-60 minutes')
		 GROUP BY mins_ago`, serviceID)
	if err != nil {
		return agg, fmt.Errorf("ai endpoint per-minute: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var minsAgo, n int
		if err := mrows.Scan(&minsAgo, &n); err != nil {
			return agg, fmt.Errorf("scan per-minute: %w", err)
		}
		if minsAgo >= 0 && minsAgo < 60 {
			agg.PerMinute[59-minsAgo] = n
		}
	}
	return agg, mrows.Err()
}
