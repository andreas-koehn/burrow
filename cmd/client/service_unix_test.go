//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathTrusted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root every file of this test is root's")
	}
	// A system file: root's, and so is every directory above it.
	sh, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if !pathTrusted(sh) {
		t.Errorf("%s is not trusted", sh)
	}
	// A file of the user who runs the test, as in ~/.local/bin or ~/.config.
	own := filepath.Join(t.TempDir(), "burrow")
	if err := os.WriteFile(own, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if pathTrusted(own) {
		t.Error("a file its user can replace is trusted")
	}
	// A link in a user's directory to a system file: whoever owns the
	// directory decides where it points tomorrow.
	link := filepath.Join(t.TempDir(), "sh")
	if err := os.Symlink(sh, link); err != nil {
		t.Fatal(err)
	}
	if pathTrusted(link) {
		t.Error("a link in a user's directory is trusted")
	}
	// The path is taken as written or not at all: one with "..", "." or a
	// doubled or trailing slash names the same file today, and what is
	// checked must be what the unit file then holds.
	dir, base := filepath.Dir(sh), filepath.Base(sh)
	for _, p := range []string{
		dir + "//" + base,
		dir + "/./" + base,
		dir + "/../" + filepath.Base(dir) + "/" + base,
		"//" + sh[1:],
		dir + "/",
	} {
		if p == filepath.Clean(p) {
			continue // "/" + "/" on a system where sh is in the root
		}
		if pathTrusted(p) {
			t.Errorf("%q is trusted although it is not a clean path", p)
		}
	}
	if !pathTrusted(dir) {
		t.Errorf("%s is not trusted", dir)
	}
	// Root's file, but through a world-writable directory's name is fine
	// only when no element of the path is a link; a relative path never is.
	for _, p := range []string{"", "burrow", "bin/sh", "/bin/../bin/missing", filepath.Join(t.TempDir(), "missing")} {
		if pathTrusted(p) {
			t.Errorf("%q is trusted", p)
		}
	}
}
