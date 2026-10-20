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
	"time"

	"github.com/ankoehn/burrow/internal/auth"
)

func TestGatewayKeys(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	userA := mustCreateUser(t, s, "gk-a@x", "user").ID
	userB := mustCreateUser(t, s, "gk-b@x", "user").ID
	admin := mustCreateUser(t, s, "gk-admin@x", "admin").ID

	k, plain, err := s.CreateGatewayKey(ctx, userA, "  laptop ", []string{"burrow-simple", "zai/*", "burrow-simple"}, "")
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

	k2, plain2, err := s.CreateGatewayKey(ctx, userB, "ci", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if plain2 == plain || k2.ID == k.ID || k2.AllowedModels == nil || len(k2.AllowedModels) != 0 {
		t.Fatalf("second key: %+v", k2)
	}

	for name, call := range map[string]func() error{
		"empty name": func() error { _, _, err := s.CreateGatewayKey(ctx, userA, " ", nil, ""); return err },
		"long name": func() error {
			_, _, err := s.CreateGatewayKey(ctx, userA, strings.Repeat("n", 121), nil, "")
			return err
		},
		"control in name": func() error { _, _, err := s.CreateGatewayKey(ctx, userA, "a\nb", nil, ""); return err },
		"bad entry": func() error {
			_, _, err := s.CreateGatewayKey(ctx, userA, "x", []string{"ok-name", "*/x"}, "")
			return err
		},
		"too many entries": func() error {
			many := make([]string, 65)
			for i := range many {
				many[i] = fmt.Sprintf("model-%d", i)
			}
			_, _, err := s.CreateGatewayKey(ctx, userA, "x", many, "")
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}

	mine, err := s.ListGatewayKeys(ctx, userA, false)
	if err != nil || len(mine) != 1 || mine[0].ID != k.ID || mine[0].LastUsed == nil {
		t.Fatalf("list own: %v %+v", err, mine)
	}
	none, err := s.ListGatewayKeys(ctx, admin, false) // a caller without keys
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("list empty: %v %#v", err, none)
	}
	all, err := s.ListGatewayKeys(ctx, admin, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %v %+v", err, all)
	}

	// Someone else's key looks like a missing one.
	if err := s.RevokeGatewayKey(ctx, userB, false, k.ID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("revoke by a stranger err = %v", err)
	}
	if err := s.RevokeGatewayKey(ctx, userA, false, "missing"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("revoke missing err = %v", err)
	}
	if _, ok, _ := s.ValidateGatewayKey(ctx, plain); !ok {
		t.Fatal("a refused revoke disabled the key")
	}
	if err := s.RevokeGatewayKey(ctx, userA, false, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ValidateGatewayKey(ctx, plain); ok || err != nil {
		t.Fatalf("revoked key: ok=%v err=%v", ok, err)
	}
	if mine, _ := s.ListGatewayKeys(ctx, userA, false); len(mine) != 1 || mine[0].RevokedAt == nil {
		t.Fatalf("revoked key in list: %+v", mine)
	}
	if err := s.RevokeGatewayKey(ctx, admin, true, k2.ID); err != nil {
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
	k, plain, err := s.CreateGatewayKey(ctx, owner, "ci", nil, "")
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
	mine, _ := s.ListGatewayKeys(ctx, owner, false)
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
	if again, _ := s.ListGatewayKeys(ctx, owner, false); !again[0].RevokedAt.Equal(first) {
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
		k, _, err := s.CreateGatewayKey(ctx, owner, "ci", nil, "")
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
				if err := s.RevokeGatewayKey(ctx, owner, false, k.ID); err != nil {
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

func TestUpdateGatewayKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, s, "gku-owner@x", "user").ID
	other := mustCreateUser(t, s, "gku-other@x", "user").ID
	admin := mustCreateUser(t, s, "gku-admin@x", "admin").ID

	k, _, err := s.CreateGatewayKey(ctx, owner, "laptop", nil, "Be brief.")
	if err != nil || k.SystemText != "Be brief." {
		t.Fatalf("create with system text: %v %+v", err, k)
	}
	str := func(v string) *string { return &v }

	// nil pointers change nothing.
	got, err := s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{})
	if err != nil || got.Name != "laptop" || got.SystemText != "Be brief." {
		t.Fatalf("no-op update: %v %+v", err, got)
	}
	got, err = s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{Name: str("  desk ")})
	if err != nil || got.Name != "desk" || got.SystemText != "Be brief." {
		t.Fatalf("rename: %v %+v", err, got)
	}
	got, err = s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{SystemText: str("Answer in German.")})
	if err != nil || got.Name != "desk" || got.SystemText != "Answer in German." {
		t.Fatalf("text: %v %+v", err, got)
	}
	got, err = s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{SystemText: str("")})
	if err != nil || got.SystemText != "" {
		t.Fatalf("clear text: %v %+v", err, got)
	}

	for name, c := range map[string]struct {
		name, text *string
	}{
		"empty name":   {str(" "), nil},
		"long name":    {str(strings.Repeat("n", 121)), nil},
		"control name": {str("a\nb"), nil},
		"long text":    {nil, str(strings.Repeat("t", MaxSystemTextBytes+1))},
	} {
		if _, err := s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{Name: c.name, SystemText: c.text}); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}
	if _, err := s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{SystemText: str(strings.Repeat("é", MaxSystemTextBytes/2))}); err != nil {
		t.Errorf("exactly %d bytes must pass: %v", MaxSystemTextBytes, err)
	}

	// A stranger and a missing id look alike.
	if _, err := s.UpdateGatewayKey(ctx, other, false, k.ID, GatewayKeyUpdate{Name: str("x")}); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("stranger err = %v", err)
	}
	if _, err := s.UpdateGatewayKey(ctx, owner, false, "missing", GatewayKeyUpdate{Name: str("x")}); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	if _, err := s.UpdateGatewayKey(ctx, "", false, k.ID, GatewayKeyUpdate{Name: str("x")}); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("empty caller err = %v", err)
	}
	if got, _ := s.UpdateGatewayKey(ctx, admin, true, k.ID, GatewayKeyUpdate{Name: str("by-admin")}); got.Name != "by-admin" {
		t.Fatalf("admin update: %+v", got)
	}

	// A revoked key can still be renamed.
	if err := s.RevokeGatewayKey(ctx, owner, false, k.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{Name: str("old laptop")})
	if err != nil || got.Name != "old laptop" || got.RevokedAt == nil {
		t.Fatalf("rename revoked: %v %+v", err, got)
	}

	if _, _, err := s.CreateGatewayKey(ctx, owner, "x", nil, strings.Repeat("t", MaxSystemTextBytes+1)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("create with long text err = %v", err)
	}
}

