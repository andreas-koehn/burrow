package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

// testDeviceCode stands in for a device code: a secret like the token. Tests
// check that it is in the body of the polls and nowhere else.
const testDeviceCode = "dc_test_000000000000000000000000"

// browserToken is the token the fake relay hands out.
const browserToken = "bur_test_0000"

// signInRelay is an HTTPS server that follows the relay's sign-in contract
// (internal/api/client_login_handlers.go) with scripted answers.
type signInRelay struct {
	*relayServer
	mu           sync.Mutex
	startStatus  int    // 0 answers 200
	verification string // "" names the relay's own /link
	interval     int
	tokenName    string
	polls        []int // statuses of the polls in order; the last one repeats
	nPoll        int
	polled       chan struct{} // gets one value per poll, when set
}

func newSignInRelay(t *testing.T, polls ...int) *signInRelay {
	t.Helper()
	s := &signInRelay{interval: 2, tokenName: "laptop", polls: polls}
	s.relayServer = newRelayServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == client.DeviceLoginStartPath:
			s.serveStart(w, r)
		case r.Method == http.MethodPost && r.URL.Path == client.DeviceLoginPollPath:
			s.servePoll(w)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		}
	})
	return s
}

func (s *signInRelay) serveStart(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	status, verification, interval := s.startStatus, s.verification, s.interval
	s.mu.Unlock()
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"x"}`)
		return
	}
	if verification == "" {
		verification = "https://" + r.Host + "/link?code=BRRW-7Q4K"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_code": testDeviceCode, "user_code": "BRRW-7Q4K",
		"verification_url": verification, "expires_in": 600, "interval": interval,
	})
}

func (s *signInRelay) servePoll(w http.ResponseWriter) {
	s.mu.Lock()
	status := http.StatusAccepted
	if len(s.polls) > 0 {
		status = s.polls[min(s.nPoll, len(s.polls)-1)]
	}
	s.nPoll++
	name, polled := s.tokenName, s.polled
	s.mu.Unlock()
	if polled != nil {
		polled <- struct{}{}
	}
	w.WriteHeader(status)
	switch status {
	case http.StatusOK:
		_ = json.NewEncoder(w).Encode(map[string]string{"token": browserToken, "token_name": name, "email": "admin@example.com"})
	case http.StatusAccepted:
		_, _ = io.WriteString(w, `{"status":"pending"}`)
	case http.StatusTooManyRequests:
		_, _ = io.WriteString(w, `{"error":"slow_down"}`)
	case http.StatusForbidden:
		_, _ = io.WriteString(w, `{"error":"access_denied"}`)
	case http.StatusGone:
		_, _ = io.WriteString(w, `{"error":"expired_token"}`)
	}
}

// asked returns the recorded requests to path.
func (s *signInRelay) asked(path string) []string {
	var out []string
	for _, r := range s.requests() {
		if strings.HasPrefix(r, "POST "+path+"\n") {
			out = append(out, r)
		}
	}
	return out
}

// hostPort is the relay as it is typed after `burrow login`.
func (s *signInRelay) hostPort() string { return strings.TrimPrefix(s.URL, "https://") }

// browserHarness is a harness whose `login` reaches the given relay.
func browserHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.realHTTP = true
	return h
}

// noSecrets fails the test when the token or the device code was printed.
func (h *harness) noSecrets() {
	h.t.Helper()
	h.noToken(browserToken)
	if strings.Contains(h.stdout.String(), testDeviceCode) || strings.Contains(h.stderr.String(), testDeviceCode) {
		h.t.Fatal("the device code was printed")
	}
}

func TestLoginBrowser_SignsIn(t *testing.T) {
	relay := newSignInRelay(t, http.StatusAccepted, http.StatusAccepted, http.StatusOK)
	h := browserHarness(t)
	if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t)); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	control := net.JoinHostPort(relay.host(t), "7000")
	want := "Open this page to sign this machine in:\n\n" +
		"  " + relay.URL + "/link?code=BRRW-7Q4K\n\n" +
		"Check that the page shows the code BRRW-7Q4K.\n" +
		"Waiting for approval…  signed in as admin@example.com (token \"laptop\")\n" +
		"Token laptop (…0000) for control endpoint " + control + ", stored in " + h.cfgPath + "\n"
	if got := h.stdout.String(); got != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", got, want)
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
	h.noSecrets()

	got := h.stored(h.cfgPath)
	if got.Token != browserToken {
		t.Fatal("the stored token is not the one the relay gave")
	}
	got.Token = ""
	if want := (client.UserConfig{Relay: relay.URL, Control: control, TokenName: "laptop"}); got != want {
		t.Fatalf("stored %v, want %v", got, want)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(h.cfgPath); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, err = %v", fi.Mode().Perm(), err)
		}
	}

	// What the relay was told about this machine: no token name without --name.
	starts := relay.asked(client.DeviceLoginStartPath)
	if len(starts) != 1 {
		t.Fatalf("%d start requests", len(starts))
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(starts[0][strings.LastIndex(starts[0], "{"):]), &meta); err != nil {
		t.Fatal(err)
	}
	wantMeta := map[string]string{"hostname": "kohns-laptop", "os": runtime.GOOS, "arch": runtime.GOARCH, "client_version": version.Version}
	if len(meta) != len(wantMeta) {
		t.Fatalf("start body %v, want %v", meta, wantMeta)
	}
	for k, v := range wantMeta {
		if meta[k] != v {
			t.Fatalf("start body %v, want %v", meta, wantMeta)
		}
	}

	// Three polls, each a little after the relay's interval, and the device
	// code in their bodies only: in no URL and in no header.
	polls := relay.asked(client.DeviceLoginPollPath)
	if len(polls) != 3 {
		t.Fatalf("%d polls, want 3", len(polls))
	}
	if s := h.sleeps(); len(s) != 3 || s[0] != 2500*time.Millisecond {
		t.Fatalf("waits %v", s)
	}
	for _, r := range relay.requests() {
		head, body := r[:strings.LastIndex(r, "\n")+1], r[strings.LastIndex(r, "\n")+1:]
		if strings.Contains(head, testDeviceCode) {
			t.Fatal("the device code is in a URL or a header")
		}
		if strings.HasPrefix(r, "POST "+client.DeviceLoginPollPath) != strings.Contains(body, testDeviceCode) {
			t.Fatal("the device code is not exactly in the bodies of the polls")
		}
	}
	// One connection carried the start and the three polls.
	if n := relay.conns.Load(); n != 1 {
		t.Fatalf("%d connections to the relay, want 1", n)
	}
	// And the sign-in works: the next command connects with it.
	if code := h.exec("http", "3000"); code != 0 {
		t.Fatalf("http: exit %d: %s", code, h.stderr.String())
	}
	if c := h.oneRun().creds; c.Control != control || c.Token != browserToken {
		t.Fatalf("connected to %q", c.Control)
	}
}

func TestLoginBrowser_OpensTheBrowserOnlyOnADesktop(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the environment of a Linux desktop")
	}
	desktop := map[string]string{"DISPLAY": ":0"}
	for name, tc := range map[string]struct {
		term    bool
		env     map[string]string
		args    []string
		openErr error
		want    int
	}{
		"desktop":                    {true, desktop, nil, nil, 1},
		"wayland":                    {true, map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, nil, nil, 1},
		"--no-browser":               {true, desktop, []string{"--no-browser"}, nil, 0},
		"no display":                 {true, nil, nil, nil, 0},
		"over ssh":                   {true, map[string]string{"DISPLAY": "localhost:10.0", "SSH_CONNECTION": "10.0.0.2 5 10.0.0.1 22"}, nil, nil, 0},
		"stdout is not a terminal":   {false, desktop, nil, nil, 0},
		"the browser does not start": {true, desktop, nil, errors.New("exec: \"xdg-open\": executable file not found in $PATH"), 1},
	} {
		t.Run(name, func(t *testing.T) {
			relay := newSignInRelay(t, http.StatusOK)
			h := browserHarness(t)
			h.stdoutTerm, h.openErr = tc.term, tc.openErr
			for k, v := range tc.env {
				h.env[k] = v
			}
			args := append([]string{"login", relay.URL, "--cacert", relay.caFile(t)}, tc.args...)
			// A browser that does not start is not an error: the address is shown.
			if code := h.execWithin(args...); code != 0 || h.stderr.Len() != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			page := relay.URL + "/link?code=BRRW-7Q4K"
			if !strings.Contains(h.stdout.String(), "  "+page+"\n") || !strings.Contains(h.stdout.String(), "the code BRRW-7Q4K.") {
				t.Fatalf("stdout = %q", h.stdout.String())
			}
			if len(h.opened) != tc.want || (tc.want == 1 && h.opened[0] != page) {
				t.Fatalf("the browser was asked to open %q, want %d call(s) with the page", h.opened, tc.want)
			}
			if h.stored(h.cfgPath).Token != browserToken {
				t.Fatal("no sign-in was stored")
			}
			h.noSecrets()
		})
	}
}

// The page that is shown and opened is on the relay the user named. A relay
// that names a page elsewhere does not get the browser sent there.
func TestLoginBrowser_PageOnAnotherHost(t *testing.T) {
	relay := newSignInRelay(t, http.StatusOK)
	relay.verification = "https://evil.example.com/link?code=BRRW-7Q4K"
	h := browserHarness(t)
	h.stdoutTerm = true
	h.env["DISPLAY"] = ":0"
	if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t)); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	own := relay.URL + "/link?code=BRRW-7Q4K"
	if !strings.Contains(h.stdout.String(), "  "+own+"\n") || strings.Contains(h.stdout.String()+h.stderr.String(), "evil.example.com") {
		t.Fatalf("stdout %q, stderr %q", h.stdout.String(), h.stderr.String())
	}
	if runtime.GOOS == "linux" && (len(h.opened) != 1 || h.opened[0] != own) {
		t.Fatalf("opened %q", h.opened)
	}
	if e := h.stderr.String(); strings.Count(e, "\n") != 1 || !strings.Contains(e, "not on "+relay.URL) || !strings.Contains(e, "the relay's own page") {
		t.Fatalf("want one line saying that the relay's own page is used, got %q", e)
	}
}

func TestLoginBrowser_Name(t *testing.T) {
	t.Run("--name is suggested to the approval page", func(t *testing.T) {
		relay := newSignInRelay(t, http.StatusOK)
		relay.tokenName = "work"
		h := browserHarness(t)
		if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t), "--name", "work"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		start := relay.asked(client.DeviceLoginStartPath)[0]
		if !strings.Contains(start, `"token_name":"work"`) || !strings.Contains(start, `"hostname":"kohns-laptop"`) {
			t.Fatalf("start request: %s", start)
		}
		if h.stored(h.cfgPath).TokenName != "work" || !strings.Contains(h.stdout.String(), `(token "work")`) {
			t.Fatalf("stdout = %q", h.stdout.String())
		}
	})
	// The name is the one the approver chose, which the relay reports.
	t.Run("the approver's name wins", func(t *testing.T) {
		relay := newSignInRelay(t, http.StatusOK)
		relay.tokenName = "renamed-in-the-dashboard"
		h := browserHarness(t)
		if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t), "--name", "work"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if got := h.stored(h.cfgPath).TokenName; got != "renamed-in-the-dashboard" {
			t.Fatalf("stored name %q", got)
		}
	})
	t.Run("a relay that reports no name", func(t *testing.T) {
		relay := newSignInRelay(t, http.StatusOK)
		relay.tokenName = ""
		h := browserHarness(t)
		if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t)); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if got := h.stored(h.cfgPath).TokenName; got != "kohns-laptop" {
			t.Fatalf("stored name %q, want the hostname", got)
		}
	})
}

func TestLoginBrowser_DeniedAndExpired(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		want   string
	}{
		"denied":  {http.StatusForbidden, "The sign-in was denied in the dashboard.\n"},
		"expired": {http.StatusGone, "The code expired. Run burrow login again.\n"},
	} {
		t.Run(name, func(t *testing.T) {
			relay := newSignInRelay(t, http.StatusAccepted, tc.status)
			h := browserHarness(t)
			code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t))
			if code != exitGeneral || h.stderr.String() != tc.want {
				t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
			}
			// The line that waited is ended before the message.
			if !strings.HasSuffix(h.stdout.String(), "Waiting for approval…  \n") || strings.Contains(h.stdout.String(), "signed in") {
				t.Fatalf("stdout = %q", h.stdout.String())
			}
			if h.hasConfig() {
				t.Fatal("a sign-in was stored")
			}
			h.noSecrets()
		})
	}
}

// A relay from before browser sign-in answers 404: one clear message with the
// way that works there, and no waiting.
func TestLoginBrowser_RelayWithoutSignIn(t *testing.T) {
	const want = "This relay does not support browser sign-in. Create a token in the dashboard (Clients → Tokens) and run: burrow login %s --token -\n" +
		"then paste the token and press Enter.\n"
	t.Run("it has discovery but no sign-in", func(t *testing.T) {
		relay := newSignInRelay(t)
		relay.startStatus = http.StatusNotFound
		h := browserHarness(t)
		code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t))
		if code != exitUsage || h.stderr.String() != strings.Replace(want, "%s", relay.hostPort(), 1) {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
		if h.stdout.Len() != 0 || h.hasConfig() || len(relay.asked(client.DeviceLoginPollPath)) != 0 || len(h.sleeps()) != 0 {
			t.Fatalf("stdout %q, stored %v, %d polls", h.stdout.String(), h.hasConfig(), len(relay.asked(client.DeviceLoginPollPath)))
		}
	})
	// Older still: no discovery either. It is not even asked to start one.
	t.Run("it has neither", func(t *testing.T) {
		h := newHarness(t) // a sign-in request fails the test
		h.answers(client.Discovery{}, client.ErrNoDiscovery)
		code := h.execWithin("login", "burrow.example.com")
		if code != exitUsage || h.stderr.String() != strings.Replace(want, "%s", "burrow.example.com", 1) {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
		if h.stdout.Len() != 0 || h.hasConfig() {
			t.Fatalf("stdout %q, stored %v", h.stdout.String(), h.hasConfig())
		}
	})
	// Through the real discovery code, against a server that knows nothing.
	t.Run("a relay that answers 404 to everything", func(t *testing.T) {
		relay := newRelayServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		})
		h := browserHarness(t)
		h.realDiscover = true
		code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t))
		if code != exitUsage || !strings.HasPrefix(h.stderr.String(), "This relay does not support browser sign-in.") ||
			strings.Count(h.stderr.String(), "\n") != 2 || h.hasConfig() {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
		// And the way the message names works against that relay.
		pr, pw := io.Pipe()
		go func() {
			_, _ = io.WriteString(pw, testToken+"\n")
			_ = pw.Close()
		}()
		h.stdinR = pr
		if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t), "--token", "-"); code != 0 {
			t.Fatalf("--token -: exit %d: %s", code, h.stderr.String())
		}
		if got := h.stored(h.cfgPath); got.Token != testToken || got.Control != net.JoinHostPort(relay.host(t), "7000") {
			t.Fatalf("stored control %q", got.Control)
		}
		h.noToken()
	})
}

// `--token` stores a token as before and has nothing to do with the sign-in
// endpoints, whatever the relay offers.
func TestLoginToken_NeverAsksTheSignInEndpoints(t *testing.T) {
	relay := newSignInRelay(t, http.StatusOK)
	for _, args := range [][]string{{"--token", testToken}, {"--token", "-"}, {"--token", testToken, "--no-browser"}} {
		h := browserHarness(t)
		h.stdoutTerm = true
		h.env["DISPLAY"] = ":0"
		pr, pw := io.Pipe()
		go func() {
			_, _ = io.WriteString(pw, testToken+"\n")
			_ = pw.Close()
		}()
		h.stdinR = pr
		if code := h.execWithin(append([]string{"login", relay.URL, "--cacert", relay.caFile(t)}, args...)...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, h.stderr.String())
		}
		_ = pr.Close()
		if h.stored(h.cfgPath).Token != testToken || len(h.opened) != 0 {
			t.Fatalf("%v: token stored: %v, browser opened %d times", args, h.stored(h.cfgPath).Token == testToken, len(h.opened))
		}
		if !strings.HasPrefix(h.stdout.String(), "Signed in to "+relay.URL+" (control endpoint ") {
			t.Fatalf("%v: stdout = %q", args, h.stdout.String())
		}
		h.noToken()
	}
	if n := len(relay.requests()); n != 0 {
		t.Fatalf("the relay got %d requests from login --token", n)
	}
}

func TestLoginBrowser_StartFails(t *testing.T) {
	t.Run("too many pending sign-ins", func(t *testing.T) {
		relay := newSignInRelay(t)
		relay.startStatus = http.StatusTooManyRequests
		h := browserHarness(t)
		code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t))
		if code != exitGeneral || h.stderr.String() != "The relay has too many pending sign-ins. Try again in a minute.\n" || h.stdout.Len() != 0 {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
	})
	t.Run("an error status", func(t *testing.T) {
		relay := newSignInRelay(t)
		relay.startStatus = http.StatusInternalServerError
		h := browserHarness(t)
		code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t))
		if code != exitGeneral || !strings.Contains(h.stderr.String(), "status 500") || strings.Count(h.stderr.String(), "\n") != 2 ||
			!strings.Contains(h.stderr.String(), "--token -") {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
	})
	t.Run("a certificate that is not trusted", func(t *testing.T) {
		relay := newSignInRelay(t)
		h := browserHarness(t)
		code := h.execWithin("login", relay.URL) // no --cacert
		if code != exitUnreachable || !strings.Contains(h.stderr.String(), "Cannot trust "+relay.URL) || !strings.Contains(h.stderr.String(), "--cacert") {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
		if len(relay.requests()) != 0 || h.hasConfig() {
			t.Fatal("a request went to a relay whose certificate is not trusted")
		}
	})
	t.Run("nothing answers", func(t *testing.T) {
		relay := newSignInRelay(t)
		ca := relay.caFile(t)
		relay.Close()
		h := browserHarness(t)
		code := h.execWithin("login", relay.URL, "--cacert", ca)
		if code != exitUnreachable || !strings.HasPrefix(h.stderr.String(), "Cannot reach "+relay.URL) {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
	})
}

// Ctrl-C while the approval is awaited: the command ends at once, with one
// line, and nothing is stored.
func TestLoginBrowser_InterruptedWhileWaiting(t *testing.T) {
	relay := newSignInRelay(t, http.StatusAccepted)
	relay.polled = make(chan struct{}, 8)
	h := browserHarness(t)
	waits := make(chan struct{}, 8)
	// The wait between two polls blocks as a timer does and ends with the
	// context only. The first one is let through, so that a poll is seen.
	first := true
	h.sleep = func(ctx context.Context, _ time.Duration) error {
		if first {
			first = false
			return nil
		}
		waits <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := newRoot(h.deps())
	root.SetArgs([]string{"login", relay.URL, "--cacert", relay.caFile(t)})
	done := make(chan int, 1)
	go func() { done <- report(&h.stderr, root.ExecuteContext(ctx)) }()

	for _, c := range []chan struct{}{relay.polled, waits} {
		select {
		case <-c:
		case code := <-done:
			t.Fatalf("the command ended by itself with %d: %s", code, h.stderr.String())
		case <-time.After(10 * time.Second):
			t.Fatal("the command did not get to waiting")
		}
	}
	cancel() // what Ctrl-C does to the command's context
	select {
	case code := <-done:
		if code != exitGeneral || h.stderr.String() != "Interrupted. Nothing was stored.\n" {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command went on waiting after Ctrl-C")
	}
	if h.hasConfig() || !strings.HasSuffix(h.stdout.String(), "Waiting for approval…  \n") {
		t.Fatalf("stored %v, stdout %q", h.hasConfig(), h.stdout.String())
	}
	if n := len(relay.asked(client.DeviceLoginPollPath)); n != 1 {
		t.Fatalf("%d polls, want the one before Ctrl-C", n)
	}
	h.noSecrets()
}

// A machine that is signed in is asked before anything is started, by the
// rules of `login --token`.
func TestLoginBrowser_AlreadySignedIn(t *testing.T) {
	t.Run("no terminal to ask on needs --force", func(t *testing.T) {
		relay := newSignInRelay(t, http.StatusOK)
		h := browserHarness(t)
		before := h.signIn()
		code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t))
		if code != exitUsage || !strings.Contains(h.stderr.String(), "already signed in to https://burrow.example.com") ||
			!strings.Contains(h.stderr.String(), "--force") {
			t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
		}
		if h.stored(h.cfgPath) != before || len(relay.requests()) != 0 || len(h.discovered) != 0 {
			t.Fatalf("the relay was asked (%d requests, %d discoveries) before the stored sign-in was settled", len(relay.requests()), len(h.discovered))
		}
	})
	t.Run("answer n keeps it", func(t *testing.T) {
		relay := newSignInRelay(t, http.StatusOK)
		h := browserHarness(t)
		before := h.signIn()
		var past <-chan struct{}
		h.terminal = true
		h.stdinR, past = openStdin(t, "n\n", "more\n")
		if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t)); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if h.stdout.String() != "Kept the stored sign-in.\n" || h.stored(h.cfgPath) != before || len(relay.requests()) != 0 {
			t.Fatalf("stdout %q, %d requests", h.stdout.String(), len(relay.requests()))
		}
		select {
		case <-past:
			t.Fatal("input after the answer line was read")
		default:
		}
	})
	t.Run("answer y signs in anew", func(t *testing.T) {
		relay := newSignInRelay(t, http.StatusOK)
		h := browserHarness(t)
		h.signIn()
		h.terminal = true
		h.stdinR, _ = openStdin(t, "y\n", "more\n")
		if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t)); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if got := h.stored(h.cfgPath); got.Token != browserToken || got.Relay != relay.URL {
			t.Fatalf("stored relay %q", got.Relay)
		}
		h.noSecrets()
	})
	t.Run("--force does not ask", func(t *testing.T) {
		relay := newSignInRelay(t, http.StatusOK)
		h := browserHarness(t)
		h.signIn()
		if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t), "--force"); code != 0 || h.stderr.Len() != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if h.stored(h.cfgPath).Token != browserToken {
			t.Fatal("the sign-in was not replaced")
		}
	})
	// A sign-in that fails leaves the stored one alone.
	t.Run("a denied sign-in keeps the stored one", func(t *testing.T) {
		relay := newSignInRelay(t, http.StatusForbidden)
		h := browserHarness(t)
		before := h.signIn()
		if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t), "--force"); code != exitGeneral {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if h.stored(h.cfgPath) != before {
			t.Fatal("the stored sign-in changed")
		}
	})
}

// The stored sign-in is settled before the relay is asked anything: a machine
// that is signed in hears that, not that some relay cannot be reached.
func TestLogin_AlreadySignedInComesBeforeDiscovery(t *testing.T) {
	for _, args := range [][]string{
		{"login", "other.example.com", "--token", testToken},
		{"login", "other.example.com", "--token", "-"},
		{"login", "other.example.com"},
	} {
		h := newHarness(t)
		before := h.signIn()
		h.answers(client.Discovery{}, &net.OpError{Op: "dial", Err: errors.New("connection refused")})
		code := h.execWithin(args...)
		if code != exitUsage || !strings.Contains(h.stderr.String(), "already signed in") || !strings.Contains(h.stderr.String(), "--force") {
			t.Fatalf("%v: exit %d, stderr %q", args, code, h.stderr.String())
		}
		if len(h.discovered) != 0 || h.stored(h.cfgPath) != before {
			t.Fatalf("%v: discovery was asked %d times", args, len(h.discovered))
		}
		h.noToken()
	}
	// With --force the relay is asked, and its failure is what is reported.
	h := newHarness(t)
	before := h.signIn()
	h.answers(client.Discovery{}, &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	if code := h.exec("login", "other.example.com", "--token", testToken, "--force"); code != exitUnreachable || h.stored(h.cfgPath) != before {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

func TestLogin_HelpNamesTheBrowserFlags(t *testing.T) {
	h := newHarness(t)
	if code := h.exec("login", "--help"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, w := range []string{"--no-browser", "--name", "--token", "--force", "approve"} {
		if !strings.Contains(h.stdout.String(), w) {
			t.Fatalf("help does not mention %q: %s", w, h.stdout.String())
		}
	}
}

// The relay made the token when it was collected. When it cannot be written
// down it is lost, and the user is told what is left to do. The token itself
// is not shown.
func TestLoginBrowser_TokenCannotBeStored(t *testing.T) {
	relay := newSignInRelay(t, http.StatusOK)
	h := browserHarness(t)
	// A file where the config's directory would be.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(blocker, "config.yaml")
	code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t), "--config", cfg)
	errOut := h.stderr.String()
	if code != exitGeneral || !strings.HasSuffix(errOut, "The token was created but could not be stored; revoke it in the dashboard (Clients, tab Tokens) and run burrow login again.\n") ||
		!strings.Contains(errOut, "not-a-directory") || strings.Count(errOut, "\n") != 2 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if strings.Contains(h.stdout.String(), "signed in") || !strings.HasSuffix(h.stdout.String(), "Waiting for approval…  \n") {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
	h.noSecrets()
}
