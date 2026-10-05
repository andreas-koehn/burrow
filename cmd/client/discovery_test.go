package main

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

// asVersion makes the client report v as its own version for the test.
func asVersion(t *testing.T, v string) {
	t.Helper()
	prev := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = prev })
}

// answers makes the harness's relay answer discovery with d and err.
func (h *harness) answers(d client.Discovery, err error) {
	h.discover = func(string, globalFlags) (client.Discovery, error) { return d, err }
}

// relayServer is an HTTPS server standing in for a relay's dashboard origin.
// It records what it was asked, so that a test can see nothing secret arrived.
type relayServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string // method, URL, headers and body of every request
}

func newRelayServer(t *testing.T, h http.HandlerFunc) *relayServer {
	t.Helper()
	rs := &relayServer{}
	rs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var sb strings.Builder
		sb.WriteString(r.Method + " " + r.URL.String() + "\n")
		_ = r.Header.Write(&sb)
		sb.Write(body)
		rs.mu.Lock()
		rs.seen = append(rs.seen, sb.String())
		rs.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *relayServer) requests() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.seen...)
}

// caFile writes the server's certificate where --cacert can read it.
func (rs *relayServer) caFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rs.Certificate().Raw})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// hostOf returns the host of the server's address.
func (rs *relayServer) host(t *testing.T) string {
	t.Helper()
	h, _, err := net.SplitHostPort(strings.TrimPrefix(rs.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func discoveryHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != client.DiscoveryPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func TestLogin_StoresTheDiscoveredControl(t *testing.T) {
	h := newHarness(t)
	h.answers(client.Discovery{Control: "burrow.example.com:7443", Version: version.Version}, nil)
	if code := h.exec("login", "burrow.example.com", "--token", testToken); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := h.stored(h.cfgPath); got.Control != "burrow.example.com:7443" || got.Relay != "https://burrow.example.com" {
		t.Fatalf("stored control %q, relay %q", got.Control, got.Relay)
	}
	if !strings.Contains(h.stdout.String(), "control endpoint burrow.example.com:7443") {
		t.Fatalf("stdout does not show the control endpoint: %q", h.stdout.String())
	}
	if len(h.discovered) != 1 || h.discovered[0] != "https://burrow.example.com" {
		t.Fatalf("discovery was asked at %v", h.discovered)
	}
	// Same host, another port: nothing to point out.
	if h.stderr.Len() != 0 {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
	h.noToken()
}

// A control endpoint on another host than the relay is used, but not silently:
// the user is told before the token is asked for, the endpoint is stored, and
// from then on the token goes there and nowhere else (ruling 3).
func TestLogin_ControlOnAnotherHost(t *testing.T) {
	h := newHarness(t)
	h.terminal, h.secret = true, testToken
	h.answers(client.Discovery{Control: "ctl.example.net:7000", Version: version.Version}, nil)
	var noteAtPrompt bool
	h.onSecret = func() { noteAtPrompt = strings.Contains(h.stderr.String(), "ctl.example.net:7000") }
	if code := h.exec("login", "burrow.example.com", "--token", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !noteAtPrompt {
		t.Fatal("the token was asked for before the other host was named")
	}
	errOut := h.stderr.String()
	if !strings.Contains(errOut, "another host") || !strings.Contains(errOut, "ctl.example.net:7000") || !strings.Contains(errOut, "burrow.example.com") {
		t.Fatalf("stderr does not say where the token will go: %q", errOut)
	}
	if !strings.Contains(h.stdout.String(), "control endpoint ctl.example.net:7000") {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
	if got := h.stored(h.cfgPath).Control; got != "ctl.example.net:7000" {
		t.Fatalf("stored control %q", got)
	}
	h.noToken()

	// Later commands connect there, without asking discovery again.
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("http: exit %d: %s", code, h.stderr.String())
	}
	if c := h.oneRun().creds; c.Control != "ctl.example.net:7000" || c.Token != testToken {
		t.Fatalf("connected to %q", c.Control)
	}
	if len(h.discovered) != 1 {
		t.Fatalf("discovery was asked %d times, want once (by login)", len(h.discovered))
	}
	// The stored token is not sent to any other endpoint, the relay's own
	// host on the default port included.
	h.runs = nil
	h.env["BURROW_SERVER"] = "burrow.example.com:7000"
	if code := h.exec("http", "3000"); code != exitNotSignedIn || len(h.runs) != 0 {
		t.Fatalf("exit %d with %d connections, want 3 and none", code, len(h.runs))
	}
}

func TestLogin_RelayWithoutDiscovery(t *testing.T) {
	for name, derr := range map[string]error{
		"404":                    client.ErrNoDiscovery,
		"not a discovery answer": client.ErrNotARelay,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.answers(client.Discovery{}, derr)
			if code := h.exec("login", "burrow.example.com", "--token", testToken); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			if got := h.stored(h.cfgPath).Control; got != "burrow.example.com:7000" {
				t.Fatalf("stored control %q", got)
			}
			errOut := h.stderr.String()
			if strings.Count(errOut, "\n") != 1 || !strings.Contains(errOut, "using burrow.example.com:7000") || !strings.Contains(errOut, "--control") {
				t.Fatalf("want one line saying what is used, got %q", errOut)
			}
			// And the client then connects on the default control port.
			if code := h.exec("http", "3000"); code != 0 {
				t.Fatalf("http: exit %d: %s", code, h.stderr.String())
			}
			if c := h.oneRun().creds.Control; c != "burrow.example.com:7000" {
				t.Fatalf("connected to %q", c)
			}
		})
	}
}

func TestLogin_ControlFlagOverridesDiscovery(t *testing.T) {
	for name, tc := range map[string]struct {
		d   client.Discovery
		err error
	}{
		"over a discovered endpoint": {client.Discovery{Control: "burrow.example.com:7443", Version: version.Version}, nil},
		"over the fallback":          {client.Discovery{}, client.ErrNoDiscovery},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.answers(tc.d, tc.err)
			if code := h.exec("login", "burrow.example.com", "--token", testToken, "--control", "ctl.example.com:7001"); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			if got := h.stored(h.cfgPath).Control; got != "ctl.example.com:7001" {
				t.Fatalf("stored control %q", got)
			}
			if h.stderr.Len() != 0 {
				t.Fatalf("stderr = %q", h.stderr.String())
			}
		})
	}
}

func TestLogin_VersionNotes(t *testing.T) {
	cases := []struct {
		name, me, relay, want string
	}{
		{"relay newer", "v0.6.0", "v0.7.0", "The relay runs v0.7.0; this client is v0.6.0. Run: burrow update\n"},
		{"same", "v0.6.0", "0.6.0", ""},
		{"client newer", "v0.7.0", "v0.6.0", "The relay runs v0.6.0; this client is v0.7.0, which is newer.\n"},
		{"development build", "dev", "v0.7.0", ""},
		{"relay says nothing", "v0.6.0", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asVersion(t, tc.me)
			h := newHarness(t)
			h.answers(client.Discovery{Control: "burrow.example.com:7000", Version: tc.relay}, nil)
			if code := h.exec("login", "burrow.example.com", "--token", testToken); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			if h.stderr.String() != tc.want {
				t.Fatalf("stderr = %q, want %q", h.stderr.String(), tc.want)
			}
		})
	}
}

