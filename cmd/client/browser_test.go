package main

import (
	"reflect"
	"testing"
)

func TestBrowserCommand_PerOS(t *testing.T) {
	const u = "https://burrow.example.com/link?code=BRRW-7Q4K"
	for goos, want := range map[string][]string{
		"linux":   {"xdg-open", u},
		"freebsd": {"xdg-open", u},
		"darwin":  {"open", u},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", u},
	} {
		name, args, ok := browserCommand(goos, u)
		if got := append([]string{name}, args...); !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v (ok %v), want %v", goos, got, ok, want)
		}
		// The URL is one argument, the last one, exactly as given.
		if args[len(args)-1] != u {
			t.Fatalf("%s: the URL is not passed as it is", goos)
		}
	}
	if _, _, ok := browserCommand("plan9", u); ok {
		t.Fatal("a program was chosen for an OS without one")
	}
}

// No shell and no command interpreter starts the browser, on any OS.
func TestBrowserCommand_NoShell(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows", "freebsd"} {
		name, args, _ := browserCommand(goos, "https://burrow.example.com/link?code=BRRW-7Q4K")
		for _, word := range append([]string{name}, args...) {
			switch word {
			case "sh", "bash", "cmd", "cmd.exe", "/c", "-c", "start", "powershell":
				t.Fatalf("%s: %q is part of the command", goos, word)
			}
		}
	}
}

func TestBrowserCommand_OnlyPlainHTTPS(t *testing.T) {
	for _, u := range []string{
		"",
		"http://burrow.example.com/link",
		"http://127.0.0.1/link",
		"http://localhost/link",
		"HTTPS://burrow.example.com/link",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"--help",
		"-https://burrow.example.com",
		" https://burrow.example.com/link",
		"https://",
		"https:///link",
		"https://user:pw@burrow.example.com/link",
		"https://burrow.example.com@evil.example.com/link",
		"https://burrow.example.com/link?code=A B",
		"https://burrow.example.com/link?code=A;rm -rf ~",
		"https://burrow.example.com/link?code=$(id)",
		"https://burrow.example.com/link?code=`id`",
		"https://burrow.example.com/link?code=A|calc",
		`https://burrow.example.com/link?code="&calc`,
		`https://burrow.example.com\@evil.example.com`,
		"https://burrow.example.com/link?code=A\nB",
		"https://burrow.example.com/link?code=\x1b[2J",
		"https://burrow.example.com/link#frag",
	} {
		for _, goos := range []string{"linux", "darwin", "windows"} {
			if name, args, ok := browserCommand(goos, u); ok {
				t.Fatalf("%s: %q would be opened with %s %q", goos, u, name, args)
			}
		}
	}
	for _, u := range []string{
		"https://burrow.example.com/link?code=BRRW-7Q4K",
		"https://burrow.example.com:8443/link?code=BRRW-7Q4K",
		"https://[::1]:8443/link?code=BRRW-7Q4K",
		"https://127.0.0.1:8443/link?code=BRRW-7Q4K&x=1",
	} {
		if _, _, ok := browserCommand("linux", u); !ok {
			t.Fatalf("%q is refused", u)
		}
	}
}

func TestDesktopSession(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		name string
		goos string
		env  func(string) string
		want bool
	}{
		{"linux with X", "linux", env("DISPLAY", ":0"), true},
		{"linux with Wayland", "linux", env("WAYLAND_DISPLAY", "wayland-0"), true},
		{"linux without a display", "linux", env(), false},
		{"linux over ssh with X forwarding", "linux", env("DISPLAY", "localhost:10.0", "SSH_CONNECTION", "10.0.0.2 51234 10.0.0.1 22"), false},
		{"linux, ssh tty", "linux", env("DISPLAY", ":0", "SSH_TTY", "/dev/pts/3"), false},
		{"mac", "darwin", env(), true},
		{"mac over ssh", "darwin", env("SSH_CONNECTION", "10.0.0.2 51234 10.0.0.1 22"), false},
		{"windows", "windows", env(), true},
		{"windows over ssh", "windows", env("SSH_CLIENT", "10.0.0.2 51234 22"), false},
		{"an OS without a browser program", "plan9", env("DISPLAY", ":0"), false},
	} {
		if got := desktopSession(tc.goos, tc.env); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
