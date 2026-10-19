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

// Issues that keep a tool off the aggregated endpoint (D11). A tool in a name
// conflict has the issue IssueNameConflict there as well.
const (
	// IssueNameTooLong: "<slug>__<served name>" is over 64 characters.
	IssueNameTooLong = "name_too_long"
	// IssueNameInvalid: the name has characters outside [A-Za-z0-9_-].
	IssueNameInvalid = "name_invalid"
	// MaxAggregatedNameLen is the longest name offered on /mcp.
	MaxAggregatedNameLen = 64
)

// AggregatedName returns the name under which a tool of source slug, served
// as served on /mcp/<slug>, is offered on /mcp: "<slug>__<served>". When that
// name is over 64 characters or has characters outside [A-Za-z0-9_-] it
// returns "" and the issue; the tool is then offered on /mcp/<slug> only,
// until a rename fixes it (D11).
func AggregatedName(slug, served string) (name, issue string) {
	name = slug + "__" + served
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return "", IssueNameInvalid
	}
	if len(name) > MaxAggregatedNameLen {
		return "", IssueNameTooLong
	}
	return name, ""
}
