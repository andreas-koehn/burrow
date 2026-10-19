// cmd/server/e2e_mcp_gateway_test.go
//
// The MCP gateway end to end, through the relay's router and the wiring of
// main (newMCPToolGateway): a tool source is created, synced and approved
// through the management API, and an MCP client with a gateway key calls it
// at /mcp/<slug>. The upstream is internal/mcptest, reached over https
// through the guarded transport (remote) or through a stand-in tunnel.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/inspector"
	"github.com/ankoehn/burrow/internal/mcpgw"
	"github.com/ankoehn/burrow/internal/mcptest"
	"github.com/ankoehn/burrow/internal/store"
)

type mcpGWEnv struct {
	*gwEnv
	st   *store.Store
	pool *mcpgw.Pool
}

// bootMCPGatewayE2E boots the relay with the tool gateway wired as main
// wires it. remote, when set, is the https upstream whose test certificate
// the pool's guarded transport trusts.
func bootMCPGatewayE2E(t *testing.T, tunnels tunnelStreamOpener, remote *mcptest.Server) *mcpGWEnv {
	t.Helper()
	env := &mcpGWEnv{}
	env.gwEnv = bootGatewayE2EDeps(t, tunnels, func(st *store.Store, v04 *v04Stack, deps *api.Deps) {
		tr := aiprovider.NewTransport(true) // the upstream listens on loopback
		if remote != nil {
			tr.TLSClientConfig = remote.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		}
		t.Cleanup(tr.CloseIdleConnections)
		pool := mcpgw.New(mcpgw.Options{Store: st, Vault: deps.CredentialVault, Remote: tr, Tunnels: proxyDialerAdapter{st: st, srv: tunnels}})
		t.Cleanup(pool.Close)
		tools := newMCPToolGateway(st, v04, pool, discardLog())
		deps.MCPSources, deps.MCPGateway, deps.MCPTools = st, mcpGatewayParts{Pool: pool, tools: tools}, tools
		env.st, env.pool = st, pool
	})
	return env
}

