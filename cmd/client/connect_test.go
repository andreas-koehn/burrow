package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
)

// These tests pin what `burrow connect` does: which options reach the client
// for every flag, for --config and for the environment variables, and which
// errors come back. They were written against the command before it was split
// into files and must keep passing unchanged.

// clientEnv are the variables config.LoadClient reads.
var clientEnv = []string{
	"BURROW_SERVER", "BURROW_TOKEN", "BURROW_TOKEN_FILE", "BURROW_INSECURE",
	"BURROW_CACERT", "BURROW_SERVER_NAME", "BURROW_LOG_LEVEL", "BURROW_LOG_FORMAT",
}

// cleanEnv removes the client's variables for the test and points every user
// config location at an empty temporary directory.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range clientEnv {
		t.Setenv(k, "") // registers the restore
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("AppData", filepath.Join(home, "appdata"))
}

// started records what a command handed to the client.
type started struct {
	calls  int
	opts   client.Options
	ctxErr error // ctx.Err() at the time of the call
	ret    error // what the fake returns
}

// captureStart replaces startClient for the test.
func captureStart(t *testing.T) *started {
	t.Helper()
	s := &started{}
	prev := startClient
	startClient = func(ctx context.Context, o client.Options) error {
		s.calls++
		s.opts = o
		if ctx == nil {
			t.Error("startClient got a nil context")
		} else {
			s.ctxErr = ctx.Err()
		}
		return s.ret
	}
	t.Cleanup(func() { startClient = prev })
	return s
}

// runConnect executes the connect command on its own, as main_test.go does.
func runConnect(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newConnectCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	return cmd.Execute()
}

// logShape reports the format and the debug setting of a logger.
func logShape(t *testing.T, l *slog.Logger) (format string, debug bool) {
	t.Helper()
	if l == nil {
		t.Fatal("no logger was passed to the client")
	}
	switch l.Handler().(type) {
	case *slog.TextHandler:
		format = "text"
	case *slog.JSONHandler:
		format = "json"
	default:
		t.Fatalf("unexpected log handler %T", l.Handler())
	}
	if !l.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info logging is off")
	}
	return format, l.Enabled(context.Background(), slog.LevelDebug)
}

// writeCA writes a self-signed certificate as PEM and returns its path.
func writeCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const twoServices = "server: relay.example.com:7000\ntoken: filetok\nservices:\n" +
	"  - { name: app, local: 127.0.0.1:3000 }\n" +
	"  - { name: web, local: 127.0.0.1:8080, type: http, remote: 9000 }\n" +
	"  - { name: db, local: 127.0.0.1:5432, type: tcp, remote: 9001 }\n"

func TestConnect_SingleTunnel_AllFlags(t *testing.T) {
	cleanEnv(t)
	s := captureStart(t)
	if err := runConnect(t, "--server", "relay.example.com:7000", "--token", "tok",
		"--local", "127.0.0.1:8080", "--remote", "9000", "--name", "web"); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 || s.ctxErr != nil {
		t.Fatalf("calls = %d, ctx err = %v", s.calls, s.ctxErr)
	}
	o := s.opts
	if o.Server != "relay.example.com:7000" || o.Token != "tok" || o.Insecure || o.RootCAs != nil || o.ServerName != "relay.example.com" {
		t.Fatalf("options: server %q insecure %v pool %v server name %q", o.Server, o.Insecure, o.RootCAs != nil, o.ServerName)
	}
	want := []client.TunnelSpec{{Name: "web", Type: "tcp", RemotePort: 9000, LocalAddr: "127.0.0.1:8080"}}
	if !reflect.DeepEqual(o.Tunnels, want) {
		t.Fatalf("tunnels = %+v, want %+v", o.Tunnels, want)
	}
	if f, debug := logShape(t, o.Logger); f != "text" || debug {
		t.Fatalf("logger = %s debug %v, want text at info", f, debug)
	}
}

