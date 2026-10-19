//go:build !windows

package mcpbridge

import (
	"os/exec"
	"syscall"
)

// platformGroup is the child's process group id.
type platformGroup struct{ pgid int }

// prepare puts the child into a process group of its own. A Ctrl-C in the
// terminal then does not reach it directly, and terminate reaches whatever
// it started.
//
// Pdeathsig is not used: Linux sends it when the OS thread that started the
// child ends, and Go ends threads on its own schedule.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (p *process) started() { p.group.pgid = p.cmd.Process.Pid }

// stop sends SIGTERM to the group.
func (p *process) stop() { _ = syscall.Kill(-p.group.pgid, syscall.SIGTERM) }

// kill sends SIGKILL to the group.
func (p *process) kill() { _ = syscall.Kill(-p.group.pgid, syscall.SIGKILL) }

func (p *process) release() {}
