//go:build !windows

package view

import "os"

// EnableANSI makes the console behind f interpret ANSI sequences. Only a
// Windows console has to be told; everywhere else there is nothing to do and
// nothing to restore.
func EnableANSI(*os.File) (restore func(), ok bool) { return func() {}, true }
