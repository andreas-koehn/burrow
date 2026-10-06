//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// isElevated reports whether this process is root.
func isElevated() bool { return os.Geteuid() == 0 }

// pathTrusted reports whether only root can change the file at path: path is
// a full path in its plain form (no "..", no ".", no doubled or trailing
// slash) with no symbolic link in it, and the file and every directory above
// it belong to root and can be written by nobody else. A service that runs as
// root must not start, or read on root's behalf, anything less. The plain
// form is required rather than produced: the caller writes the path it gave
// into the unit file, and what was checked must be what is written.
func pathTrusted(path string) bool {
	if !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return false
	}
	p := path
	// A link on the way is somebody's to point elsewhere, or root's own
	// business: either way the caller names the file itself.
	if r, err := filepath.EvalSymlinks(p); err != nil || r != p {
		return false
	}
	for {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			return false
		}
		parent := filepath.Dir(p)
		if parent == p {
			return true
		}
		p = parent
	}
}

// isLink reports whether fi, from Lstat, is a link rather than a file or a
// directory of its own.
func isLink(fi os.FileInfo) bool { return fi.Mode()&os.ModeSymlink != 0 }

// programDataDir is Windows' ProgramData directory; there is none here.
func programDataDir() string { return "" }

// makePrivateDir creates a directory only its owner can use. It fails when
// the directory exists.
func makePrivateDir(path string) error { return os.Mkdir(path, 0o700) }
