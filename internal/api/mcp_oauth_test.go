package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcpserv"
	"github.com/ankoehn/burrow/internal/oauth"
	"github.com/ankoehn/burrow/internal/store"
)

// replayVerifier accepts one OAuth access token at /mcp/burrow and hands
// back the identity it was given (a stand-in for *oauth.Server).
type replayVerifier struct {
	token string
	v     oauth.Verified
}

func (f replayVerifier) VerifyAccessToken(_ *http.Request, plain, path string) (oauth.Verified, error) {
	if plain != f.token || path != mcpserv.EndpointPath {
		return oauth.Verified{}, oauth.ErrInvalidToken
	}
	return f.v, nil
}

func (replayVerifier) Challenge(*http.Request, string, bool) string { return "Bearer" }

// replayRelay is the router with the management MCP server mounted and
// replaying through it, over a real store: an admin and a user "u@x".
func replayRelay(t *testing.T, verify func(admin, user db.User) replayVerifier) (base string, x *db.DB, admin, user db.User) {
	t.Helper()
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	st := store.New(sqldb)
	x = db.Wrap(sqldb)
	ctx := context.Background()
	if err := st.SeedAdmin(ctx, "admin@x", "password1"); err != nil {
		t.Fatal(err)
	}
	if admin, err = st.GetUserByEmail(ctx, "admin@x"); err != nil {
		t.Fatal(err)
	}
	if user, err = st.CreateUser(ctx, "u@x", "password1", "user"); err != nil {
		t.Fatal(err)
	}
	srv := newMCPServer(t, mcpTokens{}, mcpTokens{})
	srv.SetOAuth(verify(admin, user))
	router := NewRouter(Deps{
		Log: discardLog(), Users: st, Sessions: st, Roles: st, Services: st,
		Bearer: NewStoreBearerStore(st), Automation: st,
		MCP: MCPInfo{Enabled: true, Server: srv, Handler: srv},
	})
	srv.SetAPIHandler(router)
	hs := httptest.NewServer(router)
	t.Cleanup(hs.Close)
	return hs.URL, x, admin, user
}

