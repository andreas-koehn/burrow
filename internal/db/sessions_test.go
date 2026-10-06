package db

import (
	"context"
	"testing"
	"time"
)

// TestDeleteExpiredSessions_PastAndFuture verifies that DeleteExpiredSessions
// removes only expired rows and leaves future rows intact.
func TestDeleteExpiredSessions_PastAndFuture(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)

	_ = x.CreateUser(ctx, User{ID: "u1", Email: "expire@test", PasswordHash: "h", Role: "admin"})

	// One row already expired, one not yet expired.
	_ = x.CreateSession(ctx, Session{ID: "past", UserID: "u1", ExpiresAt: time.Now().UTC().Add(-time.Hour)})
	_ = x.CreateSession(ctx, Session{ID: "future", UserID: "u1", ExpiresAt: time.Now().UTC().Add(time.Hour)})

	n, err := x.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row deleted, got %d", n)
	}

	// Past row must be gone.
	if _, err := x.GetSession(ctx, "past"); err != ErrNotFound {
		t.Fatalf("expired session should be gone, got %v", err)
	}
	// Future row must survive.
	if s, err := x.GetSession(ctx, "future"); err != nil || s.UserID != "u1" {
		t.Fatalf("live session should still exist: %+v %v", s, err)
	}
}

// TestDeleteExpiredSessions_OffsetRobust tests that a session created via the
// normal store code path (time.Time binding through modernc/sqlite) is purged
// correctly. This is the offset-robustness test: it round-trips a real
// store-path ExpiresAt and confirms it is comparable to the formatted NOW used
// by DeleteExpiredSessions, regardless of modernc/sqlite's serialisation format.
func TestDeleteExpiredSessions_OffsetRobust(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)

	_ = x.CreateUser(ctx, User{ID: "u2", Email: "robust@test", PasswordHash: "h", Role: "admin"})

	// Store via the same path as store.CreateSession: bind time.Time directly.
	// modernc/sqlite v1.50.1 serialises this via time.Time.String()
	// ("YYYY-MM-DD HH:MM:SS.fffffffff +0000 UTC"); DeleteExpiredSessions handles
	// that format (see the comment on DeleteExpiredSessions in sessions.go).
	expiredAt := time.Now().UTC().Add(-2 * time.Minute)
	_ = x.CreateSession(ctx, Session{ID: "robust-expired", UserID: "u2", ExpiresAt: expiredAt})

	n, err := x.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row purged, got %d (offset-format mismatch?)", n)
	}
	if _, err := x.GetSession(ctx, "robust-expired"); err != ErrNotFound {
		t.Fatalf("store-path expired session should be purged, got %v", err)
	}
}

// TestDeleteExpiredSessions_SubSecondBoundary is the regression test for the
// cfce87f bug: the old implementation truncated "now" to whole-second precision
// ("YYYY-MM-DD HH:MM:SS") and compared it lexically against the stored
// time.Time.String() text ("YYYY-MM-DD HH:MM:SS.fffffffff +0000 UTC"). A row
// expiring at e.g. "2026-05-18 14:09:46.0025473 +0000 UTC" was NOT deleted when
// sqliteNow() produced "2026-05-18 14:09:46", because the stored value is
// lexically greater ("...46.002..." > "...46"). This test creates a session
// expiring 50 ms in the past and a session expiring 500 ms in the future, then
// asserts the past row is deleted and the future row is retained.
func TestDeleteExpiredSessions_SubSecondBoundary(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)

	_ = x.CreateUser(ctx, User{ID: "u4", Email: "subsec@test", PasswordHash: "h", Role: "admin"})

	// Expired 50 ms ago — within the current integer second.
	// The old truncated comparison left this row undeleted (the bug).
	_ = x.CreateSession(ctx, Session{
		ID:        "sub-past-50ms",
		UserID:    "u4",
		ExpiresAt: time.Now().UTC().Add(-50 * time.Millisecond),
	})
	// Expires 500 ms from now — must survive.
	_ = x.CreateSession(ctx, Session{
		ID:        "sub-future-500ms",
		UserID:    "u4",
		ExpiresAt: time.Now().UTC().Add(500 * time.Millisecond),
	})

	n, err := x.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row deleted (sub-second past), got %d", n)
	}

	// The 50ms-past row must be gone.
	if _, err := x.GetSession(ctx, "sub-past-50ms"); err != ErrNotFound {
		t.Fatalf("50ms-past session should be deleted, got %v", err)
	}
	// The 500ms-future row must still exist.
	if s, err := x.GetSession(ctx, "sub-future-500ms"); err != nil || s.UserID != "u4" {
		t.Fatalf("500ms-future session should survive: %+v %v", s, err)
	}
}

// TestDeleteExpiredSessions_Empty ensures the function is a no-op (returns 0, nil)
// when there are no expired sessions.
func TestDeleteExpiredSessions_Empty(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)

	_ = x.CreateUser(ctx, User{ID: "u3", Email: "empty@test", PasswordHash: "h", Role: "admin"})
	_ = x.CreateSession(ctx, Session{ID: "only-live", UserID: "u3", ExpiresAt: time.Now().UTC().Add(24 * time.Hour)})

	n, err := x.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 deleted, got %d", n)
	}
}

// TestDeleteExpiredSessions_Shared runs the check the Postgres test runs too.
func TestDeleteExpiredSessions_Shared(t *testing.T) {
	checkDeleteExpiredSessions(t, testDB(t), "u-sess")
}

// checkDeleteExpiredSessions: sessions whose expires_at has passed are
// purged, the others stay, on whatever database x is.
func checkDeleteExpiredSessions(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	now := time.Now().UTC()
	for id, at := range map[string]time.Time{
		"long-gone":  now.Add(-48 * time.Hour),
		"just-gone":  now.Add(-2 * time.Second),
		"soon":       now.Add(time.Minute),
		"next-month": now.Add(30 * 24 * time.Hour),
	} {
		if err := x.CreateSession(ctx, Session{ID: userID + "-" + id, UserID: userID, ExpiresAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := x.DeleteExpiredSessions(ctx)
	if err != nil || n < 2 {
		t.Fatalf("DeleteExpiredSessions: n=%d err=%v, want at least the 2 expired ones", n, err)
	}
	for id, gone := range map[string]bool{"long-gone": true, "just-gone": true, "soon": false, "next-month": false} {
		_, err := x.GetSession(ctx, userID+"-"+id)
		if gone && err != ErrNotFound {
			t.Errorf("%s: expired session still there (%v)", id, err)
		}
		if !gone && err != nil {
			t.Errorf("%s: live session purged (%v)", id, err)
		}
	}
	if n, err := x.DeleteExpiredSessions(ctx); err != nil || n != 0 {
		t.Errorf("second purge: n=%d err=%v", n, err)
	}
}
