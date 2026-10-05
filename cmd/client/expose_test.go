package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/client"
)

func TestHTTP_UsesTheStoredSignIn(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	r := h.oneRun()
	c := r.creds
	if c.Control != "burrow.example.com:7000" || c.Token != testToken || c.TokenName != "kohns-laptop" ||
		c.Relay != "https://burrow.example.com" || c.Source != client.SourceUserConfig {
		t.Fatalf("credentials: control %q name %q relay %q source %q, stored token used: %v",
			c.Control, c.TokenName, c.Relay, c.Source, c.Token == testToken)
	}
	want := []client.TunnelSpec{{Name: "kohns-laptop-3000", Type: "http", LocalAddr: "127.0.0.1:3000"}}
	if !reflect.DeepEqual(r.tunnels, want) {
		t.Fatalf("tunnels = %+v, want %+v", r.tunnels, want)
	}
	if r.g != (globalFlags{logLevel: "info", logFormat: "text"}) {
		t.Fatalf("global flags = %+v", r.g)
	}
	h.noToken()
}

func TestTCP_RemoteAndName(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code := h.exec("tcp", "5432", "--remote", "9000", "--name", "pg"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	want := []client.TunnelSpec{{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000}}
	if r := h.oneRun(); !reflect.DeepEqual(r.tunnels, want) {
		t.Fatalf("tunnels = %+v, want %+v", r.tunnels, want)
	}
}

func TestExpose_Targets(t *testing.T) {
	cases := []struct{ cmd, target, local, name string }{
		{"http", "localhost:3000", "localhost:3000", "kohns-laptop-3000"},
		{"http", "http://localhost:8080/", "localhost:8080", "kohns-laptop-8080"},
		{"tcp", "192.168.1.20:8080", "192.168.1.20:8080", "kohns-laptop-8080"},
		{"tcp", "5432", "127.0.0.1:5432", "kohns-laptop-5432"},
	}
	for _, tc := range cases {
		t.Run(tc.cmd+" "+tc.target, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			if code := h.exec(tc.cmd, tc.target); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			want := []client.TunnelSpec{{Name: tc.name, Type: tc.cmd, LocalAddr: tc.local}}
			if r := h.oneRun(); !reflect.DeepEqual(r.tunnels, want) {
				t.Fatalf("tunnels = %+v, want %+v", r.tunnels, want)
			}
		})
	}
}

func TestExpose_DefaultNameWithoutAHostname(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.hostname, h.hostErr = "", errors.New("no hostname")
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if r := h.oneRun(); r.tunnels[0].Name != "burrow-3000" {
		t.Fatalf("name = %q", r.tunnels[0].Name)
	}
}

func TestExpose_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string // all must appear on stderr
	}{
		{"http without a target", []string{"http"}, []string{"Usage:", "burrow http <target>"}},
		{"http with two targets", []string{"http", "a", "b"}, []string{"Usage:", "burrow http <target>"}},
		{"tcp without a target", []string{"tcp"}, []string{"Usage:", "burrow tcp <target>"}},
		{"https target", []string{"http", "https://localhost:3000"}, []string{"HTTPS upstreams are not supported", "Accepted forms:"}},
		{"target with a path", []string{"http", "http://localhost:3000/app"}, []string{"must not contain a path", "Accepted forms:"}},
		{"target that is no port", []string{"tcp", "nonsense"}, []string{"Accepted forms:"}},
		{"tcp --slug", []string{"tcp", "5432", "--slug", "x"}, []string{"--slug applies to http services"}},
		{"tcp --access", []string{"tcp", "5432", "--access", "open"}, []string{"--access applies to http services"}},
		{"http --remote", []string{"http", "3000", "--remote", "9000"}, []string{"--remote applies to tcp services"}},
		{"unknown access", []string{"http", "3000", "--access", "nonsense"}, []string{"open", "login", "api-key"}},
		{"relay access name", []string{"http", "3000", "--access", "burrow_login"}, []string{"open", "login", "api-key"}},
		{"bad slug", []string{"http", "3000", "--slug", "Bad_Slug"}, []string{"--slug", "3 to 40", "lowercase letters, digits and hyphens"}},
		{"short slug", []string{"http", "3000", "--slug", "ab"}, []string{"3 to 40"}},
		{"slug ending in a hyphen", []string{"http", "3000", "--slug", "abc-"}, []string{"3 to 40"}},
		{"empty name", []string{"http", "3000", "--name", " "}, []string{"--name"}},
		{"remote out of range", []string{"tcp", "5432", "--remote", "70000"}, []string{"--remote"}},
		{"negative remote", []string{"tcp", "5432", "--remote", "-1"}, []string{"--remote"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			if code := h.exec(tc.args...); code != 2 {
				t.Fatalf("exit %d, want 2; stderr %q", code, h.stderr.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(h.stderr.String(), w) {
					t.Fatalf("stderr %q does not contain %q", h.stderr.String(), w)
				}
			}
			if strings.HasPrefix(h.stderr.String(), "error:") {
				t.Fatalf("stderr = %q", h.stderr.String())
			}
			if len(h.runs) != 0 {
				t.Fatal("the client was started")
			}
		})
	}
}

