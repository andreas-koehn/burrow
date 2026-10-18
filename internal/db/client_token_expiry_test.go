package db

import (
	"context"
	"testing"
	"time"
)

func TestClientTokenExpiry(t *testing.T) {
	checkClientTokenExpiry(t, testDB(t), "u-tokexp")
}

// Migration 0030's Down block removes what its Up block adds.
func TestMigration0030_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0030_v0.11.1_client_token_expiry.sql")
}

func checkClientTokenExpiry(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	exp := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	if err := x.CreateClientToken(ctx, ClientToken{ID: userID + "-a", UserID: userID, Name: "a", TokenHash: userID + "-ha", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if err := x.CreateClientToken(ctx, ClientToken{ID: userID + "-b", UserID: userID, Name: "b", TokenHash: userID + "-hb"}); err != nil {
		t.Fatal(err)
	}
	a, err := x.GetClientTokenByHash(ctx, userID+"-ha")
	if err != nil || a.ExpiresAt == nil || !a.ExpiresAt.Equal(exp) {
		t.Fatalf("with expiry: %+v (%v), want %v", a.ExpiresAt, err, exp)
	}
	b, err := x.GetClientTokenByHash(ctx, userID+"-hb")
	if err != nil || b.ExpiresAt != nil {
		t.Fatalf("without expiry: %+v (%v)", b.ExpiresAt, err)
	}
	all, err := x.ListClientTokensByUser(ctx, userID)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %v %d", err, len(all))
	}
}
