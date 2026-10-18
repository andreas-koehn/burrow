package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeleteService_KeepsUsageAndDropsServiceScopes(t *testing.T) {
	checkDeleteServiceKeepsUsage(t, testDB(t), "u-del-usage")
}

// Migration 0031's Down block puts the foreign key back and its Up block
// takes it away again; the table keeps every column through both.
func TestMigration0031_DownAndUp(t *testing.T) {
	checkMigration0031DownAndUp(t, testDB(t), "0031_v0.11.1_usage_keep_history.sql", "u-mig31")
}

func TestDeleteService_ProviderBlocksAtomically(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)
	mustUser(t, x, "u-del-prov")
	svc := seedSvc(t, x, "u-del-prov", "llm")
	if err := x.CreateAIProvider(ctx, AIProvider{Slug: "llm", Name: "llm", Kind: "tunnel", ServiceID: svc, APIFormat: "openai"}); err != nil {
		t.Fatal(err)
	}
	err := x.DeleteService(ctx, svc)
	if !errors.Is(err, ErrServiceHasProvider) || !strings.Contains(err.Error(), "llm") {
		t.Fatalf("delete: %v, want ErrServiceHasProvider naming the provider", err)
	}
	if _, err := x.GetServiceByID(ctx, svc); err != nil {
		t.Fatalf("the service is gone: %v", err)
	}
	if err := x.DeleteService(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v", err)
	}
}

func checkDeleteServiceKeepsUsage(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	// Usage rows outlive their service, so a Postgres database keeps them between runs.
	clean := func() {
		_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM usage_events WHERE id LIKE ?`, "del-"+userID+"%")
	}
	clean()
	t.Cleanup(clean)
	svc := seedSvc(t, x, userID, "metered")
	other := seedSvc(t, x, userID, "other")

	now := time.Now().UTC()
	for i, id := range []string{svc, svc, other} {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, ts, kind, tokens_in, tokens_out, cost_usd, purpose) VALUES(?,?,?,?,?,?,?,?)`,
			"del-"+userID+string(rune('a'+i)), id, now, "openai", 100, 50, 0.5, ""); err != nil {
			t.Fatal(err)
		}
	}
	sum := func() (n int, usd float64) {
		rows, err := x.ListUsageForWindow(ctx, "24h")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ServiceID == svc || r.ServiceID == other {
				n += int(r.Requests)
				usd += r.ReportedUSD
			}
		}
		return n, usd
	}
	n0, usd0 := sum()
	if n0 != 3 || usd0 != 1.5 {
		t.Fatalf("before: %d rows, %v usd", n0, usd0)
	}

	bid, rid, keepB, keepR := "b-"+userID, "r-"+userID, "bk-"+userID, "rk-"+userID
	cleanScopes := func() {
		for _, id := range []string{bid, keepB} {
			_ = x.DeleteBudget(ctx, id)
		}
		_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM rate_limits WHERE id IN (?,?)`, rid, keepR)
	}
	cleanScopes()
	t.Cleanup(cleanScopes)
	for _, b := range []Budget{
		{ID: bid, Scope: "service", SubjectID: svc, DailyUSD: 1, ActionOnExceed: "throttle_zero"},
		{ID: keepB, Scope: "service", SubjectID: other, DailyUSD: 1, ActionOnExceed: "throttle_zero"},
	} {
		if err := x.CreateBudget(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []RateLimit{
		{ID: rid, Scope: "service", Subject: svc, Dimension: "rpm", Lim: 5, Burst: 5, Window: "minute"},
		{ID: keepR, Scope: "service", Subject: other, Dimension: "rpm", Lim: 5, Burst: 5, Window: "minute"},
	} {
		if err := x.CreateRateLimit(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	if err := x.DeleteService(ctx, svc); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, usd := sum(); n != n0 || usd != usd0 {
		t.Fatalf("after: %d rows, %v usd, want %d, %v (history is kept)", n, usd, n0, usd0)
	}
	if _, err := x.GetBudget(ctx, bid); !errors.Is(err, ErrNotFound) {
		t.Errorf("the service's budget: %v, want ErrNotFound", err)
	}
	if _, err := x.GetBudget(ctx, keepB); err != nil {
		t.Errorf("another service's budget: %v", err)
	}
	if _, err := x.GetRateLimit(ctx, rid); !errors.Is(err, ErrNotFound) {
		t.Errorf("the service's rate limit: %v, want ErrNotFound", err)
	}
	if _, err := x.GetRateLimit(ctx, keepR); err != nil {
		t.Errorf("another service's rate limit: %v", err)
	}
}

// checkMigration0031DownAndUp runs Down and Up inside a transaction that is
// rolled back. After Up a row of a service that does not exist is accepted,
// and a row with every column survives.
func checkMigration0031DownAndUp(t *testing.T, x *DB, file, userID string) {
	t.Helper()
	ctx := context.Background()
	raw, err := migrationFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	_, down, found := strings.Cut(string(raw), "-- +goose Down")
	if !found || strings.TrimSpace(down) == "" {
		t.Fatalf("%s has no Down block", file)
	}
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := tx.ExecContext(ctx, upBlock(string(raw))); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, bytes_in, bytes_out, streamed, cache_hit,
		   upstream_status, cost_usd, gateway_key_id, dialect, provider_slug, requested_model, target_model, request_id, latency_ms,
		   translated, dropped, purpose)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"mig31-"+userID, "no-such-service", "k", time.Now().UTC(), "openai", 1, 2, 3, 4, false, false, 200, 0.1, "gk", "openai", "zai", "m", "t", "r", 7, "x", "y", "guardrail-judge"); err != nil {
		t.Fatalf("a row of a service that is not there: %v", err)
	}
	var purpose string
	var latency int
	if err := tx.QueryRowContext(ctx, `SELECT purpose, latency_ms FROM usage_events WHERE id=?`, "mig31-"+userID).Scan(&purpose, &latency); err != nil || purpose != "guardrail-judge" || latency != 7 {
		t.Fatalf("row: %q %d %v", purpose, latency, err)
	}
}
