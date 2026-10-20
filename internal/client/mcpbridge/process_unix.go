//go:build !windows

package mcpbridge

import (
	"errors"
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

// groupSignal sends sig to the process group pgid; tests replace it.
var groupSignal = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

// stop sends SIGTERM to the group.
func (p *process) stop() { _ = groupSignal(p.group.pgid, syscall.SIGTERM) }

// kill sends SIGKILL to the group.
func (p *process) kill() { _ = groupSignal(p.group.pgid, syscall.SIGKILL) }

// groupEmpty reports whether nothing is left in the child's process group.
// It is asked right after the child was reaped: from then on the group id
// may be given to an unrelated process once the group is empty, while a
// process left in the group keeps the id from being reused.
func (p *process) groupEmpty() bool {
	return errors.Is(groupSignal(p.group.pgid, 0), syscall.ESRCH)
}

func (p *process) release() {}
