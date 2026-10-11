package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return New(d)
}

func TestSeedAdminIdempotentAndAuth(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.SeedAdmin(ctx, "", ""); err != nil {
		t.Fatalf("empty creds → no-op: %v", err)
	}
	if err := s.SeedAdmin(ctx, "admin@x", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedAdmin(ctx, "admin@x", "pw"); err != nil {
		t.Fatalf("second seed (users exist) → no-op: %v", err)
	}
	u, err := s.GetUserByEmail(ctx, "admin@x")
	if err != nil || u.Role != "admin" {
		t.Fatalf("admin not seeded: %+v %v", u, err)
	}
	ok, _ := s.VerifyUserPassword(ctx, "admin@x", "pw")
	if !ok {
		t.Fatal("seeded password must verify")
	}
}

func TestTokenAuthenticateAndTunnelPersist(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_ = s.SeedAdmin(ctx, "a@x", "pw")
	u, _ := s.GetUserByEmail(ctx, "a@x")
	pt, err := s.IssueClientToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.Authenticate(ctx, pt)
	if err != nil || uid != u.ID {
		t.Fatalf("Authenticate valid: uid=%s err=%v", uid, err)
	}
	if _, err := s.Authenticate(ctx, "bur_bogus"); err == nil {
		t.Fatal("unknown token must fail")
	}
	if err := s.SaveTunnel(ctx, u.ID, &tunnelFixture); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTunnelSeen(ctx, "tn1"); err != nil {
		t.Fatal(err)
	}
}

// minimal stand-in matching the server.Tunnel shape SaveTunnel needs.
var tunnelFixture = serverTunnel{ID: "tn1", Name: "web", Type: "tcp", RemotePort: 9000, LocalAddr: "127.0.0.1:3000"}

type serverTunnel = SaveTunnelArg

// newStoreWithDB opens a fresh test database and returns both the *Store and
// the underlying *sql.DB so tests can insert rows bypassing the store layer.
func newStoreWithDB(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return New(d), d
}

func TestRevokeThenAuthenticateFails(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.SeedAdmin(ctx, "a@x", "pw"); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUserByEmail(ctx, "a@x")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := s.IssueClientToken(ctx, u.ID, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, pt); err != nil {
		t.Fatalf("Authenticate before revoke: %v", err)
	}
	tokens, err := s.ListClientTokens(ctx, u.ID)
	if err != nil || len(tokens) == 0 {
		t.Fatalf("ListClientTokens: %v %v", tokens, err)
	}
	if err := s.RevokeClientToken(ctx, tokens[0].ID, u.ID); err != nil {
		t.Fatalf("RevokeClientToken: %v", err)
	}
	if _, err := s.Authenticate(ctx, pt); err == nil {
		t.Fatal("Authenticate after revoke must error")
	}
	err = s.RevokeClientToken(ctx, "does-not-exist", u.ID)
	if err == nil {
		t.Fatal("revoking non-existent token must return error")
	}
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected db.ErrNotFound, got %v", err)
	}
}

func TestVerifyUserPasswordWrong(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.SeedAdmin(ctx, "a@x", "pw"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.VerifyUserPassword(ctx, "a@x", "WRONG")
	if ok || err != nil {
		t.Fatalf("wrong password: ok=%v err=%v", ok, err)
	}
	ok, err = s.VerifyUserPassword(ctx, "missing@x", "pw")
	if ok || err != nil {
		t.Fatalf("unknown email: ok=%v err=%v", ok, err)
	}
}

// TestSeedAdminB17_DoubleSeedNoOp proves the B17 fix: calling SeedAdmin twice
// with the same email is a safe no-op (ON CONFLICT DO NOTHING is idempotent),
// no duplicate user is created, and authentication still works with the original
// password (the second call's new hash is discarded by the DB).
func TestSeedAdminB17_DoubleSeedNoOp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// Empty creds = no-op, no error.
	if err := s.SeedAdmin(ctx, "", ""); err != nil {
		t.Fatalf("empty creds must be no-op: %v", err)
	}
	if err := s.SeedAdmin(ctx, "admin@x", ""); err != nil {
		t.Fatalf("empty password must be no-op: %v", err)
	}
	if err := s.SeedAdmin(ctx, "", "password1"); err != nil {
		t.Fatalf("empty email must be no-op: %v", err)
	}

	// First real seed.
	if err := s.SeedAdmin(ctx, "admin@x", "password1"); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	// Second call with same email must not error (ON CONFLICT DO NOTHING).
	if err := s.SeedAdmin(ctx, "admin@x", "password2"); err != nil {
		t.Fatalf("second seed (same email) must be no-op: %v", err)
	}
	// Exactly one user must exist.
	n, err := s.q.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want exactly 1 user after double-seed, got %d", n)
	}
	// Original password still works (second seed's hash was discarded).
	ok, err := s.VerifyUserPassword(ctx, "admin@x", "password1")
	if !ok || err != nil {
		t.Fatalf("original password must still verify: ok=%v err=%v", ok, err)
	}
}

