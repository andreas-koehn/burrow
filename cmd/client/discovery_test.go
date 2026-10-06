package main

import (
	"bytes"
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
		r.Body = io.NopCloser(bytes.NewReader(body)) // the handler reads it again
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

// A relay from before discovery answers 404: the default control port is used.
func TestLogin_RelayWithoutDiscovery(t *testing.T) {
	h := newHarness(t)
	h.answers(client.Discovery{}, client.ErrNoDiscovery)
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
}

// Some web page at the address (a mistyped host, a proxy's page) is not a
// relay: nothing is guessed and nothing is stored.
func TestLogin_NotARelay(t *testing.T) {
	h := newHarness(t)
	h.terminal, h.secret = true, testToken
	h.answers(client.Discovery{}, client.ErrNotARelay)
	code := h.exec("login", "burrow.example.com", "--token", "-")
	errOut := h.stderr.String()
	if code != exitUnreachable || !strings.Contains(errOut, "does not look like a Burrow relay") || !strings.Contains(errOut, "--control <host:port>") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if h.hasConfig() || h.secretCalls != 0 || strings.Contains(h.stdout.String(), "Signed in") {
		t.Fatalf("stored %v, token asked %d times, stdout %q", h.hasConfig(), h.secretCalls, h.stdout.String())
	}
}

// The same with the token on a pipe: the pipe is not read, so the token stays
// with whoever writes it.
func TestLogin_NotARelay_Piped(t *testing.T) {
	h := newHarness(t)
	h.answers(client.Discovery{}, client.ErrNotARelay)
	pr, pw := io.Pipe()
	defer pr.Close()
	taken := make(chan struct{})
	// The writer blocks until somebody reads, as a pipe's writer does.
	go func() {
		if _, err := io.WriteString(pw, testToken+"\n"); err == nil {
			close(taken)
		}
	}()
	h.stdinR = pr
	code := h.execWithin("login", "burrow.example.com", "--token", "-")
	errOut := h.stderr.String()
	if code != exitUnreachable || !strings.Contains(errOut, "does not look like a Burrow relay") || !strings.Contains(errOut, "--control <host:port>") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	select {
	case <-taken:
		t.Fatal("the token was read from the pipe although nothing is stored")
	default:
	}
	if h.hasConfig() || h.secretCalls != 0 || strings.Contains(h.stdout.String(), "Signed in") {
		t.Fatalf("stored %v, token asked %d times, stdout %q", h.hasConfig(), h.secretCalls, h.stdout.String())
	}
	h.noToken()
}

// --control says where to connect, so the web address is asked only for the
// versions. When it gives no usable answer the sign-in is stored all the same,
// with one line saying that nothing was checked.
func TestLogin_ControlFlagOverridesDiscovery(t *testing.T) {
	for name, tc := range map[string]struct {
		d    client.Discovery
		err  error
		warn string // "" = nothing on stderr
	}{
		"over a discovered endpoint": {client.Discovery{Control: "burrow.example.com:7443", Version: version.Version}, nil, ""},
		"over the fallback":          {client.Discovery{}, client.ErrNoDiscovery, ""},
		"unreachable":                {client.Discovery{}, &net.OpError{Op: "dial", Err: errors.New("connection refused")}, "connection refused"},
		"no answer in time":          {client.Discovery{}, context.DeadlineExceeded, "no answer"},
		"403":                        {client.Discovery{}, &client.DiscoveryStatusError{Status: 403}, "status 403"},
		"502":                        {client.Discovery{}, &client.DiscoveryStatusError{Status: 502}, "status 502"},
		"302 from a proxy":           {client.Discovery{}, &client.DiscoveryStatusError{Status: 302, RedirectHost: "sso.example.com"}, "redirect to sso.example.com"},
		"not a discovery answer":     {client.Discovery{}, client.ErrNotARelay, "does not look like a Burrow relay"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.answers(tc.d, tc.err)
			if code := h.exec("login", "burrow.example.com", "--token", testToken, "--control", "ctl.example.com:7001"); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			if got := h.stored(h.cfgPath); got.Control != "ctl.example.com:7001" || got.Token != testToken {
				t.Fatalf("stored control %q", got.Control)
			}
			errOut := h.stderr.String()
			if tc.warn == "" {
				if errOut != "" {
					t.Fatalf("stderr = %q", errOut)
				}
				return
			}
			if strings.Count(errOut, "\n") != 1 || !strings.HasPrefix(errOut, "Warning: ") || !strings.Contains(errOut, tc.warn) ||
				!strings.Contains(errOut, "ctl.example.com:7001") {
				t.Fatalf("want one warning line with %q, got %q", tc.warn, errOut)
			}
			h.noToken()
		})
	}
}

