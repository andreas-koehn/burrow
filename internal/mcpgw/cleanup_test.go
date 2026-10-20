package mcpgw

// cleanup_test.go — the v0.12.1 cleanup of the serving side: what /mcp reads
// per request, calls of tools that are not served, and embedded resources in
// results.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcptest"
	"github.com/ankoehn/burrow/internal/store"
)

// countingStore counts the reads of the serving side's store.
type countingStore struct {
	ServeStore
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingStore) count(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[name]++
}

func (c *countingStore) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

func (c *countingStore) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

func (c *countingStore) MCPSourceBySlug(ctx context.Context, slug string) (db.MCPSource, error) {
	c.count("MCPSourceBySlug")
	return c.ServeStore.MCPSourceBySlug(ctx, slug)
}

func (c *countingStore) ListMCPTools(ctx context.Context, slug string) ([]db.MCPTool, error) {
	c.count("ListMCPTools")
	return c.ServeStore.ListMCPTools(ctx, slug)
}

func (c *countingStore) ListMCPToolsForSlugs(ctx context.Context, slugs []string) (map[string][]db.MCPTool, error) {
	c.count("ListMCPToolsForSlugs")
	return c.ServeStore.ListMCPToolsForSlugs(ctx, slugs)
}

func (c *countingStore) ListMCPSources(ctx context.Context) ([]db.MCPSource, error) {
	c.count("ListMCPSources")
	return c.ServeStore.ListMCPSources(ctx)
}

// withCountingStore puts a counting store in front of the gateway's.
func (g *gwFixture) withCountingStore(t *testing.T) *countingStore {
	t.Helper()
	cs := &countingStore{ServeStore: g.st}
	g.gw.opts.Store = cs
	return cs
}

// addSource adds an approved source slug that serves the tools of g.up.
func (g *gwFixture) addSource(t *testing.T, slug string) {
	t.Helper()
	g.remote(t, slug, g.up.URL)
	if _, err := g.pool.Sync(context.Background(), slug); err != nil {
		t.Fatal(err)
	}
	approveAll(t, g.st, slug)
}

