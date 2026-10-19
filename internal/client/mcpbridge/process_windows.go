//go:build windows

package mcpbridge

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformGroup is a job object holding the child and what it starts. 0 when
// the job could not be set up; the child alone is killed then.
type platformGroup struct{ job windows.Handle }

func prepare(*exec.Cmd) {}

// started puts the child into a job that ends all its processes when the job
// is terminated or its last handle closes (burrow itself ending included). A
// process the child starts before it is assigned escapes the job; that window
// is the time between CreateProcess and this call.
func (p *process) started() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	p.group.job = job
}

// stop: Windows has no SIGTERM for a console child; it is killed at once.
func (p *process) stop() { p.kill() }

func (p *process) kill() {
	if p.group.job != 0 {
		_ = windows.TerminateJobObject(p.group.job, 1)
	}
	_ = p.cmd.Process.Kill()
}

func (p *process) release() {
	if p.group.job != 0 {
		_ = windows.CloseHandle(p.group.job)
		p.group.job = 0
	}
}