// With --control too, a relay that did answer is believed about the minimum
// client version.
func TestLogin_ControlFlagKeepsTheMinimumVersion(t *testing.T) {
	asVersion(t, "v0.5.9")
	h := newHarness(t)
	h.answers(client.Discovery{Control: "burrow.example.com:7000", Version: "v0.6.2", MinClientVersion: "0.6.0"}, nil)
	code := h.exec("login", "burrow.example.com", "--token", testToken, "--control", "ctl.example.com:7001")
	if code != exitClientTooOld || h.hasConfig() {
		t.Fatalf("exit %d, stored %v: %s", code, h.hasConfig(), h.stderr.String())
	}
}

// Ctrl-C while the relay is asked: a plain line, nothing stored.
func TestLogin_InterruptedDuringDiscovery(t *testing.T) {
	for _, extra := range [][]string{nil, {"--control", "ctl.example.com:7001"}} {
		h := newHarness(t)
		h.answers(client.Discovery{}, context.Canceled)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		root := newRoot(h.deps())
		root.SetArgs(append([]string{"login", "burrow.example.com", "--token", testToken}, extra...))
		code := report(&h.stderr, root.ExecuteContext(ctx))
		if code != exitGeneral || h.stderr.String() != "Interrupted. Nothing was stored.\n" {
			t.Fatalf("%v: exit %d, stderr %q", extra, code, h.stderr.String())
		}
		if h.hasConfig() {
			t.Fatal("a sign-in was stored")
		}
	}
}

// --insecure is pointed out when a certificate went unchecked, which needs an
// answer; not when no connection was made.
func TestLogin_InsecureWarningNeedsAnAnswer(t *testing.T) {
	h := newHarness(t)
	h.answers(client.Discovery{}, &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	if code := h.exec("login", "burrow.example.com", "--token", testToken, "--insecure"); code != exitUnreachable {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), "not checked") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
	for _, derr := range []error{nil, client.ErrNoDiscovery, &client.DiscoveryStatusError{Status: 502}} {
		h := newHarness(t)
		if derr != nil {
			h.answers(client.Discovery{}, derr)
		}
		h.exec("login", "burrow.example.com", "--token", testToken, "--insecure")
		if !strings.Contains(h.stderr.String(), "certificate was not checked") {
			t.Fatalf("%v: stderr = %q", derr, h.stderr.String())
		}
	}
}

// noteBeforeRead is a stdin that stays open like a pipe someone still writes
// to. On the first read it records what stderr held: that is what the user saw
// before any byte of the token was taken.
type noteBeforeRead struct {
	r      io.Reader
	stderr func() string
	once   sync.Once
	seen   string
}

func (n *noteBeforeRead) Read(p []byte) (int, error) {
	n.once.Do(func() { n.seen = n.stderr() })
	return n.r.Read(p)
}