// TestChangePassword covers the success path, wrong current password (ErrInvalidCredentials),
// and minimum-length enforcement (ErrPasswordTooShort).
func TestChangePassword(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.SeedAdmin(ctx, "a@x", "oldpassword"); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUserByEmail(ctx, "a@x")
	if err != nil {
		t.Fatal(err)
	}

	// Wrong current password → ErrInvalidCredentials.
	if err := s.ChangePassword(ctx, u.ID, "wrongpassword", "newpassword1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current → ErrInvalidCredentials, got %v", err)
	}

	// New password too short → ErrPasswordTooShort.
	if err := s.ChangePassword(ctx, u.ID, "oldpassword", "short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short new password → ErrPasswordTooShort, got %v", err)
	}

	// Success path: new password works, old does not.
	if err := s.ChangePassword(ctx, u.ID, "oldpassword", "newpassword1"); err != nil {
		t.Fatalf("change password success: %v", err)
	}
	ok, err := s.VerifyUserPassword(ctx, "a@x", "newpassword1")
	if !ok || err != nil {
		t.Fatalf("new password must verify: ok=%v err=%v", ok, err)
	}
	ok, err = s.VerifyUserPassword(ctx, "a@x", "oldpassword")
	if ok || err != nil {
		t.Fatalf("old password must not verify after change: ok=%v err=%v", ok, err)
	}
}

// TestCreateUserAndListUsers covers success, duplicate-email → ErrEmailConflict,
// short password → ErrPasswordTooShort, bad role → ErrInvalidRole,
// and ListUsers never leaking password_hash.
func TestCreateUserAndListUsers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// Bad role.
	if _, err := s.CreateUser(ctx, "x@x", "password1", "superuser"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("bad role → ErrInvalidRole, got %v", err)
	}
	// Short password.
	if _, err := s.CreateUser(ctx, "x@x", "short", "user"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short password → ErrPasswordTooShort, got %v", err)
	}
	// Success.
	created, err := s.CreateUser(ctx, "new@x", "password1", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if created.ID == "" || created.Email != "new@x" || created.Role != "user" {
		t.Fatalf("create user result unexpected: %+v", created)
	}
	// Duplicate email → ErrEmailConflict.
	if _, err := s.CreateUser(ctx, "new@x", "password1", "admin"); !errors.Is(err, ErrEmailConflict) {
		t.Fatalf("duplicate email → ErrEmailConflict, got %v", err)
	}
	// ListUsersPage must not leak password_hash (PasswordHash field is zero-value).
	users, _, err := s.ListUsersPage(ctx, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("want 1 user, got %d", len(users))
	}
	if users[0].PasswordHash != "" {
		t.Fatalf("ListUsersPage must not populate PasswordHash, got %q", users[0].PasswordHash)
	}
}