func TestLogin_ClientTooOld(t *testing.T) {
	asVersion(t, "v0.5.9")
	h := newHarness(t)
	h.terminal, h.secret = true, testToken
	h.answers(client.Discovery{Control: "burrow.example.com:7000", Version: "v0.6.2", MinClientVersion: "0.6.0"}, nil)
	code := h.exec("login", "burrow.example.com", "--token", "-")
	if code != exitClientTooOld || h.stderr.String() != "This relay needs burrow 0.6.0 or newer. Run: burrow update\n" {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
	if h.hasConfig() || h.secretCalls != 0 {
		t.Fatalf("stored: %v, token asked %d times; want nothing stored and no question", h.hasConfig(), h.secretCalls)
	}
	// At the minimum, and with a build that has no version, it goes through.
	for _, me := range []string{"v0.6.0", "dev"} {
		asVersion(t, me)
		h := newHarness(t)
		h.answers(client.Discovery{Control: "burrow.example.com:7000", Version: "v0.6.0", MinClientVersion: "0.6.0"}, nil)
		if code := h.exec("login", "burrow.example.com", "--token", testToken); code != 0 {
			t.Fatalf("%s: exit %d: %s", me, code, h.stderr.String())
		}
	}
}

func TestLogin_RelayUnreachable(t *testing.T) {
	h := newHarness(t)
	h.answers(client.Discovery{}, &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	code := h.exec("login", "burrow.example.com", "--token", testToken)
	if code != exitUnreachable || !strings.Contains(h.stderr.String(), "Cannot reach https://burrow.example.com") {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
	if h.hasConfig() {
		t.Fatal("a sign-in was stored for a relay that did not answer")
	}
	h.noToken()

	h = newHarness(t)
	h.answers(client.Discovery{}, &client.DiscoveryStatusError{Status: 502})
	if code := h.exec("login", "burrow.example.com", "--token", testToken); code != exitUnreachable || h.hasConfig() ||
		!strings.Contains(h.stderr.String(), "502") {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
}

// Against real HTTPS servers, through the real discovery code.

func TestLogin_Certificate(t *testing.T) {
	relay := newRelayServer(t, discoveryHandler(`{"control":"`+"127.0.0.1"+`:7443","version":"`+version.Version+`","min_client_version":"","protocol_version":1}`))

	t.Run("not trusted", func(t *testing.T) {
		h := newHarness(t)
		h.realDiscover = true
		code := h.exec("login", relay.URL, "--token", testToken)
		errOut := h.stderr.String()
		if code != exitUnreachable || !strings.Contains(errOut, "--cacert") || !strings.Contains(errOut, "Acme Co") {
			t.Fatalf("exit %d, stderr %q; want 5, the issuer and --cacert", code, errOut)
		}
		if h.hasConfig() {
			t.Fatal("a sign-in was stored")
		}
		h.noToken()
	})
	t.Run("--cacert", func(t *testing.T) {
		h := newHarness(t)
		h.realDiscover = true
		if code := h.exec("login", relay.URL, "--token", testToken, "--cacert", relay.caFile(t)); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if got := h.stored(h.cfgPath); got.Control != "127.0.0.1:7443" || got.Relay != relay.URL {
			t.Fatalf("stored control %q, relay %q", got.Control, got.Relay)
		}
		if h.stderr.Len() != 0 {
			t.Fatalf("stderr = %q", h.stderr.String())
		}
	})
	t.Run("--insecure", func(t *testing.T) {
		h := newHarness(t)
		h.realDiscover = true
		if code := h.exec("login", relay.URL, "--token", testToken, "--insecure"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if got := h.stored(h.cfgPath).Control; got != "127.0.0.1:7443" {
			t.Fatalf("stored control %q", got)
		}
		if !strings.Contains(h.stderr.String(), "--insecure") || !strings.Contains(h.stderr.String(), "not checked") {
			t.Fatalf("no warning: %q", h.stderr.String())
		}
	})
	// Whatever happened above, the relay's web address never saw the token.
	for _, req := range relay.requests() {
		if strings.Contains(req, testToken) || strings.Contains(req, "Authorization") || strings.Contains(req, "Cookie") {
			t.Fatal("a discovery request carried a credential")
		}
	}
	if len(relay.requests()) == 0 {
		t.Fatal("the relay was never asked")
	}
}

// A new client against a relay from before discovery: its API answers 404, or
// a catch-all in front of it answers every path with the dashboard's HTML.
func TestLogin_OlderRelay(t *testing.T) {
	handlers := map[string]http.HandlerFunc{
		"404 from the API": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		},
		"html from a catch-all": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<!doctype html><html><head><title>Burrow</title></head><body><div id=\"root\"></div></body></html>")
		},
	}
	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			relay := newRelayServer(t, handler)
			want := net.JoinHostPort(relay.host(t), "7000")
			h := newHarness(t)
			h.realDiscover = true
			if code := h.exec("login", relay.URL, "--token", testToken, "--cacert", relay.caFile(t)); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			if got := h.stored(h.cfgPath).Control; got != want {
				t.Fatalf("stored control %q, want %q", got, want)
			}
			if !strings.Contains(h.stderr.String(), "using "+want) {
				t.Fatalf("stderr = %q", h.stderr.String())
			}
			if code := h.exec("http", "3000"); code != 0 {
				t.Fatalf("http: exit %d: %s", code, h.stderr.String())
			}
			if c := h.oneRun().creds; c.Control != want || c.Token != testToken {
				t.Fatalf("connected to %q", c.Control)
			}
		})
	}
}

