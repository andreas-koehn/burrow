package mcpbridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/mcptest"
)

// syncBuffer is a bytes.Buffer for a logger that several goroutines write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// statusLog records what OnStatus reports.
type statusLog struct {
	mu   sync.Mutex
	seen []Status
}

func (l *statusLog) add(s Status) {
	l.mu.Lock()
	l.seen = append(l.seen, s)
	l.mu.Unlock()
}

func (l *statusLog) last() (Status, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) == 0 {
		return Status{}, false
	}
	return l.seen[len(l.seen)-1], true
}

// fast returns options that restart at once and log into buf.
func fast(t *testing.T, buf *syncBuffer, st *statusLog) Options {
	t.Helper()
	o := Options{
		Name:       "files",
		RestartMax: -1,
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
		TerminateGrace: 2 * time.Second,
	}
	if buf != nil {
		o.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if st != nil {
		o.OnStatus = st.add
	}
	return o
}

func start(t *testing.T, o Options) *Bridge {
	t.Helper()
	b, err := Start(o)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func waitReady(t *testing.T, b *Bridge) {
	t.Helper()
	select {
	case <-b.Ready():
	case <-time.After(20 * time.Second):
		t.Fatal("the bridge did not get a tool list")
	}
}

// eventually polls cond until it holds or the time is up.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// connect opens an MCP client session to the bridge, as the relay would.
func connect(t *testing.T, b *Bridge) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "relay-test"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: "http://" + b.Addr() + "/mcp", DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("connect to the bridge: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError
}

var defaultNames = []string{"big", "echo", "fail", "mutate", "slow"}

func TestBridge_StdioChildListsAndCalls(t *testing.T) {
	st := &statusLog{}
	o := fast(t, nil, st)
	o.Command = []string{mcptest.BuildMockMCP(t)}
	b := start(t, o)
	if !strings.HasPrefix(b.Addr(), "127.0.0.1:") {
		t.Fatalf("addr = %q, want a loopback address", b.Addr())
	}
	waitReady(t, b)
	cs := connect(t, b)
	if got := toolNames(t, cs); !slices.Equal(got, defaultNames) {
		t.Fatalf("tools = %v, want %v", got, defaultNames)
	}
	if text, isErr := callText(t, cs, "echo", map[string]any{"text": "hi"}); text != "echo: hi" || isErr {
		t.Fatalf("echo = %q (error %v)", text, isErr)
	}
	// An error result of the server comes back as one.
	if text, isErr := callText(t, cs, "fail", map[string]any{"message": "nope"}); !isErr || !strings.Contains(text, "nope") {
		t.Fatalf("fail = %q (error %v)", text, isErr)
	}
	if s, ok := st.last(); !ok || s.Tools != 5 || !s.Running {
		t.Fatalf("status = %+v (%v), want 5 tools, running", s, ok)
	}
	// The read-only hint survives the bridge.
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == "echo" && (tl.Annotations == nil || !tl.Annotations.ReadOnlyHint) {
			t.Fatalf("echo lost its read-only hint: %+v", tl.Annotations)
		}
	}
}

func TestBridge_ServesToolsOnly(t *testing.T) {
	o := fast(t, nil, nil)
	o.Command = []string{mcptest.BuildMockMCP(t)}
	b := start(t, o)
	waitReady(t, b)
	cs := connect(t, b)
	caps := cs.InitializeResult().Capabilities
	if caps == nil || caps.Tools == nil {
		t.Fatalf("capabilities %+v, want tools", caps)
	}
	if caps.Prompts != nil || caps.Resources != nil || caps.Logging != nil { //nolint:staticcheck // the bridge must not advertise logging
		t.Fatalf("capabilities %+v, want tools only", caps)
	}
}

func TestBridge_HTTPUpstream(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		name := "stateless"
		if stateful {
			name = "stateful"
		}
		t.Run(name, func(t *testing.T) {
			up := mcptest.NewServer(t, mcptest.Opts{Stateful: stateful})
			o := fast(t, nil, nil)
			o.URL = up.URL
			b := start(t, o)
			waitReady(t, b)
			cs := connect(t, b)
			if got := toolNames(t, cs); !slices.Equal(got, defaultNames) {
				t.Fatalf("tools = %v", got)
			}
			if text, _ := callText(t, cs, "echo", map[string]any{"text": "one"}); text != "echo: one" {
				t.Fatalf("echo = %q", text)
			}
			// The local server forgets its sessions: the bridge opens a new one.
			up.Restart()
			if text, isErr := callText(t, cs, "echo", map[string]any{"text": "two"}); text != "echo: two" || isErr {
				t.Fatalf("echo after the restart = %q (error %v)", text, isErr)
			}
			if n := len(up.CallsTo("echo")); n != 2 {
				t.Fatalf("upstream saw %d echo calls, want 2", n)
			}
			_ = b.Close()
			select {
			case <-b.Done():
			default:
				t.Fatal("Done is open after Close")
			}
		})
	}
}