func TestConnect_SingleTunnel_Defaults(t *testing.T) {
	cleanEnv(t)
	s := captureStart(t)
	if err := runConnect(t, "--server", "10.0.0.1:7000", "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	want := []client.TunnelSpec{{Name: "", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:3000"}}
	if !reflect.DeepEqual(s.opts.Tunnels, want) {
		t.Fatalf("tunnels = %+v, want %+v", s.opts.Tunnels, want)
	}
	if s.opts.ServerName != "10.0.0.1" {
		t.Fatalf("server name = %q", s.opts.ServerName)
	}
}

func TestConnect_SingleTunnel_TLSFlagsAndHTTP(t *testing.T) {
	cleanEnv(t)
	s := captureStart(t)
	ca := writeCA(t)
	if err := runConnect(t, "--server", "relay.example.com:7000", "--token", "tok", "--type", "http",
		"--remote", "9000", "--insecure", "--cacert", ca, "--server-name", "sni.example"); err != nil {
		t.Fatal(err)
	}
	o := s.opts
	if !o.Insecure || o.RootCAs == nil || o.ServerName != "sni.example" {
		t.Fatalf("options: insecure %v pool %v server name %q", o.Insecure, o.RootCAs != nil, o.ServerName)
	}
	// --remote is passed through for http as well; the relay ignores it.
	want := []client.TunnelSpec{{Type: "http", RemotePort: 9000, LocalAddr: "127.0.0.1:3000"}}
	if !reflect.DeepEqual(o.Tunnels, want) {
		t.Fatalf("tunnels = %+v, want %+v", o.Tunnels, want)
	}
}

func TestConnect_SingleTunnel_Errors(t *testing.T) {
	empty := writeFile(t, "empty.pem", "not a certificate\n")
	missing := filepath.Join(t.TempDir(), "missing.pem")
	cases := []struct {
		name   string
		args   []string
		prefix string
		exact  string
	}{
		{name: "unknown type", args: []string{"--server", "h:7000", "--token", "t", "--type", "xyz"},
			exact: `unknown tunnel type "xyz": must be tcp or http`},
		{name: "no server", args: []string{"--token", "t"}, prefix: "invalid client config: "},
		{name: "no token", args: []string{"--server", "h:7000"}, prefix: "invalid client config: "},
		{name: "server without port", args: []string{"--server", "relay.example.com", "--token", "t"}, prefix: "invalid client config: "},
		{name: "cacert without certificates", args: []string{"--server", "h:7000", "--token", "t", "--cacert", empty},
			exact: "cacert " + empty + ": no certificates"},
		{name: "cacert missing", args: []string{"--server", "h:7000", "--token", "t", "--cacert", missing},
			prefix: "open " + missing},
		{name: "unknown flag", args: []string{"--bogus"}, exact: "unknown flag: --bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			s := captureStart(t)
			err := runConnect(t, tc.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.exact != "" && err.Error() != tc.exact {
				t.Fatalf("error = %q, want %q", err, tc.exact)
			}
			if tc.prefix != "" && !strings.HasPrefix(err.Error(), tc.prefix) {
				t.Fatalf("error = %q, want prefix %q", err, tc.prefix)
			}
			if s.calls != 0 {
				t.Fatal("the client was started")
			}
			if c := exitCodeOfConnectError(err); c != 1 {
				t.Fatalf("exit code = %d, want 1 as before", c)
			}
		})
	}
}

