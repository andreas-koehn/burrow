package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/mcptest"
)

// bridgeTools lists the tools behind a tunnel's local address, as the relay
// would see them through the tunnel.
func bridgeTools(t *testing.T, ctx context.Context, addr string) ([]string, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "relay-test"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + addr + "/", DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names, nil
}

var mockTools = []string{"big", "echo", "fail", "mutate", "slow"}

func TestMCP_CommandIsPublishedThroughTheBridge(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	mock := mcptest.BuildMockMCP(t)
	var seen []string
	h.onRun = func(ctx context.Context, tunnels []client.TunnelSpec) error {
		var err error
		seen, err = bridgeTools(t, ctx, tunnels[0].LocalAddr)
		return err
	}
	if code := h.exec("mcp", "files", "--", mock, "--name", "files"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	r := h.oneRun()
	if len(r.tunnels) != 1 {
		t.Fatalf("tunnels = %+v", r.tunnels)
	}
	tn := r.tunnels[0]
	if tn.Name != "files" || tn.Type != "http" || !strings.HasPrefix(tn.LocalAddr, "127.0.0.1:") || tn.Slug != "" || tn.Access != "" {
		t.Fatalf("tunnel = %+v", tn)
	}
	if tn.MCP == nil || !reflect.DeepEqual(tn.MCP.Command, []string{mock, "--name", "files"}) || tn.MCP.RestartMax != -1 || tn.MCP.Status == nil {
		t.Fatalf("MCP = %+v", tn.MCP)
	}
	if !slices.Equal(seen, mockTools) {
		t.Fatalf("tools through the bridge = %v", seen)
	}
	if tools, running, known := tn.MCP.Status.Get(); !known || tools != 5 {
		t.Fatalf("status = %d %v %v", tools, running, known)
	}
	// The command has ended: so has the bridge.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := bridgeTools(t, ctx, tn.LocalAddr); err == nil {
		t.Fatal("the bridge still answers after the command ended")
	}
	h.noToken()
}

func TestMCP_URLIsPublishedThroughTheBridge(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	h := newHarness(t)
	h.signIn()
	var seen []string
	h.onRun = func(ctx context.Context, tunnels []client.TunnelSpec) error {
		var err error
		seen, err = bridgeTools(t, ctx, tunnels[0].LocalAddr)
		return err
	}
	if code := h.exec("mcp", "notes", up.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	tn := h.oneRun().tunnels[0]
	if tn.Type != "http" || tn.MCP == nil || tn.MCP.URL != up.URL || len(tn.MCP.Command) != 0 || tn.LocalAddr == "" {
		t.Fatalf("tunnel = %+v (%+v)", tn, tn.MCP)
	}
	if !slices.Equal(seen, mockTools) {
		t.Fatalf("tools through the bridge = %v", seen)
	}
}

func TestMCP_EnvAndRestartMax(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	mock := mcptest.BuildMockMCP(t)
	if code := h.exec("mcp", "files", "--env", "A=1", "--env", "B=x=y", "--restart-max", "2", "--", mock); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	m := h.oneRun().tunnels[0].MCP
	if !reflect.DeepEqual(m.Env, []string{"A=1", "B=x=y"}) || m.RestartMax != 2 {
		t.Fatalf("MCP = %+v", m)
	}
}

func TestMCP_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"nothing", []string{"mcp"}, "a name"},
		{"name only", []string{"mcp", "files"}, "a command after --"},
		{"nothing after the dash", []string{"mcp", "files", "--"}, "a command after --"},
		{"command without the dash", []string{"mcp", "files", "npx"}, "put it after --"},
		{"two before the dash", []string{"mcp", "files", "extra", "--", "npx"}, "Usage:"},
		{"url and more", []string{"mcp", "files", "http://localhost:1/mcp", "x"}, "Usage:"},
		{"empty name", []string{"mcp", " ", "--", "npx"}, "name"},
		{"env without =", []string{"mcp", "files", "--env", "NOVALUE", "--", "npx"}, "KEY=VALUE"},
		{"env with an empty key", []string{"mcp", "files", "--env", "=v", "--", "npx"}, "KEY=VALUE"},
		{"env with a url", []string{"mcp", "files", "--env", "A=1", "http://localhost:1/mcp"}, "--env applies to a command"},
		{"restart-max with a url", []string{"mcp", "files", "--restart-max", "1", "http://localhost:1/mcp"}, "--restart-max applies to a command"},
		{"restart-max below -1", []string{"mcp", "files", "--restart-max", "-2", "--", "npx"}, "--restart-max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			if code := h.exec(tc.args...); code != exitUsage {
				t.Fatalf("exit %d, want %d; stderr %q", code, exitUsage, h.stderr.String())
			}
			if !strings.Contains(h.stderr.String(), tc.want) {
				t.Fatalf("stderr %q does not contain %q", h.stderr.String(), tc.want)
			}
			if len(h.runs) != 0 {
				t.Fatal("the client was started")
			}
		})
	}
}

func TestMCP_GivingUpBeforeTheFirstToolListEndsTheCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	h := newHarness(t)
	h.signIn()
	code := h.exec("mcp", "files", "--restart-max", "0", "--", "sh", "-c", "exit 3")
	if code != exitGeneral {
		t.Fatalf("exit %d, want %d; stderr %q", code, exitGeneral, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "not restarted again") {
		t.Fatalf("stderr %q does not say that the server is not restarted", h.stderr.String())
	}
	if len(h.runs) != 0 {
		t.Fatal("the client was started for a server that never answered")
	}
}

func TestMCP_GivingUpWhileRunningEndsTheCommand(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	mock := mcptest.BuildMockMCP(t)
	h.onRun = func(ctx context.Context, tunnels []client.TunnelSpec) error {
		c := mcp.NewClient(&mcp.Implementation{Name: "relay-test"}, nil)
		cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + tunnels[0].LocalAddr + "/", DisableStandaloneSSE: true}, nil)
		if err != nil {
			return err
		}
		defer func() { _ = cs.Close() }()
		// This call ends the child; it is not started again.
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "x"}})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second):
			t.Error("the run was not ended when the server was given up")
			return nil
		}
	}
	code := h.exec("mcp", "files", "--restart-max", "0", "--", mock, "--crash-after", "1")
	if code != exitGeneral || !strings.Contains(h.stderr.String(), "not restarted again") {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}

func TestUp_StartsABridgeForEveryMCPService(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	mock := mcptest.BuildMockMCP(t)
	yml := filepath.Join(t.TempDir(), "burrow.yaml")
	body := "services:\n" +
		"  - name: files\n    type: mcp\n    command: " + mock + "\n    args: [--name, files]\n    env: { A: b }\n" +
		"  - { name: web, local: 3000, type: http }\n"
	if err := os.WriteFile(yml, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	h.onRun = func(ctx context.Context, tunnels []client.TunnelSpec) error {
		var err error
		seen, err = bridgeTools(t, ctx, tunnels[0].LocalAddr)
		return err
	}
	if code := h.exec("up", "--file", yml); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	tunnels := h.oneRun().tunnels
	if len(tunnels) != 2 || tunnels[0].Type != "http" || !strings.HasPrefix(tunnels[0].LocalAddr, "127.0.0.1:") || tunnels[0].MCP == nil {
		t.Fatalf("tunnels = %+v", tunnels)
	}
	if tunnels[1] != (client.TunnelSpec{Name: "web", Type: "http", LocalAddr: "3000"}) {
		t.Fatalf("the http service changed: %+v", tunnels[1])
	}
	if !slices.Equal(seen, mockTools) {
		t.Fatalf("tools through the bridge = %v", seen)
	}
}
