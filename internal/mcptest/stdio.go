package mcptest

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// BuildMockMCP compiles the stdio mock into the test's temp dir and returns
// the binary path. Needs a Go toolchain, which `go test` implies.
func BuildMockMCP(t testing.TB) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("mcptest: no go toolchain on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "mockmcp")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	// The test's working directory is its package directory, inside the module.
	cmd := exec.Command(gobin, "build", "-o", out, "github.com/ankoehn/burrow/internal/mcptest/cmd/mockmcp")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mcptest: build mockmcp: %v\n%s", err, b)
	}
	return out
}

// StdioCommand builds the mock and returns a command for it with args
// (--crash-after N, --name X), for mcp.CommandTransport or a supervisor.
func StdioCommand(t testing.TB, args ...string) *exec.Cmd {
	t.Helper()
	return exec.Command(BuildMockMCP(t), args...) //nolint:gosec // a binary this package just built
}
