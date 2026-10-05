package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/client"
)

// stored reads the user config the command wrote.
func (h *harness) stored(path string) client.UserConfig {
	h.t.Helper()
	c, err := client.LoadUserConfig(path)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) hasConfig() bool {
	h.t.Helper()
	_, err := os.Stat(h.cfgPath)
	return err == nil
}

func TestLoginToken_StoresTheSignIn(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want client.UserConfig
	}{
		{"host", []string{"login", "burrow.example.com", "--token", testToken},
			client.UserConfig{Relay: "https://burrow.example.com", Control: "burrow.example.com:7000", TokenName: "kohns-laptop"}},
		{"url with port and slash", []string{"login", "https://burrow.example.com:8443/", "--token", testToken},
			client.UserConfig{Relay: "https://burrow.example.com:8443", Control: "burrow.example.com:7000", TokenName: "kohns-laptop"}},
		{"host and port", []string{"login", "burrow.example.com:8443", "--token", testToken},
			client.UserConfig{Relay: "https://burrow.example.com:8443", Control: "burrow.example.com:7000", TokenName: "kohns-laptop"}},
		{"upper case", []string{"login", "HTTPS://Burrow.Example.com", "--token", testToken},
			client.UserConfig{Relay: "https://burrow.example.com", Control: "burrow.example.com:7000", TokenName: "kohns-laptop"}},
		{"ipv6", []string{"login", "https://[::1]:8443", "--token", testToken},
			client.UserConfig{Relay: "https://[::1]:8443", Control: "[::1]:7000", TokenName: "kohns-laptop"}},
		{"--control", []string{"login", "burrow.example.com", "--token", testToken, "--control", "ctl.example.com:7001"},
			client.UserConfig{Relay: "https://burrow.example.com", Control: "ctl.example.com:7001", TokenName: "kohns-laptop"}},
		{"--name", []string{"login", "burrow.example.com", "--token", testToken, "--name", "work-laptop"},
			client.UserConfig{Relay: "https://burrow.example.com", Control: "burrow.example.com:7000", TokenName: "work-laptop"}},
		{"token with spaces around it", []string{"login", "burrow.example.com", "--token", "  " + testToken + " "},
			client.UserConfig{Relay: "https://burrow.example.com", Control: "burrow.example.com:7000", TokenName: "kohns-laptop"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if code := h.exec(tc.args...); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			got := h.stored(h.cfgPath)
			if got.Token != testToken {
				t.Fatal("the stored token is not the one given")
			}
			got.Token = ""
			if got != tc.want {
				t.Fatalf("stored %v, want %v", got, tc.want)
			}
			if !strings.Contains(h.stdout.String(), h.cfgPath) {
				t.Fatalf("stdout %q does not name the file", h.stdout.String())
			}
			h.noToken()
			if runtime.GOOS != "windows" {
				fi, err := os.Stat(h.cfgPath)
				if err != nil || fi.Mode().Perm() != 0o600 {
					t.Fatalf("mode = %v, err = %v", fi.Mode().Perm(), err)
				}
			}
		})
	}
}

func TestLoginToken_ConfigFlagChoosesTheFile(t *testing.T) {
	h := newHarness(t)
	other := filepath.Join(t.TempDir(), "sub", "x.yaml")
	if code := h.exec("login", "burrow.example.com", "--token", testToken, "--config", other); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if h.stored(other).Token != testToken || h.hasConfig() {
		t.Fatal("the sign-in went to the wrong file")
	}
	if !strings.Contains(h.stdout.String(), other) {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
}

func TestLoginToken_NameFallsBackWithoutAHostname(t *testing.T) {
	h := newHarness(t)
	h.hostname, h.hostErr = "", errors.New("no hostname")
	if code := h.exec("login", "burrow.example.com", "--token", testToken); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if n := h.stored(h.cfgPath).TokenName; n == "" {
		t.Fatal("the token has no name")
	}
}

func TestLoginToken_FromStdin(t *testing.T) {
	h := newHarness(t)
	h.stdin = testToken + "\r\n"
	if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if h.stored(h.cfgPath).Token != testToken {
		t.Fatal("the stored token is not the line from stdin")
	}
	h.noToken()

	for name, in := range map[string]string{"empty": "", "blank": " \n", "two lines": "bur_aaaaaaaa\nbur_bbbbbbbb\n"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.stdin = in
			if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 2 || h.hasConfig() {
				t.Fatalf("exit %d, config written: %v", code, h.hasConfig())
			}
			h.noToken("bur_aaaaaaaa", "bur_bbbbbbbb")
		})
	}
}

