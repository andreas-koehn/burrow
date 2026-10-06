package main

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// openableURL reports whether target may be handed to the program that opens
// the browser: a plain https URL without credentials, made of nothing but
// letters, digits and -._~:/?=&%[]. Nothing in it can be read as an option, a
// second argument or a command.
func openableURL(target string) bool {
	if !strings.HasPrefix(target, "https://") || len(target) > 512 {
		return false
	}
	for i := 0; i < len(target); i++ {
		c := target[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~:/?=&%[]", c) >= 0) {
			return false
		}
	}
	u, err := url.Parse(target)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Opaque == ""
}

// browserCommand returns the program that opens target in the default browser
// on goos, and its arguments. The URL is one argument of its own; no shell is
// involved. ok is false when target is not an openable URL or goos has no such
// program.
func browserCommand(goos, target string) (name string, args []string, ok bool) {
	if !openableURL(target) {
		return "", nil, false
	}
	switch goos {
	case "darwin":
		return "open", []string{target}, true
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", target}, true
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly", "solaris":
		return "xdg-open", []string{target}, true
	}
	return "", nil, false
}

// desktopSession reports whether a browser opened here would appear in front
// of the person who typed the command: not over SSH, and on Linux and the BSDs
// only with a display.
func desktopSession(goos string, getenv func(string) string) bool {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" || getenv("SSH_CLIENT") != "" {
		return false
	}
	switch goos {
	case "darwin", "windows":
		return true
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly", "solaris":
		return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
	}
	return false
}

// openBrowser opens target in the default browser and does not wait for it.
// It is deps.openBrowser outside tests. A failure is for the caller to ignore:
// the address is on the screen either way.
func openBrowser(target string) error {
	name, args, ok := browserCommand(runtime.GOOS, target)
	if !ok {
		return errors.New("no program to open a browser with")
	}
	if runtime.GOOS == "windows" {
		// By its full path, so that nothing else named rundll32 is run.
		if root := os.Getenv("SystemRoot"); root != "" {
			name = filepath.Join(root, "System32", "rundll32.exe")
		}
	}
	// No stdin, stdout or stderr: what the program prints does not land in
	// the middle of the sign-in.
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
