package mcpserv

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// guidePath is the operator's guide to this endpoint. The test reads it from
// the source tree so that the tool table in it cannot drift from the registry.
const guidePath = "../../docs/guide/mcp.md"

func TestGuide_MatchesTheServer(t *testing.T) {
	raw, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("read %s: %v", guidePath, err)
	}
	guide := string(raw)

	// One table row per tool: | `name` | what it does | `permission` | kind |
	row := regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9_]*)` \\|[^|]*\\| `([a-z:_]+)` \\| ([^|]*) \\|$")
	documented := map[string][2]string{}
	for _, m := range row.FindAllStringSubmatch(guide, -1) {
		documented[m[1]] = [2]string{m[2], m[3]}
	}
	tools := buildTools(nil)
	if len(documented) != len(tools) {
		t.Errorf("the guide's tool table has %d rows, the registry %d tools", len(documented), len(tools))
	}
	for _, name := range orderedTools {
		tl := tools[name]
		doc, ok := documented[name]
		if !ok {
			t.Errorf("tool %s is missing from the guide's table", name)
			continue
		}
		if doc[0] != string(tl.Permission) {
			t.Errorf("%s: the guide names permission %s, the registry %s", name, doc[0], tl.Permission)
		}
		kind := doc[1]
		if tl.ReadOnly != (kind == "read") || tl.Destructive != strings.Contains(kind, "destructive") ||
			tl.Idempotent != strings.Contains(kind, "repeatable") {
			t.Errorf("%s: the guide says %q; registry: readOnly=%v destructive=%v idempotent=%v",
				name, kind, tl.ReadOnly, tl.Destructive, tl.Idempotent)
		}
	}

	// The numbers and names an operator copies from the guide.
	for _, want := range append([]string{
		EndpointPath,
		fmt.Sprintf("%d per token and minute", DefaultRateLimit),
		"1 MiB",
		fmt.Sprintf("%d seconds", int(toolTimeout/time.Second)),
		"Authorization: Bearer",
	}, protocolVersions...) {
		if !strings.Contains(guide, want) {
			t.Errorf("the guide does not mention %q", want)
		}
	}
}