// postMCP sends one raw JSON-RPC POST with the key.
func (g *gwFixture) postMCP(t *testing.T, key, path string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// What /mcp reads per request does not grow with the number of sources the
// key names: the tools of all of them come in one query.
func TestAggregated_StoreReadsDoNotGrowWithSources(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	g := newGWFixture(t, up)
	key, _ := g.key(t, g.userID, []string{"gh__*", "s2__*", "s3__*", "s4__*"}, false)
	counter := g.withCountingStore(t)
	list := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	if code := g.postMCP(t, key, "/mcp", list); code != http.StatusOK {
		t.Fatalf("tools/list: %d", code)
	}
	one := counter.total()
	g.addSource(t, "s2")
	g.addSource(t, "s3")
	g.addSource(t, "s4")
	counter.reset()
	if code := g.postMCP(t, key, "/mcp", list); code != http.StatusOK {
		t.Fatalf("tools/list: %d", code)
	}
	if four := counter.total(); four != one {
		t.Fatalf("store reads per request: %d with one source, %d with four (%v)", one, four, counter.calls)
	}
	if one > 2 {
		t.Fatalf("store reads per request: %d (%v), want the sources and one query for their tools", one, counter.calls)
	}
	// The list still holds every source's tools.
	if got := names(t, g.connect(t, key, "", "")); len(got) != 20 {
		t.Fatalf("tools = %v", got)
	}
}

// A request the cheap checks refuse (a batch, a body over the limit, a method
// beyond tools) reads no tools: the server is built after them.
func TestServe_CheapChecksBeforeTheToolsAreRead(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	g := newGWFixture(t, up)
	key, _ := g.key(t, g.userID, []string{"gh__*"}, false)
	counter := g.withCountingStore(t)
	big := append([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"`), bytes.Repeat([]byte("a"), MaxRequestBytes)...)
	big = append(big, `"}}`...)
	for _, path := range []string{"/mcp", "/mcp/gh"} {
		for name, c := range map[string]struct {
			body []byte
			want int
		}{
			"batch":     {[]byte(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`), http.StatusBadRequest},
			"too large": {big, http.StatusRequestEntityTooLarge},
			"resources": {[]byte(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`), http.StatusOK},
		} {
			counter.reset()
			if code := g.postMCP(t, key, path, c.body); code != c.want {
				t.Fatalf("%s %s: status %d, want %d", path, name, code, c.want)
			}
			counter.mu.Lock()
			reads := counter.calls["ListMCPTools"] + counter.calls["ListMCPToolsForSlugs"] + counter.calls["ListMCPSources"]
			counter.mu.Unlock()
			if reads != 0 {
				t.Errorf("%s %s: the tools were read (%v)", path, name, counter.calls)
			}
		}
	}
}

// A call of a tool the source has but does not serve (pending approval, or
// hidden) answers "unknown tool" as before, and leaves a denied audit row,
// sampled once per key, source and minute like the limit refusals.
func TestServe_UnservedToolCallsAreAudited(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	g := newGWFixture(t, up)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	g.gw.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	yes := true
	if _, err := g.st.UpdateMCPToolOverrides(ctx, "gh", "fail", store.MCPToolOverridesUpdate{Hidden: &yes}); err != nil {
		t.Fatal(err)
	}
	up.ChangeDescription("slow", "Sleep longer.")
	if _, err := g.pool.Sync(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	key, k := g.key(t, g.userID, []string{"gh__*"}, false)
	deniedRows := func() []map[string]any {
		_, events, _ := g.rec.snapshot()
		var out []map[string]any
		for _, e := range events {
			if e.Result != "denied" {
				continue
			}
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			out = append(out, p)
		}
		return out
	}
	for _, endpoint := range []struct{ slug, prefix string }{{"gh", ""}, {"", "gh__"}} {
		cs := g.connect(t, key, endpoint.slug, "")
		if _, err := call(t, cs, endpoint.prefix+"fail", nil); !isUnknownTool(err) {
			t.Fatalf("%q hidden tool: %v, want unknown tool", endpoint.slug, err)
		}
		rows := deniedRows()
		if len(rows) == 0 {
			t.Fatalf("%q: no denied row for a call of a hidden tool", endpoint.slug)
		}
		last := rows[len(rows)-1]
		if last["tool"] != "fail" || last["reason"] != "tool hidden" || last["gateway_key_id"] != k.ID {
			t.Fatalf("%q: denied row %v", endpoint.slug, last)
		}
		// The same minute: no second row, for this or another unserved tool.
		n := len(rows)
		if _, err := call(t, cs, endpoint.prefix+"slow", nil); !isUnknownTool(err) {
			t.Fatalf("%q pending tool: %v", endpoint.slug, err)
		}
		if len(deniedRows()) != n {
			t.Fatalf("%q: a second row within the minute: %v", endpoint.slug, deniedRows())
		}
		mu.Lock()
		now = now.Add(time.Minute)
		mu.Unlock()
		if _, err := call(t, cs, endpoint.prefix+"slow", nil); !isUnknownTool(err) {
			t.Fatalf("%q pending tool: %v", endpoint.slug, err)
		}
		rows = deniedRows()
		if len(rows) != n+1 || rows[n]["tool"] != "slow" || rows[n]["reason"] != "tool not approved" {
			t.Fatalf("%q: rows after a minute %v", endpoint.slug, rows)
		}
		mu.Lock()
		now = now.Add(time.Minute)
		mu.Unlock()
	}
	// A tool the source does not have at all leaves no row.
	cs := g.connect(t, key, "gh", "")
	n := len(deniedRows())
	if _, err := call(t, cs, "nonexistent", nil); !isUnknownTool(err) {
		t.Fatal(err)
	}
	if len(deniedRows()) != n {
		t.Fatal("a nonexistent tool left a denied row")
	}
}

// The answer patterns read the text of embedded resources in a result too.
func TestAnswerText_EmbeddedResources(t *testing.T) {
	secret := "bgw_Zq3xV9pL2mN8rT4kW7yB1cD6fG0hJ5sA9eU3iO2abc"
	got := answerText(&mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "see the file"},
		&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///x", MIMEType: "text/plain", Text: secret}},
	}})
	if !strings.Contains(got, secret) || !strings.Contains(got, "see the file") {
		t.Fatalf("answerText = %q", got)
	}
}