func TestLogin_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no relay", []string{"login"}, "burrow login <relay>"},
		{"no relay, with a token", []string{"login", "--token", testToken}, "burrow login <relay>"},
		{"two relays", []string{"login", "a.example.com", "b.example.com", "--token", testToken}, "burrow login <relay>"},
		{"http relay", []string{"login", "http://x", "--token", "t"}, "https"},
		{"other scheme", []string{"login", "ftp://x", "--token", "t"}, "https"},
		{"path", []string{"login", "https://burrow.example.com/app", "--token", "t"}, "path"},
		{"path without scheme", []string{"login", "burrow.example.com/app", "--token", "t"}, "path"},
		{"query", []string{"login", "https://burrow.example.com/?a=b", "--token", "t"}, "path"},
		{"credentials", []string{"login", "https://user:hunter2secret@burrow.example.com", "--token", "t"}, "credentials"},
		{"credentials without scheme", []string{"login", "user:hunter2secret@burrow.example.com", "--token", "t"}, "credentials"},
		{"bad port", []string{"login", "burrow.example.com:port", "--token", "t"}, "relay address"},
		{"blank relay", []string{"login", " ", "--token", "t"}, "relay address"},
		{"empty token", []string{"login", "burrow.example.com", "--token", ""}, "token"},
		{"blank token", []string{"login", "burrow.example.com", "--token", "  "}, "token"},
		{"token with a space inside", []string{"login", "burrow.example.com", "--token", "bur_aaaa bbbb"}, "token"},
		{"bad control", []string{"login", "burrow.example.com", "--token", "t", "--control", "ctl.example.com"}, "--control"},
		{"control with a bad port", []string{"login", "burrow.example.com", "--token", "t", "--control", "ctl.example.com:x"}, "--control"},
		{"blank name", []string{"login", "burrow.example.com", "--token", "t", "--name", " "}, "--name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if code := h.exec(tc.args...); code != 2 {
				t.Fatalf("exit %d, want 2; stderr %q", code, h.stderr.String())
			}
			if !strings.Contains(h.stderr.String(), tc.want) {
				t.Fatalf("stderr %q does not contain %q", h.stderr.String(), tc.want)
			}
			if h.hasConfig() {
				t.Fatal("a config was written")
			}
			h.noToken("hunter2secret", "bur_aaaa bbbb", "bbbb")
		})
	}
}

func TestLogin_WithoutTokenPointsAtTheTokenPath(t *testing.T) {
	h := newHarness(t)
	code := h.exec("login", "burrow.example.com")
	if code != 2 || h.hasConfig() {
		t.Fatalf("exit %d, config written: %v", code, h.hasConfig())
	}
	for _, w := range []string{"Browser sign-in", "newer version", "burrow login burrow.example.com --token"} {
		if !strings.Contains(h.stderr.String(), w) {
			t.Fatalf("stderr %q does not contain %q", h.stderr.String(), w)
		}
	}
}

