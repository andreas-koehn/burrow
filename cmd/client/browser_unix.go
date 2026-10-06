//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detachOpener starts the program that opens the browser in a session of its
// own: it is not in the foreground process group of burrow's terminal, so
// Ctrl-C and a closing terminal end burrow and not the browser.
func detachOpener(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
