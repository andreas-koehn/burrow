//go:build postgres

package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestDirectAIProvider_Postgres runs the direct-provider statements against a
// live Postgres: create with its backing row, upstream update, the model
// catalog, the name guard of GetOrCreateService and the cascading delete.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestDirectAIProvider_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres direct provider test")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	ctx := context.Background()
	// The database outlives the test run: tolerate what an earlier run left.
	_ = x.CreateUser(ctx, User{ID: "u-direct-pg", Email: "u-direct-pg@test.invalid", PasswordHash: "h", Role: "user"})
	_ = x.DeleteAIProviderAndBacking(ctx, "pg-direct")

	svc := Service{ID: "prov-pg-direct", UserID: "u-direct-pg", Name: "PG Direct", Type: "http", AccessMode: "api_key"}
	p := AIProvider{Slug: "pg-direct", Name: "PG Direct", Kind: "tunnel", APIFormat: "openai", BaseURL: "https://x.example/v1", CredentialSlot: "S"}
	if err := x.CreateDirectAIProvider(ctx, svc, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.DeleteAIProviderAndBacking(ctx, "pg-direct") })
	got, err := x.GetAIProvider(ctx, "pg-direct")
	if err != nil || got.Kind != "direct" || got.ServiceID != svc.ID {
		t.Fatalf("get: %v %+v", err, got)
	}

	got.BaseURL, got.Billing, got.ExtraHeaders = "https://y.example/v2", "flat", map[string]string{"X-Title": "Burrow"}
	if err := x.UpdateAIProviderUpstream(ctx, got); err != nil {
		t.Fatal(err)
	}
	if after, _ := x.GetAIProvider(ctx, "pg-direct"); after.BaseURL != "https://y.example/v2" || after.Billing != "flat" || after.ExtraHeaders["X-Title"] != "Burrow" {
		t.Fatalf("after update: %+v", after)
	}

	if _, err := x.GetOrCreateService(ctx, "u-direct-pg", "PG Direct", "http"); !errors.Is(err, ErrServiceNameReserved) {
		t.Fatalf("name guard err = %v", err)
	}

	models := []AIProviderModel{{ModelID: "b", DisplayName: "B", ContextLength: 8192}, {ModelID: "a"}}
	for i := 0; i < 2; i++ {
		if err := x.ReplaceAIProviderModels(ctx, "pg-direct", models); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.UpsertAIProviderModel(ctx, AIProviderModel{ProviderSlug: "pg-direct", ModelID: "a", DisplayName: "A"}); err != nil {
		t.Fatal(err)
	}
	list, err := x.ListAIProviderModels(ctx, "pg-direct")
	if err != nil || len(list) != 2 || list[0].ModelID != "a" || list[0].DisplayName != "A" || list[1].ContextLength != 8192 {
		t.Fatalf("models: %v %+v", err, list)
	}
	if err := x.CreateServiceAPIKey(ctx, ServiceAPIKey{ID: "k-direct-pg", ServiceID: svc.ID, Name: "ci", KeyHash: "h-direct-pg"}); err != nil {
		t.Fatal(err)
	}

	if err := x.DeleteAIProviderAndBacking(ctx, "pg-direct"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetServiceByID(ctx, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backing service survived: %v", err)
	}
	if keys, _ := x.ListServiceAPIKeys(ctx, svc.ID); len(keys) != 0 {
		t.Fatalf("keys survived: %+v", keys)
	}
	if list, _ := x.ListAIProviderModels(ctx, "pg-direct"); len(list) != 0 {
		t.Fatalf("models survived: %+v", list)
	}
	if err := x.DeleteAIProviderAndBacking(ctx, "pg-direct"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

// TestReportedCostAndProviderRename_Postgres runs the statements of the
// reported-cost task against a live Postgres: cost_usd written as a value and
// as NULL, the three aggregate expressions ListUsageForWindow adds, the rename
// that carries the backing service's name, and the conditional upstream update.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestReportedCostAndProviderRename_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres reported cost test")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	ctx := context.Background()
	_ = x.CreateUser(ctx, User{ID: "u-cost-pg", Email: "u-cost-pg@test.invalid", PasswordHash: "h", Role: "user"})
	for _, slug := range []string{"pg-cost", "pg-cost2"} {
		_ = x.DeleteAIProviderAndBacking(ctx, slug)
	}

	svc := Service{ID: "svc-pg-cost", UserID: "u-cost-pg", Name: "PG Cost", Type: "direct", AccessMode: "api_key"}
	p := AIProvider{Slug: "pg-cost", Name: "PG Cost", APIFormat: "openai", BaseURL: "https://x.example/v1", CredentialSlot: "S"}
	if err := x.CreateDirectAIProvider(ctx, svc, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = x.DeleteAIProviderAndBacking(ctx, "pg-cost")
		_ = x.DeleteAIProviderAndBacking(ctx, "pg-cost2")
	})

	quarter, zero := 0.25, 0.0
	for id, ev := range map[string]UsageEvent{
		"pg-cost-1": {TokensIn: 100, TokensOut: 50, CostUSD: &quarter},
		"pg-cost-2": {TokensIn: 10, TokensOut: 5},
		"pg-cost-3": {TokensIn: 7, TokensOut: 3, CostUSD: &zero},
	} {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, cost_usd)
			 VALUES(?,?,?,?,?,?,?,?)`,
			id, svc.ID, "k1", time.Now().UTC(), "openai", ev.TokensIn, ev.TokensOut, ev.CostUSD); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	var u UsageRow
	if err := x.sqlDB.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(tokens_in), 0),
		       COALESCE(SUM(cost_usd), 0)   AS reported_usd,
		       COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN tokens_in  ELSE 0 END), 0) AS priced_tokens_in,
		       COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN tokens_out ELSE 0 END), 0) AS priced_tokens_out
		  FROM usage_events
		 WHERE service_id = ?
		 GROUP BY service_id, api_key_id, kind`, svc.ID).Scan(&u.TokensIn, &u.ReportedUSD, &u.PricedTokensIn, &u.PricedTokensOut); err != nil {
		t.Fatal(err)
	}
	if u.TokensIn != 117 || u.ReportedUSD != 0.25 || u.PricedTokensIn != 10 || u.PricedTokensOut != 5 {
		t.Fatalf("aggregate = %+v, want tokens 117, reported 0.25, priced 10/5", u)
	}

	if err := x.UpdateAIProvider(ctx, "pg-cost", "pg-cost2", "PG Cost Two"); err != nil {
		t.Fatal(err)
	}
	if backing, _ := x.GetServiceByID(ctx, svc.ID); backing.Name != "PG Cost Two" {
		t.Fatalf("backing service = %+v, want name PG Cost Two", backing)
	}

	calls := 0
	if err := x.ModifyAIProviderUpstream(ctx, "pg-cost2", func(p AIProvider) (AIProvider, error) {
		if calls++; calls == 1 {
			other := p
			other.Billing = "flat"
			if err := x.UpdateAIProviderUpstream(ctx, other); err != nil {
				t.Fatal(err)
			}
		}
		p.BaseURL = "https://y.example/v2"
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	if after, _ := x.GetAIProvider(ctx, "pg-cost2"); calls != 2 || after.BaseURL != "https://y.example/v2" || after.Billing != "flat" {
		t.Fatalf("calls=%d after=%+v, want 2 calls and both changes", calls, after)
	}
}
