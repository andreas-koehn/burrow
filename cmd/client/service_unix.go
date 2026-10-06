//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// isElevated reports whether this process is root.
func isElevated() bool { return os.Geteuid() == 0 }

// executableTrusted reports whether only root can change the binary at exe:
// the file and every directory above it belong to root and can be written by
// nobody else. A service that runs as root must not start anything less.
func executableTrusted(exe string) bool {
	p, err := filepath.EvalSymlinks(exe)
	if err != nil || !filepath.IsAbs(p) {
		return false
	}
	for {
		fi, err := os.Stat(p)
		if err != nil {
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

// programDataDir is Windows' ProgramData directory; there is none here.
func programDataDir() string { return "" }

// makePrivateDir creates a directory only its owner can use. It fails when
// the directory exists.
func makePrivateDir(path string) error { return os.Mkdir(path, 0o700) }