// A discovery answer that points somewhere else by redirect is not followed,
// and nothing is stored.
func TestLogin_RedirectIsNotFollowed(t *testing.T) {
	other := newRelayServer(t, discoveryHandler(`{"control":"evil.example.com:7000","version":"0.6.0","min_client_version":"","protocol_version":1}`))
	relay := newRelayServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+client.DiscoveryPath, http.StatusFound)
	})
	h := newHarness(t)
	h.realDiscover = true
	code := h.exec("login", relay.URL, "--token", testToken, "--insecure")
	if code != exitUnreachable || h.hasConfig() {
		t.Fatalf("exit %d, stored %v: %s", code, h.hasConfig(), h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "redirect") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
	if n := len(other.requests()); n != 0 {
		t.Fatalf("the redirect target was asked %d times", n)
	}
}

func TestStatus_RelayVersion(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.answers(client.Discovery{Control: "burrow.example.com:7000", Version: "0.6.0"}, nil)
	if code := h.exec("status"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "Relay:    https://burrow.example.com (version 0.6.0)\n") {
		t.Fatalf("stdout:\n%s", h.stdout.String())
	}
	if len(h.discovered) != 1 || h.discovered[0] != "https://burrow.example.com" {
		t.Fatalf("discovery was asked at %v", h.discovered)
	}
	h.noToken()

	// Not reachable, an older relay, a relay that names no version: unknown,
	// and never a failure.
	for _, tc := range []struct {
		d   client.Discovery
		err error
	}{
		{client.Discovery{}, &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
		{client.Discovery{}, client.ErrNoDiscovery},
		{client.Discovery{}, context.DeadlineExceeded},
		{client.Discovery{Control: "burrow.example.com:7000"}, nil},
	} {
		h.answers(tc.d, tc.err)
		if code := h.exec("status"); code != 0 || h.stderr.Len() != 0 {
			t.Fatalf("%v: exit %d, stderr %q", tc.err, code, h.stderr.String())
		}
		if !strings.Contains(h.stdout.String(), "Relay:    https://burrow.example.com (version unknown)\n") {
			t.Fatalf("%v: stdout:\n%s", tc.err, h.stdout.String())
		}
	}
}

// Without a relay address there is nowhere to ask, and no host is guessed.
func TestStatus_NoRelayNoDiscovery(t *testing.T) {
	h := newHarness(t)
	h.env["BURROW_SERVER"], h.env["BURROW_TOKEN"] = "env.example.com:7000", testToken
	if code := h.exec("status"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if len(h.discovered) != 0 || !strings.Contains(h.stdout.String(), "Relay:    not known\n") {
		t.Fatalf("asked %v; stdout:\n%s", h.discovered, h.stdout.String())
	}
}

// status against a relay that accepts and never answers ends by itself.
func TestStatus_SlowRelayDoesNotHang(t *testing.T) {
	release := make(chan struct{})
	relay := newRelayServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	prev := statusDiscoveryTimeout
	statusDiscoveryTimeout = 200_000_000 // 200 ms
	defer func() { statusDiscoveryTimeout = prev }()

	h := newHarness(t)
	h.realDiscover = true
	c := client.UserConfig{Relay: relay.URL, Control: "127.0.0.1:7000", Token: testToken, TokenName: "kohns-laptop"}
	if err := client.SaveUserConfig(h.cfgPath, c); err != nil {
		t.Fatal(err)
	}
	if code := h.execWithin("status", "--insecure"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "(version unknown)") {
		t.Fatalf("stdout:\n%s", h.stdout.String())
	}
}

// The commands an older client has do not ask discovery: `connect` and the
// commands that use a stored sign-in connect to the control endpoint only.
func TestDiscovery_OnlyLoginStatusAndDoctorAsk(t *testing.T) {
	s := captureStart(t)
	h := newHarness(t)
	h.signIn()
	for _, args := range [][]string{
		{"connect", "--server", "burrow.example.com:7000", "--token", testToken, "--local", "127.0.0.1:3000", "--remote", "9000"},
		{"http", "3000"},
		{"tcp", "5432"},
		{"logout"},
	} {
		if code := h.exec(args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, h.stderr.String())
		}
	}
	if s.calls != 1 || s.opts.Server != "burrow.example.com:7000" {
		t.Fatalf("connect started the client %d times with server %q", s.calls, s.opts.Server)
	}
	if len(h.discovered) != 0 {
		t.Fatalf("discovery was asked by a command that only connects: %v", h.discovered)
	}
}