// The piped path of the note about a control endpoint on another host.
func TestLogin_ControlOnAnotherHost_Piped(t *testing.T) {
	h := newHarness(t)
	h.answers(client.Discovery{Control: "ctl.example.net:7000", Version: version.Version}, nil)
	pr, pw := io.Pipe()
	in := &noteBeforeRead{r: pr, stderr: func() string { return h.stderr.String() }}
	h.stdinR = in
	// The writer waits for the command to read, as a pipe's writer does.
	go func() {
		_, _ = io.WriteString(pw, testToken+"\n")
		_ = pw.Close()
	}()
	if code := h.execWithin("login", "burrow.example.com", "--token", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(in.seen, "another host") || !strings.Contains(in.seen, "ctl.example.net:7000") {
		t.Fatalf("stderr before the first read of the token: %q", in.seen)
	}
	if got := h.stored(h.cfgPath); got.Control != "ctl.example.net:7000" || got.Token != testToken {
		t.Fatalf("stored control %q", got.Control)
	}
	h.noToken()
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
	t.Run("not trusted, with --control", func(t *testing.T) {
		h := newHarness(t)
		h.realDiscover = true
		code := h.exec("login", relay.URL, "--token", testToken, "--control", "127.0.0.1:7001")
		if code != exitUnreachable || !strings.Contains(h.stderr.String(), "--cacert") || h.hasConfig() {
			t.Fatalf("exit %d, stored %v, stderr %q", code, h.hasConfig(), h.stderr.String())
		}
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

// A new client against a relay from before discovery, whose API answers 404:
// the sign-in is stored for the default control port and the client connects
// there.
func TestLogin_OlderRelay(t *testing.T) {
	relay := newRelayServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	})
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
}

// A catch-all that answers every path with a page (an SPA in front of an
// older relay, or another site altogether) is not taken for a relay. With
// --control the user has said where to connect, and that is stored.
func TestLogin_CatchAllPage(t *testing.T) {
	relay := newRelayServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><html><head><title>Burrow</title></head><body><div id=\"root\"></div></body></html>")
	})
	ca := relay.caFile(t)
	h := newHarness(t)
	h.realDiscover = true
	code := h.exec("login", relay.URL, "--token", testToken, "--cacert", ca)
	if code != exitUnreachable || h.hasConfig() || !strings.Contains(h.stderr.String(), "does not look like a Burrow relay") ||
		!strings.Contains(h.stderr.String(), "--control") {
		t.Fatalf("exit %d, stored %v, stderr %q", code, h.hasConfig(), h.stderr.String())
	}
	control := net.JoinHostPort(relay.host(t), "7000")
	if code := h.exec("login", relay.URL, "--token", testToken, "--cacert", ca, "--control", control); code != 0 {
		t.Fatalf("with --control: exit %d: %s", code, h.stderr.String())
	}
	if got := h.stored(h.cfgPath).Control; got != control || !strings.HasPrefix(h.stderr.String(), "Warning: ") {
		t.Fatalf("stored control %q, stderr %q", got, h.stderr.String())
	}
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("http: exit %d: %s", code, h.stderr.String())
	}
	if c := h.oneRun().creds; c.Control != control {
		t.Fatalf("connected to %q", c.Control)
	}
}

// An SSO proxy in front of the dashboard answers with a redirect; a closed
// port answers nothing. With --control the sign-in is stored, and the redirect
// is still not followed.
func TestLogin_ControlFlagWhenTheWebAddressIsNoHelp(t *testing.T) {
	other := newRelayServer(t, discoveryHandler(`{"control":"evil.example.com:7000","version":"0.6.0","min_client_version":"","protocol_version":1}`))
	proxy := newRelayServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/login", http.StatusFound)
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "https://" + l.Addr().String()
	l.Close()
	for name, addr := range map[string]string{"302": proxy.URL, "closed port": closed} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.realDiscover = true
			if code := h.exec("login", addr, "--token", testToken, "--insecure", "--control", "127.0.0.1:7001"); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			if got := h.stored(h.cfgPath); got.Control != "127.0.0.1:7001" || got.Relay != addr {
				t.Fatalf("stored control %q, relay %q", got.Control, got.Relay)
			}
			if !strings.Contains(h.stderr.String(), "Warning: ") {
				t.Fatalf("stderr = %q", h.stderr.String())
			}
			h.noToken()
		})
	}
	if n := len(other.requests()); n != 0 {
		t.Fatalf("the redirect target was asked %d times", n)
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
