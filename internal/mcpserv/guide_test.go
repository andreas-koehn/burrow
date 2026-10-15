package mcpserv

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// guidePath is the operator's guide to this endpoint. The test reads it from
// the source tree so that the numbers in it cannot drift from the server.
const guidePath = "../../docs/guide/mcp.md"

func TestGuide_MatchesTheServer(t *testing.T) {
	raw, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("read %s: %v", guidePath, err)
	}
	guide := string(raw)

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
