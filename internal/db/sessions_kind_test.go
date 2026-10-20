package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Round 2 (migration 0038): a session has a kind, and a services session
// names the dashboard session that handed it off. Ending a dashboard session
// ends the services sessions it handed off; signing out everywhere else and
// the suspension sweep still end every session.
func TestSessionKindAndParent(t *testing.T) {
	checkSessionKindAndParent(t, testDB(t), "u-kind")
}

// Migration 0038's Down block removes what its Up block adds.
func TestMigration0038_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0038_v0.12.0_session_kind.sql")
}

func checkSessionKindAndParent(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	exp := time.Now().UTC().Add(time.Hour)
	for _, s := range []Session{
		{ID: userID + "-dash", UserID: userID, ExpiresAt: exp},
		{ID: userID + "-dash2", UserID: userID, ExpiresAt: exp, Kind: SessionKindDashboard},
		{ID: userID + "-svc", UserID: userID, ExpiresAt: exp, Kind: SessionKindServices, ParentID: userID + "-dash"},
		{ID: userID + "-svc2", UserID: userID, ExpiresAt: exp, Kind: SessionKindServices, ParentID: userID + "-dash2"},
		{ID: userID + "-svc-orphan", UserID: userID, ExpiresAt: exp, Kind: SessionKindServices},
	} {
		if err := x.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := x.GetSession(ctx, userID+"-dash")
	if err != nil || got.Kind != SessionKindDashboard || got.ParentID != "" {
		t.Fatalf("dashboard session read back as %+v (%v)", got, err)
	}
	got, err = x.GetSession(ctx, userID+"-svc")
	if err != nil || got.Kind != SessionKindServices || got.ParentID != userID+"-dash" {
		t.Fatalf("services session read back as %+v (%v)", got, err)
	}
	if ls, _ := x.ListSessionsByUser(ctx, userID); len(ls) != 5 {
		t.Fatalf("%d sessions listed, want 5", len(ls))
	}

	// Logout of a dashboard session ends the services session it handed off.
	if children, err := x.DeleteSessionWithChildren(ctx, userID+"-dash"); err != nil || len(children) != 1 || children[0] != userID+"-svc" {
		t.Fatalf("logout: children %v (%v), want [%s]", children, err, userID+"-svc")
	}
	if _, err := x.GetSession(ctx, userID+"-svc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the linked services session survived the logout: %v", err)
	}
	// Revoking one (scoped to its owner) does the same.
	if _, err := x.DeleteSessionForUserWithChildren(ctx, userID+"-dash2", "someone-else"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke of another user's session: %v", err)
	}
	if _, err := x.GetSession(ctx, userID+"-svc2"); err != nil {
		t.Fatalf("a refused revoke ended the services session: %v", err)
	}
	if children, err := x.DeleteSessionForUserWithChildren(ctx, userID+"-dash2", userID); err != nil || len(children) != 1 || children[0] != userID+"-svc2" {
		t.Fatalf("revoke: children %v (%v)", children, err)
	}
	if _, err := x.GetSession(ctx, userID+"-svc2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the linked services session survived the revoke: %v", err)
	}
	// Ending a services session ends only itself.
	if _, err := x.GetSession(ctx, userID+"-svc-orphan"); err != nil {
		t.Fatalf("an unlinked services session went with the others: %v", err)
	}

	// Sign out everywhere else: services sessions go too, also those the
	// kept session handed off.
	for _, s := range []Session{
		{ID: userID + "-keep", UserID: userID, ExpiresAt: exp},
		{ID: userID + "-keep-svc", UserID: userID, ExpiresAt: exp, Kind: SessionKindServices, ParentID: userID + "-keep"},
	} {
		if err := x.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := x.DeleteSessionsByUserExcept(ctx, userID, userID+"-keep"); err != nil || n != 2 {
		t.Fatalf("revoke all but one: %d (%v), want 2", n, err)
	}
	if ls, _ := x.ListSessionsByUser(ctx, userID); len(ls) != 1 || ls[0].ID != userID+"-keep" {
		t.Fatalf("left after revoke all: %+v", ls)
	}
}
