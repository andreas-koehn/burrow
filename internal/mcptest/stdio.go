package mcptest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// BuildMockMCP compiles the stdio mock once per test process and returns the
// binary path. The binary lives in a temp directory created with
// os.MkdirTemp that is deliberately not removed: there is no TestMain to hook
// a cleanup on, and the file is small. Needs a Go toolchain on PATH, which
// `go test` implies.
func BuildMockMCP(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		gobin, err := exec.LookPath("go")
		if err != nil {
			buildErr = fmt.Errorf("no go toolchain on PATH: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "mcptest-mockmcp-")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "mockmcp")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		// The first caller's working directory is its package directory,
		// inside the module.
		cmd := exec.Command(gobin, "build", "-o", out, "github.com/ankoehn/burrow/internal/mcptest/cmd/mockmcp")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build mockmcp: %w\n%s", err, b)
			return
		}
		buildPath = out
	})
	if buildErr != nil {
		t.Fatalf("mcptest: %v", buildErr)
	}
	return buildPath
}

// StdioCommand builds the mock (once per process) and returns a command for it with args
// (--crash-after N, --name X), for mcp.CommandTransport or a supervisor.
func StdioCommand(t testing.TB, args ...string) *exec.Cmd {
	t.Helper()
	return exec.Command(BuildMockMCP(t), args...) //nolint:gosec // a binary this package just built
}
