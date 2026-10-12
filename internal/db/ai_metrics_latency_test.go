package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The p95 latency of a service is taken over the requests of the last 24h an
// upstream answered (a status of 200-499): cache hits, rows without a latency
// or a status, failures, the gateway's own 5xx and older rows are left out.
func TestAIEndpointCounts24h_LatencyP95(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u-lat")
	svc := seedSvc(t, x, "u-lat", "svc-lat")
	quiet := seedSvc(t, x, "u-lat", "svc-lat-quiet")
	now := time.Now().UTC()
	insert := func(id, service string, ago time.Duration, latency int64, cacheHit bool, upstream int) {
		t.Helper()
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, streamed, cache_hit, latency_ms, upstream_status)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, service, "k1", now.Add(-ago), "openai", 1, 1, false, cacheHit, latency, upstream); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 20; i++ {
		insert(fmt.Sprintf("lat-%d", i), svc, time.Minute, int64(i*100), false, 200) // 100 … 2000 ms
	}
	insert("lat-hit", svc, time.Minute, 1, true, 200)          // a cache hit
	insert("lat-zero", svc, time.Minute, 0, false, 200)        // from before the column
	insert("lat-old", svc, 25*time.Hour, 99_000, false, 200)   // outside the day
	insert("lat-nostatus", svc, time.Minute, 90_000, false, 0) // nothing answered
	insert("lat-502", svc, time.Minute, 91_000, false, 502)    // the gateway's own, after the whole chain
	insert("lat-504", svc, time.Minute, 92_000, false, 504)
	insert("lat-500", svc, time.Minute, 93_000, false, 500) // an upstream that failed
	insert("lat-quiet", quiet, time.Minute, 1, true, 200)   // only a cache hit
	// A service whose only requests failed has no p95; one with a single
	// answered request has that request's latency; a 4xx is an answer.
	failing := seedSvc(t, x, "u-lat", "svc-lat-failing")
	insert("lat-f1", failing, time.Minute, 5_000, false, 502)
	insert("lat-f2", failing, time.Minute, 6_000, false, 0)
	single := seedSvc(t, x, "u-lat", "svc-lat-single")
	insert("lat-s1", single, time.Minute, 321, false, 404)

	counts, err := x.AIEndpointCounts24h(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts[svc]; got.LatencyP95Ms != 1900 || got.Requests != 26 || got.CacheHits != 1 {
		t.Errorf("counts[svc] = %+v, want p95 1900 ms over 20 answered requests, 26 requests, 1 cache hit", got)
	}
	if got := counts[quiet]; got.LatencyP95Ms != 0 || got.Requests != 1 {
		t.Errorf("counts[quiet] = %+v, want no p95", got)
	}
	if got := counts[failing]; got.LatencyP95Ms != 0 || got.Requests != 2 {
		t.Errorf("counts[failing] = %+v, want no p95 for a service nothing answered", got)
	}
	if got := counts[single]; got.LatencyP95Ms != 321 {
		t.Errorf("counts[single] = %+v, want the one row's 321 ms", got)
	}
}