func TestConnect_SingleTunnel_Environment(t *testing.T) {
	t.Run("log level and format come from the environment", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("BURROW_LOG_LEVEL", "debug")
		t.Setenv("BURROW_LOG_FORMAT", "json")
		s := captureStart(t)
		if err := runConnect(t, "--server", "h:7000", "--token", "t"); err != nil {
			t.Fatal(err)
		}
		if f, debug := logShape(t, s.opts.Logger); f != "json" || !debug {
			t.Fatalf("logger = %s debug %v, want json at debug", f, debug)
		}
	})
	t.Run("flags win over BURROW_SERVER and BURROW_TOKEN", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("BURROW_SERVER", "env.example.com:7000")
		t.Setenv("BURROW_TOKEN", "envtok")
		s := captureStart(t)
		if err := runConnect(t, "--server", "flag.example.com:7000", "--token", "flagtok"); err != nil {
			t.Fatal(err)
		}
		if s.opts.Server != "flag.example.com:7000" || s.opts.Token != "flagtok" {
			t.Fatalf("server %q, token from flag: %v", s.opts.Server, s.opts.Token == "flagtok")
		}
	})
	// The flags are always handed to the loader, also when empty, so they hide
	// the variables below. That is how the command behaves today; these cases
	// keep it from changing unnoticed.
	t.Run("BURROW_SERVER and BURROW_TOKEN without flags are not used", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("BURROW_SERVER", "env.example.com:7000")
		t.Setenv("BURROW_TOKEN", "envtok")
		s := captureStart(t)
		err := runConnect(t)
		if err == nil || !strings.HasPrefix(err.Error(), "invalid client config: ") || s.calls != 0 {
			t.Fatalf("error = %v, calls = %d", err, s.calls)
		}
	})
	t.Run("BURROW_TOKEN_FILE without --token is not used", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("BURROW_TOKEN_FILE", writeFile(t, "tok", "filetok\n"))
		s := captureStart(t)
		err := runConnect(t, "--server", "h:7000")
		if err == nil || !strings.HasPrefix(err.Error(), "invalid client config: ") || s.calls != 0 {
			t.Fatalf("error = %v, calls = %d", err, s.calls)
		}
	})
	t.Run("BURROW_INSECURE, BURROW_SERVER_NAME and BURROW_CACERT are not used", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("BURROW_INSECURE", "true")
		t.Setenv("BURROW_SERVER_NAME", "env.sni")
		t.Setenv("BURROW_CACERT", writeCA(t))
		s := captureStart(t)
		if err := runConnect(t, "--server", "h:7000", "--token", "t"); err != nil {
			t.Fatal(err)
		}
		if o := s.opts; o.Insecure || o.ServerName != "h" || o.RootCAs != nil {
			t.Fatalf("options: insecure %v server name %q pool %v", o.Insecure, o.ServerName, o.RootCAs != nil)
		}
	})
}

func TestConnect_ConfigFile(t *testing.T) {
	cleanEnv(t)
	// The environment plays no part in --config mode.
	t.Setenv("BURROW_SERVER", "env.example.com:7000")
	t.Setenv("BURROW_TOKEN", "envtok")
	t.Setenv("BURROW_LOG_LEVEL", "debug")
	t.Setenv("BURROW_LOG_FORMAT", "json")
	s := captureStart(t)
	yml := writeFile(t, "burrow.yaml", twoServices)
	if err := runConnect(t, "--config", yml); err != nil {
		t.Fatal(err)
	}
	o := s.opts
	if s.calls != 1 || o.Server != "relay.example.com:7000" || o.Token != "filetok" || o.Insecure || o.RootCAs != nil || o.ServerName != "relay.example.com" {
		t.Fatalf("calls %d server %q token from file %v insecure %v pool %v server name %q",
			s.calls, o.Server, o.Token == "filetok", o.Insecure, o.RootCAs != nil, o.ServerName)
	}
	want := []client.TunnelSpec{
		{Name: "app", Type: "tcp", LocalAddr: "127.0.0.1:3000"},
		{Name: "web", Type: "http", LocalAddr: "127.0.0.1:8080"},
		{Name: "db", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9001},
	}
	if !reflect.DeepEqual(o.Tunnels, want) {
		t.Fatalf("tunnels = %+v, want %+v", o.Tunnels, want)
	}
	if f, debug := logShape(t, o.Logger); f != "text" || debug {
		t.Fatalf("logger = %s debug %v, want text at info", f, debug)
	}
}

