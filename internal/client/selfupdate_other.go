//go:build !unix

package client

import "os/exec"

// ownProcessGroup does nothing here: the end of the context kills the
// process, as os/exec does by default.
func ownProcessGroup(*exec.Cmd) {}
