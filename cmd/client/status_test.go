package main

import (
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/client"
)

func TestStatus_SignedIn(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code := h.exec("status"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	h.noToken()
	out := h.stdout.String()
	for _, w := range []string{
		"https://burrow.example.com", "burrow.example.com:7000",
		"kohns-laptop (…WXYZ)", client.SourceUserConfig, versionLine(),
	} {
		if !strings.Contains(out, w) {
			t.Fatalf("stdout does not contain %q:\n%s", w, out)
		}
	}
	if len(h.runs) != 0 {
		t.Fatal("status connected a tunnel")
	}
}

func TestStatus_NotSignedIn(t *testing.T) {
	h := newHarness(t)
	code := h.exec("status")
	if code != 3 || h.stderr.String() != "Not signed in. Run: burrow login <your relay address>\n" || h.stdout.Len() != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, h.stderr.String(), h.stdout.String())
	}
}

// The token line never has empty parentheses or a dangling separator.
func TestStatus_TokenLine(t *testing.T) {
	cases := []struct {
		name, tokenName, token, want string
	}{
		{"name and tail", "work-laptop", testToken, "Token:    work-laptop (…WXYZ)\n"},
		{"short token shows the name alone", "work-laptop", "abcd", "Token:    work-laptop\n"},
		{"no name", "", testToken, "Token:    …WXYZ\n"},
		{"neither", "", "abc", "Token:    set\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := client.UserConfig{Relay: "https://burrow.example.com", Control: "burrow.example.com:7000", Token: tc.token, TokenName: tc.tokenName}
			if err := client.SaveUserConfig(h.cfgPath, c); err != nil {
				t.Fatal(err)
			}
			if code := h.exec("status"); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(h.stdout.String(), tc.want) {
				t.Fatalf("stdout does not contain %q:\n%s", tc.want, h.stdout.String())
			}
		})
	}
}

func TestStatus_FromTheEnvironment(t *testing.T) {
	h := newHarness(t)
	h.env["BURROW_SERVER"], h.env["BURROW_TOKEN"] = "env.example.com:7000", testToken
	if code := h.exec("status"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	h.noToken()
	out := h.stdout.String()
	for _, w := range []string{"env.example.com:7000", "Token:    …WXYZ\n", client.SourceEnvironment, "Relay:    not known\n"} {
		if !strings.Contains(out, w) {
			t.Fatalf("stdout does not contain %q:\n%s", w, out)
		}
	}
}
