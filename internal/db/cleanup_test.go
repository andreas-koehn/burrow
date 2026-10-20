package db

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// v0.12.1 cleanup: the parts of internal/db the cleanup round changed. Each
// check runs on SQLite here and on Postgres in cleanup_postgres_test.go.

func TestDeleteSession_ParentAlreadyGone(t *testing.T) {
	checkDeleteSessionParentGone(t, testDB(t), "u-gone")
}

func TestMigration0039_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0039_v0.12.1_cache_guard_gen.sql")
}

func TestListServiceAIConfigsRaw(t *testing.T) {
	checkListServiceAIConfigs(t, testDB(t), "u-aicfg")
}

// A services session whose dashboard session's row is already gone (removed
// by a path that did not take its children along) still ends when that
// session is deleted: DeleteSession deletes the children by parent_id even
// when the parent row is missing. The owner-scoped delete does so only for
// the owner's own children.
func checkDeleteSessionParentGone(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	exp := time.Now().UTC().Add(time.Hour)
	for _, s := range []Session{
		{ID: userID + "-svc", UserID: userID, ExpiresAt: exp, Kind: SessionKindServices, ParentID: userID + "-gone"},
		{ID: userID + "-svc2", UserID: userID, ExpiresAt: exp, Kind: SessionKindServices, ParentID: userID + "-gone2"},
		{ID: userID + "-other", UserID: userID, ExpiresAt: exp, Kind: SessionKindServices, ParentID: userID + "-kept"},
	} {
		if err := x.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.DeleteSession(ctx, userID+"-gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetSession(ctx, userID+"-svc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the child of a gone parent survived DeleteSession: %v", err)
	}
	// Scoped to another user: nothing of this user's goes, and the parent is
	// not found.
	if _, err := x.DeleteSessionForUserWithChildren(ctx, userID+"-gone2", "someone-else"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scoped delete for another user: %v", err)
	}
	if _, err := x.GetSession(ctx, userID+"-svc2"); err != nil {
		t.Fatalf("another user's delete removed this user's session: %v", err)
	}
	if _, err := x.DeleteSessionForUserWithChildren(ctx, userID+"-gone2", userID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scoped delete of a gone parent: %v, want ErrNotFound", err)
	}
	if _, err := x.GetSession(ctx, userID+"-svc2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the owner's child of a gone parent survived: %v", err)
	}
	if _, err := x.GetSession(ctx, userID+"-other"); err != nil {
		t.Fatalf("an unrelated services session went too: %v", err)
	}
}

func checkListServiceAIConfigs(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	// Postgres keeps the blob as jsonb and gives it back normalised: the
	// blobs are compared by what they hold.
	want := map[string]bool{}
	for i, name := range []string{"a", "b"} {
		svc, err := x.GetOrCreateService(ctx, userID, userID+"-"+name, "http")
		if err != nil {
			t.Fatal(err)
		}
		cfg := `{"guardrails":{"enabled":` + map[int]string{0: "true", 1: "false"}[i] + `},"svc":"` + svc.ID + `"}`
		if err := x.UpsertServiceAIConfig(ctx, svc.ID, []byte(cfg)); err != nil {
			t.Fatal(err)
		}
		want[svc.ID] = i == 0
	}
	raws, err := x.ListServiceAIConfigsRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range raws {
		var c struct {
			Svc        string `json:"svc"`
			Guardrails struct {
				Enabled bool `json:"enabled"`
			} `json:"guardrails"`
		}
		if json.Unmarshal(r, &c) == nil {
			if _, ok := want[c.Svc]; ok {
				got[c.Svc] = c.Guardrails.Enabled
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed %q, want %v among them", raws, want)
	}
}

func TestListMCPToolsForSlugs(t *testing.T) {
	checkListMCPToolsForSlugs(t, testDB(t), "u-mcpslugs")
}

// The tools of several sources come back in one query, by source, ordered by
// name; a slug without tools has no entry, and no slugs is no query.
func checkListMCPToolsForSlugs(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.CreateUser(ctx, User{ID: userID, Email: userID + "@test.invalid", PasswordHash: "h", Role: "user"})
	now := time.Now().UTC()
	for _, slug := range []string{userID + "-a", userID + "-b", userID + "-c"} {
		_ = x.DeleteMCPSourceAndBacking(ctx, slug)
		if err := x.CreateRemoteMCPSource(ctx, Service{ID: "svc-" + slug, UserID: userID, Name: "mcp:" + slug, AccessMode: "api_key"},
			MCPSource{Slug: slug, Name: slug, URL: "https://x.example/mcp", TimeoutS: 60, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = x.DeleteMCPSourceAndBacking(ctx, slug) })
	}
	seen := func(names ...string) []MCPToolSeen {
		var out []MCPToolSeen
		for _, n := range names {
			out = append(out, MCPToolSeen{Name: n, Hash: "h-" + n, Definition: `{"name":"` + n + `"}`, GuardrailHits: "[]"})
		}
		return out
	}
	if _, err := x.ApplyMCPToolSync(ctx, userID+"-a", seen("zeta", "alpha"), now, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := x.ApplyMCPToolSync(ctx, userID+"-b", seen("beta"), now, ""); err != nil {
		t.Fatal(err)
	}
	got, err := x.ListMCPToolsForSlugs(ctx, []string{userID + "-a", userID + "-b", userID + "-c"})
	if err != nil {
		t.Fatal(err)
	}
	a, b := got[userID+"-a"], got[userID+"-b"]
	if len(got) != 2 || len(a) != 2 || a[0].Name != "alpha" || a[1].Name != "zeta" || len(b) != 1 || b[0].Name != "beta" || b[0].SourceSlug != userID+"-b" {
		t.Fatalf("tools by slug: %+v", got)
	}
	if got, err := x.ListMCPToolsForSlugs(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no slugs: %v %v", got, err)
	}
}
