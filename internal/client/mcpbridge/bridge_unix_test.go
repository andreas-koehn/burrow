//go:build !windows

package mcpbridge

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/mcptest"
)

// alive reports whether pid is a process that has not ended. A zombie (ended,
// not yet reaped by whoever inherited it) counts as ended.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := strings.LastIndexByte(string(b), ')'); i > 0 && len(b) > i+2 && b[i+2] == 'Z' {
			return false
		}
	}
	return true
}

func TestBridge_CloseEndsTheChild(t *testing.T) {
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

func TestBridge_CloseEndsGrandchildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	o := fast(t, nil, nil)
	o.Command = []string{"sh", "-c", `sleep 300 & echo $! > "$1"; exec "$0"`, mcptest.BuildMockMCP(t), pidFile}
	b := start(t, o)
	waitReady(t, b)
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || !alive(pid) {
		t.Fatalf("no running grandchild (%q, %v)", raw, err)
	}
	child := b.pidForTest()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the grandchild to end", func() bool { return !alive(pid) })
	if alive(child) {
		t.Fatalf("the child %d is still there", child)
	}
}

// A child that writes something other than JSON-RPC to stdout ends the
// session while it keeps running: the bridge ends it and starts it again.
func TestBridge_RestartsWhenTheSessionDies(t *testing.T) {
	buf := &syncBuffer{}
	o := fast(t, buf, nil)
	// The shell is the child and stays after the server ends: only the
	// session tells that something is wrong.
	o.Command = []string{"sh", "-c", `{ sleep 0.3; echo "this is not JSON"; } & "$0"; sleep 300`, mcptest.BuildMockMCP(t)}
	b := start(t, o)
	waitReady(t, b)
	eventually(t, "a restart after the session ended", func() bool { return b.startsForTest() >= 2 })
	if !strings.Contains(buf.String(), "session ended") {
		t.Fatalf("the log does not say the session ended:\n%s", buf.String())
	}
}

func (b *Bridge) pidForTest() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pid
}

// recordGroupSignals counts the signals sent to process groups (not the
// probe, signal 0) while the test runs.
func recordGroupSignals(t *testing.T) func() []syscall.Signal {
	t.Helper()
	var mu sync.Mutex
	var sent []syscall.Signal
	orig := groupSignal
	groupSignal = func(pgid int, sig syscall.Signal) error {
		if sig != 0 {
			mu.Lock()
			sent = append(sent, sig)
			mu.Unlock()
		}
		return orig(pgid, sig)
	}
	t.Cleanup(func() { groupSignal = orig })
	return func() []syscall.Signal {
		mu.Lock()
		defer mu.Unlock()
		return append([]syscall.Signal(nil), sent...)
	}
}

// Round 2: once the child has ended, its process group id can be reused
// when nothing is left in the group. terminate then sends no signal to the
// group; when something the child started is still there (which keeps the id
// from being reused), it is killed as before.
func TestTerminate_NoGroupKillWhenTheGroupIsEmpty(t *testing.T) {
	sent := recordGroupSignals(t)
	p, err := startProcess([]string{"sh", "-c", "exit 0"}, nil, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.closePipes()
	<-p.exited
	p.terminate(time.Second)
	if got := sent(); len(got) != 0 {
		t.Fatalf("signals sent to the empty group of an ended child: %v", got)
	}

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	p, err = startProcess([]string{"sh", "-c", `sleep 300 & echo $! > "$0"; exit 0`, pidFile}, nil, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.closePipes()
	<-p.exited
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || !alive(pid) {
		t.Fatalf("no running grandchild (%q, %v)", raw, err)
	}
	p.terminate(time.Second)
	if got := sent(); len(got) != 1 || got[0] != syscall.SIGKILL {
		t.Fatalf("signals sent to a group with a grandchild left: %v, want one SIGKILL", got)
	}
	eventually(t, "the grandchild to end", func() bool { return !alive(pid) })
}
