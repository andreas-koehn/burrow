package mcpgw

import (
	"sort"

	"github.com/ankoehn/burrow/internal/db"
)

// IssueNameConflict is the serve issue of a tool that is not served because
// another tool of the same source would be served under the same name.
const IssueNameConflict = "name_conflict"

// ServedSet returns the tools of one source that the gateway serves, ordered
// by served name, and the tools left out because their served names collide.
//
// The approve and rename routes refuse a collision, but a sync can still
// make one: a removed tool that comes back with its approved definition is
// approved again without a check, and its native name may be another tool's
// rename by then (unhiding is not checked either). Such a name is ambiguous,
// so neither tool is served: a call by that name could otherwise reach the
// tool the caller did not mean, and which one wins would depend on the
// order of the rows. conflicts maps the native name of every tool left out
// to the served name they share; it is nil when there is none.
func ServedSet(rows []db.MCPTool) (tools []Tool, conflicts map[string]string) {
	byName := map[string][]Tool{}
	for _, row := range rows {
		t, ok := Served(row)
		if !ok {
			continue
		}
		byName[t.Name] = append(byName[t.Name], t)
	}
	for name, ts := range byName {
		if len(ts) == 1 {
			tools = append(tools, ts[0])
			continue
		}
		if conflicts == nil {
			conflicts = map[string]string{}
		}
		for _, t := range ts {
			conflicts[t.Native] = name
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, conflicts
}
