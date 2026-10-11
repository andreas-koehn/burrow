package store

import (
	"context"
	"errors"
	"testing"
)

// A suspended user's client token opens no session: the live ones are closed
// when the user is suspended, and a client that reconnects with the token it
// still holds is turned away until the user is active again.
func TestSuspendedUsersTokenDoesNotAuthenticate(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "a@x", "password1", "user")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.IssueClientToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	auth := func() error {
		if _, err := s.Authenticate(ctx, token); err != nil {
			return err
		}
		if _, _, err := s.AuthenticateNamed(ctx, token); err != nil {
			return err
		}
		_, _, _, err := s.AuthenticateToken(ctx, token)
		return err
	}
	if err := auth(); err != nil {
		t.Fatalf("active user: %v", err)
	}
	if err := s.SetUserStatus(ctx, u.ID, "suspended"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Authenticate for a suspended user: %v, want ErrUnauthorized", err)
	}
	if _, _, err := s.AuthenticateNamed(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("AuthenticateNamed for a suspended user: %v, want ErrUnauthorized", err)
	}
	if _, _, _, err := s.AuthenticateToken(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("AuthenticateToken for a suspended user: %v, want ErrUnauthorized", err)
	}
	if err := s.SetUserStatus(ctx, u.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if err := auth(); err != nil {
		t.Errorf("reactivated user: %v", err)
	}
}
