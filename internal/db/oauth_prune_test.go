package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Final review: pruning deletes expired tokens and the grants that were
// revoked more than 30 days ago (with whatever tokens they still hold); a
// live grant, and one revoked recently, stay.
func TestPruneOAuth_ExpiredTokensAndOldRevokedGrants(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)
	if err := x.CreateUser(ctx, User{ID: "u1", Email: "u1@x", PasswordHash: "h", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	grant := func(id string) {
		t.Helper()
		g := OAuthGrant{ID: id, ClientID: "bcl_x", ClientName: "X", UserID: "u1", Resource: "https://r.example/mcp", Scopes: []string{"mcp:tools"}, CreatedAt: now.Add(-60 * 24 * time.Hour)}
		if err := x.CreateOAuthGrant(ctx, g, "",
			OAuthToken{Hash: id + "-live", Kind: "refresh", GrantID: id, FamilyID: id, ExpiresAt: now.Add(time.Hour), CreatedAt: now},
			OAuthToken{Hash: id + "-old", Kind: "access", GrantID: id, FamilyID: id, ExpiresAt: now.Add(-time.Minute), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	grant("live")
	grant("recent")
	grant("old")
	if err := x.RevokeOAuthGrant(ctx, "recent", now.Add(-29*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := x.RevokeOAuthGrant(ctx, "old", now.Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := x.PruneOAuth(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live", "recent"} {
		if _, err := x.GetOAuthGrant(ctx, id); err != nil {
			t.Errorf("grant %s after pruning: %v", id, err)
		}
	}
	if _, err := x.GetOAuthGrant(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("grant revoked 31 days ago after pruning: %v", err)
	}
	if _, _, err := x.GetOAuthToken(ctx, "live-live"); err != nil {
		t.Errorf("live token after pruning: %v", err)
	}
	if _, _, err := x.GetOAuthToken(ctx, "live-old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token after pruning: %v", err)
	}
}
