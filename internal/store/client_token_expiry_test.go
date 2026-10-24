package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
)

// A client token with an expiry opens no session once it has passed, on every
// path that verifies a token; one without an expiry never lapses.
func TestExpiredClientTokenDoesNotAuthenticate(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mustCreateUser(t, s, "exp@x", "user")

	future := time.Now().Add(time.Hour).UTC()
	live, err := s.IssueClientTokenUntil(ctx, u.ID, "live", &future, "")
	if err != nil {
		t.Fatal(err)
	}
	never, err := s.IssueClientToken(ctx, u.ID, "never")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).UTC()
	dead, err := s.IssueClientTokenUntil(ctx, u.ID, "dead", &past, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, tok := range []string{live, never} {
		if _, _, _, err := s.AuthenticateToken(ctx, tok); err != nil {
			t.Errorf("valid token refused: %v", err)
		}
	}
	if _, err := s.Authenticate(ctx, dead); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Authenticate expired: %v", err)
	}
	if _, _, err := s.AuthenticateNamed(ctx, dead); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("AuthenticateNamed expired: %v", err)
	}
	if _, _, _, err := s.AuthenticateToken(ctx, dead); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("AuthenticateToken expired: %v", err)
	}

	toks, err := s.ListClientTokens(ctx, u.ID)
	if err != nil || len(toks) != 3 {
		t.Fatalf("list: %v %+v", err, toks)
	}
	byName := map[string]*time.Time{}
	for _, tk := range toks {
		byName[tk.Name] = tk.ExpiresAt
	}
	if byName["never"] != nil || byName["live"] == nil || !byName["live"].Equal(future.Truncate(time.Second)) && !byName["live"].Equal(future) {
		t.Errorf("expiries = %v", byName)
	}
}

func TestIssueClientToken_AuditCarriesExpiry(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mustCreateUser(t, s, "aud@x", "user")
	sink := &auditSink{}
	s.SetAuditLogger(sink)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := s.IssueClientTokenUntil(ctx, u.ID, "laptop", &exp, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueClientToken(ctx, u.ID, "forever"); err != nil {
		t.Fatal(err)
	}
	if len(sink.ev) != 2 || sink.ev[0].Action != audit.ActionTokenMint {
		t.Fatalf("events = %+v", sink.ev)
	}
	var p map[string]any
	if err := json.Unmarshal(sink.ev[0].Payload, &p); err != nil || p["expires_at"] != exp.Format(time.RFC3339) {
		t.Errorf("payload = %s (%v)", sink.ev[0].Payload, err)
	}
	if err := json.Unmarshal(sink.ev[1].Payload, &p); err != nil || p["expires_at"] != nil {
		t.Errorf("no-expiry payload = %s (%v)", sink.ev[1].Payload, err)
	}
}
