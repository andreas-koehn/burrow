package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAIGatewayKeys(t *testing.T) { checkAIGatewayKeys(t, testDB(t), "u1", "u2") }

// checkAIGatewayKeys exercises the ai_gateway_keys statements. It runs
// against SQLite here and against a live Postgres in the postgres-tagged test.
func checkAIGatewayKeys(t *testing.T, x *DB, u1, u2 string) {
	t.Helper()
	ctx := context.Background()
	reset := func() {
		_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM ai_gateway_keys`)
		_ = x.DeleteUser(ctx, u1)
		_ = x.DeleteUser(ctx, u2)
	}
	reset()
	t.Cleanup(reset)
	mustUser(t, x, u1)
	mustUser(t, x, u2)
	k := AIGatewayKey{ID: "gk1", Name: "laptop", KeyHash: "h1", KeyPrefix: "bgw_abcd", UserID: u1,
		AllowedModels: []string{"burrow-simple", "zai/*"}}
	if err := x.CreateAIGatewayKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetAIGatewayKeyByHash(ctx, "h1")
	if err != nil || got.ID != "gk1" || got.Name != "laptop" || got.KeyPrefix != "bgw_abcd" || got.UserID != u1 ||
		len(got.AllowedModels) != 2 || got.AllowedModels[1] != "zai/*" || got.RevokedAt != nil || got.LastUsed != nil || got.CreatedAt.IsZero() {
		t.Fatalf("get: %v %+v", err, got)
	}
	if err := x.CreateAIGatewayKey(ctx, AIGatewayKey{ID: "gk-dup", Name: "x", KeyHash: "h1", KeyPrefix: "p", UserID: u1}); err == nil {
		t.Fatal("duplicate hash must fail")
	}
	time.Sleep(1100 * time.Millisecond) // created_at has second resolution
	if err := x.CreateAIGatewayKey(ctx, AIGatewayKey{ID: "gk2", Name: "ci", KeyHash: "h2", KeyPrefix: "bgw_wxyz", UserID: u2}); err != nil {
		t.Fatal(err)
	}
	got2, err := x.GetAIGatewayKey(ctx, "gk2")
	if err != nil || got2.AllowedModels == nil || len(got2.AllowedModels) != 0 {
		t.Fatalf("nil allowed: %v %#v", err, got2.AllowedModels)
	}

	if err := x.TouchAIGatewayKey(ctx, "gk1"); err != nil {
		t.Fatal(err)
	}
	got, _ = x.GetAIGatewayKey(ctx, "gk1")
	if got.LastUsed == nil {
		t.Fatal("LastUsed not set")
	}
	if err := x.RevokeAIGatewayKey(ctx, "gk1"); err != nil {
		t.Fatal(err)
	}
	first, _ := x.GetAIGatewayKey(ctx, "gk1")
	if first.RevokedAt == nil {
		t.Fatal("RevokedAt not set")
	}
	time.Sleep(1100 * time.Millisecond)
	if err := x.RevokeAIGatewayKey(ctx, "gk1"); err != nil {
		t.Fatal(err)
	}
	second, _ := x.GetAIGatewayKey(ctx, "gk1")
	if !second.RevokedAt.Equal(*first.RevokedAt) {
		t.Fatalf("second revoke moved the timestamp: %v -> %v", first.RevokedAt, second.RevokedAt)
	}
	if byHash, err := x.GetAIGatewayKeyByHash(ctx, "h1"); err != nil || byHash.RevokedAt == nil {
		t.Fatalf("revoked by hash: %v %+v", err, byHash)
	}
	if _, err := x.GetAIGatewayKeyByHash(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing hash err = %v", err)
	}
	if err := x.RevokeAIGatewayKey(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing err = %v", err)
	}

	mine, err := x.ListAIGatewayKeys(ctx, u1)
	if err != nil || len(mine) != 1 || mine[0].ID != "gk1" {
		t.Fatalf("list u1: %v %+v", err, mine)
	}
	all, err := x.ListAIGatewayKeys(ctx, "")
	if err != nil || len(all) != 2 || all[0].ID != "gk2" {
		t.Fatalf("list all (newest first): %v %+v", err, all)
	}

	if _, err := x.sqlDB.ExecContext(ctx, `DELETE FROM users WHERE id=?`, u1); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetAIGatewayKey(ctx, "gk1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key should cascade with user, got %v", err)
	}
}
