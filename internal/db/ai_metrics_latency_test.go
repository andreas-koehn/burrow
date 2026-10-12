package db

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// TestAIEndpointCounts24h_LatencyP95 runs the checks the Postgres test runs
// too.
func TestAIEndpointCounts24h_LatencyP95(t *testing.T) {
	x := testDB(t)
	t.Run("definition", func(t *testing.T) { checkAIEndpointLatency(t, x, "u-lat") })
	t.Run("against a sort", func(t *testing.T) { checkAIEndpointLatencyAgainstSort(t, x, "u-lat-sort") })
}

// insertLatencyRow writes one usage row with the columns the latency read
// looks at. The flags are bound as booleans, the way the usage sink binds
// them.
func insertLatencyRow(t *testing.T, x *DB, id, service string, at time.Time, latency int64, cacheHit bool, upstream int) {
	t.Helper()
	if _, err := x.sqlDB.ExecContext(context.Background(),
		`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, streamed, cache_hit, latency_ms, upstream_status)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, service, "k1", at, "openai", 1, 1, false, cacheHit, latency, upstream); err != nil {
		t.Fatal(err)
	}
}

// checkAIEndpointLatency: the p95 latency of a service is taken over the
// requests of the last 24h an upstream answered (a 2xx): cache hits, rows
// without a latency or a status, failures, the gateway's own 5xx and older
// rows are left out.
func checkAIEndpointLatency(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	svc := seedSvc(t, x, userID, "svc-lat")
	quiet := seedSvc(t, x, userID, "svc-lat-quiet")
	now := time.Now().UTC()
	insert := func(id, service string, ago time.Duration, latency int64, cacheHit bool, upstream int) {
		t.Helper()
		insertLatencyRow(t, x, userID+"-"+id, service, now.Add(-ago), latency, cacheHit, upstream)
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
	insert("lat-429", svc, time.Minute, 60_000, false, 429) // the gateway's provider_busy, after the queue
	insert("lat-quiet", quiet, time.Minute, 1, true, 200)   // only a cache hit
	// A service whose only requests failed has no p95; one with a single
	// answered request has that request's latency;
	failing := seedSvc(t, x, userID, "svc-lat-failing")
	insert("lat-f1", failing, time.Minute, 5_000, false, 502)
	insert("lat-f2", failing, time.Minute, 6_000, false, 0)
	failing2 := seedSvc(t, x, userID, "svc-lat-refused")
	insert("lat-r1", failing2, time.Minute, 60_000, false, 429)
	single := seedSvc(t, x, userID, "svc-lat-single")
	insert("lat-s1", single, time.Minute, 321, false, 200)

	counts, err := x.AIEndpointCounts24h(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts[svc]; got.LatencyP95Ms != 1900 || got.Requests != 27 || got.CacheHits != 1 {
		t.Errorf("counts[svc] = %+v, want p95 1900 ms over 20 answered requests, 27 requests, 1 cache hit", got)
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
	if got := counts[failing2]; got.LatencyP95Ms != 0 || got.Requests != 1 {
		t.Errorf("counts[failing2] = %+v, want a 429 to give no p95", got)
	}

	// The two reads the API makes instead of the one above give the same
	// figures: the counts without a latency, and the latency only for the
	// services that have one.
	requests, err := x.AIEndpointRequests24h(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p95, err := x.AIEndpointLatencyP95(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := requests[svc]; got.Requests != 27 || got.CacheHits != 1 || got.LatencyP95Ms != 0 {
		t.Errorf("requests[svc] = %+v, want 27 requests, 1 cache hit and no latency", got)
	}
	if p95[svc] != 1900 || p95[single] != 321 {
		t.Errorf("p95: svc %d, single %d, want 1900 and 321", p95[svc], p95[single])
	}
	for name, id := range map[string]string{"quiet": quiet, "failing": failing, "refused": failing2} {
		if ms, ok := p95[id]; ok {
			t.Errorf("p95 has an entry (%d ms) for the %s service, which no upstream answered with a 2xx", ms, name)
		}
	}
}

// checkAIEndpointLatencyAgainstSort compares the read with the definition
// done by hand: sort the latencies of a service, take the value at rank
// ceil(0.95 n). Sizes around the places where the rank moves, many equal
// values, and rows that do not count in between.
func checkAIEndpointLatencyAgainstSort(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	rng := rand.New(rand.NewSource(95))
	now := time.Now().UTC()
	want := map[string]int{}
	row := 0
	for _, n := range []int{1, 2, 19, 20, 21, 39, 40, 41, 100, 101, 333} {
		svc := seedSvc(t, x, userID, fmt.Sprintf("svc-sort-%d", n))
		// Few distinct values for the small sizes, so that ties sit on the rank.
		spread := int64(7)
		if n > 50 {
			spread = 2000
		}
		latencies := make([]int64, n)
		for i := range latencies {
			latencies[i] = 1 + rng.Int63n(spread)
			row++
			insertLatencyRow(t, x, fmt.Sprintf("%s-%d", userID, row), svc,
				now.Add(-time.Duration(rng.Intn(23*3600))*time.Second), latencies[i], false, 200+rng.Intn(100))
		}
		// Rows that must not move the figure, each slower than every counted one.
		for _, skip := range []struct {
			ago      time.Duration
			latency  int64
			cacheHit bool
			status   int
		}{
			{time.Minute, 50_000, true, 200},
			{time.Minute, 0, false, 200},
			{25 * time.Hour, 50_000, false, 200},
			{time.Minute, 50_000, false, 0},
			{time.Minute, 50_000, false, 199},
			{time.Minute, 50_000, false, 300},
			{time.Minute, 50_000, false, 429},
			{time.Minute, 50_000, false, 502},
		} {
			row++
			insertLatencyRow(t, x, fmt.Sprintf("%s-%d", userID, row), svc, now.Add(-skip.ago), skip.latency, skip.cacheHit, skip.status)
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		rank := (95*n + 99) / 100 // ceil(0.95 n)
		want[svc] = int(latencies[rank-1])
	}
	p95, err := x.AIEndpointLatencyP95(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := x.AIEndpointCounts24h(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for svc, ms := range want {
		if p95[svc] != ms {
			t.Errorf("%s: p95 %d ms, the sorted values give %d", svc, p95[svc], ms)
		}
		if counts[svc].LatencyP95Ms != ms {
			t.Errorf("%s: the combined read gives %d ms, want %d", svc, counts[svc].LatencyP95Ms, ms)
		}
	}
}

func TestNearestRankP95(t *testing.T) {
	if _, ok := nearestRankP95(nil); ok {
		t.Error("no values gave a percentile")
	}
	for _, c := range []struct {
		hist []latencyCount
		want int
	}{
		{[]latencyCount{{ms: 7, n: 1}}, 7},
		{[]latencyCount{{ms: 1, n: 19}, {ms: 9, n: 1}}, 1}, // n=20: rank 19
		{[]latencyCount{{ms: 1, n: 18}, {ms: 9, n: 2}}, 9}, // n=20: rank 19 is the first 9
		{[]latencyCount{{ms: 1, n: 95}, {ms: 2, n: 5}}, 1}, // n=100: rank 95
		{[]latencyCount{{ms: 1, n: 95}, {ms: 2, n: 6}}, 2}, // n=101: rank 96
		{[]latencyCount{{ms: 5, n: 1}, {ms: 6, n: 1}}, 6},  // n=2: rank 2
		{[]latencyCount{{ms: 3, n: 1 << 40}, {ms: 4, n: 1}}, 3},
	} {
		if got, ok := nearestRankP95(c.hist); !ok || got != c.want {
			t.Errorf("nearestRankP95(%v) = %d, %v, want %d", c.hist, got, ok, c.want)
		}
	}
}
