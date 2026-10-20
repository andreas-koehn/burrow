package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// Round 2: each host accepts only its own kind of session. A services
// session lives no longer than the dashboard session that handed it off, and
// cannot be made from one that is gone.
func TestSessionKinds(t *testing.T) {
	ctx := context.Background()
	st, sqlDB := newStoreWithDB(t)
	q := db.Wrap(sqlDB)
	if err := q.CreateUser(ctx, db.User{ID: "u1", Email: "a@x", PasswordHash: "h", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	dash, err := st.CreateSession(ctx, "u1", "ua", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := st.CreateServicesSession(ctx, "u1", "ua", "127.0.0.1", dash)
	if err != nil {
		t.Fatal(err)
	}
	if uid, err := st.ValidateSession(ctx, dash); err != nil || uid != "u1" {
		t.Fatalf("dashboard session on the dashboard: %q %v", uid, err)
	}
	if _, err := st.ValidateSession(ctx, svc); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("services session taken as a dashboard session: %v", err)
	}
	if uid, err := st.ValidateServicesSession(ctx, svc); err != nil || uid != "u1" {
		t.Fatalf("services session on the services host: %q %v", uid, err)
	}
	if _, err := st.ValidateServicesSession(ctx, dash); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("dashboard session taken as a services session: %v", err)
	}
	row, err := q.GetSession(ctx, svc)
	if err != nil || row.Kind != db.SessionKindServices || row.ParentID != dash {
		t.Fatalf("services session row %+v (%v)", row, err)
	}

	// No longer than the parent: a parent with an hour left gives an hour.
	if err := q.CreateSession(ctx, db.Session{ID: "short", UserID: "u1", ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateServicesSession(ctx, "u1", "ua", "127.0.0.1", "short")
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := q.GetSession(ctx, child); row.ExpiresAt.After(time.Now().UTC().Add(time.Hour + time.Minute)) {
		t.Fatalf("services session expires %v, after its parent", row.ExpiresAt)
	}
	// Not from a parent that is gone, a services session, or another user's.
	if err := q.CreateUser(ctx, db.User{ID: "u2", Email: "b@x", PasswordHash: "h", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"no-such-session", svc} {
		if _, err := st.CreateServicesSession(ctx, "u1", "ua", "127.0.0.1", parent); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("services session from parent %q: %v", parent, err)
		}
	}
	if _, err := st.CreateServicesSession(ctx, "u2", "ua", "127.0.0.1", dash); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("services session of u2 from u1's session: %v", err)
	}

	// Logout of the dashboard session ends the services session.
	if err := st.DeleteSession(ctx, dash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ValidateServicesSession(ctx, svc); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("services session after the dashboard logout: %v", err)
	}
}

// Fix round 1: ending a dashboard session ends the services sessions it
// handed off, and each of them leaves its own session.delete row, under the
// hash of its id, whether the parent ended by logout or by a revoke.
func TestSessionKinds_CascadeIsAudited(t *testing.T) {
	ctx := context.Background()
	st, sqlDB := newStoreWithDB(t)
	if err := db.Wrap(sqlDB).CreateUser(ctx, db.User{ID: "u1", Email: "a@x", PasswordHash: "h", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	sink := &auditSink{}
	st.SetAuditLogger(sink)
	deleted := func() map[string]bool {
		out := map[string]bool{}
		for _, e := range sink.ev {
			if e.Action == audit.ActionSessionDelete {
				out[e.SubjectID] = true
			}
		}
		return out
	}
	for _, end := range []func(parent string) error{
		func(parent string) error { return st.DeleteSession(ctx, parent) },
		func(parent string) error { return st.RevokeSession(ctx, parent, "u1") },
	} {
		sink.ev = nil
		parent, _ := st.CreateSession(ctx, "u1", "ua", "127.0.0.1")
		a, _ := st.CreateServicesSession(ctx, "u1", "ua", "127.0.0.1", parent)
		b, _ := st.CreateServicesSession(ctx, "u1", "ua", "127.0.0.1", parent)
		if err := end(parent); err != nil {
			t.Fatal(err)
		}
		got := deleted()
		for _, id := range []string{parent, a, b} {
			if !got[audit.SessionRef(id)] {
				t.Errorf("no session.delete row for %s (rows: %v)", audit.SessionRef(id), got)
			}
			if got[id] {
				t.Errorf("a session.delete row names the raw id %s", id)
			}
		}
		if len(got) != 3 {
			t.Errorf("%d session.delete rows, want 3", len(got))
		}
	}
}