// A usage error is reported before the sign-in is looked at.
func TestExpose_UsageErrorComesBeforeNotSignedIn(t *testing.T) {
	h := newHarness(t)
	if code := h.exec("http", "https://localhost:3000"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestExpose_NotSignedIn(t *testing.T) {
	for _, cmd := range []string{"http", "tcp"} {
		t.Run(cmd, func(t *testing.T) {
			h := newHarness(t)
			code := h.exec(cmd, "3000")
			if code != 3 || h.stderr.String() != "Not signed in. Run: burrow login <your relay address>\n" || h.stdout.Len() != 0 {
				t.Fatalf("exit %d, stderr %q, stdout %q", code, h.stderr.String(), h.stdout.String())
			}
			if len(h.runs) != 0 {
				t.Fatal("the client was started")
			}
		})
	}
}

func TestExpose_BlankStoredValuesCountAsNotSignedIn(t *testing.T) {
	for name, c := range map[string]client.UserConfig{
		"blank token":   {Relay: "https://burrow.example.com", Control: "burrow.example.com:7000", Token: " \t\n"},
		"blank control": {Relay: "https://burrow.example.com", Control: "  ", Token: testToken},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if err := client.SaveUserConfig(h.cfgPath, c); err != nil {
				t.Fatal(err)
			}
			if code := h.exec("http", "3000"); code != 3 || len(h.runs) != 0 {
				t.Fatalf("exit %d, runs %d", code, len(h.runs))
			}
		})
	}
}

func TestExpose_Environment(t *testing.T) {
	h := newHarness(t)
	h.env["BURROW_SERVER"] = "env.example.com:7000"
	h.env["BURROW_TOKEN"] = testToken
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	c := h.oneRun().creds
	if c.Control != "env.example.com:7000" || c.Token != testToken || c.Source != client.SourceEnvironment || c.Relay != "" || c.TokenName != "" {
		t.Fatalf("credentials: control %q source %q relay %q name %q", c.Control, c.Source, c.Relay, c.TokenName)
	}
}

func TestExpose_EnvironmentWinsOverTheStoredSignIn(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.env["BURROW_SERVER"] = "env.example.com:7000"
	if code := h.exec("tcp", "5432"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	c := h.oneRun().creds
	if c.Control != "env.example.com:7000" || c.Token != testToken || c.Source != client.SourceEnvironment || c.TokenName != "kohns-laptop" {
		t.Fatalf("credentials: control %q source %q name %q", c.Control, c.Source, c.TokenName)
	}
}

func TestExpose_ConfigFlagPointsTheUserConfigElsewhere(t *testing.T) {
	h := newHarness(t)
	other := filepath.Join(t.TempDir(), "x.yaml")
	if err := client.SaveUserConfig(other, client.UserConfig{Relay: "https://other.example.com", Control: "other.example.com:7000", Token: testToken, TokenName: "other"}); err != nil {
		t.Fatal(err)
	}
	if code := h.exec("http", "3000", "--config", other); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if c := h.oneRun().creds; c.Control != "other.example.com:7000" || c.TokenName != "other" {
		t.Fatalf("credentials: control %q name %q", c.Control, c.TokenName)
	}
	// and the flag may come before the command
	h.runs = nil
	if code := h.exec("--config", other, "tcp", "5432"); code != 0 || h.oneRun().creds.Control != "other.example.com:7000" {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

func TestExpose_GlobalFlags(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code := h.exec("http", "3000", "--log", "json", "--insecure", "--cacert", "/tmp/ca.pem", "--server-name", "sni.example"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	want := globalFlags{logLevel: "info", logFormat: "json", cacert: "/tmp/ca.pem", serverName: "sni.example", insecure: true}
	if g := h.oneRun().g; g != want {
		t.Fatalf("global flags = %+v, want %+v", g, want)
	}

	h.runs = nil
	if code := h.exec("http", "3000", "--log", "xml"); code != 2 || len(h.runs) != 0 || !strings.Contains(h.stderr.String(), "text or json") {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}

// Without --log the level and format come from the environment, as they do for `connect`.
func TestExpose_LogSettingsFromTheEnvironment(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.env["BURROW_LOG_LEVEL"], h.env["BURROW_LOG_FORMAT"] = "debug", "json"
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if g := h.oneRun().g; g.logLevel != "debug" || g.logFormat != "json" {
		t.Fatalf("global flags = %+v", g)
	}
	h.runs = nil
	if code := h.exec("http", "3000", "--log", "text"); code != 0 || h.oneRun().g.logFormat != "text" {
		t.Fatalf("exit %d", code)
	}
}

// --slug and --access are checked now and sent from the protocol task on.
func TestHTTP_SlugAndAccessAreAcceptedButNotSentYet(t *testing.T) {
	for _, access := range []string{"open", "login", "api-key"} {
		t.Run(access, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			if code := h.exec("http", "3000", "--slug", "my-app", "--access", access); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			want := []client.TunnelSpec{{Name: "kohns-laptop-3000", Type: "http", LocalAddr: "127.0.0.1:3000"}}
			if r := h.oneRun(); !reflect.DeepEqual(r.tunnels, want) {
				t.Fatalf("tunnels = %+v", r.tunnels)
			}
			if !strings.Contains(h.stderr.String(), "applied once the relay supports it") {
				t.Fatalf("stderr = %q", h.stderr.String())
			}
		})
	}
	h := newHarness(t)
	h.signIn()
	if h.exec("http", "3000"); h.stderr.Len() != 0 {
		t.Fatalf("a note was printed without the flags: %q", h.stderr.String())
	}
}

func TestExpose_HowTheRunEnds(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	// Ctrl-C ends a foreground command without an error.
	h.runErr = context.Canceled
	if code := h.exec("http", "3000"); code != 0 || h.stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
	h.runErr = errors.New("boom")
	if code := h.exec("tcp", "5432"); code != 1 || h.stderr.String() != "error: boom\n" {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}
