package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/client"
)

const servicesOnly = "services:\n" +
	"  - { name: app, local: 127.0.0.1:3000 }\n" +
	"  - { name: web, local: 127.0.0.1:8080, type: http }\n"

func servicesNamed(name string) string {
	return "services:\n  - { name: " + name + ", local: 127.0.0.1:3000 }\n"
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUp_FindsBurrowYAML(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	cwd := t.TempDir()
	t.Chdir(cwd)
	inUserDir := filepath.Join(filepath.Dir(h.cfgPath), "burrow.yaml")
	inCwd := filepath.Join(cwd, "burrow.yaml")
	given := filepath.Join(t.TempDir(), "other.yaml")

	name := func() string {
		t.Helper()
		h.runs = nil
		if code := h.exec("up"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		return h.oneRun().tunnels[0].Name
	}

	// none
	if code := h.exec("up"); code != 2 || len(h.runs) != 0 {
		t.Fatalf("exit %d, runs %d", code, len(h.runs))
	}
	for _, w := range []string{"--file", "./burrow.yaml", inUserDir} {
		if !strings.Contains(h.stderr.String(), w) {
			t.Fatalf("stderr %q does not name %q", h.stderr.String(), w)
		}
	}

	writeAt(t, inUserDir, servicesNamed("from-user-dir"))
	if got := name(); got != "from-user-dir" {
		t.Fatalf("service = %q", got)
	}
	writeAt(t, inCwd, servicesNamed("from-cwd"))
	if got := name(); got != "from-cwd" {
		t.Fatalf("service = %q", got)
	}
	writeAt(t, given, servicesNamed("from-flag"))
	h.runs = nil
	if code := h.exec("up", "--file", given); code != 0 || h.oneRun().tunnels[0].Name != "from-flag" {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}

	// A --file that is not there is an error; the other places are not tried.
	h.runs = nil
	if code := h.exec("up", "--file", filepath.Join(cwd, "missing.yaml")); code == 0 || len(h.runs) != 0 {
		t.Fatalf("exit %d, runs %d", code, len(h.runs))
	}
}

// The user config directory is where the sign-in lives by default, also when
// --config points the sign-in somewhere else.
func TestUp_UserConfigDirectoryIsNotMovedByConfigFlag(t *testing.T) {
	h := newHarness(t)
	t.Chdir(t.TempDir())
	other := filepath.Join(t.TempDir(), "x.yaml")
	if err := client.SaveUserConfig(other, client.UserConfig{Control: "other.example.com:7000", Token: testToken}); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(filepath.Dir(h.cfgPath), "burrow.yaml"), servicesOnly)
	if code := h.exec("up", "--config", other); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if c := h.oneRun().creds; c.Control != "other.example.com:7000" {
		t.Fatalf("control = %q", c.Control)
	}
}

func TestUp_FileWithoutServerAndTokenUsesTheSignIn(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	yml := writeFile(t, "burrow.yaml", servicesOnly)
	if code := h.exec("up", "--file", yml); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	r := h.oneRun()
	if c := r.creds; c.Control != "burrow.example.com:7000" || c.Token != testToken || c.Source != client.SourceUserConfig || c.TokenName != "kohns-laptop" {
		t.Fatalf("credentials: control %q source %q name %q", c.Control, c.Source, c.TokenName)
	}
	// services without `type` are tcp, as before
	want := []client.TunnelSpec{
		{Name: "app", Type: "tcp", LocalAddr: "127.0.0.1:3000"},
		{Name: "web", Type: "http", LocalAddr: "127.0.0.1:8080"},
	}
	if !reflect.DeepEqual(r.tunnels, want) {
		t.Fatalf("tunnels = %+v, want %+v", r.tunnels, want)
	}
	if r.g != (globalFlags{logLevel: "info", logFormat: "text"}) {
		t.Fatalf("global flags = %+v", r.g)
	}
	h.noToken()
}

func TestUp_FileWithServerAndTokenUsesItsOwn(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	yml := writeFile(t, "burrow.yaml", twoServices)
	if code := h.exec("up", "--file", yml); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	r := h.oneRun()
	if c := r.creds; c.Control != "relay.example.com:7000" || c.Token != "filetok" || c.Source != client.SourceFile || c.TokenName != "" || c.Relay != "" {
		t.Fatalf("credentials: control %q source %q name %q relay %q", c.Control, c.Source, c.TokenName, c.Relay)
	}
	if len(r.tunnels) != 3 {
		t.Fatalf("tunnels = %+v", r.tunnels)
	}
}

func TestUp_FileWithOnlyAServerTakesTheStoredToken(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	yml := writeFile(t, "burrow.yaml", "server: relay.example.com:7000\n"+servicesOnly)
	if code := h.exec("up", "--file", yml); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if c := h.oneRun().creds; c.Control != "relay.example.com:7000" || c.Token != testToken || c.Source != client.SourceFile {
		t.Fatalf("credentials: control %q source %q", c.Control, c.Source)
	}
}

// The spec's precedence: the environment comes before burrow.yaml for `up`.
func TestUp_EnvironmentComesBeforeTheFile(t *testing.T) {
	h := newHarness(t)
	h.env["BURROW_SERVER"] = "env.example.com:7000"
	yml := writeFile(t, "burrow.yaml", twoServices)
	if code := h.exec("up", "--file", yml); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if c := h.oneRun().creds; c.Control != "env.example.com:7000" || c.Token != "filetok" || c.Source != client.SourceEnvironment {
		t.Fatalf("credentials: control %q source %q", c.Control, c.Source)
	}
}

func TestUp_NotSignedIn(t *testing.T) {
	h := newHarness(t)
	yml := writeFile(t, "burrow.yaml", servicesOnly)
	code := h.exec("up", "--file", yml)
	if code != 3 || h.stderr.String() != "Not signed in. Run: burrow login <your relay address>\n" || len(h.runs) != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}

func TestUp_Errors(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code := h.exec("up", "extra"); code != 2 {
		t.Fatalf("exit %d for an argument", code)
	}
	bad := writeFile(t, "burrow.yaml", "services: []\n")
	if code := h.exec("up", "--file", bad); code != 1 || !strings.HasPrefix(h.stderr.String(), "error: loadfileconfig: ") {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
	if len(h.runs) != 0 {
		t.Fatal("the client was started")
	}
}
