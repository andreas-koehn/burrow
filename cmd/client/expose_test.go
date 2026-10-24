package main

import (
	"context"
	"errors"
	"os"
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
	if code := h.exec("tcp", "5432", "--remote", "9000", "--name", "pg", "--access", "ip"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	want := []client.TunnelSpec{{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000, Access: "ip"}}
	if r := h.oneRun(); !reflect.DeepEqual(r.tunnels, want) {
		t.Fatalf("tunnels = %+v, want %+v", r.tunnels, want)
	}
}

// --proxy-protocol v2 makes the client write a PROXY v2 header to its
// target (tcp complete, E2).
func TestTCP_ProxyProtocol(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code := h.exec("tcp", "8443", "--name", "web", "--access", "open", "--proxy-protocol", "v2"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	want := []client.TunnelSpec{{Name: "web", Type: "tcp", LocalAddr: "127.0.0.1:8443", Access: "open", ProxyProtocol: "v2"}}
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
			if tc.cmd == "tcp" {
				want[0].AccessDefault = "private" // D2: the default of a new client
			}
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
		{"ftp target", []string{"http", "ftp://localhost:3000"}, []string{"https://", "h2c://", "Accepted forms:"}},
		{"tcp https target", []string{"tcp", "https://localhost:3000"}, []string{"http services", "Accepted forms:"}},
		{"tcp h2c target", []string{"tcp", "h2c://localhost:3000"}, []string{"http services", "Accepted forms:"}},
		{"tcp --upstream-protocol", []string{"tcp", "5432", "--upstream-protocol", "h2c"}, []string{"--upstream-protocol applies to http services"}},
		{"unknown upstream protocol", []string{"http", "3000", "--upstream-protocol", "grpc"}, []string{"http1, https, h2c or h2"}},
		{"https target against h2c", []string{"http", "https://localhost:3000", "--upstream-protocol", "h2c"}, []string{"https://"}},
		{"h2c target against h2", []string{"http", "h2c://localhost:3000", "--upstream-protocol", "h2"}, []string{"h2c://"}},
		{"grpc without a target", []string{"grpc"}, []string{"Usage:", "burrow grpc <target>"}},
		{"grpc --remote", []string{"grpc", "50051", "--remote", "9000"}, []string{"--remote applies to tcp services"}},
		{"grpc --upstream-protocol http1", []string{"grpc", "50051", "--upstream-protocol", "http1"}, []string{"h2c or h2"}},
		{"target with a path", []string{"http", "http://localhost:3000/app"}, []string{"must not contain a path", "Accepted forms:"}},
		{"target that is no port", []string{"tcp", "nonsense"}, []string{"Accepted forms:"}},
		{"tcp --slug", []string{"tcp", "5432", "--slug", "x"}, []string{"--slug applies to http services"}},
		{"tcp --access", []string{"tcp", "5432", "--access", "login"}, []string{"--access must be one of: open, ip, private"}},
		{"http --remote", []string{"http", "3000", "--remote", "9000"}, []string{"--remote applies to tcp services"}},
		{"unknown access", []string{"http", "3000", "--access", "nonsense"}, []string{"open", "login", "api-key"}},
		{"relay access name", []string{"http", "3000", "--access", "burrow_login"}, []string{"open", "login", "api-key"}},
		{"bad slug", []string{"http", "3000", "--slug", "Bad_Slug"}, []string{"--slug", "3 to 40", "lowercase letters, digits and hyphens"}},
		{"short slug", []string{"http", "3000", "--slug", "ab"}, []string{"3 to 40"}},
		{"slug ending in a hyphen", []string{"http", "3000", "--slug", "abc-"}, []string{"3 to 40"}},
		{"empty name", []string{"http", "3000", "--name", " "}, []string{"--name"}},
		{"remote out of range", []string{"tcp", "5432", "--remote", "70000"}, []string{"--remote"}},
		{"negative remote", []string{"tcp", "5432", "--remote", "-1"}, []string{"--remote"}},
		{"http --proxy-protocol", []string{"http", "3000", "--proxy-protocol", "v2"}, []string{"--proxy-protocol applies to tcp services"}},
		{"unknown proxy protocol", []string{"tcp", "8443", "--proxy-protocol", "v1"}, []string{"--proxy-protocol must be v2"}},
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
	if code := h.exec("http", "ftp://localhost:3000"); code != 2 {
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

// The stored token belongs to the relay it was issued by. A server from the
// environment may be the same control endpoint written differently; any other
// server does not get the stored token.
func TestExpose_StoredTokenGoesOnlyToItsOwnRelay(t *testing.T) {
	for _, same := range []string{"burrow.example.com:7000", "Burrow.Example.COM:7000", "burrow.example.com.:7000"} {
		h := newHarness(t)
		h.signIn()
		h.env["BURROW_SERVER"] = same
		if code := h.exec("tcp", "5432"); code != 0 {
			t.Fatalf("%s: exit %d: %s", same, code, h.stderr.String())
		}
		if c := h.oneRun().creds; c.Token != testToken || c.TokenName != "kohns-laptop" || c.Source != client.SourceEnvironment {
			t.Fatalf("%s: source %q name %q", same, c.Source, c.TokenName)
		}
	}
	for _, other := range []string{"env.example.com:7000", "burrow.example.com:7001"} {
		for _, cmd := range [][]string{{"http", "3000"}, {"tcp", "5432"}, {"status"}} {
			h := newHarness(t)
			h.signIn()
			h.env["BURROW_SERVER"] = other
			code := h.exec(cmd...)
			if code != 3 || len(h.runs) != 0 || h.stdout.Len() != 0 {
				t.Fatalf("%s %v: exit %d, runs %d, stdout %q", other, cmd, code, len(h.runs), h.stdout.String())
			}
			for _, w := range []string{"Not signed in to " + other, "https://burrow.example.com", "burrow login "} {
				if !strings.Contains(h.stderr.String(), w) {
					t.Fatalf("%s %v: stderr %q does not contain %q", other, cmd, h.stderr.String(), w)
				}
			}
			h.noToken()
		}
	}
	// A token given explicitly goes wherever the server says.
	h := newHarness(t)
	h.signIn()
	h.env["BURROW_SERVER"], h.env["BURROW_TOKEN"] = "env.example.com:7000", "bur_explicit_token_0001"
	if code := h.exec("http", "3000"); code != 0 || h.oneRun().creds.Token != "bur_explicit_token_0001" {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
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

// --slug and --access travel in the tunnel spec, the access mode under the
// relay's name.
func TestHTTP_SlugAndAccessAreSent(t *testing.T) {
	for access, mode := range map[string]string{"open": "open", "login": "burrow_login", "api-key": "api_key"} {
		t.Run(access, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			if code := h.exec("http", "3000", "--access", access, "--slug", "my-app"); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			want := []client.TunnelSpec{{Name: "kohns-laptop-3000", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app", Access: mode}}
			if r := h.oneRun(); !reflect.DeepEqual(r.tunnels, want) {
				t.Fatalf("tunnels = %+v, want %+v", r.tunnels, want)
			}
			if h.stderr.Len() != 0 {
				t.Fatalf("stderr = %q", h.stderr.String())
			}
		})
	}
	t.Run("one of the two", func(t *testing.T) {
		h := newHarness(t)
		h.signIn()
		if code := h.exec("http", "3000", "--slug", "my-app"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if sp := h.oneRun().tunnels[0]; sp.Slug != "my-app" || sp.Access != "" {
			t.Fatalf("spec = %+v", sp)
		}
		h.runs = nil
		if code := h.exec("http", "3000", "--access", "api-key"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if sp := h.oneRun().tunnels[0]; sp.Slug != "" || sp.Access != "api_key" {
			t.Fatalf("spec = %+v", sp)
		}
	})
	// The help no longer says the flags are for later.
	h := newHarness(t)
	h.exec("http", "--help")
	if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, "does not send them yet") || !strings.Contains(out, "--slug") {
		t.Fatalf("help: %s", out)
	}
}

// `up` passes on what burrow.yaml wishes for its services.
func TestUp_SlugAndAccessFromTheFile(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	file := filepath.Join(t.TempDir(), "burrow.yaml")
	if err := os.WriteFile(file, []byte("services:\n  - { name: app, local: 127.0.0.1:3000, type: http, slug: my-app, access: login }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.exec("up", "--file", file); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	want := []client.TunnelSpec{{Name: "app", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app", Access: "burrow_login"}}
	if r := h.oneRun(); !reflect.DeepEqual(r.tunnels, want) {
		t.Fatalf("tunnels = %+v", r.tunnels)
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

func TestHTTP_UpstreamProtocol(t *testing.T) {
	for _, c := range []struct {
		name  string
		args  []string
		local string
		want  string
	}{
		{"none", []string{"http", "3000"}, "127.0.0.1:3000", ""},
		{"flag h2c", []string{"http", "50051", "--upstream-protocol", "h2c"}, "127.0.0.1:50051", "h2c"},
		{"flag http1", []string{"http", "3000", "--upstream-protocol", "http1"}, "127.0.0.1:3000", "http1"},
		{"https scheme", []string{"http", "https://localhost:8443"}, "localhost:8443", "https"},
		{"https scheme with h2", []string{"http", "https://localhost:8443", "--upstream-protocol", "h2"}, "localhost:8443", "h2"},
		{"h2c scheme", []string{"http", "h2c://localhost:50051"}, "localhost:50051", "h2c"},
		{"grpc", []string{"grpc", "50051"}, "127.0.0.1:50051", "h2c"},
		{"grpc h2c scheme", []string{"grpc", "h2c://localhost:50051"}, "localhost:50051", "h2c"},
		{"grpc https scheme", []string{"grpc", "https://localhost:50443"}, "localhost:50443", "h2"},
		{"grpc flag h2", []string{"grpc", "localhost:50443", "--upstream-protocol", "h2"}, "localhost:50443", "h2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			if code := h.exec(c.args...); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			sp := h.oneRun().tunnels[0]
			if sp.Type != "http" || sp.LocalAddr != c.local || sp.UpstreamProtocol != c.want {
				t.Fatalf("spec = %+v, want local %s protocol %q", sp, c.local, c.want)
			}
		})
	}
	// The other flags of burrow http work on burrow grpc.
	h := newHarness(t)
	h.signIn()
	if code := h.exec("grpc", "50051", "--name", "api", "--slug", "my-api", "--access", "api-key"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if sp := h.oneRun().tunnels[0]; sp.Name != "api" || sp.Slug != "my-api" || sp.Access != "api_key" || sp.UpstreamProtocol != "h2c" {
		t.Fatalf("spec = %+v", sp)
	}
}

func TestUp_UpstreamProtocolFromTheFile(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	file := filepath.Join(t.TempDir(), "burrow.yaml")
	if err := os.WriteFile(file, []byte("services:\n  - { name: api, local: 127.0.0.1:50051, type: http, upstream_protocol: h2c }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.exec("up", "--file", file); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if sp := h.oneRun().tunnels[0]; sp.UpstreamProtocol != "h2c" {
		t.Fatalf("spec = %+v", sp)
	}
}

func TestReport_UpstreamNotApplied(t *testing.T) {
	var b strings.Builder
	code := report(&b, &client.UpstreamNotAppliedError{Name: "api", Protocol: "h2c"})
	out := b.String()
	if code == 0 || !strings.Contains(out, "h2c") || !strings.Contains(out, "api") || !strings.Contains(out, "--upstream-protocol http1") {
		t.Fatalf("code %d: %q", code, out)
	}
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("out = %q", out)
	}
}
