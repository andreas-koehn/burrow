//go:build !unix

package main

import "os/exec"

// detachOpener does nothing here: the program that opens the browser on
// Windows hands the address to the shell and ends at once.
func detachOpener(*exec.Cmd) {}
