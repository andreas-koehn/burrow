package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcpserv"
)

// fakeToolsLister satisfies MCPToolsLister with a canned inventory.
type fakeToolsLister struct{ rows []mcpserv.ToolDescriptor }

func (f *fakeToolsLister) Tools() []mcpserv.ToolDescriptor { return f.rows }

// newMCPTestServer wires the minimal Deps needed for the /api/v1/mcp/*
// surface. role lets the test pick the caller's role; info is the MCPInfo
// the handlers render.
func newMCPTestServer(t *testing.T, role string, info MCPInfo) (*httptest.Server, *authClient) {
	t.Helper()
	d := Deps{
		Log:   discardLog(),
		Users: &fakeUserStore{role: role},
		MCP:   info,
	}
	srv := httptest.NewServer(NewRouter(d))
	t.Cleanup(srv.Close)
	return srv, authedClient(t, srv)
}

func TestMCPStatus_AdminSeesConfig(t *testing.T) {
	_, c := newMCPTestServer(t, "admin", MCPInfo{
		Enabled: true,
		Listen:  ":7800",
		TokenID: "tok-mcp-1",
	})
	r := c.get(t, "/api/v1/mcp/status")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got mcpStatusResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Listen != ":7800" || got.TokenID != "tok-mcp-1" || got.Endpoint != "/mcp/burrow" {
		t.Fatalf("status mismatch: %+v", got)
	}
}

// What a relay started by cmd/server answers: the three old keys are still
// there for clients written against them, and "endpoint" says where MCP is.
func TestMCPStatus_AsWiredByTheServer(t *testing.T) {
	_, c := newMCPTestServer(t, "admin", MCPInfo{
		Enabled: true,
		Handler: http.NotFoundHandler(),
	})
	r := c.get(t, "/api/v1/mcp/status")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	const want = `{"enabled":true,"listen":"","token_id":"","endpoint":"/mcp/burrow"}`
	if got := strings.TrimSpace(readBody(t, r)); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
}

func TestMCPStatus_Disabled(t *testing.T) {
	_, c := newMCPTestServer(t, "admin", MCPInfo{})
	r := c.get(t, "/api/v1/mcp/status")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", r.StatusCode)
	}
	var got mcpStatusResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Listen != "" || got.TokenID != "" || got.Endpoint != "/mcp/burrow" {
		t.Fatalf("want zero-value status with the endpoint, got %+v", got)
	}
}

func TestMCPTools_AdminSeesInventory(t *testing.T) {
	tools := []mcpserv.ToolDescriptor{
		{Name: "tunnels_list", Description: "x", Permission: "tunnels:read:any",
			ParametersSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "services_list", Description: "y", Permission: "services:configure:own",
			ParametersSchema: json.RawMessage(`{"type":"object"}`)},
	}
	_, c := newMCPTestServer(t, "admin", MCPInfo{Server: &fakeToolsLister{rows: tools}})
	r := c.get(t, "/api/v1/mcp/tools")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got []mcpToolResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tools, want 2", len(got))
	}
	if got[0].Name != "tunnels_list" || got[0].Permission != "tunnels:read:any" {
		t.Errorf("tool[0] mismatch: %+v", got[0])
	}
	if string(got[0].ParametersSchema) != `{"type":"object"}` {
		t.Errorf("schema mismatch: %s", got[0].ParametersSchema)
	}
}

func TestMCPTools_NilServer_ReturnsEmptyArray(t *testing.T) {
	_, c := newMCPTestServer(t, "admin", MCPInfo{})
	r := c.get(t, "/api/v1/mcp/tools")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", r.StatusCode)
	}
	body := readBody(t, r)
	if body != "[]\n" {
		t.Fatalf("body=%q want \"[]\\n\"", body)
	}
}

