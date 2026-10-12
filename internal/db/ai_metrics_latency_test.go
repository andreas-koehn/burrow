package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPercentile95(t *testing.T) {
	seq := func(n int) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(n - i) // descending: the function sorts
		}
		return out
	}
	for _, tc := range []struct {
		in   []int64
		want int64
	}{
		{nil, 0},
		{[]int64{7}, 7},
		{[]int64{10, 20}, 20},
		{seq(20), 19},
		{seq(100), 95},
		{seq(101), 96},
	} {
		if got := percentile95(tc.in); got != tc.want {
			t.Errorf("percentile95 of %d values = %d, want %d", len(tc.in), got, tc.want)
		}
	}
}

// The p95 latency of a service is taken over the requests of the last 24h an
// upstream answered: cache hits, rows without a latency and older rows are
// left out.
func TestAIEndpointCounts24h_LatencyP95(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u-lat")
	svc := seedSvc(t, x, "u-lat", "svc-lat")
	quiet := seedSvc(t, x, "u-lat", "svc-lat-quiet")
	now := time.Now().UTC()
	insert := func(id, service string, ago time.Duration, latency int64, cacheHit bool) {
		t.Helper()
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, streamed, cache_hit, latency_ms)
			 VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, service, "k1", now.Add(-ago), "openai", 1, 1, false, cacheHit, latency); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 20; i++ {
		insert(fmt.Sprintf("lat-%d", i), svc, time.Minute, int64(i*100), false) // 100 … 2000 ms
	}
	insert("lat-hit", svc, time.Minute, 1, true)        // a cache hit
	insert("lat-zero", svc, time.Minute, 0, false)      // from before the column
	insert("lat-old", svc, 25*time.Hour, 99_000, false) // outside the day
	insert("lat-quiet", quiet, time.Minute, 1, true)    // only a cache hit

	counts, err := x.AIEndpointCounts24h(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts[svc]; got.LatencyP95Ms != 1900 || got.Requests != 22 || got.CacheHits != 1 {
		t.Errorf("counts[svc] = %+v, want p95 1900 ms over 20 answered requests, 22 requests, 1 cache hit", got)
	}
	if got := counts[quiet]; got.LatencyP95Ms != 0 || got.Requests != 1 {
		t.Errorf("counts[quiet] = %+v, want no p95", got)
	}
}
