package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

// testToken stands in for a client token. Tests check that it never shows up
// in output and never print it themselves.
const testToken = "bur_0123456789abcdefWXYZ"

type runCall struct {
	creds   client.Credentials
	tunnels []client.TunnelSpec
	g       globalFlags
}

// harness runs the root command in-process without touching the environment,
// the real user config or the network.
type harness struct {
	t           *testing.T
	stdout      bytes.Buffer
	stderr      bytes.Buffer
	stdin       string
	stdinR      io.Reader // used instead of stdin when set
	secretErr   error
	env         map[string]string
	hostname    string
	hostErr     error
	terminal    bool
	viewTerm    bool   // stdout is a terminal that can show the status view
	secret      string // what a person types at the hidden token prompt
	secretCalls int
	typed       string // keys typed at the hidden prompt; they go through the real reader
	cfgPath     string // where the user config lives when --config is not given
	runs        []runCall
	runErr      error
	// discover answers a discovery request; nil stands for a relay of this
	// client's version whose control endpoint is <relay host>:7000.
	discover func(relay string, g globalFlags) (client.Discovery, error)
	// realDiscover sends discovery through the real code, to a test server.
	realDiscover bool
	discovered   []string // the relay addresses discovery was asked at
	onSecret     func()   // called when the hidden token prompt opens

	// realHTTP lets `login` reach a test server's sign-in endpoints. Without
	// it a request to them fails the test.
	realHTTP   bool
	stdoutTerm bool     // stdout is a terminal
	opened     []string // what the browser was asked to open
	openErr    error    // what opening the browser returns
	// sleep replaces the wait between two polls; nil waits for a five
	// hundredth of the time on a real timer.
	sleep func(ctx context.Context, d time.Duration) error
	mu    sync.Mutex
	slept []time.Duration // the waits the sign-in asked for
}

// sleeps returns the waits the sign-in asked for so far.
func (h *harness) sleeps() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.slept...)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cleanEnv(t)
	return &harness{
		t: t, env: map[string]string{}, hostname: "kohns-laptop",
		cfgPath: filepath.Join(t.TempDir(), "burrow", "config.yaml"),
	}
}

func (h *harness) deps() deps {
	var stdin io.Reader = strings.NewReader(h.stdin)
	if h.stdinR != nil {
		stdin = h.stdinR
	}
	return deps{
		stdout:   &h.stdout,
		stderr:   &h.stderr,
		stdin:    stdin,
		hostname: func() (string, error) { return h.hostname, h.hostErr },
		userConfigPath: func(override string) (string, error) {
			if override != "" {
				return override, nil
			}
			return h.cfgPath, nil
		},
		getenv: func(k string) string { return h.env[k] },
		run: func(_ context.Context, creds client.Credentials, tunnels []client.TunnelSpec, g globalFlags) error {
			h.runs = append(h.runs, runCall{creds, tunnels, g})
			return h.runErr
		},
		isTerminal:   func() bool { return h.terminal },
		viewTerminal: func() bool { return h.viewTerm },
		discover: func(ctx context.Context, relay string, g globalFlags) (client.Discovery, error) {
			h.discovered = append(h.discovered, relay)
			switch {
			case h.realDiscover:
				return discoverRelay(ctx, relay, g)
			case h.discover != nil:
				return h.discover(relay, g)
			}
			u, err := url.Parse(relay)
			if err != nil {
				return client.Discovery{}, err
			}
			return client.Discovery{Control: net.JoinHostPort(u.Hostname(), defaultControlPort), Version: version.Version, ProtocolVersion: 1}, nil
		},
		relayHTTP: func(g globalFlags) (*http.Client, error) {
			if h.realHTTP {
				return signInHTTPClient(g)
			}
			return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				h.t.Error("the command sent a sign-in request; this test has no relay for it")
				return nil, errors.New("no relay in this test")
			})}, nil
		},
		openBrowser: func(target string) error {
			h.opened = append(h.opened, target)
			return h.openErr
		},
		stdoutTerminal: func() bool { return h.stdoutTerm },
		sleep: func(ctx context.Context, d time.Duration) error {
			h.mu.Lock()
			h.slept = append(h.slept, d)
			h.mu.Unlock()
			if h.sleep != nil {
				return h.sleep(ctx, d)
			}
			// A timer that blocks like the real one, only shorter.
			t := time.NewTimer(d / 500)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
		readSecret: func() (string, error) {
			h.secretCalls++
			if h.onSecret != nil {
				h.onSecret()
			}
			if h.typed != "" {
				// the real reader, on input that stays open like a terminal
				return readHiddenLine(keys(h.t, h.typed))
			}
			return h.secret, h.secretErr
		},
	}
}

// exec runs one command line and returns the exit code main would use. What
// main would print for an error is in h.stderr.
func (h *harness) exec(args ...string) int {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	root := newRoot(h.deps())
	root.SetArgs(args)
	return report(&h.stderr, root.Execute())
}

// signIn stores a sign-in the way `burrow login` does.
func (h *harness) signIn() client.UserConfig {
	h.t.Helper()
	c := client.UserConfig{
		Relay: "https://burrow.example.com", Control: "burrow.example.com:7000",
		Token: testToken, TokenName: "kohns-laptop",
	}
	if err := client.SaveUserConfig(h.cfgPath, c); err != nil {
		h.t.Fatal(err)
	}
	return c
}