// A key is only as good as its owner: it stops working when the owner is
// suspended or gone, or when the owner's role no longer grants
// ai:configure:own or :any. The refusal is the one an unknown key gets.
func TestValidateGatewayKey_Owner(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	valid := func(plain string) bool {
		t.Helper()
		_, ok, err := s.ValidateGatewayKey(ctx, plain)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		return ok
	}
	mint := func(owner string) string {
		t.Helper()
		_, plain, err := s.CreateGatewayKey(ctx, owner, "ci", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return plain
	}

	t.Run("suspended and reactivated", func(t *testing.T) {
		u := mustCreateUser(t, s, "own-susp@x", "user")
		plain := mint(u.ID)
		if !valid(plain) {
			t.Fatal("a key of an active owner was refused")
		}
		if err := s.SetUserStatus(ctx, u.ID, "suspended"); err != nil {
			t.Fatal(err)
		}
		if valid(plain) {
			t.Fatal("a key of a suspended owner was accepted")
		}
		if err := s.SetUserStatus(ctx, u.ID, "active"); err != nil {
			t.Fatal(err)
		}
		if !valid(plain) {
			t.Fatal("a key stays refused after its owner is active again")
		}
	})

	t.Run("owner gone", func(t *testing.T) {
		u := mustCreateUser(t, s, "own-gone@x", "user")
		plain := mint(u.ID)
		// The foreign key would delete the key with its owner, and the key
		// lookup alone would then refuse it. With foreign keys off on one
		// connection the key row stays and only the owner is gone, so the
		// owner lookup is what has to refuse it.
		conn, err := s.q.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{"PRAGMA foreign_keys=OFF", "DELETE FROM users WHERE id='" + u.ID + "'", "PRAGMA foreign_keys=ON"} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		_ = conn.Close()
		var n int
		if err := s.q.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_gateway_keys WHERE user_id = ?`, u.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("precondition: key rows of the deleted owner = %d (%v), want 1", n, err)
		}
		if valid(plain) {
			t.Fatal("a key whose owner is missing was accepted")
		}
	})

	t.Run("role without the permission", func(t *testing.T) {
		if err := s.CreateRole(ctx, "viewer-only", "", []string{"ai:read:own"}, false); err != nil {
			t.Fatal(err)
		}
		u := mustCreateUser(t, s, "own-role@x", "user")
		plain := mint(u.ID)
		if !valid(plain) {
			t.Fatal("a key of a user role was refused")
		}
		if err := s.UpdateUserRole(ctx, u.ID, "viewer-only"); err != nil {
			t.Fatal(err)
		}
		if valid(plain) {
			t.Fatal("a key whose owner lost ai:configure was accepted")
		}
		if err := s.CreateRole(ctx, "keeper", "", []string{"ai:configure:own"}, false); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateUserRole(ctx, u.ID, "keeper"); err != nil {
			t.Fatal(err)
		}
		if !valid(plain) {
			t.Fatal("a key of a custom role with ai:configure:own was refused")
		}
		if err := s.CreateRole(ctx, "any-keeper", "", []string{"ai:configure:any"}, false); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateUserRole(ctx, u.ID, "any-keeper"); err != nil {
			t.Fatal(err)
		}
		if !valid(plain) {
			t.Fatal("a key of a custom role with ai:configure:any was refused")
		}
	})

	t.Run("admin", func(t *testing.T) {
		if u := mustCreateUser(t, s, "own-admin@x", "admin"); !valid(mint(u.ID)) {
			t.Fatal("a key of an admin was refused")
		}
	})
}

// A key's allow-lists and read-only switch can be set at creation and changed
// afterwards; a field left out stays as it is.
func TestGatewayKeyTools(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, s, "gkt-owner@x", "user").ID
	other := mustCreateUser(t, s, "gkt-other@x", "user").ID

	// A key made the old way may call no tools.
	k0, _, err := s.CreateGatewayKey(ctx, owner, "old", nil, "")
	if err != nil || k0.AllowedTools == nil || len(k0.AllowedTools) != 0 || k0.ToolsReadOnly {
		t.Fatalf("default key: %v %+v", err, k0)
	}

	k, plain, err := s.CreateGatewayKeyFrom(ctx, owner, NewGatewayKey{
		Name: "agent", AllowedModels: []string{"zai/*"}, AllowedTools: []string{"github__*", "jira__x", "github__*"}, ToolsReadOnly: true,
	})
	if err != nil || fmt.Sprint(k.AllowedTools) != "[github__* jira__x]" || !k.ToolsReadOnly || fmt.Sprint(k.AllowedModels) != "[zai/*]" {
		t.Fatalf("create: %v %+v", err, k)
	}
	if got, ok, err := s.ValidateGatewayKey(ctx, plain); err != nil || !ok || fmt.Sprint(got.AllowedTools) != "[github__* jira__x]" || !got.ToolsReadOnly {
		t.Fatalf("validate carries the tool fields: %v %v %+v", err, ok, got)
	}

	models := func(v ...string) *[]string { return &v }
	yes := true
	no := false

	// Nothing given: nothing changes.
	got, err := s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{})
	if err != nil || fmt.Sprint(got.AllowedTools) != "[github__* jira__x]" || !got.ToolsReadOnly || fmt.Sprint(got.AllowedModels) != "[zai/*]" {
		t.Fatalf("no-op: %v %+v", err, got)
	}
	// Models only.
	got, err = s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{AllowedModels: models("burrow-simple", "burrow-simple")})
	if err != nil || fmt.Sprint(got.AllowedModels) != "[burrow-simple]" || fmt.Sprint(got.AllowedTools) != "[github__* jira__x]" {
		t.Fatalf("models: %v %+v", err, got)
	}
	// Tools and the switch.
	got, err = s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{AllowedTools: models("fs__read"), ToolsReadOnly: &no})
	if err != nil || fmt.Sprint(got.AllowedTools) != "[fs__read]" || got.ToolsReadOnly || fmt.Sprint(got.AllowedModels) != "[burrow-simple]" {
		t.Fatalf("tools: %v %+v", err, got)
	}
	// An empty list clears: no tools, every model.
	got, err = s.UpdateGatewayKey(ctx, owner, false, k.ID, GatewayKeyUpdate{AllowedTools: models(), AllowedModels: models(), ToolsReadOnly: &yes})
	if err != nil || len(got.AllowedTools) != 0 || got.AllowedTools == nil || len(got.AllowedModels) != 0 || !got.ToolsReadOnly {
		t.Fatalf("clear: %v %+v", err, got)
	}
	stored, _ := s.q.GetAIGatewayKey(ctx, k.ID)
	if len(stored.AllowedTools) != 0 || !stored.ToolsReadOnly || len(stored.AllowedModels) != 0 {
		t.Fatalf("stored: %+v", stored)
	}

	// Validation as at creation; nothing is changed by a refused update.
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("s__t%d", i)
	}
	manyModels := make([]string, 65)
	for i := range manyModels {
		manyModels[i] = fmt.Sprintf("m%d", i)
	}
	for name, u := range map[string]GatewayKeyUpdate{
		"bad tool":     {AllowedTools: models("github")},
		"bare star":    {AllowedTools: models("*")},
		"too many":     {AllowedTools: &many},
		"bad model":    {AllowedModels: models("*/x")},
		"many models":  {AllowedModels: &manyModels},
		"good and bad": {AllowedTools: models("gh__a", "Gh__b")},
	} {
		if _, err := s.UpdateGatewayKey(ctx, owner, false, k.ID, u); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}
	for name, n := range map[string]NewGatewayKey{
		"create bad tool": {Name: "x", AllowedTools: []string{"nope"}},
		"create reserved": {Name: "x", AllowedTools: []string{"burrow__*"}},
	} {
		if _, _, err := s.CreateGatewayKeyFrom(ctx, owner, n); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}
	stored, _ = s.q.GetAIGatewayKey(ctx, k.ID)
	if len(stored.AllowedTools) != 0 || !stored.ToolsReadOnly {
		t.Fatalf("a refused update changed the key: %+v", stored)
	}

	// A stranger cannot, and cannot tell a key from a missing one.
	if _, err := s.UpdateGatewayKey(ctx, other, false, k.ID, GatewayKeyUpdate{AllowedTools: models("gh__*")}); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("stranger err = %v", err)
	}
}

// Partial updates that run at the same time each change only their own
// field: none of them writes back a stale copy of the fields it left alone
// (a read-modify-write would lose the other updates).
func TestUpdateGatewayKey_ConcurrentPartialUpdates(t *testing.T) {
	checkConcurrentKeyUpdates(t, newStore(t))
}

func checkConcurrentKeyUpdates(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	owner := mustCreateUser(t, s, fmt.Sprintf("gk-conc-%d@x", time.Now().UnixNano()), "user").ID
	k, _, err := s.CreateGatewayKey(ctx, owner, "start", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 25; round++ {
		name, text := fmt.Sprintf("name-%d", round), fmt.Sprintf("text-%d", round)
		tools, ro := []string{fmt.Sprintf("src%d__*", round)}, round%2 == 0
		updates := []GatewayKeyUpdate{{Name: &name}, {SystemText: &text}, {AllowedTools: &tools}, {ToolsReadOnly: &ro}}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, u := range updates {
			wg.Add(1)
			go func(u GatewayKeyUpdate) {
				defer wg.Done()
				<-start
				if _, err := s.UpdateGatewayKey(ctx, owner, false, k.ID, u); err != nil {
					t.Error(err)
				}
			}(u)
		}
		close(start)
		wg.Wait()
		got, err := s.q.GetAIGatewayKey(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != name || got.SystemText != text || len(got.AllowedTools) != 1 || got.AllowedTools[0] != tools[0] || got.ToolsReadOnly != ro {
			t.Fatalf("round %d: lost an update: name %q text %q tools %v read-only %v", round, got.Name, got.SystemText, got.AllowedTools, got.ToolsReadOnly)
		}
	}
}
