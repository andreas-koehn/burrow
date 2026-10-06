package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestRoot_UnknownCommandSuggests(t *testing.T) {
	for typed, want := range map[string]string{"statsu": "status", "logn": "login", "conect": "connect"} {
		h := newHarness(t)
		code := h.exec(typed)
		s := h.stderr.String()
		if code != 2 || !strings.Contains(s, `unknown command "`+typed+`" for "burrow"`) || !strings.Contains(s, "Did you mean this?") || !strings.Contains(s, "\t"+want+"\n") {
			t.Fatalf("%s: exit %d, stderr %q", typed, code, s)
		}
	}
	h := newHarness(t)
	if code := h.exec("zzzzzz"); code != 2 || strings.Contains(h.stderr.String(), "Did you mean") {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}

func TestReport_StoredTokenForAnotherRelay(t *testing.T) {
	user := client.UserConfig{Relay: "https://burrow.example.com", Control: "burrow.example.com:7000", Token: testToken}
	_, err := client.Resolve(client.Sources{EnvServer: "other.example.com:7001", User: &user})
	var b bytes.Buffer
	code := report(&b, fmt.Errorf("resolve: %w", err))
	want := "Not signed in to other.example.com:7001. The stored sign-in is for https://burrow.example.com.\n" +
		"Run: burrow login other.example.com --control other.example.com:7001\n"
	if code != 3 || b.String() != want {
		t.Fatalf("report = %d %q, want 3 %q", code, b.String(), want)
	}
}

// The relay's refusals become the spec's messages and exit codes.
func TestReport_Refusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
		code int
	}{
		{"token rejected", &client.RefusedError{Code: "invalid_token", Message: "invalid token", Auth: true},
			"The relay rejected this machine's token. It may have been revoked. Run: burrow login <relay>\n", 4},
		{"token rejected, wrapped", fmt.Errorf("x: %w", &client.RefusedError{Code: "invalid_token", Message: "invalid token", Auth: true}),
			"The relay rejected this machine's token. It may have been revoked. Run: burrow login <relay>\n", 4},
		{"client too old", &client.RefusedError{Code: "client_too_old", Message: "client too old: this relay needs burrow 0.9.0 or newer", Auth: true},
			"This relay needs burrow 0.9.0 or newer. Run: burrow update\n", 6},
		{"client too old, minimum with a v", &client.RefusedError{Code: "client_too_old", Message: "needs v1.10.3", Auth: true},
			"This relay needs burrow 1.10.3 or newer. Run: burrow update\n", 6},
		{"client too old, no minimum in the text", &client.RefusedError{Code: "client_too_old", Message: "too old", Auth: true},
			"This relay needs a newer burrow. Run: burrow update\n", 6},
		{"slug taken", &client.RefusedError{Code: "slug_taken", Message: "slug already in use; try: abc234"},
			"The relay refused the slug: slug already in use; try: abc234\n", 1},
		{"slug invalid", &client.RefusedError{Code: "slug_invalid", Message: "slug must be 3-40 characters"},
			"The relay refused the slug: slug must be 3-40 characters\n", 1},
		{"access invalid", &client.RefusedError{Code: "access_invalid", Message: "burrow_login requires a configured auth_domain"},
			"The relay refused the access mode: burrow_login requires a configured auth_domain\n", 1},
		{"forbidden", &client.RefusedError{Code: "forbidden", Message: "your role may not choose a slug or an access mode"},
			"The relay refused: your role may not choose a slug or an access mode\n", 1},
		// Anything else is printed as it always was.
		{"another refusal", &client.RefusedError{Code: "port_unavailable", Message: "port 9000 in use"}, "error: register failed: port 9000 in use\n", 1},
		{"no code", &client.RefusedError{Message: "nope", Auth: true}, "error: auth failed: nope\n", 1},
		// The relay never sends this one; its text stays an ordinary error.
		{"http tunnels not configured", &client.RefusedError{Message: "http tunnels not configured"}, "error: register failed: http tunnels not configured\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if code := report(&b, tc.err); code != tc.code || b.String() != tc.want {
				t.Fatalf("report = %d %q, want %d %q", code, b.String(), tc.code, tc.want)
			}
			if got := exitCode(tc.err); got != tc.code {
				t.Fatalf("exitCode = %d, want %d", got, tc.code)
			}
		})
	}
}

// The relay's reason is text from outside: what could move the cursor or
// colour the terminal is not printed, and it does not grow without bound.
func TestReport_RefusalTextIsMadeSafe(t *testing.T) {
	var b bytes.Buffer
	report(&b, &client.RefusedError{Code: "slug_taken", Message: "taken\x1b[2J\r\nsecond line‮" + strings.Repeat("x", 2000)})
	out := b.String()
	if strings.ContainsAny(out[:len(out)-1], "\x1b\r\n‮") {
		t.Fatalf("control characters reached the terminal: %q", out)
	}
	if len(out) > 400 {
		t.Fatalf("%d bytes were printed", len(out))
	}
}

// An older relay took the registration and ignored --access: the service is
// open. The client has stopped; it says why, where the service is and what to do.
func TestReport_AccessNotApplied(t *testing.T) {
	cases := []struct {
		err  *client.AccessNotAppliedError
		want string
	}{
		{&client.AccessNotAppliedError{Name: "my-app", Access: "burrow_login", URL: "https://relay.example.com/svc/abc234/"},
			"This relay is older and cannot restrict access from the client: --access login was not applied to service my-app. Nothing is served from this machine.\n" +
				"The service exists on the relay (https://relay.example.com/svc/abc234/) and is open to anyone with the URL unless its access mode was set in the dashboard. " +
				"Set the access mode there or delete the service, or run again without --access.\n"},
		{&client.AccessNotAppliedError{Name: "my-app", Access: "api_key"},
			"This relay is older and cannot restrict access from the client: --access api-key was not applied to service my-app. Nothing is served from this machine.\n" +
				"The service exists on the relay and is open to anyone with the URL unless its access mode was set in the dashboard. " +
				"Set the access mode there or delete the service, or run again without --access.\n"},
	}
	for _, tc := range cases {
		for _, err := range []error{tc.err, fmt.Errorf("x: %w", tc.err)} {
			var b bytes.Buffer
			if code := report(&b, err); code != 1 || b.String() != tc.want {
				t.Fatalf("report = %d\n got %q\nwant %q", code, b.String(), tc.want)
			}
		}
	}
	// Name and address come from outside.
	var b bytes.Buffer
	report(&b, &client.AccessNotAppliedError{Name: "a\x1b[2Jb", Access: "x\x1b[31m", URL: "https://relay.example.com/\r\nx"})
	if out := b.String(); strings.ContainsAny(strings.ReplaceAll(out, "\n", ""), "\x1b\r") || strings.Contains(out, "relay.example.com") {
		t.Fatalf("unsafe text was printed: %q", out)
	}
}