// source creates a tool source through the API, syncs it, approves every
// tool, turns the backing service's inspector on and returns its service id.
func (e *mcpGWEnv) source(t *testing.T, body map[string]any) string {
	t.Helper()
	slug := body["slug"].(string)
	code, raw := e.admin(t, "POST", "/api/v1/mcp/sources", body)
	var src struct {
		ServiceID string `json:"service_id"`
	}
	if code != http.StatusCreated || json.Unmarshal(raw, &src) != nil {
		t.Fatalf("create source: %d %s", code, raw)
	}
	if code, raw := e.admin(t, "POST", "/api/v1/mcp/sources/"+slug+"/sync", nil); code != http.StatusOK {
		t.Fatalf("sync: %d %s", code, raw)
	}
	code, raw = e.admin(t, "GET", "/api/v1/mcp/sources/"+slug+"/tools", nil)
	var tools []struct {
		Name        string `json:"name"`
		CurrentHash string `json:"current_hash"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &tools) != nil || len(tools) == 0 {
		t.Fatalf("list tools: %d %s", code, raw)
	}
	hashes := map[string]string{}
	for _, x := range tools {
		hashes[x.Name] = x.CurrentHash
	}
	if code, raw := e.admin(t, "POST", "/api/v1/mcp/sources/"+slug+"/approve-all", map[string]any{"hashes": hashes}); code != http.StatusOK {
		t.Fatalf("approve all: %d %s", code, raw)
	}
	if code, raw := e.admin(t, "PUT", "/api/v1/services/"+src.ServiceID+"/ai-config", map[string]any{"inspector": map[string]any{"enabled": true, "max_requests": 20}}); code/100 != 2 {
		t.Fatalf("inspector on: %d %s", code, raw)
	}
	return src.ServiceID
}

// key creates a gateway key through the API.
func (e *mcpGWEnv) key(t *testing.T, tools []string, readOnly bool) string {
	t.Helper()
	k, _ := e.keyWithID(t, tools, readOnly)
	return k
}

func (e *mcpGWEnv) keyWithID(t *testing.T, tools []string, readOnly bool) (key, id string) {
	t.Helper()
	code, raw := e.admin(t, "POST", "/api/v1/ai/keys", map[string]any{"name": "tools", "allowed_tools": tools, "tools_read_only": readOnly})
	var k struct{ Key, ID string }
	if code != http.StatusCreated || json.Unmarshal(raw, &k) != nil || k.Key == "" || k.ID == "" {
		t.Fatalf("create key: %d %s", code, raw)
	}
	return k.Key, k.ID
}

func (e *mcpGWEnv) connect(t *testing.T, slug, key string) *mcp.ClientSession {
	t.Helper()
	return mcpConnectAs(t, e.srv.URL+"/mcp/"+slug, mcpBearerTransport{token: key, userAgent: "gw-e2e"}, "")
}

// gatewayAudit returns the mcp.gateway.call rows, oldest first.
func (e *mcpGWEnv) gatewayAudit(t *testing.T) []struct{ Result, Subject, Payload string } {
	t.Helper()
	rows, err := e.sqldb.Query(`SELECT result, subject_id, payload FROM audit_events WHERE action = ? ORDER BY id`, audit.ActionMCPGatewayCall)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct{ Result, Subject, Payload string }
	for rows.Next() {
		var r struct{ Result, Subject, Payload string }
		if err := rows.Scan(&r.Result, &r.Subject, &r.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func toolNames(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	return strings.Join(mcpToolNames(t, cs), ",")
}

// A remote source, stateless and stateful: a key calls what its allow-list
// names, with the source's credential injected upstream and the caller's
// key never passed on; the call leaves a usage row, an inspector entry and
// an audit row. Other tools, other methods and a suspended owner are refused.
func TestE2E_MCPGatewayRemote(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		t.Run(map[bool]string{false: "stateless", true: "stateful"}[stateful], func(t *testing.T) {
			up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: stateful, Authorize: func(r *http.Request) bool {
				return r.Header.Get("Authorization") == "Bearer "+gwAlphaSecret
			}})
			e := bootMCPGatewayE2E(t, &fakeHTTPTunnelLookup{}, up)
			serviceID := e.source(t, map[string]any{"slug": "gh", "kind": "remote", "url": up.URL, "credential_slot": "ALPHA"})
			key, keyID := e.keyWithID(t, []string{"gh__*"}, false)

			cs := e.connect(t, "gh", key)
			if got := toolNames(t, cs); got != "big,echo,fail,mutate,slow" {
				t.Fatalf("tools = %s", got)
			}
			if got := mcpCall(t, cs, false, "echo", map[string]any{"text": "hello"}); got != "echo: hello" {
				t.Fatalf("echo = %q", got)
			}
			call := up.CallsTo("echo")[0]
			for _, vals := range call.Header {
				for _, v := range vals {
					if strings.Contains(v, key) {
						t.Fatal("the caller's gateway key reached the upstream")
					}
				}
			}
			var kind, provider, model, gk string
			var status, bytesIn, bytesOut, latency int
			usage := func(tool string) {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for {
					err := e.sqldb.QueryRow(`SELECT kind, provider_slug, requested_model, upstream_status, gateway_key_id, bytes_in, bytes_out, latency_ms
					  FROM usage_events WHERE kind = 'mcp' AND requested_model = ?`, tool).Scan(&kind, &provider, &model, &status, &gk, &bytesIn, &bytesOut, &latency)
					if err == nil || time.Now().After(deadline) {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			usage("echo")
			if kind != "mcp" || provider != "gh" || model != "echo" || status != http.StatusOK || gk != keyID || bytesIn == 0 || bytesOut == 0 {
				t.Fatalf("usage row = %s %s %s %d key %q bytes %d/%d", kind, provider, model, status, gk, bytesIn, bytesOut)
			}
			mcpCall(t, cs, false, "slow", map[string]any{"ms": 40})
			if usage("slow"); latency < 40 || gk != keyID {
				t.Fatalf("usage row of a 40 ms call: latency %d ms, key %q", latency, gk)
			}
			ring := e.v04.InspectorMgr.Get(serviceID)
			if ring == nil || ring.Len() != 2 {
				t.Fatal("no inspector entries on the backing service")
			}
			if en := ring.List(inspector.ListQuery{})[1]; en.MCP == nil || en.MCP.Tool != "echo" || en.Provider != "gh" || en.Status != http.StatusOK {
				t.Fatalf("inspector entry = %+v", en)
			}
			if rows := e.gatewayAudit(t); len(rows) != 1 || rows[0].Result != "ok" || rows[0].Subject != "gh" || strings.Contains(rows[0].Payload, key) {
				t.Fatalf("audit rows = %+v", rows)
			}

			// Another key sees only what it names; the rest is unknown.
			echoOnly := e.connect(t, "gh", e.key(t, []string{"gh__echo"}, false))
			if got := toolNames(t, echoOnly); got != "echo" {
				t.Fatalf("gh__echo key: tools = %s", got)
			}
			if _, err := echoOnly.CallTool(context.Background(), &mcp.CallToolParams{Name: "mutate", Arguments: map[string]any{"text": "x"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("a tool outside the allow-list: %v", err)
			}
			if got := toolNames(t, e.connect(t, "gh", e.key(t, []string{"gh__*"}, true))); got != "big,echo,slow" {
				t.Fatalf("read-only key: tools = %s", got)
			}
			// Tools only (D3).
			var we *jsonrpc.Error
			if _, err := cs.ListResources(context.Background(), nil); !errors.As(err, &we) || we.Code != jsonrpc.CodeMethodNotFound || !strings.Contains(we.Message, "tools only") {
				t.Fatalf("resources/list: %v", err)
			}

			// The key of a suspended owner stops working.
			ctx := context.Background()
			u, err := e.st.CreateUser(ctx, "dev@test", "password1-very-strong", "user")
			if err != nil {
				t.Fatal(err)
			}
			_, plain, err := e.st.CreateGatewayKeyFrom(ctx, u.ID, store.NewGatewayKey{Name: "dev", AllowedTools: []string{"gh__*"}})
			if err != nil {
				t.Fatal(err)
			}
			if got := toolNames(t, e.connect(t, "gh", plain)); got == "" {
				t.Fatal("the user's key sees no tools")
			}
			if err := e.st.SetUserStatus(ctx, u.ID, "suspended"); err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/mcp/gh", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
			req.Header.Set("Authorization", "Bearer "+plain)
			req.Header.Set("Content-Type", "application/json")
			resp, err := e.plain.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
				t.Fatalf("suspended owner: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}
			// An unknown source is 404; the management server is untouched.
			req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/mcp/nope", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+key)
			resp, _ = e.plain.Do(req)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("unknown source: %d", resp.StatusCode)
			}
		})
	}
}

// A tunnel source: the call goes through the client's tunnel to the MCP
// server behind it (the Host it is addressed with is pinned by mcpgw's
// TestTunnelSource_HostIsTheLocalAddress).
func TestE2E_MCPGatewayTunnel(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	tunnels := &gwTunnels{}
	e := bootMCPGatewayE2E(t, tunnels, nil)
	ctx := context.Background()
	admin, err := e.st.GetUserByEmail(ctx, e.adminEmail)
	if err != nil {
		t.Fatal(err)
	}
	const serviceID = "svc-notes"
	if err := db.Wrap(e.sqldb).CreateService(ctx, db.Service{ID: serviceID, UserID: admin.ID, Name: "notes", Type: "http", AccessMode: "api_key"}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	tunnels.connect(serviceID, up.Listener.Addr().String())
	e.source(t, map[string]any{"slug": "notes", "kind": "tunnel", "service_id": serviceID})
	cs := e.connect(t, "notes", e.key(t, []string{"notes__echo"}, false))
	if got := toolNames(t, cs); got != "echo" {
		t.Fatalf("tools = %s", got)
	}
	if got := mcpCall(t, cs, false, "echo", map[string]any{"text": "via tunnel"}); got != "echo: via tunnel" {
		t.Fatalf("echo = %q", got)
	}
	if n := len(up.CallsTo("echo")); n != 1 {
		t.Fatalf("upstream calls = %d", n)
	}
}

// /mcp serves a remote and a tunnel source together, through the real router
// and wiring: each tool is named <slug>__<tool>, both sources have an "echo",
// and each call reaches its own upstream.
func TestE2E_MCPGatewayAggregated(t *testing.T) {
	remote := mcptest.NewServer(t, mcptest.Opts{TLS: true, Authorize: func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+gwAlphaSecret
	}})
	local := mcptest.NewServer(t, mcptest.Opts{})
	tunnels := &gwTunnels{}
	e := bootMCPGatewayE2E(t, tunnels, remote)
	ctx := context.Background()
	admin, err := e.st.GetUserByEmail(ctx, e.adminEmail)
	if err != nil {
		t.Fatal(err)
	}
	const serviceID = "svc-notes"
	if err := db.Wrap(e.sqldb).CreateService(ctx, db.Service{ID: serviceID, UserID: admin.ID, Name: "notes", Type: "http", AccessMode: "api_key"}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	tunnels.connect(serviceID, local.Listener.Addr().String())
	e.source(t, map[string]any{"slug": "gh", "kind": "remote", "url": remote.URL, "credential_slot": "ALPHA"})
	e.source(t, map[string]any{"slug": "notes", "kind": "tunnel", "service_id": serviceID})

	key := e.key(t, []string{"gh__echo", "notes__*"}, false)
	cs := mcpConnectAs(t, e.srv.URL+"/mcp", mcpBearerTransport{token: key, userAgent: "gw-e2e"}, "")
	names := toolNames(t, cs)
	if !strings.HasPrefix(names, "gh__echo,notes__") || strings.Contains(names, "gh__mutate") {
		t.Fatalf("tools = %s", names)
	}
	if got := mcpCall(t, cs, false, "gh__echo", map[string]any{"text": "remote"}); got != "echo: remote" {
		t.Fatalf("gh__echo = %q", got)
	}
	if got := mcpCall(t, cs, false, "notes__echo", map[string]any{"text": "local"}); got != "echo: local" {
		t.Fatalf("notes__echo = %q", got)
	}
	if len(remote.CallsTo("echo")) != 1 || len(local.CallsTo("echo")) != 1 {
		t.Fatalf("upstream calls: remote %d, local %d", len(remote.CallsTo("echo")), len(local.CallsTo("echo")))
	}
	// No key, no tools.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
}
