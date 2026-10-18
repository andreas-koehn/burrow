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
	now := time.Now().UTC()
	if ids, err := x.ListExpiredClientTokenIDs(ctx, now); err != nil || contains(ids, userID+"-a") || contains(ids, userID+"-b") {
		t.Fatalf("expired now: %v (%v)", ids, err)
	}
	if ids, err := x.ListExpiredClientTokenIDs(ctx, exp.Add(time.Second)); err != nil || !contains(ids, userID+"-a") || contains(ids, userID+"-b") {
		t.Fatalf("expired later: %v (%v)", ids, err)
	}
	// The sweep looks back an hour only: sessions of a token that expired
	// earlier were closed by an earlier sweep.
	old := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	recent := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	for id, at := range map[string]time.Time{userID + "-old": old, userID + "-recent": recent} {
		at := at
		if err := x.CreateClientToken(ctx, ClientToken{ID: id, UserID: userID, Name: id, TokenHash: id + "-h", ExpiresAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	if ids, err := x.ListExpiredClientTokenIDs(ctx, time.Now()); err != nil || contains(ids, userID+"-old") || !contains(ids, userID+"-recent") {
		t.Fatalf("sweep window: %v (%v)", ids, err)
	}
	all, err := x.ListClientTokensByUser(ctx, userID)
	if err != nil || len(all) != 4 {
		t.Fatalf("list: %v %d", err, len(all))
	}
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
