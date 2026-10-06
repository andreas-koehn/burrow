//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExecutableTrusted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root every file of this test is root's")
	}
	// A system binary: root's, and so is every directory above it.
	if !executableTrusted("/bin/sh") {
		t.Error("/bin/sh is not trusted")
	}
	// A binary of the user who runs the test, as in ~/.local/bin.
	own := filepath.Join(t.TempDir(), "burrow")
	if err := os.WriteFile(own, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if executableTrusted(own) {
		t.Error("a binary its user can replace is trusted")
	}
	// A link in a user's directory to a system binary is the system binary;
	// the command resolves links before it asks, and so does this.
	link := filepath.Join(t.TempDir(), "sh")
	if err := os.Symlink("/bin/sh", link); err != nil {
		t.Fatal(err)
	}
	if !executableTrusted(link) {
		t.Error("a link to a system binary is not trusted")
	}
	for _, p := range []string{"", "burrow", filepath.Join(t.TempDir(), "missing")} {
		if executableTrusted(p) {
			t.Errorf("%q is trusted", p)
		}
	}
}
