package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// checkTokenlessRequests inserts usage rows and checks that the window query
// counts only the 2xx, non-cache, cost-less requests without tokens. It runs
// on SQLite and Postgres alike (cache_hit is INTEGER there, BOOLEAN here).
func checkTokenlessRequests(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	if err := x.CreateUser(ctx, User{ID: userID, Email: userID + "@test.invalid", PasswordHash: "h", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	svc, err := x.GetOrCreateService(ctx, userID, "svc", "http")
	if err != nil {
		t.Fatal(err)
	}
	cost := func(v float64) any { return v }
	rows := []struct {
		id        string
		in, out   int
		hit       bool
		status    int
		cost      any
		wantCount bool
	}{
		{"1", 0, 0, false, 200, nil, true},
		{"2", 0, 0, false, 200, nil, true},
		{"3", 5, 0, false, 200, nil, false},       // has tokens
		{"4", 0, 0, true, 200, nil, false},        // cache hit
		{"5", 0, 0, false, 500, nil, false},       // failed
		{"6", 0, 0, false, 200, cost(0.1), false}, // reported cost
	}
	want := int64(0)
	for _, r := range rows {
		if r.wantCount {
			want++
		}
		if _, err := x.ObservedExec(ctx, `
			INSERT INTO usage_events (id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, bytes_in, bytes_out,
			  streamed, cache_hit, upstream_status, cost_usd, gateway_key_id, dialect, provider_slug, requested_model, target_model,
			  request_id, latency_ms, translated, dropped)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			userID+"-"+r.id, svc.ID, "", time.Now().UTC(), "openai", r.in, r.out, 0, 0,
			false, r.hit, r.status, r.cost, "", "openai", "p", "m", "t", "", 0, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := x.ListUsageForWindow(ctx, "today")
	if err != nil {
		t.Fatalf("ListUsageForWindow: %v", err)
	}
	var n, requests int64
	for _, u := range got {
		if u.ServiceID == svc.ID {
			n += u.TokenlessRequests
			requests += u.Requests
		}
	}
	if requests != int64(len(rows)) || n != want {
		t.Fatalf("requests %d tokenless %d, want %d and %d", requests, n, len(rows), want)
	}
}

func TestTokenlessRequests_SQLite(t *testing.T) {
	raw, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(raw); err != nil {
		t.Fatal(err)
	}
	x := Wrap(raw)
	t.Cleanup(func() { _ = x.Close() })
	checkTokenlessRequests(t, x, "u-tokenless")
}
