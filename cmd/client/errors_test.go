package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ankoehn/burrow/internal/client"
)

// exitCodeOfConnectError is the exit code main gives an error of `connect`.
func exitCodeOfConnectError(err error) int { return exitCode(err) }

func TestExitCode(t *testing.T) {
	_, targetErr := client.ParseTarget("https://localhost:3000")
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"not signed in", client.ErrNotSignedIn, 3},
		{"not signed in, wrapped", fmt.Errorf("resolve: %w", client.ErrNotSignedIn), 3},
		{"bad target", targetErr, 2},
		{"bad target, wrapped", fmt.Errorf("x: %w", targetErr), 2},
		{"usage", usageErrorf("need a target"), 2},
		{"explicit code", &exitError{code: 5, msg: "unreachable"}, 5},
		{"explicit code, wrapped", fmt.Errorf("x: %w", &exitError{code: 4, msg: "rejected"}), 4},
		{"anything else", errors.New("boom"), 1},
		{"interrupted connect", context.Canceled, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Fatalf("exitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReport(t *testing.T) {
	_, targetErr := client.ParseTarget("https://localhost:3000")
	cases := []struct {
		name string
		err  error
		want string
		code int
	}{
		{"nil prints nothing", nil, "", 0},
		{"unclassified keeps the prefix", errors.New("boom"), "error: boom\n", 1},
		{"the spec's line is printed as it is", client.ErrNotSignedIn, "Not signed in. Run: burrow login <your relay address>\n", 3},
		{"also when wrapped", fmt.Errorf("x: %w", client.ErrNotSignedIn), "Not signed in. Run: burrow login <your relay address>\n", 3},
		{"usage", usageErrorf("need %d target", 1), "need 1 target\n", 2},
		{"bad target", targetErr, targetErr.Error() + "\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if code := report(&b, tc.err); code != tc.code || b.String() != tc.want {
				t.Fatalf("report = %d %q, want %d %q", code, b.String(), tc.code, tc.want)
			}
		})
	}
}

func TestMessagesAreTheSpecs(t *testing.T) {
	if msgNotSignedIn != client.ErrNotSignedIn.Error() || msgNotSignedIn != "Not signed in. Run: burrow login <your relay address>" {
		t.Fatalf("msgNotSignedIn = %q", msgNotSignedIn)
	}
	if msgTokenRejected != "The relay rejected this machine's token. It may have been revoked. Run: burrow login <relay>" {
		t.Fatalf("msgTokenRejected = %q", msgTokenRejected)
	}
	if got := fmt.Sprintf(msgRelayUnreachable, "burrow.example.com:7000"); got != "Cannot reach burrow.example.com:7000. Check the address and that port 7000 is open. Details: burrow doctor" {
		t.Fatalf("msgRelayUnreachable = %q", got)
	}
	if got := fmt.Sprintf(msgClientTooOld, "1.2.0"); got != "This relay needs burrow 1.2.0 or newer. Run: burrow update" {
		t.Fatalf("msgClientTooOld = %q", got)
	}
}

func TestRoot_UsageErrors(t *testing.T) {
	cases := [][]string{
		{"bogus"},
		{"--bogus"},
		{"http", "3000", "--bogus"},
		{"status", "--log", "xml"},
		{"status", "extra"},
		{"logout", "extra"},
	}
	for _, args := range cases {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			if code := h.exec(args...); code != 2 {
				t.Fatalf("exit %d, want 2; stderr %q", code, h.stderr.String())
			}
			if h.stderr.Len() == 0 || bytes.HasPrefix(h.stderr.Bytes(), []byte("error:")) {
				t.Fatalf("stderr = %q", h.stderr.String())
			}
		})
	}
}

func TestRoot_NoCommandPrintsHelp(t *testing.T) {
	h := newHarness(t)
	if code := h.exec(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, name := range []string{"connect", "http", "tcp", "up", "login", "logout", "status", "version"} {
		if !bytes.Contains(h.stdout.Bytes(), []byte("\n  "+name+" ")) {
			t.Fatalf("help does not list %q:\n%s", name, h.stdout.String())
		}
	}
}

func TestVersionCommand(t *testing.T) {
	h := newHarness(t)
	if code := h.exec("version"); code != 0 || h.stdout.String() != versionLine()+"\n" {
		t.Fatalf("exit %d, stdout %q", code, h.stdout.String())
	}
}