func TestMCPStatus_UnauthenticatedReturns401(t *testing.T) {
	srv := httptest.NewServer(NewRouter(Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/mcp/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

func TestMCPStatus_UserWithoutPermReturns403(t *testing.T) {
	_, c := newMCPTestServer(t, "user", MCPInfo{})
	r := c.get(t, "/api/v1/mcp/status")
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", r.StatusCode, readBody(t, r))
	}
}

func TestMCPTools_UserWithoutPermReturns403(t *testing.T) {
	_, c := newMCPTestServer(t, "user", MCPInfo{})
	r := c.get(t, "/api/v1/mcp/tools")
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", r.StatusCode)
	}
}

// ---------- /mcp/burrow on the main router ----------------------------------

// mcpTokens satisfies mcpserv.BearerStore and mcpserv.UserLookup for one
// automation token of one admin.
type mcpTokens struct {
	plaintext string
	perms     []string
}

func (m mcpTokens) LookupBearer(_ context.Context, hash string) (mcpserv.TokenInfo, error) {
	if hash != sha256Hex(m.plaintext) {
		return mcpserv.TokenInfo{}, db.ErrNotFound
	}
	return mcpserv.TokenInfo{ID: "tok1", UserID: "u1", Permissions: m.perms}, nil
}

func (mcpTokens) TouchBearer(context.Context, string) error { return nil }

func (mcpTokens) GetUserByID(_ context.Context, id string) (db.User, error) {
	return db.User{ID: id, Role: "admin", Status: "active"}, nil
}

// lockedLog is a log sink the request logger and the test share: the logger
// writes its line after the response has gone out.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// mcpAuth sends the Authorization header of an MCP client.
type mcpAuth struct{ token string }

func (a mcpAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}

// The SDK's client, on both protocol revisions, through the router the
// relay serves: request logger, recoverer and the dashboard's catch-all
// included.
func TestMCPMount_SDKClientThroughTheRouter(t *testing.T) {
	const token = "bua_routertesttoken123"
	var logs lockedLog
	srv := mcpserv.New(mcpTokens{plaintext: token, perms: []string{"tunnels:read:any", "audit:read"}},
		mcpTokens{}, nil, discardLog())
	hs := httptest.NewServer(NewRouter(Deps{
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
		SPA: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>dashboard</html>") }),
		MCP: MCPInfo{Enabled: true, Server: srv, Handler: srv},
	}))
	t.Cleanup(hs.Close)

	for _, revision := range []string{"2026-07-28", "2025-11-25"} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "api-test", Version: "0"}, nil).Connect(ctx,
			&mcp.StreamableClientTransport{
				Endpoint:             hs.URL + "/mcp/burrow",
				HTTPClient:           &http.Client{Transport: mcpAuth{token: token}},
				DisableStandaloneSSE: true,
				MaxRetries:           -1,
			}, &mcp.ClientSessionOptions{ProtocolVersion: revision})
		if err != nil {
			cancel()
			t.Fatalf("%s: connect: %v", revision, err)
		}
		res, err := cs.ListTools(ctx, nil)
		cancel()
		if err != nil {
			t.Fatalf("%s: tools/list: %v", revision, err)
		}
		var names []string
		for _, tl := range res.Tools {
			names = append(names, tl.Name)
		}
		sort.Strings(names)
		if got := strings.Join(names, ","); got != "audit_search,tunnels_list" {
			t.Errorf("%s: tools = %s", revision, got)
		}
		_ = cs.Close()
	}

	// The request log names the path and the status, never the token.
	if !strings.Contains(logs.String(), "/mcp/burrow") {
		t.Fatalf("no request log line for /mcp/burrow: %q", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Error("the request log carries the bearer token")
	}
}

// Which requests the route hands to the MCP handler, with the dashboard's
// catch-all present as on a real relay.
func TestMCPMount_Routing(t *testing.T) {
	var seen []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>dashboard</html>") })
	hs := httptest.NewServer(NewRouter(Deps{Log: discardLog(), SPA: spa, MCP: MCPInfo{Handler: h}}))
	t.Cleanup(hs.Close)

	do := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, hs.URL+path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// The three methods of MCP's HTTP transport reach the handler without a
	// session and without a CSRF token.
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		if code, _ := do(method, "/mcp/burrow"); code != http.StatusTeapot {
			t.Errorf("%s /mcp/burrow: %d, want the MCP handler", method, code)
		}
	}
	if want := "POST /mcp/burrow,GET /mcp/burrow,DELETE /mcp/burrow"; strings.Join(seen, ",") != want {
		t.Errorf("handler saw %v", seen)
	}
	// Nothing else does: no other method, no other path.
	seen = nil
	for _, c := range [][2]string{
		{http.MethodPut, "/mcp/burrow"},
		{http.MethodPost, "/mcp/burrow/"},
		{http.MethodPost, "/mcp/burrow/tools"},
		{http.MethodPost, "/mcp"},
		{http.MethodPost, "/mcp/github"},
	} {
		if code, _ := do(c[0], c[1]); code == http.StatusTeapot {
			t.Errorf("%s %s reached the MCP handler", c[0], c[1])
		}
	}
	if len(seen) != 0 {
		t.Errorf("handler saw %v", seen)
	}
}

