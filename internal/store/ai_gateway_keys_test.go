package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/ankoehn/burrow/internal/audit"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ankoehn/burrow/internal/auth"
)

func TestGatewayKeys(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	userA := mustCreateUser(t, s, "gk-a@x", "user").ID
	userB := mustCreateUser(t, s, "gk-b@x", "user").ID
	admin := mustCreateUser(t, s, "gk-admin@x", "admin").ID

	k, plain, err := s.CreateGatewayKey(ctx, userA, "  laptop ", []string{"burrow-simple", "zai/*", "burrow-simple"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "bgw_") || len(plain) != 4+43 || strings.ContainsAny(plain[4:], "+/=") {
		t.Fatalf("plaintext has the wrong shape (length %d)", len(plain))
	}
	if k.ID == "" || k.Name != "laptop" || k.KeyPrefix != plain[:8] || k.UserID != userA || k.CreatedAt.IsZero() ||
		k.LastUsed != nil || k.RevokedAt != nil || fmt.Sprint(k.AllowedModels) != "[burrow-simple zai/*]" {
		t.Fatalf("view: %+v", k)
	}
	if dump := fmt.Sprintf("%+v", k); strings.Contains(dump, plain) || strings.Contains(dump, auth.HashToken(plain)) {
		t.Fatal("the view carries the key or its hash")
	}
	// Only the hash is stored.
	row, err := s.q.GetAIGatewayKey(ctx, k.ID)
	if err != nil || row.KeyHash != auth.HashToken(plain) || strings.Contains(fmt.Sprintf("%+v", row), plain) {
		t.Fatalf("stored row: %v", err)
	}

	got, ok, err := s.ValidateGatewayKey(ctx, plain)
	if err != nil || !ok || got.ID != k.ID || got.UserID != userA || fmt.Sprint(got.AllowedModels) != "[burrow-simple zai/*]" {
		t.Fatalf("validate: %v %v %+v", err, ok, got)
	}
	if row, _ := s.q.GetAIGatewayKey(ctx, k.ID); row.LastUsed == nil {
		t.Fatal("LastUsed not set by a successful validation")
	}
	for _, presented := range []string{
		"", "sk-other", "bgw_", "bgw_" + strings.Repeat("A", 43), plain[:len(plain)-1], plain + "A",
		strings.ToUpper(plain[:4]) + plain[4:], " " + plain, auth.HashToken(plain),
	} {
		if _, ok, err := s.ValidateGatewayKey(ctx, presented); ok || err != nil {
			t.Errorf("a wrong key (length %d) was accepted or failed: ok=%v err=%v", len(presented), ok, err)
		}
	}

	k2, plain2, err := s.CreateGatewayKey(ctx, userB, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain2 == plain || k2.ID == k.ID || k2.AllowedModels == nil || len(k2.AllowedModels) != 0 {
		t.Fatalf("second key: %+v", k2)
	}

	for name, call := range map[string]func() error{
		"empty name":      func() error { _, _, err := s.CreateGatewayKey(ctx, userA, " ", nil); return err },
		"long name":       func() error { _, _, err := s.CreateGatewayKey(ctx, userA, strings.Repeat("n", 121), nil); return err },
		"control in name": func() error { _, _, err := s.CreateGatewayKey(ctx, userA, "a\nb", nil); return err },
		"bad entry":       func() error { _, _, err := s.CreateGatewayKey(ctx, userA, "x", []string{"ok-name", "*/x"}); return err },
		"too many entries": func() error {
			many := make([]string, 65)
			for i := range many {
				many[i] = fmt.Sprintf("model-%d", i)
			}
			_, _, err := s.CreateGatewayKey(ctx, userA, "x", many)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}

	mine, err := s.ListGatewayKeys(ctx, userA, "user")
	if err != nil || len(mine) != 1 || mine[0].ID != k.ID || mine[0].LastUsed == nil {
		t.Fatalf("list own: %v %+v", err, mine)
	}
	none, err := s.ListGatewayKeys(ctx, admin, "user") // a caller without keys
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("list empty: %v %#v", err, none)
	}
	all, err := s.ListGatewayKeys(ctx, admin, "admin")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %v %+v", err, all)
	}

	// Someone else's key looks like a missing one.
	if err := s.RevokeGatewayKey(ctx, userB, "user", k.ID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("revoke by a stranger err = %v", err)
	}
	if err := s.RevokeGatewayKey(ctx, userA, "user", "missing"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("revoke missing err = %v", err)
	}
	if _, ok, _ := s.ValidateGatewayKey(ctx, plain); !ok {
		t.Fatal("a refused revoke disabled the key")
	}
	if err := s.RevokeGatewayKey(ctx, userA, "user", k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ValidateGatewayKey(ctx, plain); ok || err != nil {
		t.Fatalf("revoked key: ok=%v err=%v", ok, err)
	}
	if mine, _ := s.ListGatewayKeys(ctx, userA, "user"); len(mine) != 1 || mine[0].RevokedAt == nil {
		t.Fatalf("revoked key in list: %+v", mine)
	}
	if err := s.RevokeGatewayKey(ctx, admin, "admin", k2.ID); err != nil {
		t.Fatalf("admin revoke: %v", err)
	}
	if _, ok, _ := s.ValidateGatewayKey(ctx, plain2); ok {
		t.Fatal("key revoked by an admin still validates")
	}
}

func TestNewGatewayKey_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		k, err := newGatewayKey()
		if err != nil || len(k) != gatewayKeyLen || seen[k] {
			t.Fatalf("key %d: err=%v length=%d repeated=%v", i, err, len(k), seen[k])
		}
		seen[k] = true
	}
}

