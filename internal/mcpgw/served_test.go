package mcpgw

import (
	"context"
	"testing"

	"github.com/ankoehn/burrow/internal/mcptest"
	"github.com/ankoehn/burrow/internal/store"
)

// A removed tool that a sync approves again can come back under a name that
// another tool is renamed to by then. Neither tool is served under the
// ambiguous name, and both are reported (T05 review).
func TestServedSet_DuplicateServedNamesServeNeither(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Tools: []mcptest.ToolSpec{mcptest.Echo("echo"), mcptest.Echo("mutate"), mcptest.Echo("other")}})
	f := newFixture(t, up)
	f.remote(t, "gh", up.URL)
	ctx := context.Background()
	if _, err := f.pool.Sync(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	approveAll(t, f.st, "gh")

	// echo disappears upstream; mutate is renamed to its free name.
	up.SetTools(mcptest.Echo("mutate"), mcptest.Echo("other"))
	if _, err := f.pool.Sync(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	rename := "echo"
	if _, err := f.st.UpdateMCPToolOverrides(ctx, "gh", "mutate", store.MCPToolOverridesUpdate{Rename: &rename}); err != nil {
		t.Fatalf("rename onto a removed tool's name: %v", err)
	}
	// echo comes back unchanged: the sync approves it again without a check.
	up.SetTools(mcptest.Echo("echo"), mcptest.Echo("mutate"), mcptest.Echo("other"))
	if _, err := f.pool.Sync(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	rows, err := f.st.ListMCPTools(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	served := 0
	for _, r := range rows {
		if _, ok := Served(r); ok {
			served++
		}
	}
	if served != 3 {
		t.Fatalf("precondition: %d rows servable on their own, want 3 (echo, mutate as echo, other)", served)
	}

	// The order of the rows must not matter.
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}} {
		rs := rows[:0:0]
		for _, i := range order {
			rs = append(rs, rows[i])
		}
		tools, conflicts := ServedSet(rs)
		if len(tools) != 1 || tools[0].Name != "other" {
			t.Fatalf("order %v: served %+v, want only other", order, tools)
		}
		if conflicts["echo"] != "echo" || conflicts["mutate"] != "echo" || len(conflicts) != 2 {
			t.Fatalf("order %v: conflicts = %v, want echo and mutate under echo", order, conflicts)
		}
	}
}