func TestBridge_RefreshesTheToolList(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	st := &statusLog{}
	o := fast(t, nil, st)
	o.URL = up.URL
	o.RefreshInterval = 30 * time.Millisecond
	b := start(t, o)
	waitReady(t, b)
	cs := connect(t, b)
	up.SetTools(mcptest.Echo("echo"), mcptest.Echo("added"))
	eventually(t, "the new tool list", func() bool {
		return slices.Equal(toolNames(t, cs), []string{"added", "echo"})
	})
	eventually(t, "the status", func() bool {
		s, _ := st.last()
		return s.Tools == 2
	})
}

func TestBridge_RestartsACrashedChild(t *testing.T) {
	buf := &syncBuffer{}
	st := &statusLog{}
	o := fast(t, buf, st)
	o.Command = []string{mcptest.BuildMockMCP(t), "--crash-after", "1"}
	b := start(t, o)
	waitReady(t, b)
	cs := connect(t, b)
	// The first call ends the child without an answer.
	if _, isErr := callText(t, cs, "echo", map[string]any{"text": "x"}); !isErr {
		t.Fatal("a call that crashed the child was not an error result")
	}
	eventually(t, "the restart", func() bool { return b.restartsForTest() >= 1 && b.running() })
	// The tool list stays while the child restarts, and the new child answers.
	if got := toolNames(t, cs); !slices.Equal(got, defaultNames) {
		t.Fatalf("tools after the restart = %v", got)
	}
	if !strings.Contains(buf.String(), "source=files") || !strings.Contains(buf.String(), "exit status 3") {
		t.Fatalf("log does not report the exit:\n%s", buf.String())
	}
}

func TestBridge_RestartsWithBackoffAndStopsAtTheMax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	var mu sync.Mutex
	var waits []time.Duration
	o := fast(t, nil, nil)
	o.Command = []string{"sh", "-c", "exit 3"}
	o.RestartMax = 3
	o.BackoffMin, o.BackoffMax = time.Millisecond, 4*time.Millisecond
	o.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return nil
	}
	b := start(t, o)
	select {
	case <-b.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("the supervisor did not give up")
	}
	if b.Err() == nil || !strings.Contains(b.Err().Error(), "restart") {
		t.Fatalf("Err = %v, want one that names the restarts", b.Err())
	}
	if n := b.startsForTest(); n != 4 {
		t.Fatalf("the child was started %d times, want 4 (one start, three restarts)", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 3 {
		t.Fatalf("waited %d times, want 3: %v", len(waits), waits)
	}
	// Full-jitter exponential: the n-th wait is at most min·2ⁿ, capped at max.
	for i, d := range waits {
		limit := time.Millisecond << i
		limit = min(limit, 4*time.Millisecond)
		if d < 0 || d > limit {
			t.Fatalf("wait %d = %v, want within [0, %v]", i, d, limit)
		}
	}
	// Ready no longer blocks a waiter.
	select {
	case <-b.Ready():
	default:
		t.Fatal("Ready is still open after the supervisor gave up")
	}
}

func TestBridge_RestartMaxZeroNeverRestarts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	o := fast(t, nil, nil)
	o.Command = []string{"sh", "-c", "exit 1"}
	o.RestartMax = 0
	b := start(t, o)
	<-b.Done()
	if n := b.startsForTest(); n != 1 {
		t.Fatalf("started %d times, want 1", n)
	}
}

