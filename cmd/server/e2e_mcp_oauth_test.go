package main

// e2e_mcp_oauth_test.go — the MCP endpoints as OAuth resources (spec D22,
// D23), through the real router and wiring: /mcp/burrow, /mcp and
// /mcp/<slug> take access tokens issued for themselves only; the management
// server's replay acts as the token's user while the REST API refuses the
// token; a gateway grant's allow-list acts like a key's and its calls are
// attributed to the grant; revoking the grant ends the token at once.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcptest"
)

// mintAccess stores a grant of userID for the resource path and returns an
// access token of it, as the token endpoint would have issued it.
func (e *mcpGWEnv) mintAccess(t *testing.T, userID, path string, scopes, tools []string, readOnly bool) (token, grantID string) {
	t.Helper()
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	grantID = hex.EncodeToString(raw)
	token = "boa_" + strings.ToUpper(hex.EncodeToString(raw)) + "TOKEN"
	sum := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	if err := db.Wrap(e.sqldb).CreateOAuthGrant(context.Background(), db.OAuthGrant{
		ID: grantID, ClientID: "bcl_test", ClientName: "Agent", UserID: userID, Resource: e.srv.URL + path,
		Scopes: scopes, AllowedTools: tools, ToolsReadOnly: readOnly, CreatedAt: now,
	}, "", db.OAuthToken{Hash: hex.EncodeToString(sum[:]), Kind: "access", GrantID: grantID, FamilyID: grantID, ExpiresAt: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	return token, grantID
}

// postMCP sends a tools/list to path with the token and returns the answer.
func (e *mcpGWEnv) postMCP(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestE2E_MCPResourcesTakeOAuthTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	remote := mcptest.NewServer(t, mcptest.Opts{TLS: true, Authorize: func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+gwAlphaSecret
	}})
	e := bootMCPGatewayE2E(t, &gwTunnels{}, remote)
	ctx := context.Background()
	admin, err := e.st.GetUserByEmail(ctx, e.adminEmail)
	if err != nil {
		t.Fatal(err)
	}
	e.source(t, map[string]any{"slug": "gh", "kind": "remote", "url": remote.URL, "credential_slot": "ALPHA"})

	burrow, _ := e.mintAccess(t, admin.ID, "/mcp/burrow", []string{"burrow:read"}, nil, false)
	gateway, gatewayGrant := e.mintAccess(t, admin.ID, "/mcp", []string{"mcp:tools"}, []string{"gh__echo"}, false)
	source, _ := e.mintAccess(t, admin.ID, "/mcp/gh", []string{"mcp:tools"}, []string{"gh__*"}, true)

	// /mcp/burrow: the read scope opens the read tools of the admin's role,
	// and the replay answers as the admin.
	cs := mcpConnect(t, e.srv.URL+"/mcp/burrow", burrow, "")
	names := mcpToolNames(t, cs)
	if !contains(names, "audit_events_list") || contains(names, "service_create") {
		t.Fatalf("/mcp/burrow with burrow:read: %v", names)
	}
	if got := mcpCall(t, cs, false, "me_get", map[string]any{}); !strings.Contains(got, `"email":"`+e.adminEmail+`"`) {
		t.Fatalf("me_get = %s", got)
	}
	// The REST API refuses the very same token.
	if code := rest(t, http.MethodGet, e.srv.URL+"/api/v1/me", burrow, nil); code != http.StatusUnauthorized {
		t.Fatalf("REST with an OAuth token: %d", code)
	}

	// /mcp: the grant's allow-list, the call attributed to the grant.
	cs = mcpConnect(t, e.srv.URL+"/mcp", gateway, "")
	if got := strings.Join(mcpToolNames(t, cs), ","); got != "gh__echo" {
		t.Fatalf("/mcp grant: tools = %s", got)
	}
	if got := mcpCall(t, cs, false, "gh__echo", map[string]any{"text": "via oauth"}); got != "echo: via oauth" {
		t.Fatalf("gh__echo = %q", got)
	}
	var grantCol, keyCol string
	if err := e.sqldb.QueryRow(`SELECT oauth_grant_id, gateway_key_id FROM usage_events WHERE kind = 'mcp' ORDER BY ts DESC LIMIT 1`).Scan(&grantCol, &keyCol); err != nil {
		t.Fatal(err)
	}
	if grantCol != gatewayGrant || keyCol != "" {
		t.Fatalf("usage row: oauth_grant_id %q gateway_key_id %q", grantCol, keyCol)
	}
	rows := e.gatewayAudit(t)
	if len(rows) == 0 {
		t.Fatal("no mcp.gateway.call row")
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(rows[len(rows)-1].Payload), &payload)
	if payload["oauth_grant_id"] != gatewayGrant || payload["gateway_key_id"] != nil {
		t.Fatalf("audit payload %v", payload)
	}

	// /mcp/gh: the read-only source grant there.
	cs = mcpConnect(t, e.srv.URL+"/mcp/gh", source, "")
	if names := toolNames(t, cs); strings.Contains(names, "mutate") || !strings.Contains(names, "echo") {
		t.Fatalf("/mcp/gh read-only grant: %s", names)
	}

	// Each token at every other resource: 401 invalid_token, pointing to the
	// metadata of the resource it was presented at.
	for _, c := range []struct{ token, at string }{
		{burrow, "/mcp"}, {burrow, "/mcp/gh"}, {gateway, "/mcp/burrow"}, {gateway, "/mcp/gh"}, {source, "/mcp"}, {source, "/mcp/burrow"},
	} {
		resp := e.postMCP(t, c.at, c.token)
		want := `Bearer resource_metadata="` + e.srv.URL + `/.well-known/oauth-protected-resource` + c.at + `"`
		if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), want) ||
			!strings.HasSuffix(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
			t.Errorf("token for another resource at %s: %d %q", c.at, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	// Without a token: the challenge without an error code, and the
	// metadata it names.
	resp := e.postMCP(t, "/mcp/gh", "")
	if want := `Bearer resource_metadata="` + e.srv.URL + `/.well-known/oauth-protected-resource/mcp/gh", scope="mcp:tools"`; resp.Header.Get("WWW-Authenticate") != want {
		t.Fatalf("challenge: %q", resp.Header.Get("WWW-Authenticate"))
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	r, err := http.Get(e.srv.URL + "/.well-known/oauth-protected-resource/mcp/gh")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(r.Body).Decode(&prm)
	_ = r.Body.Close()
	if prm.Resource != e.srv.URL+"/mcp/gh" || len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != e.srv.URL {
		t.Fatalf("metadata %+v", prm)
	}

	// Revoking the grant through the API ends its token at once.
	if code, raw := e.admin(t, http.MethodDelete, "/api/v1/oauth/grants/"+gatewayGrant, nil); code/100 != 2 {
		t.Fatalf("revoke: %d %s", code, raw)
	}
	if resp := e.postMCP(t, "/mcp", gateway); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked grant at /mcp: %d", resp.StatusCode)
	}
}
