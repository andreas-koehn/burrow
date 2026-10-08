//go:build unix

package client

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A staged binary that does not answer in time is ended together with
// everything it started: nothing of a download that failed its check keeps
// running after the update gave up.
func TestReplaceExecutable_TimeoutEndsWhatTheStagedBinaryStarted(t *testing.T) {
	prev := stagedCheckTimeout
	stagedCheckTimeout = 500 * time.Millisecond
	t.Cleanup(func() { stagedCheckTimeout = prev })

	dir := t.TempDir()
	current := filepath.Join(dir, "burrow")
	writeFile(t, current, "old client", 0o755)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	newBin := filepath.Join(t.TempDir(), "burrow")
	// The child keeps the output open, as a helper of a real program would.
	writeFile(t, newBin, "#!/bin/sh\n/bin/sleep 300 &\necho $! >"+pidFile+"\nwait\n", 0o755)

	err := replaceExecutable(current, newBin, thisPlatform("0.6.0"), false)
	if !errors.Is(err, ErrRunCheck) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v, want the timeout as ErrRunCheck", err)
	}
	raw, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("the staged binary did not get as far as starting its child: %v", rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil || pid <= 1 {
		t.Fatalf("child pid %q", raw)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(3 * time.Second)
	for processRuns(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d, started by the staged binary, still runs after the check timed out", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, _ := os.ReadFile(current); string(got) != "old client" {
		t.Fatalf("the old binary changed: %q", got)
	}
	if got := names(t, dir); len(got) != 1 {
		t.Fatalf("directory holds %v", got)
	}
}

// processRuns reports whether pid is a process that still executes. A killed
// process nobody has collected yet (it was handed to an init that does not
// reap, as in a container) is not one.
func processRuns(pid int) bool {
	// Signal 0 only asks whether the process is there.
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true // no /proc here: the signal's answer stands
	}
	// "<pid> (<name>) <state> …"; the name may hold spaces and brackets.
	rest := string(stat[bytes.LastIndexByte(stat, ')')+1:])
	return !strings.HasPrefix(strings.TrimSpace(rest), "Z")
}