// Without a handler the route is still there and says so.
func TestMCPMount_NotConfigured_503(t *testing.T) {
	hs := httptest.NewServer(NewRouter(Deps{Log: discardLog()}))
	t.Cleanup(hs.Close)
	resp, err := http.Post(hs.URL+"/mcp/burrow", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "mcp endpoint is not configured") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

// ---------- the per-address limit in front of the handler -------------------

// countingTokens counts the lookups that reach the token store.
type countingTokens struct {
	mcpTokens
	mu    sync.Mutex
	calls int
}

func (c *countingTokens) LookupBearer(ctx context.Context, hash string) (mcpserv.TokenInfo, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.mcpTokens.LookupBearer(ctx, hash)
}

func (c *countingTokens) lookups() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestMCPMount_PerAddressLimit(t *testing.T) {
	const token = "bua_addresslimittoken1"
	const limit = 5
	store := &countingTokens{mcpTokens: mcpTokens{plaintext: token, perms: []string{"tunnels:read:any"}}}
	srv := mcpserv.New(store, mcpTokens{}, nil, discardLog())
	hs := httptest.NewServer(NewRouter(Deps{
		Log:            discardLog(),
		TrustedProxies: []string{"127.0.0.0/8"},
		MCP:            MCPInfo{Enabled: true, Server: srv, Handler: srv, RateLimitPerIP: limit},
	}))
	t.Cleanup(hs.Close)

	post := func(bearer, xff string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/mcp/burrow", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-Forwarded-For", xff)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	// The first address spends its allowance on bearers that do not exist.
	for i := 0; i < limit; i++ {
		if r := post("bua_nosuchtoken", "198.51.100.1"); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("request %d: %d, want 401", i, r.StatusCode)
		}
	}
	if got := store.lookups(); got != limit {
		t.Fatalf("lookups = %d, want %d", got, limit)
	}
	for i := 0; i < 3; i++ {
		r := post("bua_nosuchtoken", "198.51.100.1")
		if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
			t.Fatalf("over the limit: %d Retry-After=%q", r.StatusCode, r.Header.Get("Retry-After"))
		}
	}
	// A valid token from that address is refused too: the limit is per address.
	if r := post(token, "198.51.100.1"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("valid token from the limited address: %d, want 429", r.StatusCode)
	}
	if got := store.lookups(); got != limit {
		t.Fatalf("lookups = %d after the limit, want %d: refused requests reached the store", got, limit)
	}
	// Another address is not affected.
	if r := post("bua_nosuchtoken", "198.51.100.2"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second address: %d, want 401", r.StatusCode)
	}
	if got := store.lookups(); got != limit+1 {
		t.Fatalf("lookups = %d, want %d", got, limit+1)
	}
}

// mcpLimitServer serves the router with the given allowance, a counting
// token store and the given trusted proxies.
func mcpLimitServer(t *testing.T, limit int, proxies []string) (*httptest.Server, *countingTokens, string) {
	t.Helper()
	const token = "bua_limitservertoken01"
	store := &countingTokens{mcpTokens: mcpTokens{plaintext: token, perms: []string{"tunnels:read:any"}}}
	srv := mcpserv.New(store, mcpTokens{}, nil, discardLog())
	hs := httptest.NewServer(NewRouter(Deps{
		Log:            discardLog(),
		TrustedProxies: proxies,
		MCP:            MCPInfo{Enabled: true, Server: srv, Handler: srv, RateLimitPerIP: limit},
	}))
	t.Cleanup(hs.Close)
	return hs, store, token
}

