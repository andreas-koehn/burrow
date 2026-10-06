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

// openerEnv is the environment for the program that opens the browser: env
// without burrow's own variables (BURROW_TOKEN, BURROW_TOKEN_FILE and the
// rest), in any spelling of the prefix. The result is never nil, which os/exec
// would read as "inherit everything".
func openerEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if len(kv) >= 7 && strings.EqualFold(kv[:7], "BURROW_") {
			continue
		}
		out = append(out, kv)
	}
	return out
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
	// the middle of the sign-in. No BURROW_ variable: a token in the
	// environment stays with burrow. A session of its own: Ctrl-C at burrow's
	// terminal does not reach the browser it started.
	cmd := exec.Command(name, args...)
	cmd.Env = openerEnv(os.Environ())
	detachOpener(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Nobody waits for the browser, but the program is reaped when it ends,
	// which may be at once or when the browser closes.
	go func() { _ = cmd.Wait() }()
	return nil
}
