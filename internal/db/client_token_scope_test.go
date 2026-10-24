package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMigration0043_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0043_v0.13.0_client_token_scope.sql")
}

func TestClientTokenScope_SQLite(t *testing.T) { checkClientTokenScope(t, testDB(t), "u-scope-sqlite") }

// checkClientTokenScope: a token keeps its scope, the default is all, an
// unknown scope is refused by the store and by the database, and a sign-in
// request keeps the wished scope until the decision replaces it.
func checkClientTokenScope(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshTokenUser(t, x, userID)
	for _, tc := range []struct{ id, scope, want string }{
		{"sc-def", "", "all"}, {"sc-all", "all", "all"}, {"sc-fwd", "forward", "forward"}, {"sc-pub", "publish", "publish"},
	} {
		if err := x.CreateClientToken(ctx, ClientToken{ID: userID + tc.id, UserID: userID, Name: tc.id, TokenHash: "h-" + userID + tc.id, Scope: tc.scope}); err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		ct, err := x.GetClientTokenByHash(ctx, "h-"+userID+tc.id)
		if err != nil || ct.Scope != tc.want {
			t.Fatalf("%s: scope %q err %v, want %q", tc.id, ct.Scope, err, tc.want)
		}
	}
	if err := x.CreateClientToken(ctx, ClientToken{ID: userID + "bad", UserID: userID, Name: "bad", TokenHash: "h-" + userID + "bad", Scope: "root"}); err == nil {
		t.Fatal("an unknown scope was stored")
	}
	// The database itself refuses what the store would not send.
	if _, err := x.DB().ExecContext(ctx, `UPDATE client_tokens SET scope='root' WHERE id=?`, userID+"sc-all"); err == nil {
		t.Fatal("the database accepted an unknown scope")
	}
	list, err := x.ListClientTokensByUser(ctx, userID)
	if err != nil || len(list) != 4 {
		t.Fatalf("list: %v %+v", err, list)
	}
	got := map[string]string{}
	for _, ct := range list {
		got[ct.Name] = ct.Scope
	}
	if got["sc-fwd"] != "forward" || got["sc-pub"] != "publish" || got["sc-def"] != "all" {
		t.Fatalf("listed scopes: %v", got)
	}

	// Sign-in requests.
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	code := strings.ToUpper(strings.ReplaceAll(userID, "-", ""))
	code = (code + "XXXXXXXX")[:8]
	req := ClientLoginRequest{DeviceCodeHash: "dh-" + userID, UserCode: code, SourceIP: "203.0.113.9", Scope: "forward", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}
	_, _ = x.DB().ExecContext(ctx, `DELETE FROM client_login_requests`)
	if ok, err := x.InsertClientLogin(ctx, req, 100, 100); err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	r, err := x.GetClientLoginByUserCode(ctx, code, t0)
	if err != nil || r.Scope != "forward" {
		t.Fatalf("request scope %q %v", r.Scope, err)
	}
	if ok, err := x.DecideClientLogin(ctx, code, ClientLoginApproved, userID, "", "publish", t0); err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	r, _ = x.GetClientLoginByDeviceHash(ctx, "dh-"+userID)
	if r.Scope != "publish" {
		t.Fatalf("decided scope %q", r.Scope)
	}
	// Collected: the token gets the request's scope.
	ct := ClientToken{ID: userID + "-col", UserID: userID, Name: "col", TokenHash: "h-" + userID + "-col", Scope: r.Scope}
	if ok, err := x.CollectClientLogin(ctx, "dh-"+userID, t0, ct); err != nil || !ok {
		t.Fatalf("collect: %v %v", ok, err)
	}
	if got, _ := x.GetClientTokenByHash(ctx, "h-"+userID+"-col"); got.Scope != "publish" {
		t.Fatalf("collected scope %q", got.Scope)
	}
}