func mcpDo(t *testing.T, method, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// Requests the perimeter refuses anyway, or that carry no bearer, do not use
// up the allowance of the address; requests with a bearer do.
func TestMCPMount_LimitCountsOnlyBearerRequests(t *testing.T) {
	const limit = 3
	hs, store, token := mcpLimitServer(t, limit, nil)
	url := hs.URL + "/mcp/burrow"

	for i := 0; i < 3*limit; i++ {
		r := mcpDo(t, http.MethodPost, url, map[string]string{
			"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site",
			"Authorization": "Basic Zm9vOmJhcg==",
		})
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-origin: %d, want 403", r.StatusCode)
		}
		if r := mcpDo(t, http.MethodPost, url, nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no bearer: %d, want 401", r.StatusCode)
		}
	}
	if store.lookups() != 0 {
		t.Fatalf("lookups = %d, want 0", store.lookups())
	}
	// The allowance is intact: limit bearer requests (valid or not) count.
	for i := 0; i < limit; i++ {
		b := token
		if i%2 == 1 {
			b = "bua_nosuchtoken"
		}
		if r := mcpDo(t, http.MethodPost, url, map[string]string{"Authorization": "Bearer " + b}); r.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("bearer request %d refused with 429 too early", i)
		}
	}
	r := mcpDo(t, http.MethodPost, url, map[string]string{"Authorization": "Bearer " + token})
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("after the limit: %d Retry-After=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	if len(r.Header.Values("Retry-After")) != 1 {
		t.Errorf("Retry-After set %d times", len(r.Header.Values("Retry-After")))
	}
	if store.lookups() != limit {
		t.Errorf("lookups = %d, want %d", store.lookups(), limit)
	}
}

// With no trusted proxy configured the key is the TCP peer: forged address
// headers neither avoid the limit nor give a request its own allowance.
func TestMCPMount_LimitKeyIsNotCallerControlled(t *testing.T) {
	const limit = 3
	hs, store, _ := mcpLimitServer(t, limit, nil)
	var codes []int
	for i := 0; i < limit+3; i++ {
		ip := "203.0.113." + string(rune('1'+i))
		r := mcpDo(t, http.MethodPost, hs.URL+"/mcp/burrow", map[string]string{
			"Authorization":   "Bearer bua_nosuchtoken",
			"X-Forwarded-For": ip, "X-Real-IP": ip, "True-Client-IP": ip,
		})
		codes = append(codes, r.StatusCode)
	}
	for i, c := range codes {
		want := http.StatusUnauthorized
		if i >= limit {
			want = http.StatusTooManyRequests
		}
		if c != want {
			t.Fatalf("codes = %v, want the first %d to be 401 and the rest 429", codes, limit)
		}
	}
	if store.lookups() != limit {
		t.Errorf("lookups = %d, want %d", store.lookups(), limit)
	}
}

// What each method answers, per case, with the real handler behind the router.
func TestMCPMount_StatusPerMethodAndCase(t *testing.T) {
	hs, _, token := mcpLimitServer(t, 1000, nil)
	url := hs.URL + "/mcp/burrow"
	cross := map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}
	with := func(h map[string]string, bearer string) map[string]string {
		m := map[string]string{}
		for k, v := range h {
			m[k] = v
		}
		if bearer != "" {
			m["Authorization"] = "Bearer " + bearer
		}
		return m
	}
	cases := []struct {
		name, method string
		hdr          map[string]string
		want         int
	}{
		{"get, no bearer", http.MethodGet, nil, 401},
		{"get, bearer", http.MethodGet, with(nil, token), 405},
		{"get, cross-origin, no bearer", http.MethodGet, cross, 401},
		{"delete, no bearer", http.MethodDelete, nil, 401},
		{"delete, bearer", http.MethodDelete, with(nil, token), 405},
		{"delete, cross-origin", http.MethodDelete, cross, 403},
		{"post, cross-origin", http.MethodPost, cross, 403},
		{"post, no bearer", http.MethodPost, nil, 401},
	}
	for _, c := range cases {
		if r := mcpDo(t, c.method, url, c.hdr); r.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, r.StatusCode, c.want)
		}
	}
}