// noToken fails the test when the token was printed, without repeating it.
func (h *harness) noToken(tokens ...string) {
	h.t.Helper()
	for _, tok := range append(tokens, testToken) {
		if strings.Contains(h.stdout.String(), tok) || strings.Contains(h.stderr.String(), tok) {
			h.t.Fatal("the token was printed")
		}
	}
}

// oneRun returns the single call a command made to the client.
func (h *harness) oneRun() runCall {
	h.t.Helper()
	if len(h.runs) != 1 {
		h.t.Fatalf("the client was started %d times, want once; stderr: %s", len(h.runs), h.stderr.String())
	}
	return h.runs[0]
}

func TestRunClient_BuildsTheOptions(t *testing.T) {
	cleanEnv(t)
	ca := writeCA(t)
	creds := client.Credentials{Control: "relay.example.com:7000", Token: testToken, Source: client.SourceUserConfig}
	tunnels := []client.TunnelSpec{{Name: "a", Type: "http", LocalAddr: "127.0.0.1:3000"}}

	t.Run("defaults", func(t *testing.T) {
		s := captureStart(t)
		if err := runClient(context.Background(), creds, tunnels, globalFlags{logLevel: "info", logFormat: "text"}); err != nil {
			t.Fatal(err)
		}
		o := s.opts
		if s.calls != 1 || s.ctxErr != nil || o.Server != "relay.example.com:7000" || o.Token != testToken ||
			o.Insecure || o.RootCAs != nil || o.ServerName != "relay.example.com" || len(o.Tunnels) != 1 || o.Tunnels[0] != tunnels[0] {
			t.Fatalf("calls %d server %q insecure %v pool %v server name %q tunnels %+v",
				s.calls, o.Server, o.Insecure, o.RootCAs != nil, o.ServerName, o.Tunnels)
		}
		if f, debug := logShape(t, o.Logger); f != "text" || debug {
			t.Fatalf("logger = %s debug %v", f, debug)
		}
	})
	t.Run("tls settings and log format", func(t *testing.T) {
		s := captureStart(t)
		g := globalFlags{logLevel: "debug", logFormat: "json", cacert: ca, serverName: "sni.example", insecure: true}
		if err := runClient(context.Background(), creds, tunnels, g); err != nil {
			t.Fatal(err)
		}
		o := s.opts
		if !o.Insecure || o.RootCAs == nil || o.ServerName != "sni.example" {
			t.Fatalf("insecure %v pool %v server name %q", o.Insecure, o.RootCAs != nil, o.ServerName)
		}
		if f, debug := logShape(t, o.Logger); f != "json" || !debug {
			t.Fatalf("logger = %s debug %v", f, debug)
		}
	})
	t.Run("a control endpoint without a port leaves the server name empty", func(t *testing.T) {
		s := captureStart(t)
		c := creds
		c.Control = "relay.example.com"
		if err := runClient(context.Background(), c, tunnels, globalFlags{}); err != nil {
			t.Fatal(err)
		}
		if s.opts.ServerName != "" {
			t.Fatalf("server name = %q", s.opts.ServerName)
		}
	})
	t.Run("cacert without certificates", func(t *testing.T) {
		s := captureStart(t)
		p := writeFile(t, "x.pem", "nothing")
		err := runClient(context.Background(), creds, tunnels, globalFlags{cacert: p})
		if err == nil || err.Error() != "cacert "+p+": no certificates" || s.calls != 0 {
			t.Fatalf("error = %v, calls = %d", err, s.calls)
		}
	})
	t.Run("the client's error is returned as it is", func(t *testing.T) {
		s := captureStart(t)
		s.ret = io.ErrUnexpectedEOF
		if err := runClient(context.Background(), creds, tunnels, globalFlags{}); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestResolveCredentials_TokenFileVariable(t *testing.T) {
	h := newHarness(t)
	h.env["BURROW_SERVER"] = "env.example.com:7000"
	h.env["BURROW_TOKEN"] = "bur_from_the_variable"
	h.env["BURROW_TOKEN_FILE"] = writeFile(t, "tok", testToken+"\n")
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	// As in the relay's configuration, the _FILE form wins.
	if r := h.oneRun(); r.creds.Token != testToken || r.creds.Source != client.SourceEnvironment {
		t.Fatalf("token from the file: %v, source %q", r.creds.Token == testToken, r.creds.Source)
	}

	h.runs = nil
	h.env["BURROW_TOKEN_FILE"] = filepath.Join(t.TempDir(), "missing")
	if code := h.exec("http", "3000"); code != 1 || len(h.runs) != 0 {
		t.Fatalf("exit %d with an unreadable BURROW_TOKEN_FILE, runs %d", code, len(h.runs))
	}
	if !strings.Contains(h.stderr.String(), "BURROW_TOKEN_FILE") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
	h.noToken("bur_from_the_variable")
}

func TestResolveCredentials_BrokenUserConfig(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfgPath, []byte("token: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.exec("http", "3000"); code != 1 || len(h.runs) != 0 {
		t.Fatalf("exit %d, runs %d", code, len(h.runs))
	}
	if !strings.Contains(h.stderr.String(), "burrow login") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
	// A broken file does not get in the way when it is not needed.
	h.env["BURROW_SERVER"], h.env["BURROW_TOKEN"] = "env.example.com:7000", testToken
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}