// The system revokes a key by its id alone (a gateway_key budget with action
// disable_key): the key stops validating at once and stays listed as revoked.
func TestRevokeGatewayKeyByID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, s, "gk-sys@x", "user").ID
	k, plain, err := s.CreateGatewayKey(ctx, owner, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := &auditSink{}
	s.SetAuditLogger(sink)
	if err := func() error { _, err := s.RevokeGatewayKeyByID(ctx, "missing", "bud-1"); return err }(); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("missing key err = %v", err)
	}
	if len(sink.ev) != 0 {
		t.Fatalf("a revoke that did nothing was audited: %+v", sink.ev)
	}
	if revoked, err := s.RevokeGatewayKeyByID(ctx, k.ID, "bud-1"); err != nil || !revoked {
		t.Fatalf("revoke: revoked=%v err=%v", revoked, err)
	}
	// Audited as the system's doing, with the budget that caused it and
	// nothing of the key but its id and name.
	if len(sink.ev) != 1 {
		t.Fatalf("audit events = %+v, want one", sink.ev)
	}
	ev := sink.ev[0]
	if ev.Action != audit.ActionAIGatewayKeyRevoke || ev.ActorID != "system" || ev.SubjectID != k.ID || ev.SubjectLabel != "ci" || ev.Result != "ok" ||
		string(ev.Payload) != `{"budget_id":"bud-1","reason":"budget_exceeded"}` {
		t.Fatalf("audit event = %+v payload %s", ev, ev.Payload)
	}
	if strings.Contains(fmt.Sprintf("%+v %s", ev, ev.Payload), plain) || strings.Contains(fmt.Sprintf("%+v %s", ev, ev.Payload), plain[4:20]) {
		t.Fatal("the audit event carries key material")
	}
	if _, ok, err := s.ValidateGatewayKey(ctx, plain); ok || err != nil {
		t.Fatalf("revoked key: ok=%v err=%v", ok, err)
	}
	mine, _ := s.ListGatewayKeys(ctx, owner, "user")
	if len(mine) != 1 || mine[0].RevokedAt == nil {
		t.Fatalf("list: %+v", mine)
	}
	first := *mine[0].RevokedAt
	if revoked, err := s.RevokeGatewayKeyByID(ctx, k.ID, "bud-1"); err != nil || revoked {
		t.Fatalf("second revoke: revoked=%v err=%v, want nothing changed", revoked, err)
	}
	if len(sink.ev) != 1 {
		t.Fatalf("revoking a revoked key was audited again: %+v", sink.ev)
	}
	if again, _ := s.ListGatewayKeys(ctx, owner, "user"); !again[0].RevokedAt.Equal(first) {
		t.Fatalf("a second revoke moved revoked_at: %v -> %v", first, again[0].RevokedAt)
	}
}

// Two budgets on one key cross at the same moment, or a budget crosses while
// its owner revokes the key by hand: the key is revoked once, and the revoke
// is audited once, by whoever changed the row.
func TestRevokeGatewayKeyByID_Concurrent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, s, "gk-race@x", "user").ID
	sink := &auditSink{}
	s.SetAuditLogger(sink)
	for round := 0; round < 5; round++ {
		k, _, err := s.CreateGatewayKey(ctx, owner, "ci", nil)
		if err != nil {
			t.Fatal(err)
		}
		sink.mu.Lock()
		sink.ev = nil
		sink.mu.Unlock()
		const n = 8
		var wg sync.WaitGroup
		var revoked atomic.Int32
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				ok, err := s.RevokeGatewayKeyByID(ctx, k.ID, fmt.Sprintf("bud-%d", i))
				if err != nil {
					t.Error(err)
				}
				if ok {
					revoked.Add(1)
				}
			}(i)
		}
		// In every other round the owner revokes by hand in the same instant.
		byHand := round%2 == 1
		if byHand {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := s.RevokeGatewayKey(ctx, owner, "user", k.ID); err != nil {
					t.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()
		sink.mu.Lock()
		events := len(sink.ev)
		sink.mu.Unlock()
		// The manual revoke is audited by its handler, not by the store: the
		// store's events are the system's, and there is one per revoke that
		// changed the row (none when the owner was first).
		got := int(revoked.Load())
		if got > 1 || events != got || (!byHand && got != 1) {
			t.Fatalf("round %d (by hand: %v): %d system revokes reported, %d audit events; want exactly one of each among the system's revokes, none when the owner was first",
				round, byHand, got, events)
		}
		key, err := s.q.GetAIGatewayKey(ctx, k.ID)
		if err != nil || key.RevokedAt == nil {
			t.Fatalf("round %d: the key is not revoked (%v)", round, err)
		}
	}
}