// TestDeleteUserCascade proves that deleting a user removes associated
// sessions, client_tokens, and tunnels via ON DELETE CASCADE.
func TestDeleteUserCascade(t *testing.T) {
	ctx := context.Background()
	s, sqlDB := newStoreWithDB(t)
	q := db.Wrap(sqlDB)

	// Create user with sessions, token, tunnel.
	u, err := s.CreateUser(ctx, "del@x", "password1", "user")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := s.CreateSession(ctx, u.ID, "ua", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := s.IssueClientToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTunnel(ctx, u.ID, &SaveTunnelArg{ID: "tn-del", Name: "web", Type: "tcp", RemotePort: 9001, LocalAddr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}

	// Verify the token authenticates before the user is deleted.
	if _, err := s.Authenticate(ctx, pt); err != nil {
		t.Fatalf("token must authenticate before user delete: %v", err)
	}

	// Delete user.
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	// Session must be gone (cascade).
	_, err = q.GetSession(ctx, sid)
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("session must cascade-delete: %v", err)
	}
	// Token must be gone (cascade): after user delete, Authenticate must fail.
	if _, err := s.Authenticate(ctx, pt); err == nil {
		t.Fatal("token must be invalid after user cascade-delete")
	}
	// Tunnel must be gone (cascade) — check via raw query.
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM tunnels WHERE id=?`, "tn-del").Scan(&n); err != nil || n != 0 {
		t.Fatalf("tunnel must cascade-delete: n=%d err=%v", n, err)
	}

	// Delete non-existent user → db.ErrNotFound.
	if err := s.DeleteUser(ctx, "does-not-exist"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("delete non-existent → db.ErrNotFound, got %v", err)
	}
}

func TestValidateSessionExpiry(t *testing.T) {
	ctx := context.Background()
	st, sqlDB := newStoreWithDB(t)
	q := db.Wrap(sqlDB)

	// Seed a user directly through the db layer.
	if err := q.CreateUser(ctx, db.User{ID: "u1", Email: "a@x", PasswordHash: "h", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	// Create a valid session via the store.
	id, err := st.CreateSession(ctx, "u1", "ua", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := st.ValidateSession(ctx, id)
	if err != nil || uid != "u1" {
		t.Fatalf("ValidateSession live: uid=%s err=%v", uid, err)
	}

	// Insert an already-expired session directly via the db layer.
	if err := q.CreateSession(ctx, db.Session{
		ID:        "exp",
		UserID:    "u1",
		ExpiresAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// ValidateSession must return ErrUnauthorized for the expired session.
	_, err = st.ValidateSession(ctx, "exp")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired session: expected ErrUnauthorized, got %v", err)
	}

	// The expired session must have been deleted best-effort by ValidateSession.
	_, err = q.GetSession(ctx, "exp")
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expired session should be deleted, got %v", err)
	}
}

// AuthenticateToken names the token a client logged in with, so that the
// relay can record the session on it; AuthenticateNamed keeps its answer.
func TestAuthenticateTokenReturnsTheTokenID(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_ = s.SeedAdmin(ctx, "a@x", "pw")
	u, _ := s.GetUserByEmail(ctx, "a@x")
	pt, err := s.IssueClientToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	mine, err := s.ListClientTokens(ctx, u.ID)
	if err != nil || len(mine) != 1 {
		t.Fatalf("tokens: %v %+v", err, mine)
	}

	uid, tokenID, name, err := s.AuthenticateToken(ctx, pt)
	if err != nil || uid != u.ID || tokenID != mine[0].ID || name != "laptop" {
		t.Fatalf("AuthenticateToken = %q %q %q %v, want %q %q laptop", uid, tokenID, name, err, u.ID, mine[0].ID)
	}
	if uid, name, err := s.AuthenticateNamed(ctx, pt); err != nil || uid != u.ID || name != "laptop" {
		t.Fatalf("AuthenticateNamed = %q %q %v", uid, name, err)
	}
	if _, _, _, err := s.AuthenticateToken(ctx, "bur_bogus"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown token err = %v, want ErrUnauthorized", err)
	}
	if after, _ := s.ListClientTokens(ctx, u.ID); len(after) != 1 || after[0].LastUsed == nil {
		t.Fatalf("AuthenticateToken must touch last_used: %+v", after)
	}

	// The session of that token is recorded and read back for every user.
	rec := db.ClientSessionRecord{OS: "linux", Arch: "amd64", ClientVersion: "0.9.0", RemoteAddr: "10.0.0.5:4242", Served: []string{"web"}}
	if err := s.RecordClientSession(ctx, tokenID, rec); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListAllClientTokens(ctx)
	if err != nil || len(all) != 1 || all[0].ID != tokenID || all[0].LastSeen == nil || all[0].LastOS != "linux" ||
		len(all[0].LastServed) != 1 || all[0].LastServed[0] != "web" {
		t.Fatalf("all tokens: %v %+v", err, all)
	}
	if err := s.RecordClientSession(ctx, "no-such-token", rec); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("record for a revoked token err = %v, want db.ErrNotFound", err)
	}
}