func TestBridge_ChildEnvAndStderrReachTheLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	buf := &syncBuffer{}
	o := fast(t, buf, nil)
	o.Command = []string{"sh", "-c", `echo "hello from $BRIDGE_TEST_VALUE" >&2; exec "$0"`, mcptest.BuildMockMCP(t)}
	o.Env = []string{"BRIDGE_TEST_VALUE=the-child"}
	b := start(t, o)
	waitReady(t, b)
	eventually(t, "the stderr line", func() bool { return strings.Contains(buf.String(), "hello from the-child") })
	if !strings.Contains(buf.String(), "source=files") {
		t.Fatalf("the stderr line does not name the source:\n%s", buf.String())
	}
}

func TestBridge_CloseEndsTheChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("checks the pid with signal 0")
	}
	o := fast(t, nil, nil)
	o.Command = []string{mcptest.BuildMockMCP(t)}
	b := start(t, o)
	waitReady(t, b)
	pid := b.pidForTest()
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		t.Fatalf("no running child (pid %d)", pid)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the child %d is still there after Close (kill 0: %v)", pid, err)
	}
	select {
	case <-b.Done():
	default:
		t.Fatal("Done is open after Close")
	}
	if b.Err() != nil {
		t.Fatalf("Err after Close = %v, want nil", b.Err())
	}
}

func TestTerminate_SIGTERMThenKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh and signals")
	}
	t.Run("exits on SIGTERM", func(t *testing.T) {
		p, err := startProcess([]string{"sh", "-c", "while :; do sleep 0.05; done"}, nil, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer p.closePipes()
		began := time.Now()
		p.terminate(5 * time.Second)
		if d := time.Since(began); d > 3*time.Second {
			t.Fatalf("terminate took %v for a process that ends on SIGTERM", d)
		}
	})
	t.Run("killed after the grace period", func(t *testing.T) {
		p, err := startProcess([]string{"sh", "-c", `trap "" TERM; while :; do sleep 0.05; done`}, nil, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer p.closePipes()
		time.Sleep(100 * time.Millisecond) // let the shell set its trap
		began := time.Now()
		p.terminate(300 * time.Millisecond)
		d := time.Since(began)
		if d < 300*time.Millisecond {
			t.Fatalf("terminate returned after %v, before the grace period", d)
		}
		select {
		case <-p.exited:
		default:
			t.Fatal("the process is still running")
		}
	})
}

func TestStart_Validation(t *testing.T) {
	cases := []struct {
		name string
		o    Options
	}{
		{"neither", Options{Name: "x"}},
		{"both", Options{Name: "x", Command: []string{"a"}, URL: "http://127.0.0.1:1/mcp"}},
		{"bad scheme", Options{Name: "x", URL: "ftp://127.0.0.1/mcp"}},
		{"no host", Options{Name: "x", URL: "http:///mcp"}},
		{"empty command", Options{Name: "x", Command: []string{""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := Start(tc.o); err == nil {
				_ = b.Close()
				t.Fatal("Start accepted it")
			}
		})
	}
}

func TestBridge_CommandNotFoundKeepsTrying(t *testing.T) {
	buf := &syncBuffer{}
	o := fast(t, buf, nil)
	o.Command = []string{"burrow-test-no-such-command"}
	b := start(t, o)
	eventually(t, "two attempts", func() bool { return b.startsForTest() >= 2 })
	if _, err := exec.LookPath("burrow-test-no-such-command"); err == nil {
		t.Skip("the command exists here")
	}
	if !strings.Contains(buf.String(), "burrow-test-no-such-command") {
		t.Fatalf("the log does not name the command:\n%s", buf.String())
	}
}
