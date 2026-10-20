//go:build postgres

package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestOAuth_Postgres runs the authorization server's statements against a
// live Postgres: clients, a code redeemed twice, a grant with its tokens,
// refresh rotation, revocation, the client delete that revokes its grants,
// and pruning.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestOAuth_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres oauth test")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	ctx := context.Background()
	// The database outlives the test run: tolerate what an earlier run left.
	_ = x.CreateUser(ctx, User{ID: "u-oauth-pg", Email: "u-oauth-pg@test.invalid", PasswordHash: "h", Role: "user"})
	_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_grants WHERE user_id='u-oauth-pg'`)
	_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_codes WHERE user_id='u-oauth-pg'`)
	_ = x.DeleteOAuthClient(ctx, "bcl_pg", time.Now())

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := x.CreateOAuthClient(ctx, OAuthClient{ClientID: "bcl_pg", Name: "PG", RedirectURIs: []string{"https://a.example/cb"}, CreatedBy: "u-oauth-pg", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	c, err := x.GetOAuthClient(ctx, "bcl_pg")
	if err != nil || c.Name != "PG" || len(c.RedirectURIs) != 1 || !c.CreatedAt.Equal(now) {
		t.Fatalf("get client: %+v %v", c, err)
	}
	if list, err := x.ListOAuthClients(ctx); err != nil || len(list) == 0 {
		t.Fatalf("list clients: %v %v", list, err)
	}
	if _, err := x.GetOAuthClient(ctx, "bcl_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown client: %v", err)
	}

	code := OAuthCode{Hash: "pg-code", ClientID: "bcl_pg", ClientName: "PG", UserID: "u-oauth-pg", RedirectURI: "https://a.example/cb",
		CodeChallenge: "ch", Resource: "https://r.example/mcp", Scopes: []string{"mcp:tools"}, AllowedTools: []string{"gh__*"},
		ToolsReadOnly: true, ExpiresAt: now.Add(time.Minute), CreatedAt: now}
	_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_codes WHERE code_hash='pg-code'`)
	if err := x.CreateOAuthCode(ctx, code); err != nil {
		t.Fatal(err)
	}
	got, err := x.RedeemOAuthCode(ctx, "pg-code")
	if err != nil || got.Used || got.AllowedTools[0] != "gh__*" || !got.ToolsReadOnly || !got.ExpiresAt.Equal(code.ExpiresAt) {
		t.Fatalf("redeem: %+v %v", got, err)
	}

	g := OAuthGrant{ID: "g-pg", ClientID: "bcl_pg", ClientName: "PG", UserID: "u-oauth-pg", Resource: code.Resource,
		Scopes: code.Scopes, AllowedTools: code.AllowedTools, ToolsReadOnly: true, CreatedAt: now}
	tokens := []OAuthToken{
		{Hash: "pg-access", Kind: "access", GrantID: "g-pg", FamilyID: "g-pg", ExpiresAt: now.Add(time.Hour), CreatedAt: now},
		{Hash: "pg-refresh", Kind: "refresh", GrantID: "g-pg", FamilyID: "g-pg", ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now},
	}
	if err := x.CreateOAuthGrant(ctx, g, "pg-code", tokens...); err != nil {
		t.Fatal(err)
	}
	again, err := x.RedeemOAuthCode(ctx, "pg-code")
	if err != nil || !again.Used || again.GrantID != "g-pg" {
		t.Fatalf("second redeem: %+v %v", again, err)
	}
	// A second use before the link: the link is refused.
	code2 := code
	code2.Hash = "pg-code2"
	_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_codes WHERE code_hash='pg-code2'`)
	if err := x.CreateOAuthCode(ctx, code2); err != nil {
		t.Fatal(err)
	}
	if peek, err := x.GetOAuthCode(ctx, "pg-code2"); err != nil || peek.Used {
		t.Fatalf("peek: %+v %v", peek, err)
	}
	if _, err := x.RedeemOAuthCode(ctx, "pg-code2"); err != nil {
		t.Fatal(err)
	}
	if late, err := x.RedeemOAuthCode(ctx, "pg-code2"); err != nil || !late.Used || late.GrantID != "" {
		t.Fatalf("reuse before link: %+v %v", late, err)
	}
	gLate := g
	gLate.ID = "g-pg-late"
	if err := x.CreateOAuthGrant(ctx, gLate, "pg-code2"); !errors.Is(err, ErrOAuthCodeReused) {
		t.Fatalf("link after reuse: %v", err)
	}
	tok, grant, err := x.GetOAuthToken(ctx, "pg-access")
	if err != nil || tok.Kind != "access" || grant.ID != "g-pg" || !grant.ToolsReadOnly || grant.RevokedAt != nil || tok.UsedAt != nil {
		t.Fatalf("get token: %+v %+v %v", tok, grant, err)
	}
	if err := x.TouchOAuthGrant(ctx, "g-pg", now); err != nil {
		t.Fatal(err)
	}
	if gg, _ := x.GetOAuthGrant(ctx, "g-pg"); gg.LastUsed == nil {
		t.Fatal("touch did not set last_used")
	}
	next := []OAuthToken{
		{Hash: "pg-access2", Kind: "access", GrantID: "g-pg", FamilyID: "g-pg", ExpiresAt: now.Add(time.Hour), CreatedAt: now},
		{Hash: "pg-refresh2", Kind: "refresh", GrantID: "g-pg", FamilyID: "g-pg", ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now},
	}
	if ok, err := x.RotateOAuthRefresh(ctx, "pg-refresh", now, next...); err != nil || !ok {
		t.Fatalf("rotate: %v %v", ok, err)
	}
	if ok, err := x.RotateOAuthRefresh(ctx, "pg-refresh", now, OAuthToken{Hash: "pg-x", Kind: "access", GrantID: "g-pg", FamilyID: "g-pg", ExpiresAt: now}); err != nil || ok {
		t.Fatalf("second rotate: %v %v", ok, err)
	}
	if used, _, _ := x.GetOAuthToken(ctx, "pg-refresh"); used.UsedAt == nil {
		t.Fatal("used_at not set")
	}
	if list, err := x.ListOAuthGrants(ctx, "u-oauth-pg"); err != nil || len(list) != 1 {
		t.Fatalf("list grants: %v %v", list, err)
	}
	if err := x.DeleteOAuthToken(ctx, "pg-access2"); err != nil {
		t.Fatal(err)
	}
	if err := x.RevokeOAuthGrant(ctx, "g-pg", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.GetOAuthToken(ctx, "pg-refresh2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tokens of a revoked grant: %v", err)
	}
	if list, _ := x.ListOAuthGrants(ctx, "u-oauth-pg"); len(list) != 0 {
		t.Fatalf("revoked grant listed: %v", list)
	}
	if err := x.RevokeOAuthGrant(ctx, "g-none", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown grant: %v", err)
	}

	// Deleting the client revokes the grants it still holds.
	g2 := g
	g2.ID = "g-pg2"
	if err := x.CreateOAuthGrant(ctx, g2, "", OAuthToken{Hash: "pg-a3", Kind: "access", GrantID: "g-pg2", FamilyID: "g-pg2", ExpiresAt: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	code3 := code
	code3.Hash = "pg-code3"
	_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_codes WHERE code_hash='pg-code3'`)
	if err := x.CreateOAuthCode(ctx, code3); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteOAuthClient(ctx, "bcl_pg", now); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetOAuthCode(ctx, "pg-code3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open code of a deleted client: %v", err)
	}
	if gg, _ := x.GetOAuthGrant(ctx, "g-pg2"); gg.RevokedAt == nil {
		t.Fatal("grant of a deleted client not revoked")
	}
	if _, _, err := x.GetOAuthToken(ctx, "pg-a3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token of a deleted client: %v", err)
	}
	if err := x.DeleteOAuthClient(ctx, "bcl_pg", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := x.PruneOAuth(ctx, now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := x.RedeemOAuthCode(ctx, "pg-code"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned code: %v", err)
	}
	// A grant revoked more than 30 days ago goes too.
	if _, err := x.GetOAuthGrant(ctx, "g-pg2"); err != nil {
		t.Fatalf("grant revoked two days ago was pruned: %v", err)
	}
	if err := x.PruneOAuth(ctx, now.Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetOAuthGrant(ctx, "g-pg2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant revoked 31 days ago: %v", err)
	}
	_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM oauth_grants WHERE user_id='u-oauth-pg'`)
}
