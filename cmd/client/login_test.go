package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

	// Blank lines around the token do not matter.
	for _, in := range []string{testToken + "\n\n", testToken + "\n \r\n\n", "\n" + testToken, testToken} {
		h := newHarness(t)
		h.stdin = in
		if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 0 || h.stored(h.cfgPath).Token != testToken {
			t.Fatalf("input with blank lines: exit %d: %s", code, h.stderr.String())
		}
	}

	for name, in := range map[string]string{"empty": "", "blank": " \n", "two lines": "bur_aaaaaaaa\nbur_bbbbbbbb\n", "two lines apart": "bur_aaaaaaaa\n\nbur_bbbbbbbb"} {
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
	for _, w := range []string{"Browser sign-in", "newer version", "burrow login burrow.example.com --token -", "paste the token", "press Enter"} {
		if !strings.Contains(h.stderr.String(), w) {
			t.Fatalf("stderr %q does not contain %q", h.stderr.String(), w)
		}
	}
	// A token on the command line ends up in the shell history and the process list.
	if strings.Contains(h.stderr.String(), "--token <") {
		t.Fatalf("the message suggests a token on the command line: %q", h.stderr.String())
	}
}

func TestLogin_MessagesAndHelpSteerToStdin(t *testing.T) {
	h := newHarness(t)
	if code := h.exec("login", "burrow.example.com", "--token", ""); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if s := h.stderr.String(); strings.Contains(s, "--token <") || !strings.Contains(s, "burrow login burrow.example.com --token -") || !strings.Contains(s, "paste the token") {
		t.Fatalf("stderr = %q", s)
	}
	if code := h.exec("login", "--help"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := h.stdout.String(); strings.Contains(s, "--token <") || !strings.Contains(s, "--token -") || !strings.Contains(s, "shell history") {
		t.Fatalf("help = %q", s)
	}
}

// On a terminal `--token -` asks for the token on stderr and reads it without echo.
func TestLoginToken_PromptOnATerminal(t *testing.T) {
	h := newHarness(t)
	h.terminal, h.secret = true, " "+testToken+"\n"
	h.stdin = "this line must not be read as the token\n"
	if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if h.secretCalls != 1 || h.stored(h.cfgPath).Token != testToken {
		t.Fatalf("hidden prompt used %d times; token stored from it: %v", h.secretCalls, h.stored(h.cfgPath).Token == testToken)
	}
	if !strings.Contains(h.stderr.String(), "Token") || strings.Contains(h.stdout.String(), "Token:") {
		t.Fatalf("the prompt belongs on stderr: stderr %q stdout %q", h.stderr.String(), h.stdout.String())
	}
	h.noToken()

	t.Run("nothing typed", func(t *testing.T) {
		h := newHarness(t)
		h.terminal, h.secret = true, ""
		if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 2 || h.hasConfig() {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("Ctrl-D at the hidden prompt", func(t *testing.T) {
		h := newHarness(t)
		h.terminal, h.secretErr = true, io.EOF
		code := h.exec("login", "burrow.example.com", "--token", "-")
		if code != 2 || h.hasConfig() || strings.HasPrefix(h.stderr.String(), "error:") || !strings.Contains(h.stderr.String(), "The token is empty") {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
	})
	t.Run("not a terminal reads the line and shows no prompt", func(t *testing.T) {
		h := newHarness(t)
		h.stdin = testToken + "\n"
		if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 0 || h.secretCalls != 0 || h.stderr.Len() != 0 {
			t.Fatalf("exit %d, prompt calls %d, stderr %q", code, h.secretCalls, h.stderr.String())
		}
	})
	t.Run("already signed in: asked first, then the hidden prompt", func(t *testing.T) {
		h := newHarness(t)
		h.signIn()
		h.terminal, h.stdin, h.secret = true, "y\n", "bur_new_token_9876543210"
		if code := h.exec("login", "other.example.com", "--token", "-"); code != 0 || h.stored(h.cfgPath).Token != "bur_new_token_9876543210" {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		h.noToken("bur_new_token_9876543210")
	})
}

func TestLogin_AlreadySignedIn(t *testing.T) {
	const newToken = "bur_new_token_9876543210"
	args := []string{"login", "other.example.com", "--token", newToken}

	t.Run("answer n keeps the sign-in", func(t *testing.T) {
		for _, answer := range []string{"n\n", "\n", "nope\n"} {
			h := newHarness(t)
			before := h.signIn()
			h.terminal, h.stdin = true, answer
			if code := h.exec(args...); code != 0 {
				t.Fatalf("answer %q: exit %d: %s", answer, code, h.stderr.String())
			}
			if h.stored(h.cfgPath) != before {
				t.Fatalf("answer %q: the stored sign-in changed", answer)
			}
			// The question is for the person at the terminal: stderr, so that it
			// shows when stdout is piped. An answered question adds no blank line.
			wantQ := "This machine is already signed in to https://burrow.example.com. Replace the stored sign-in? [y/N] "
			if h.stderr.String() != wantQ || h.stdout.String() != "Kept the stored sign-in.\n" {
				t.Fatalf("answer %q: stderr %q, stdout %q", answer, h.stderr.String(), h.stdout.String())
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
		if strings.Contains(h.stdout.String(), "?") || h.stderr.Len() != 0 {
			t.Fatalf("a question was asked: %q %q", h.stdout.String(), h.stderr.String())
		}
	})
	// A "terminal" that gives no answer at all, like /dev/null, cannot be asked.
	t.Run("end of input instead of an answer needs --force", func(t *testing.T) {
		h := newHarness(t)
		before := h.signIn()
		h.terminal, h.stdin = true, ""
		code := h.exec(args...)
		if code != 2 || !strings.Contains(h.stderr.String(), "--force") || h.stored(h.cfgPath) != before {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
		if strings.Contains(h.stdout.String(), "Kept") {
			t.Fatalf("stdout = %q", h.stdout.String())
		}
		h.noToken(newToken)
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
		h.stdin = newToken + "\n" // stdin holds the token, so nobody can be asked
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

// openStdin is a stdin that, like a terminal, gives what was typed so far and
// then blocks instead of ending. readPast is closed when the further input was
// taken, which means the command read beyond the first line.
func openStdin(t *testing.T, first, extra string) (r io.Reader, readPast <-chan struct{}) {
	t.Helper()
	pr, pw := io.Pipe()
	past := make(chan struct{})
	go func() {
		if _, err := pw.Write([]byte(first)); err != nil {
			return
		}
		if _, err := pw.Write([]byte(extra)); err == nil {
			close(past)
		}
	}()
	t.Cleanup(func() { pr.Close() })
	return pr, past
}

// execWithin fails the test when the command does not return: it is waiting
// for input nobody will type.
func (h *harness) execWithin(args ...string) int {
	h.t.Helper()
	done := make(chan int, 1)
	go func() { done <- h.exec(args...) }()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		h.t.Fatal("the command did not return after the answer line")
		return -1
	}
}

// On a terminal the input does not end after the answer. The question must
// return with the line alone, and must not take anything typed after it.
func TestLogin_AlreadySignedIn_OpenTerminal(t *testing.T) {
	const newToken = "bur_new_token_9876543210"
	const typedAhead = "bur_typed_ahead_00000001\n"

	for _, tokenArg := range []string{newToken, "-"} {
		kind := "value"
		if tokenArg == "-" {
			kind = "-"
		}
		t.Run("n, --token "+kind, func(t *testing.T) {
			h := newHarness(t)
			before := h.signIn()
			var past <-chan struct{}
			h.terminal, h.secret = true, newToken
			h.stdinR, past = openStdin(t, "n\n", typedAhead)
			if code := h.execWithin("login", "other.example.com", "--token", tokenArg); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			if h.stdout.String() != "Kept the stored sign-in.\n" || h.stored(h.cfgPath) != before || h.secretCalls != 0 {
				t.Fatalf("stdout %q, hidden prompt calls %d", h.stdout.String(), h.secretCalls)
			}
			select {
			case <-past:
				t.Fatal("input after the answer line was read")
			default:
			}
			h.noToken(newToken, strings.TrimSpace(typedAhead))
		})
		t.Run("y, --token "+kind, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			var past <-chan struct{}
			h.terminal, h.secret = true, newToken
			h.stdinR, past = openStdin(t, "y\n", typedAhead)
			if code := h.execWithin("login", "other.example.com", "--token", tokenArg); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			wantPrompts := 0
			if tokenArg == "-" {
				wantPrompts = 1
			}
			if h.stored(h.cfgPath).Token != newToken || h.secretCalls != wantPrompts {
				t.Fatalf("new token stored: %v, hidden prompt calls %d", h.stored(h.cfgPath).Token == newToken, h.secretCalls)
			}
			// Nothing after the answer was taken from the terminal: the hidden
			// prompt gets it all, and no token is read in clear and dropped.
			select {
			case <-past:
				t.Fatal("input after the answer line was read")
			default:
			}
			h.noToken(newToken, strings.TrimSpace(typedAhead))
		})
	}
}

// keys feeds what a terminal in raw mode delivers, one write per chunk, and
// then stays open as a terminal does: a reader that waits for more than it
// needs never returns.
func keys(t *testing.T, chunks ...string) *io.PipeReader {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		for _, c := range chunks {
			if _, err := pw.Write([]byte(c)); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { pr.Close() })
	return pr
}

// hiddenLine runs readHiddenLine and fails the test when it does not return.
func hiddenLine(t *testing.T, r io.Reader) (string, error) {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readHiddenLine(r)
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt is still waiting although the line was complete")
		return "", nil
	}
}

func TestReadHiddenLine(t *testing.T) {
	const tok = "bur_test_0000"
	const ps, pe = "\x1b[200~", "\x1b[201~"
	long := strings.Repeat("a", 5000)
	cases := []struct {
		name  string
		typed []string
		want  string
		err   error
	}{
		{"typed, Enter", []string{tok + "\r"}, tok, nil},
		{"one key at a time", strings.Split(tok+"\r", ""), tok, nil},
		{"line feed", []string{tok + "\n"}, tok, nil},
		{"CRLF", []string{tok + "\r\n"}, tok, nil},
		{"empty line", []string{"\r"}, "", nil},
		{"bracketed paste, Enter", []string{ps + tok + pe + "\r"}, tok, nil},
		{"bracketed paste, markers split across writes", []string{"\x1b", "[2", "00", "~bur_test", "_0000\x1b[", "201", "~", "\r"}, tok, nil},
		{"bracketed paste with CRLF inside", []string{ps + tok + "\r\n" + pe}, tok, nil},
		{"Backspace", []string{"bur_tesX\x7ft_0000\r"}, tok, nil},
		{"Ctrl-H", []string{"bur_tesXY\x08\x08t_0000\r"}, tok, nil},
		{"Backspace on nothing", []string{"\x7f\x08" + tok + "\r"}, tok, nil},
		{"Ctrl-U", []string{"wrong\x15" + tok + "\r"}, tok, nil},
		{"Ctrl-W", []string{"wrong\x17" + tok + "\r"}, tok, nil},
		{"4096 characters", []string{long[:4096] + "\r"}, long[:4096], nil},

		{"Esc then Enter", []string{"\x1b\r"}, "", errControlKeys},
		{"Esc, later Enter", []string{"\x1b", "\r"}, "", errControlKeys},
		{"Esc then Ctrl-C", []string{"\x1b\x03"}, "", errInterrupted},
		{"arrow key mid-token, Enter", []string{"bur_test\x1b[D_0000\r"}, "", errControlKeys},
		{"arrow key, SS3 form", []string{"bur_test\x1bOD_0000\r"}, "", errControlKeys},
		{"half a paste marker", []string{"\x1b[20x" + tok + "\r"}, "", errControlKeys},
		{"half a paste marker then Ctrl-C", []string{"\x1b[20\x03"}, "", errInterrupted},
		{"invalid UTF-8 byte", []string{"bur_\xc3test_0000\r"}, "", errControlKeys},
		{"Latin-1 letter", []string{"bur_\xe4\r"}, "", errControlKeys},
		{"UTF-8 letter", []string{"bur_ä\r"}, "", errControlKeys},
		{"invalid byte, then Ctrl-C", []string{"bur_\xc3test_0000", "\x03"}, "", errInterrupted},
		{"refused line, Backspace does not repair it", []string{"bur_\xc3\x7ftest\r"}, "", errControlKeys},
		{"refused line, Ctrl-U does not repair it", []string{"\x1b[D\x15" + tok + "\r"}, "", errControlKeys},
		{"tab inside", []string{"bur_test\t0000\r"}, "", errControlKeys},
		{"space inside", []string{"bur_test 0000\r"}, "", errControlKeys},
		{"NUL inside", []string{"bur_test\x000000\r"}, "", errControlKeys},
		{"5000 characters", []string{long + "\r"}, "", errControlKeys},
		{"4097 characters", []string{long[:4097] + "\r"}, "", errControlKeys},
		{"Ctrl-D on an empty line", []string{"\x04"}, "", io.EOF},
		{"Ctrl-D mid-token, Enter", []string{"bur_te\x04st\r"}, "", errControlKeys},
		{"Ctrl-D on a refused empty line, Enter", []string{"\x1b[D\x15\x04\r"}, "", errControlKeys},
		{"Ctrl-C", []string{"\x03"}, "", errInterrupted},
		{"Ctrl-C mid-token", []string{"bur_te\x03"}, "", errInterrupted},
		{"Ctrl-C inside a paste", []string{ps + "bur_te\x03"}, "", errInterrupted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := hiddenLine(t, keys(t, tc.typed...))
			if line != tc.want || !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Fatalf("got %d characters (want %d) and error %v (want %v)", len(line), len(tc.want), err, tc.err)
			}
		})
	}

	t.Run("the input ends", func(t *testing.T) {
		// never a partial token
		for _, in := range []string{"", "bur_te", "\x1b"} {
			if line, err := hiddenLine(t, strings.NewReader(in)); line != "" || !errors.Is(err, io.EOF) {
				t.Fatalf("input of %d bytes: got %d characters and %v", len(in), len(line), err)
			}
		}
		pr, pw := io.Pipe()
		broken := errors.New("terminal gone")
		go func() { _, _ = pw.Write([]byte("bur_te")); pw.CloseWithError(broken) }()
		if line, err := hiddenLine(t, pr); line != "" || !errors.Is(err, broken) {
			t.Fatalf("got %d characters and %v", len(line), err)
		}
	})

	t.Run("nothing beyond the line end is consumed", func(t *testing.T) {
		for _, first := range []string{"one\r", "\x1b[D\r", "x\x03", "\x04"} {
			pr := keys(t, first+"rest")
			_, _ = hiddenLine(t, pr)
			rest := make([]byte, 4)
			if _, err := io.ReadFull(pr, rest); err != nil || string(rest) != "rest" {
				t.Fatalf("after %q: what follows was consumed (%q left, %v)", first, rest, err)
			}
		}
	})
}

// The login flow with the real reader behind the prompt.
func TestLoginToken_PromptRunsTheReader(t *testing.T) {
	h := newHarness(t)
	h.terminal, h.typed = true, "wrong\x15\x1b[200~"+testToken+"\x1b[201~\r"
	if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 0 || h.stored(h.cfgPath).Token != testToken {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	h.noToken()

	for name, typed := range map[string]string{
		"Esc then Enter":      "\x1b\r",
		"arrow key mid-token": "bur_test\x1b[D_0000\r",
		"invalid UTF-8":       "bur_\xc3test_0000\r",
		"Ctrl-D mid-token":    "bur_te\x04st_0000\r",
	} {
		h := newHarness(t)
		h.terminal, h.typed = true, typed
		code := h.exec("login", "burrow.example.com", "--token", "-")
		if code != 2 || h.hasConfig() || !strings.Contains(h.stderr.String(), "Nothing was stored") || strings.Contains(h.stderr.String(), "0000") {
			t.Fatalf("%s: exit %d, config written %v", name, code, h.hasConfig())
		}
	}
	h = newHarness(t)
	h.terminal, h.typed = true, "\x04"
	if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 2 || h.hasConfig() || !strings.Contains(h.stderr.String(), "The token is empty") {
		t.Fatalf("Ctrl-D: exit %d, stderr %q", code, h.stderr.String())
	}
}

func TestLoginToken_ControlKeysAtTheHiddenPrompt(t *testing.T) {
	h := newHarness(t)
	h.terminal, h.secretErr = true, errControlKeys
	code := h.exec("login", "burrow.example.com", "--token", "-")
	if code != 2 || h.hasConfig() || strings.HasPrefix(h.stderr.String(), "error:") || !strings.Contains(h.stderr.String(), "Nothing was stored") || !strings.Contains(h.stderr.String(), "paste the token") {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}

// A token is printable ASCII without spaces. Anything else is refused on every
// way in, and not repeated. At the prompt the keys go through the real reader.
func TestLogin_RejectsWhatCannotBeAToken(t *testing.T) {
	cases := []struct {
		name, tok string
		// editKey: at the prompt the byte is an editing key, so only the flag
		// and the pipe are checked.
		editKey bool
	}{
		{"non-ASCII letter", "bur_ätest_0000", false},
		{"Latin-1 byte", "bur_\xe4test_0000", false},
		{"invalid UTF-8", "bur_\xc3test_0000", false},
		{"escape sequence", "bur_test\x1b[D_0000", false},
		{"lone escape", "bur_test\x1b_0000", false},
		{"control character", "bur_test\x00_0000", false},
		{"tab inside", "bur_test\t0000", false},
		{"space inside", "bur_test 0000", false},
		{"non-breaking space", "bur_test\u00a00000", false},
		{"zero-width space", "bur_test\u200b0000", false},
		{"paste marker", "\x1b[200~bur_test_0000", true},
		{"delete character", "bur_test\x7f0000", true},
		{"Ctrl-U", "bur_test\x150000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, how := range []string{"flag", "stdin", "prompt"} {
				h := newHarness(t)
				args := []string{"login", "burrow.example.com", "--token", "-"}
				switch how {
				case "flag":
					args[3] = tc.tok
				case "stdin":
					h.stdin = tc.tok + "\n"
				case "prompt":
					if tc.editKey {
						continue
					}
					h.terminal, h.typed = true, tc.tok+"\r"
				}
				code := h.exec(args...)
				if code != 2 || h.hasConfig() || !strings.Contains(h.stderr.String(), "Nothing was stored") {
					t.Fatalf("%s: exit %d, config written %v, stderr %q", how, code, h.hasConfig(), strings.ToValidUTF8(h.stderr.String(), "?"))
				}
				if strings.Contains(h.stderr.String(), "0000") || strings.Contains(h.stdout.String(), "0000") {
					t.Fatalf("%s: the input was repeated", how)
				}
			}
		})
	}
}