func TestConnect_ConfigFile_TLSFlags(t *testing.T) {
	cleanEnv(t)
	s := captureStart(t)
	yml := writeFile(t, "burrow.yaml", twoServices)
	if err := runConnect(t, "--config", yml, "--insecure", "--cacert", writeCA(t), "--server-name", "sni.example"); err != nil {
		t.Fatal(err)
	}
	if o := s.opts; !o.Insecure || o.RootCAs == nil || o.ServerName != "sni.example" {
		t.Fatalf("options: insecure %v pool %v server name %q", o.Insecure, o.RootCAs != nil, o.ServerName)
	}
}

func TestConnect_ConfigFile_Errors(t *testing.T) {
	cleanEnv(t)
	yml := writeFile(t, "burrow.yaml", twoServices)
	empty := writeFile(t, "empty.pem", "nothing\n")
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	cases := []struct {
		name          string
		args          []string
		exact, prefix string
	}{
		{name: "with --server", args: []string{"--config", yml, "--server", "h:7000"}, exact: "--config cannot be combined with --server"},
		{name: "with --remote", args: []string{"--config", yml, "--remote", "9000"}, exact: "--config cannot be combined with --remote"},
		{name: "missing file", args: []string{"--config", missing}, prefix: "loadfileconfig: read " + missing + ": "},
		{name: "cacert without certificates", args: []string{"--config", yml, "--cacert", empty}, exact: "cacert " + empty + ": no certificates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := captureStart(t)
			err := runConnect(t, tc.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.exact != "" && err.Error() != tc.exact {
				t.Fatalf("error = %q, want %q", err, tc.exact)
			}
			if tc.prefix != "" && !strings.HasPrefix(err.Error(), tc.prefix) {
				t.Fatalf("error = %q, want prefix %q", err, tc.prefix)
			}
			if s.calls != 0 {
				t.Fatal("the client was started")
			}
		})
	}
}

// What the client returns is what the command returns: main turns it into
// "error: …" and exit code 1, also for the cancelled context after Ctrl-C.
func TestConnect_ReturnsTheClientsError(t *testing.T) {
	for _, mode := range []string{"single", "config"} {
		t.Run(mode, func(t *testing.T) {
			cleanEnv(t)
			s := captureStart(t)
			s.ret = context.Canceled
			args := []string{"--server", "h:7000", "--token", "t"}
			if mode == "config" {
				args = []string{"--config", writeFile(t, "burrow.yaml", twoServices)}
			}
			err := runConnect(t, args...)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if c := exitCodeOfConnectError(err); c != 1 {
				t.Fatalf("exit code = %d, want 1 as before", c)
			}
		})
	}
}

// From here on: `connect` as a command of the root.

// The root's --config is the user config; `connect` keeps its own --config,
// the path to burrow.yaml.
func TestConnect_ThroughRoot_ConfigIsBurrowYAML(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.env["BURROW_SERVER"], h.env["BURROW_TOKEN"] = "env.example.com:7000", "envtok"
	s := captureStart(t)
	yml := writeFile(t, "burrow.yaml", twoServices)
	if code := h.exec("connect", "--config", yml); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	o := s.opts
	if s.calls != 1 || o.Server != "relay.example.com:7000" || o.Token != "filetok" || len(o.Tunnels) != 3 || o.ServerName != "relay.example.com" {
		t.Fatalf("calls %d server %q token from file %v tunnels %d", s.calls, o.Server, o.Token == "filetok", len(o.Tunnels))
	}
	if f, debug := logShape(t, o.Logger); f != "text" || debug {
		t.Fatalf("logger = %s debug %v", f, debug)
	}
	if len(h.runs) != 0 {
		t.Fatal("connect went through the replaceable run of the new commands")
	}
}

