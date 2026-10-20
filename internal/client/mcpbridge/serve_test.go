package mcpbridge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/mcptest"
)

// servedNames is the sorted list of tools the bridge serves now.
func servedNames(b *Bridge) []string {
	b.toolsMu.Lock()
	defer b.toolsMu.Unlock()
	var out []string
	for n := range b.served {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func tool(name, desc string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: desc, InputSchema: map[string]any{"type": "object"}}
}

// Round 2: two fetches of the tool list can overlap (the first after a
// connect and the refresher). The list of the fetch that started last is the
// one served; an older one that finishes later is dropped.
func TestBridge_OnlyTheNewestToolListIsServed(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	o := fast(t, nil, nil)
	o.URL = up.URL
	o.RefreshInterval = time.Hour
	b := start(t, o)
	waitReady(t, b)
	older, newer := b.nextListGen(), b.nextListGen()
	b.serve(newer, []*mcp.Tool{tool("new", "")})
	b.serve(older, []*mcp.Tool{tool("old", "")})
	if got := servedNames(b); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("served %v after an older list finished last, want [new]", got)
	}
	b.serve(b.nextListGen(), []*mcp.Tool{tool("newest", "")})
	if got := servedNames(b); !slices.Equal(got, []string{"newest"}) {
		t.Fatalf("served %v, want [newest]", got)
	}
}

// Round 2: a tool whose definition the SDK refuses is logged once per
// definition, not at every refresh; a changed definition is logged again.
func TestBridge_RefusedToolIsLoggedOncePerDefinition(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	buf := &syncBuffer{}
	o := fast(t, buf, nil)
	o.URL = up.URL
	o.RefreshInterval = time.Hour
	b := start(t, o)
	waitReady(t, b)
	bad := func(desc string) *mcp.Tool {
		return &mcp.Tool{Name: "bad", Description: desc, InputSchema: map[string]any{"type": "string"}}
	}
	count := func() int { return strings.Count(buf.String(), "tool left out") }
	for i := 0; i < 3; i++ {
		b.serve(b.nextListGen(), []*mcp.Tool{tool("echo", ""), bad("v1")})
	}
	if n := count(); n != 1 {
		t.Fatalf("refused tool logged %d times over three refreshes, want 1:\n%s", n, buf.String())
	}
	if got := servedNames(b); !slices.Equal(got, []string{"echo"}) {
		t.Fatalf("served %v", got)
	}
	b.serve(b.nextListGen(), []*mcp.Tool{tool("echo", ""), bad("v2")})
	if n := count(); n != 2 {
		t.Fatalf("a changed refused definition: logged %d times in all, want 2", n)
	}
	// Gone and back: logged again.
	b.serve(b.nextListGen(), []*mcp.Tool{tool("echo", "")})
	b.serve(b.nextListGen(), []*mcp.Tool{tool("echo", ""), bad("v2")})
	if n := count(); n != 3 {
		t.Fatalf("a refused tool that came back: logged %d times in all, want 3", n)
	}
}
