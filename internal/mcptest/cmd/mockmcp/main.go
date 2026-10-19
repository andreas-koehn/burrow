// Command mockmcp is a stdio MCP server with the mcptest tool set, for tests
// of `burrow mcp`. See package mcptest.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/mcptest"
)

func main() {
	name := flag.String("name", "mockmcp", "server implementation name")
	crash := flag.Int("crash-after", 0, "exit with status 3 when the Nth tools/call arrives, without answering (0: never)")
	flag.Parse()

	var n atomic.Int64
	srv := mcptest.NewSDKServer(*name, nil, func(mcptest.Call) {
		if *crash > 0 && n.Add(1) >= int64(*crash) {
			os.Exit(3)
		}
	})
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "mockmcp:", err)
		os.Exit(1)
	}
}