func TestConnect_ThroughRoot_SingleTunnel(t *testing.T) {
	h := newHarness(t)
	h.signIn() // plays no part: connect asks for --server and --token
	s := captureStart(t)
	if code := h.exec("connect", "--server", "relay.example.com:7000", "--token", "tok", "--insecure", "--server-name", "sni.example"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if o := s.opts; o.Server != "relay.example.com:7000" || o.Token != "tok" || !o.Insecure || o.ServerName != "sni.example" {
		t.Fatalf("server %q insecure %v server name %q", o.Server, o.Insecure, o.ServerName)
	}
	if code := h.exec("connect"); code != 1 || !strings.HasPrefix(h.stderr.String(), "error: invalid client config: ") {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}

// Errors of `connect` keep the "error:" prefix and exit code 1, usage errors included.
func TestConnect_ThroughRoot_ErrorsAsBefore(t *testing.T) {
	yml := writeFile(t, "burrow.yaml", twoServices)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"connect", "--bogus"}, "error: unknown flag: --bogus\n"},
		{[]string{"connect", "--config", yml, "--name", "x"}, "error: --config cannot be combined with --name\n"},
		{[]string{"connect", "--server", "h:7000", "--token", "t", "--type", "xyz"}, "error: unknown tunnel type \"xyz\": must be tcp or http\n"},
	}
	for _, tc := range cases {
		h := newHarness(t)
		s := captureStart(t)
		if code := h.exec(tc.args...); code != 1 || h.stderr.String() != tc.want || s.calls != 0 {
			t.Fatalf("%v: exit %d, stderr %q", tc.args, code, h.stderr.String())
		}
	}
	h := newHarness(t)
	s := captureStart(t)
	s.ret = context.Canceled
	if code := h.exec("connect", "--config", yml); code != 1 || h.stderr.String() != "error: context canceled\n" {
		t.Fatalf("interrupted: exit %d, stderr %q", code, h.stderr.String())
	}
}

// `connect --config` needs server and token in the file, as it always has:
// neither the environment nor a stored sign-in fills them in.
func TestConnect_ConfigFile_IncompleteFailsAsBefore(t *testing.T) {
	svc := "services:\n  - { name: app, local: 127.0.0.1:3000 }\n"
	cases := []struct{ name, content, want string }{
		{"no server", "token: filetok\n" + svc, "loadfileconfig: server is required"},
		{"no token", "server: relay.example.com:7000\n" + svc, "loadfileconfig: exactly one of token or token_file is required"},
		{"neither", svc, "loadfileconfig: server is required"},
		{"no server and no services", "token: filetok\n", "loadfileconfig: server is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			h.env["BURROW_SERVER"], h.env["BURROW_TOKEN"] = "env.example.com:7000", "envtok"
			t.Setenv("BURROW_SERVER", "env.example.com:7000")
			t.Setenv("BURROW_TOKEN", "envtok")
			s := captureStart(t)
			yml := writeFile(t, "burrow.yaml", tc.content)
			if code := h.exec("connect", "--config", yml); code != 1 || h.stderr.String() != "error: "+tc.want+"\n" || s.calls != 0 {
				t.Fatalf("exit %d, stderr %q, calls %d", code, h.stderr.String(), s.calls)
			}
			if err := runConnect(t, "--config", yml); err == nil || err.Error() != tc.want {
				t.Fatalf("standalone: error = %v", err)
			}
		})
	}
}

// Every error of connect has the prefix and exit code 1, a bad --log included.
func TestConnect_ThroughRoot_BadLogFlag(t *testing.T) {
	h := newHarness(t)
	s := captureStart(t)
	code := h.exec("connect", "--server", "h:7000", "--token", "t", "--log", "xml")
	if code != 1 || h.stderr.String() != "error: --log must be text or json\n" || s.calls != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}

// --log is a flag of the root; given to connect it chooses the format.
func TestConnect_ThroughRoot_LogFlag(t *testing.T) {
	h := newHarness(t)
	s := captureStart(t)
	if code := h.exec("connect", "--server", "h:7000", "--token", "t", "--log", "json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if f, _ := logShape(t, s.opts.Logger); f != "json" {
		t.Fatalf("format = %s", f)
	}
	if code := h.exec("connect", "--config", writeFile(t, "burrow.yaml", twoServices), "--log", "json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if f, _ := logShape(t, s.opts.Logger); f != "json" {
		t.Fatalf("format = %s", f)
	}
}
