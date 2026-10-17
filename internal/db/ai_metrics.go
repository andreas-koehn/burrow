package db

// ai_metrics.go — trailing-24h aggregation over usage_events for the AI-endpoint
// dashboard surfaces (GET /api/v1/ai/providers and .../{slug}/metrics). The proxy
// hot path (internal/aimeter SQLSink) writes one usage_events row per proxied
// request; these read-side aggregations turn that into the numbers the UI shows.

import (
	"context"
	"fmt"
	"time"
)

// Every time boundary below is computed in Go, in UTC, and bound, and no
// date function of either database is used: the statements run on SQLite and
// Postgres alike. cache_hit is INTEGER 0/1 on SQLite and BOOLEAN on Postgres;
// "CASE WHEN cache_hit THEN 1 ELSE 0 END" counts it on both.

// purposeGuardrailJudge is usage_events.purpose of a call the relay made to
// the judge model (aigw.PurposeGuardrailJudge). Such a row is metered under
// the judged request's service, but its request and its latency are the
// judge model's: the provider figures below leave it out (request counts,
// the per-minute series, the p95 that fastest routing reads); tokens and
// cost keep it.
const purposeGuardrailJudge = "guardrail-judge"

// AIEndpointCount is the per-service trailing-24h request + cache-hit summary
// used by the AI-endpoints list.
type AIEndpointCount struct {
	Requests  int
	CacheHits int
	// LatencyP95Ms is the nearest-rank 95th percentile of latency_ms over the
	// requests of the last 24h that an upstream answered with a 2xx
	// (upstream_status 200-299; no cache hits, no other status, no rows from
	// before the column existed); 0 when there is none. See
	// AIEndpointLatencyP95.
	LatencyP95Ms int
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

// AIEndpointCounts24h returns per-service request and cache-hit counts over
// the trailing 24h plus the p95 latency, keyed by service_id: the two reads
// below, both run now. The API does not call it per request: it reads the
// counts with AIEndpointRequests24h and takes the latency from a value it
// keeps for a minute (cmd/server, aiMetricsStore), because the latency read
// is the expensive one.
func (x *DB) AIEndpointCounts24h(ctx context.Context) (map[string]AIEndpointCount, error) {
	out, err := x.AIEndpointRequests24h(ctx)
	if err != nil {
		return nil, err
	}
	p95, err := x.AIEndpointLatencyP95(ctx)
	if err != nil {
		return nil, err
	}
	for sid, ms := range p95 {
		c := out[sid]
		c.LatencyP95Ms = ms
		out[sid] = c
	}
	return out, nil
}

// AIEndpointRequests24h returns per-service request + cache-hit counts over
// the trailing 24h, keyed by service_id. LatencyP95Ms is not filled.
func (x *DB) AIEndpointRequests24h(ctx context.Context) (map[string]AIEndpointCount, error) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	rows, err := x.sqlDB.QueryContext(ctx, `
		SELECT service_id,
		       COUNT(*) AS requests,
		       CAST(COALESCE(SUM(CASE WHEN cache_hit THEN 1 ELSE 0 END), 0) AS BIGINT) AS cache_hits
		  FROM usage_events
		 WHERE ts >= ? AND purpose <> ?
		 GROUP BY service_id`, since, purposeGuardrailJudge)
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

// AIEndpointLatencyP95 returns the p95 latency in ms per service over the
// trailing 24h, keyed by service_id: the nearest-rank percentile, the value
// at rank ceil(0.95 n) of the service's latencies in ascending order. A
// service without a counted request has no entry.
//
// Only requests an upstream answered with a 2xx count. Any other status is
// left out: a 5xx is a failure of the upstream, or the gateway's own 502/504
// when none answered, and its latency is the whole chain's; a 4xx the gateway
// writes (chiefly provider_busy, 429) carries the wait in the queue; 0 is a
// request that got no status. Cache hits and rows from before the latency
// column (0 ms) are left out too.
//
// The database returns how often each latency occurred per service, in
// ascending order, and the rank is taken here from the running total. That
// gives the value a ROW_NUMBER over the same rows gives (the statement this
// replaced), without the window: SQLite materialises every partition of a
// window, which made this read several times slower than everything else the
// provider list needs, on the one connection SQLite has. Index
// idx_usage_events_ts serves the range. The statement is plain GROUP BY and
// ORDER BY and runs on SQLite and Postgres alike.
func (x *DB) AIEndpointLatencyP95(ctx context.Context) (map[string]int, error) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	rows, err := x.sqlDB.QueryContext(ctx, `
		SELECT service_id, latency_ms, COUNT(*) AS n
		  FROM usage_events
		 WHERE ts >= ? AND latency_ms > 0 AND NOT cache_hit
		   AND upstream_status BETWEEN 200 AND 299 AND purpose <> ?
		 GROUP BY service_id, latency_ms
		 ORDER BY service_id, latency_ms`, since, purposeGuardrailJudge)
	if err != nil {
		return nil, fmt.Errorf("ai endpoint latency 24h: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	var (
		current string
		hist    []latencyCount // of the service being read, ascending
	)
	for rows.Next() {
		var sid string
		var lc latencyCount
		if err := rows.Scan(&sid, &lc.ms, &lc.n); err != nil {
			return nil, fmt.Errorf("scan ai endpoint latency: %w", err)
		}
		if sid != current {
			if ms, ok := nearestRankP95(hist); ok {
				out[current] = ms
			}
			current, hist = sid, hist[:0]
		}
		hist = append(hist, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if ms, ok := nearestRankP95(hist); ok {
		out[current] = ms
	}
	return out, nil
}

// latencyCount says that n requests took ms milliseconds.
type latencyCount struct {
	ms, n int64
}

// nearestRankP95 returns the 95th percentile, nearest rank, of the values in
// hist, which is ordered by ms ascending: the value at rank ceil(0.95 n) of
// all n values. In integers, like the SQL it replaced: (95 n + 99) / 100.
func nearestRankP95(hist []latencyCount) (int, bool) {
	var n int64
	for _, h := range hist {
		n += h.n
	}
	if n == 0 {
		return 0, false
	}
	rank := (95*n + 99) / 100
	var seen int64
	for _, h := range hist {
		seen += h.n
		if seen >= rank {
			return int(h.ms), true
		}
	}
	return int(hist[len(hist)-1].ms), true
}

// AIEndpointMetrics24h returns the trailing-24h aggregate for one service plus a
// 60-bucket requests-per-minute series over the trailing hour.
func (x *DB) AIEndpointMetrics24h(ctx context.Context, serviceID string) (AIEndpointAgg, error) {
	var agg AIEndpointAgg
	// One clock reading for the three queries. Whole seconds, as the minute
	// buckets are counted in.
	now := time.Now().UTC().Truncate(time.Second)
	dayAgo := now.Add(-24 * time.Hour)

	// Judge calls count for tokens (they are spent), not as requests.
	row := x.sqlDB.QueryRowContext(ctx, `
		SELECT CAST(COALESCE(SUM(CASE WHEN purpose <> ? THEN 1 ELSE 0 END), 0) AS BIGINT),
		       CAST(COALESCE(SUM(tokens_in), 0) AS BIGINT),
		       CAST(COALESCE(SUM(tokens_out), 0) AS BIGINT),
		       CAST(COALESCE(SUM(CASE WHEN cache_hit THEN 1 ELSE 0 END), 0) AS BIGINT)
		  FROM usage_events
		 WHERE service_id = ? AND ts >= ?`, purposeGuardrailJudge, serviceID, dayAgo)
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
		 WHERE service_id = ? AND ts >= ? AND purpose <> ?`, serviceID, now.Add(-60*time.Minute), purposeGuardrailJudge)
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
