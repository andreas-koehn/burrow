package main

// e2e_mcp_live_test.go — the SDK's MCP client against a relay that is
// already running: a container on this machine, or a deployment.
//
// Skipped unless both variables are set:
//
//	BURROW_LIVE_MCP_URL    e.g. https://burrow.example.com/mcp/burrow
//	BURROW_LIVE_MCP_TOKEN  an automation token (bua_…) of that relay
//
// The test only reads: it lists the tools the token may call and calls
// every read-only one that needs no argument. It prints tool names and
// result sizes, never a result and never the token.

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLiveMCP(t *testing.T) {
	url, token := os.Getenv("BURROW_LIVE_MCP_URL"), os.Getenv("BURROW_LIVE_MCP_TOKEN")
	if url == "" || token == "" {
		t.Skip("set BURROW_LIVE_MCP_URL and BURROW_LIVE_MCP_TOKEN to run against a live relay")
	}
	for _, revision := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(revision, func(t *testing.T) {
			cs := mcpConnect(t, url, token, revision)
			init := cs.InitializeResult()
			if init.ProtocolVersion != revision || init.ServerInfo == nil || init.ServerInfo.Name != "burrow" {
				t.Fatalf("handshake: revision %q, server %+v", init.ProtocolVersion, init.ServerInfo)
			}
			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			if len(res.Tools) == 0 {
				t.Fatal("the token sees no tool: mint one with a permission list (docs/guide/mcp.md)")
			}
			for _, tool := range res.Tools {
				schema, _ := json.Marshal(tool.InputSchema)
				var in struct {
					Required []string `json:"required"`
				}
				_ = json.Unmarshal(schema, &in)
				if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || len(in.Required) > 0 {
					t.Logf("%-26s listed, not called", tool.Name)
					continue
				}
				out, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name, Arguments: map[string]any{}})
				if err != nil {
					t.Errorf("%s: %v", tool.Name, err)
					continue
				}
				structured, _ := json.Marshal(out.StructuredContent)
				if out.IsError || len(out.Content) != 1 || len(structured) < 2 || structured[0] != '{' {
					t.Errorf("%s: isError=%v, %d content blocks, structuredContent %d bytes", tool.Name, out.IsError, len(out.Content), len(structured))
					continue
				}
				t.Logf("%-26s ok, %d bytes", tool.Name, len(structured))
			}
		})
	}
}
