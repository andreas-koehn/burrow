//go:build unix

package client

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own and makes the end
// of its context kill the whole group. A staged binary that does not answer
// `version` in time is ended with everything it started, not alone.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The group's id is the id of its first process.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
