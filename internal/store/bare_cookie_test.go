package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A session read from the bare burrow_session cookie while Secure cookies are
// on counts only when it was created before the bare-cookie cutoff (the first
// start of v0.12.1): such a cookie is a browser's from before the upgrade. One
// created later was planted by another host of the site with Domain=, since
// Burrow writes only the __Host- name now.
func TestValidateBareSession_Cutoff(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.SeedAdmin(ctx, "admin@x", "pw"); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUserByEmail(ctx, "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := s.CreateSession(ctx, u.ID, "ua", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}

	// No cutoff known (start-up did not run): the safe side, refused.
	if _, err := s.ValidateBareSession(ctx, sid); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("no cutoff: %v, want ErrUnauthorized", err)
	}
	// Created before the cutoff: an old browser's cookie, accepted.
	s.SetBareCookieCutoff(time.Now().Add(time.Hour))
	if uid, err := s.ValidateBareSession(ctx, sid); err != nil || uid != u.ID {
		t.Fatalf("old session: %q %v", uid, err)
	}
	// Created after the cutoff: refused, though the session itself is live.
	s.SetBareCookieCutoff(time.Now().Add(-time.Hour))
	if _, err := s.ValidateBareSession(ctx, sid); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("new session: %v, want ErrUnauthorized", err)
	}
	if uid, err := s.ValidateSession(ctx, sid); err != nil || uid != u.ID {
		t.Fatalf("the same session under the __Host- name: %q %v", uid, err)
	}
	// The usual checks still apply to an old one.
	s.SetBareCookieCutoff(time.Now().Add(time.Hour))
	if _, err := s.ValidateBareSession(ctx, "no-such-session"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown session: %v", err)
	}
	svc, err := s.CreateServicesSession(ctx, u.ID, "ua", "1.2.3.4", sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateBareSession(ctx, svc); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("services session: %v", err)
	}
}