func connectWith(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "api-test", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{
			Endpoint: url, HTTPClient: &http.Client{Transport: mcpAuth{token: token}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

const replayToken = "boa_replaytesttoken"

// A tool call made with an OAuth token reaches the REST API as the token's
// user, through the in-process marker: the API answers for that user.
func TestReplayMarker_ActsAsTheGrantsUser(t *testing.T) {
	base, _, _, _ := replayRelay(t, func(_, user db.User) replayVerifier {
		return replayVerifier{token: replayToken, v: oauth.Verified{
			AccessToken: oauth.AccessToken{GrantID: "g1", UserID: user.ID, Resource: "x", ExpiresAt: time.Now().Add(time.Hour)},
			User:        user, Permissions: []string{"tunnels:read:own"},
		}}
	})
	cs := connectWith(t, base+mcpserv.EndpointPath, replayToken)
	text, isErr := toolText(t, cs, "me_get", nil)
	var me struct {
		Email string `json:"email"`
	}
	if isErr || json.Unmarshal([]byte(text), &me) != nil || me.Email != "u@x" {
		t.Fatalf("me_get through the replay: %v %s", isErr, text)
	}
}

// The API intersects the marker's permissions with the user's role as it is
// when the replayed request arrives, as it does for an automation token.
func TestReplayMarker_RoleIsReadAgain(t *testing.T) {
	base, _, _, _ := replayRelay(t, func(_, user db.User) replayVerifier {
		stale := user
		stale.Role = "admin" // what the verifier saw; the store says "user"
		return replayVerifier{token: replayToken, v: oauth.Verified{
			AccessToken: oauth.AccessToken{GrantID: "g1", UserID: user.ID, Resource: "x", ExpiresAt: time.Now().Add(time.Hour)},
			User:        stale, Permissions: []string{"users:read"},
		}}
	})
	cs := connectWith(t, base+mcpserv.EndpointPath, replayToken)
	text, isErr := toolText(t, cs, "burrow_call_operation", map[string]any{"operation_id": "usersList"})
	if !isErr || !strings.Contains(text, "403") {
		t.Fatalf("users_list for a role without users:read: %v %s", isErr, text)
	}
}

// A suspended user's replayed call is refused by the API too.
func TestReplayMarker_SuspendedUser(t *testing.T) {
	var uid string
	base, x, _, _ := replayRelay(t, func(_, user db.User) replayVerifier {
		uid = user.ID
		return replayVerifier{token: replayToken, v: oauth.Verified{
			AccessToken: oauth.AccessToken{GrantID: "g1", UserID: user.ID, Resource: "x", ExpiresAt: time.Now().Add(time.Hour)},
			User:        user, Permissions: []string{"tunnels:read:own"},
		}}
	})
	cs := connectWith(t, base+mcpserv.EndpointPath, replayToken)
	if err := x.UpdateUserStatus(context.Background(), uid, "suspended"); err != nil {
		t.Fatal(err)
	}
	text, isErr := toolText(t, cs, "me_get", nil)
	if !isErr || !strings.Contains(text, "401") {
		t.Fatalf("me_get for a suspended user: %v %s", isErr, text)
	}
}

// The REST API never accepts an OAuth access token over HTTP, not even one
// the verifier would take at /mcp/burrow.
func TestRESTRefusesOAuthTokens(t *testing.T) {
	base, _, _, _ := replayRelay(t, func(_, user db.User) replayVerifier {
		return replayVerifier{token: replayToken, v: oauth.Verified{
			AccessToken: oauth.AccessToken{GrantID: "g1", UserID: user.ID, Resource: "x", ExpiresAt: time.Now().Add(time.Hour)},
			User:        user, Permissions: []string{"tunnels:read:own"},
		}}
	})
	for _, path := range []string{"/api/v1/me", "/api/v1/services", "/api/v1/mcp/sources"} {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("Authorization", "Bearer "+replayToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with an OAuth token: %d, want 401", path, resp.StatusCode)
		}
	}
}

// A replayed call of an OAuth token mints nothing that outlives the access
// token: an automation token without an expiry, or one that expires later,
// is refused; one within the hour is minted.
func TestReplayMarker_MintsNothingThatOutlivesTheToken(t *testing.T) {
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	base, _, _, _ := replayRelay(t, func(admin, _ db.User) replayVerifier {
		return replayVerifier{token: replayToken, v: oauth.Verified{
			AccessToken: oauth.AccessToken{GrantID: "g1", UserID: admin.ID, Resource: "x", ExpiresAt: expires},
			User:        admin, Permissions: []string{"automation:tokens:manage:own", "tunnels:read:own"},
		}}
	})
	cs := connectWith(t, base+mcpserv.EndpointPath, replayToken)
	mint := func(body map[string]any) (string, bool) {
		return toolText(t, cs, "burrow_call_operation", map[string]any{"operation_id": "automationTokenCreate", "arguments": map[string]any{"body": body}})
	}
	for _, body := range []map[string]any{
		{"name": "forever", "permissions": []string{"tunnels:read:own"}},
		{"name": "later", "permissions": []string{"tunnels:read:own"}, "expires_at": expires.Add(time.Hour).Format(time.RFC3339)},
	} {
		if text, isErr := mint(body); !isErr || !strings.Contains(text, "403") {
			t.Fatalf("%s: %v %s", body["name"], isErr, text)
		}
	}
	if text, isErr := mint(map[string]any{"name": "within", "permissions": []string{"tunnels:read:own"}, "expires_at": expires.Add(-time.Minute).Format(time.RFC3339)}); isErr {
		t.Fatalf("a token within the hour: %s", text)
	}
}

// storeGrant writes a grant (no tokens) for the tests of the source API.
func storeGrant(t *testing.T, x *db.DB, id, userID, resource string, tools []string) {
	t.Helper()
	if err := x.CreateOAuthGrant(context.Background(), db.OAuthGrant{
		ID: id, ClientID: "bcl_x", ClientName: "Client " + id, UserID: userID, Resource: resource,
		Scopes: []string{"mcp:tools"}, AllowedTools: tools,
	}, ""); err != nil {
		t.Fatal(err)
	}
}

func grantRevoked(t *testing.T, x *db.DB, id string) bool {
	t.Helper()
	g, err := x.GetOAuthGrant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return g.RevokedAt != nil
}

// Deleting a tool source revokes the grants issued for /mcp/<slug> (whatever
// origin they were issued under), so a later source of the same slug is not
// reached by them. Grants for /mcp and for other sources stay.
func TestMCPSources_DeleteRevokesTheSourcesGrants(t *testing.T) {
	f := newMCPSourceFixture(t)
	admin := f.session(t, "admin")
	if rec := f.do(t, http.MethodPost, "/api/v1/mcp/sources", map[string]any{
		"slug": "github", "kind": "remote", "url": "https://mcp.example.com/mcp", "credential_slot": "GH_MCP",
	}, admin); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	x := db.Wrap(f.sqldb)
	storeGrant(t, x, "g-src", "u-admin", "https://relay.test/mcp/github", []string{"github__*"})
	storeGrant(t, x, "g-old", "u-user", "https://old.example/mcp/github", []string{"github__*"})
	storeGrant(t, x, "g-other", "u-admin", "https://relay.test/mcp/githubx", []string{"githubx__*"})
	storeGrant(t, x, "g-agg", "u-admin", "https://relay.test/mcp", []string{"github__*"})
	if rec := f.do(t, http.MethodDelete, "/api/v1/mcp/sources/github", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	for id, want := range map[string]bool{"g-src": true, "g-old": true, "g-other": false, "g-agg": false} {
		if got := grantRevoked(t, x, id); got != want {
			t.Errorf("%s revoked = %v, want %v", id, got, want)
		}
	}
	last := f.aud.events[len(f.aud.events)-1]
	var p map[string]any
	_ = json.Unmarshal(last.Payload, &p)
	if last.Action != "mcp.source.delete" || p["oauth_grants_revoked"] != float64(2) {
		t.Fatalf("audit %s %v", last.Action, p)
	}
}