func TestLogin_AlreadySignedIn(t *testing.T) {
	const newToken = "bur_new_token_9876543210"
	args := []string{"login", "other.example.com", "--token", newToken}

	t.Run("answer n keeps the sign-in", func(t *testing.T) {
		for _, answer := range []string{"n\n", "\n", "", "nope\n"} {
			h := newHarness(t)
			before := h.signIn()
			h.terminal, h.stdin = true, answer
			if code := h.exec(args...); code != 0 {
				t.Fatalf("answer %q: exit %d: %s", answer, code, h.stderr.String())
			}
			if h.stored(h.cfgPath) != before {
				t.Fatalf("answer %q: the stored sign-in changed", answer)
			}
			if !strings.Contains(h.stdout.String(), "https://burrow.example.com") {
				t.Fatalf("the question does not name the current relay: %q", h.stdout.String())
			}
			h.noToken(newToken)
		}
	})
	t.Run("answer y replaces it", func(t *testing.T) {
		for _, answer := range []string{"y\n", "Y\n", "yes\n", " y \n"} {
			h := newHarness(t)
			h.signIn()
			h.terminal, h.stdin = true, answer
			if code := h.exec(args...); code != 0 {
				t.Fatalf("answer %q: exit %d: %s", answer, code, h.stderr.String())
			}
			if got := h.stored(h.cfgPath); got.Token != newToken || got.Relay != "https://other.example.com" || got.Control != "other.example.com:7000" {
				t.Fatalf("answer %q: stored relay %q control %q, new token stored: %v", answer, got.Relay, got.Control, got.Token == newToken)
			}
			h.noToken(newToken)
		}
	})
	t.Run("--force replaces without asking", func(t *testing.T) {
		h := newHarness(t)
		h.signIn()
		if code := h.exec(append(args, "--force")...); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if h.stored(h.cfgPath).Token != newToken {
			t.Fatal("the sign-in was not replaced")
		}
		if strings.Contains(h.stdout.String(), "?") {
			t.Fatalf("a question was asked: %q", h.stdout.String())
		}
	})
	t.Run("not a terminal needs --force", func(t *testing.T) {
		h := newHarness(t)
		before := h.signIn()
		h.stdin = "y\n"
		code := h.exec(args...)
		if code != 2 || !strings.Contains(h.stderr.String(), "--force") || h.stored(h.cfgPath) != before {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
	})
	t.Run("a token from stdin needs --force", func(t *testing.T) {
		h := newHarness(t)
		before := h.signIn()
		h.terminal, h.stdin = true, newToken+"\n" // stdin holds the token, so nobody can be asked
		code := h.exec("login", "other.example.com", "--token", "-")
		if code != 2 || !strings.Contains(h.stderr.String(), "--force") || h.stored(h.cfgPath) != before {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
		h.noToken(newToken)
		h.stdin = newToken + "\n"
		if code := h.exec("login", "other.example.com", "--token", "-", "--force"); code != 0 || h.stored(h.cfgPath).Token != newToken {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
	t.Run("a stored file without a token is replaced without asking", func(t *testing.T) {
		h := newHarness(t)
		if err := client.SaveUserConfig(h.cfgPath, client.UserConfig{Relay: "https://burrow.example.com"}); err != nil {
			t.Fatal(err)
		}
		if code := h.exec(args...); code != 0 || h.stored(h.cfgPath).Token != newToken {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
	t.Run("a broken stored file is replaced without asking", func(t *testing.T) {
		h := newHarness(t)
		writeAt(t, h.cfgPath, "token: [\n")
		if code := h.exec(args...); code != 0 || h.stored(h.cfgPath).Token != newToken {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
}

func TestLogout(t *testing.T) {
	t.Run("signed in", func(t *testing.T) {
		h := newHarness(t)
		h.signIn()
		code := h.exec("logout")
		want := "Signed out. Revoke the token in the dashboard: https://burrow.example.com/clients?tab=tokens\n"
		if code != 0 || h.stdout.String() != want || h.hasConfig() {
			t.Fatalf("exit %d, stdout %q, file still there: %v", code, h.stdout.String(), h.hasConfig())
		}
		h.noToken()
	})
	t.Run("not signed in", func(t *testing.T) {
		h := newHarness(t)
		if code := h.exec("logout"); code != 0 || h.stdout.String() != "Not signed in.\n" {
			t.Fatalf("exit %d, stdout %q", code, h.stdout.String())
		}
	})
	t.Run("stored file without a relay", func(t *testing.T) {
		h := newHarness(t)
		if err := client.SaveUserConfig(h.cfgPath, client.UserConfig{Control: "c.example.com:7000", Token: testToken}); err != nil {
			t.Fatal(err)
		}
		code := h.exec("logout")
		if code != 0 || h.hasConfig() || !strings.HasPrefix(h.stdout.String(), "Signed out. Revoke the token in the dashboard") || strings.Contains(h.stdout.String(), "/clients?tab=tokens") {
			t.Fatalf("exit %d, stdout %q", code, h.stdout.String())
		}
	})
	t.Run("broken stored file is removed too", func(t *testing.T) {
		h := newHarness(t)
		writeAt(t, h.cfgPath, "token: [\n")
		if code := h.exec("logout"); code != 0 || h.hasConfig() || !strings.HasPrefix(h.stdout.String(), "Signed out.") {
			t.Fatalf("exit %d, stdout %q", code, h.stdout.String())
		}
	})
	t.Run("--config", func(t *testing.T) {
		h := newHarness(t)
		h.signIn()
		other := filepath.Join(t.TempDir(), "x.yaml")
		if err := client.SaveUserConfig(other, client.UserConfig{Relay: "https://other.example.com", Control: "o:7000", Token: testToken}); err != nil {
			t.Fatal(err)
		}
		if code := h.exec("logout", "--config", other); code != 0 || !h.hasConfig() {
			t.Fatalf("exit %d, default config still there: %v", code, h.hasConfig())
		}
		if _, err := os.Stat(other); !os.IsNotExist(err) {
			t.Fatalf("the file given with --config is still there: %v", err)
		}
	})
}
