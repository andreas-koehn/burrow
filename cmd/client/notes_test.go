package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/testutil"
	"github.com/ankoehn/burrow/internal/version"
)

// useStderr replaces the client's stderr for the test.
func useStderr(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	prev := viewErr
	viewErr = b
	t.Cleanup(func() { viewErr = prev })
	return b
}

// useVersion sets the client's own version for the test.
func useVersion(t *testing.T, v string) {
	t.Helper()
	prev := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = prev })
}

var (
	existing = client.RegisteredTunnel{
		TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000",
		URL: "https://relay.example.com/svc/p7baeh/", AccessMode: "burrow_login",
		DashboardURL: "https://relay.example.com/services/svc-1",
		Ignored:      client.OptionSet{Slug: true, Access: true},
	}
	msgExisting = "This service already exists with slug p7baeh and access login. " +
		"--slug and --access apply only when a service is created. " +
		"Change them in the dashboard: https://relay.example.com/services/svc-1"
)

func TestRegistrationNotes(t *testing.T) {
	if got := ignoredNote(existing, true); got != msgExisting {
		t.Errorf("ignored:\n got %s\nwant %s", got, msgExisting)
	}
	// With several services the note names the one it is about.
	if got := ignoredNote(existing, false); !strings.HasPrefix(got, "Service my-app already exists with slug p7baeh and access login. ") {
		t.Errorf("several services: %s", got)
	}
	// What the relay did not say is left out.
	bare := existing
	bare.URL, bare.DashboardURL, bare.AccessMode = "", "", ""
	if got := ignoredNote(bare, true); got != "This service already exists. --slug and --access apply only when a service is created. Change them in the dashboard." {
		t.Errorf("without details: %s", got)
	}
	// What the relay says goes to a terminal: nothing that is not a slug, a
	// known kind of name or a plain https address is repeated.
	evil := existing
	evil.URL = "https://relay.example.com/svc/\x1b[2Jx/"
	evil.AccessMode = "open\x1b[31m"
	evil.DashboardURL = "https://relay.example.com/\r\nservices"
	evil.Name = "a\x1b[2Jb\nc"
	for _, single := range []bool{true, false} {
		if got := ignoredNote(evil, single); strings.ContainsAny(got, "\x1b\r\n") {
			t.Errorf("control characters in %q", got)
		}
	}

	older := client.RegisteredTunnel{Name: "my-app", Type: "http", SlugUnacknowledged: true}
	if got := slugUnacknowledgedNote(older, true); got != "This relay does not support --slug yet, so it was not applied. Change the slug in the dashboard." {
		t.Errorf("older relay: %s", got)
	}
	if got := slugUnacknowledgedNote(older, false); got != "Service my-app: this relay does not support --slug yet, so it was not applied. Change the slug in the dashboard." {
		t.Errorf("older relay, several services: %s", got)
	}
}

// fgRun runs runClient the way a command after `connect` does, with a client
// that plays script against the observer, and returns the options it got.
func fgRun(t *testing.T, logsAsked bool, g globalFlags, tunnels []client.TunnelSpec, script func(ob client.Observer)) client.Options {
	t.Helper()
	var opts client.Options
	useClient(t, func(_ context.Context, o client.Options) error {
		opts = o
		if script != nil {
			if o.Observer == nil {
				t.Error("the client has no observer")
				return nil
			}
			script(o.Observer)
		}
		return nil
	})
	if err := runClient(foregroundContext(context.Background(), logsAsked), viewCreds, tunnels, g); err != nil {
		t.Fatal(err)
	}
	return opts
}

var oneTunnel = []client.TunnelSpec{{Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "wanted", Access: "open"}}

// Without the status view the client prints log lines; what it has to tell
// about --slug and --access is one more of them, once.
func TestNotes_LogLinesMode(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	errOut := useStderr(t)
	opts := fgRun(t, true, globalFlags{logLevel: "info", logFormat: "text"}, oneTunnel, func(ob client.Observer) {
		for i := 0; i < 3; i++ { // the relay says it again after every reconnect
			ob.State(client.StateConnected, "", 0)
			ob.Registered(existing)
			ob.State(client.StateReconnecting, "session closed", time.Second)
		}
	})
	if !opts.StopOnRefusal {
		t.Error("a command after connect must stop on a refusal that will not change")
	}
	out := errOut.String()
	if n := strings.Count(out, "This service already exists with slug p7baeh and access login."); n != 1 {
		t.Fatalf("the note was printed %d times:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "https://relay.example.com/services/svc-1") {
		t.Fatalf("not a log line with the dashboard address:\n%s", out)
	}
	// Nothing about the origin or the version here: those belong to the view.
	if strings.Contains(out, "origin") || strings.Contains(out, "burrow update") {
		t.Fatalf("view-only notes in the log:\n%s", out)
	}
}

func TestNotes_OlderRelayIsSaidOnce(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	errOut := useStderr(t)
	fgRun(t, false, globalFlags{logLevel: "info", logFormat: "json"}, oneTunnel, func(ob client.Observer) {
		older := client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000",
			URL: "https://relay.example.com/svc/abc234/", SlugUnacknowledged: true}
		ob.State(client.StateConnected, "", 0)
		ob.Registered(older)
		ob.Registered(older)
	})
	out := errOut.String()
	if n := strings.Count(out, "This relay does not support --slug yet, so it was not applied."); n != 1 {
		t.Fatalf("the note was printed %d times:\n%s", n, out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("with the json format every line is JSON:\n%s", out)
	}
}

// A service created with api-key access lets nobody in until a key exists.
// The key is made in the dashboard; the client only says where.
func TestNotes_APIKeyServiceNeedsAKey(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	errOut := useStderr(t)
	created := client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000",
		URL: "https://relay.example.com/svc/my-app/", AccessMode: "api_key", Created: true,
		DashboardURL: "https://relay.example.com/services/svc-1"}
	fgRun(t, false, globalFlags{logLevel: "info", logFormat: "text"}, oneTunnel, func(ob client.Observer) {
		ob.Registered(created)
		// After a reconnect the service exists; nothing is said again.
		created.Created = false
		ob.Registered(created)
	})
	out := errOut.String()
	if n := strings.Count(out, "This service was created with api-key access and has no API key yet: every request is refused until one exists. Create one in the dashboard: https://relay.example.com/services/svc-1"); n != 1 {
		t.Fatalf("said %d times:\n%s", n, out)
	}
	if strings.Contains(out, "buk_") {
		t.Fatal("a key was printed")
	}
}

// Nothing is said when nothing was wished for or everything was applied.
func TestNotes_QuietWhenThereIsNothingToSay(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	errOut := useStderr(t)
	fgRun(t, false, globalFlags{logLevel: "info", logFormat: "text"}, oneTunnel, func(ob client.Observer) {
		ob.State(client.StateConnecting, "", 0)
		ob.State(client.StateConnected, "", 0)
		if so, ok := ob.(client.SessionObserver); ok {
			so.Session(client.SessionInfo{RelayVersion: "99.0.0"})
		}
		ob.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000",
			URL: "https://relay.example.com/svc/wanted/", AccessMode: "open", Created: true})
		ob.Connection("t1", time.Now(), "203.0.113.7:1")
		ob.ConnectionClosed("t1")
		ob.Latency(time.Millisecond)
		ob.LocalTarget("127.0.0.1:3000", true)
	})
	if errOut.String() != "" {
		t.Fatalf("stderr: %s", errOut.String())
	}
}

// When the relay cannot be reached at the first attempt, the spec's line is
// printed once; the client keeps trying. With log lines asked for, nothing is
// added to them.
func TestNotes_RelayUnreachableAtTheFirstAttempt(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	const want = "Cannot reach relay.example.com:7000. Check the address and that port 7000 is open. Details: burrow doctor\n"
	script := func(ob client.Observer) {
		ob.State(client.StateConnecting, "", 0)
		ob.State(client.StateReconnecting, "dial: dial tcp 192.0.2.1:7000: connect: connection refused", time.Second)
		ob.State(client.StateConnecting, "", 0)
		ob.State(client.StateReconnecting, "dial: dial tcp 192.0.2.1:7000: connect: connection refused", 2*time.Second)
	}
	errOut := useStderr(t)
	fgRun(t, false, globalFlags{logLevel: "info", logFormat: "text"}, oneTunnel, script)
	if errOut.String() != want {
		t.Fatalf("stderr = %q", errOut.String())
	}

	t.Run("not with --log", func(t *testing.T) {
		errOut := useStderr(t)
		fgRun(t, true, globalFlags{logLevel: "info", logFormat: "text"}, oneTunnel, script)
		if errOut.String() != "" {
			t.Fatalf("stderr = %q", errOut.String())
		}
	})
	t.Run("not after the relay was reached once", func(t *testing.T) {
		errOut := useStderr(t)
		fgRun(t, false, globalFlags{logLevel: "info", logFormat: "text"}, oneTunnel, func(ob client.Observer) {
			ob.State(client.StateConnected, "", 0)
			ob.State(client.StateReconnecting, "dial: dial tcp 192.0.2.1:7000: connect: connection refused", time.Second)
		})
		if errOut.String() != "" {
			t.Fatalf("stderr = %q", errOut.String())
		}
	})
	t.Run("not for a certificate, which has a message of its own", func(t *testing.T) {
		errOut := useStderr(t)
		fgRun(t, false, globalFlags{logLevel: "info", logFormat: "text"}, oneTunnel, func(ob client.Observer) {
			ob.State(client.StateReconnecting, "dial: tls: failed to verify certificate: x509: certificate signed by unknown authority", time.Second)
		})
		if errOut.String() != "" {
			t.Fatalf("stderr = %q", errOut.String())
		}
	})
	t.Run("not for a refusal", func(t *testing.T) {
		errOut := useStderr(t)
		fgRun(t, false, globalFlags{logLevel: "info", logFormat: "text"}, oneTunnel, func(ob client.Observer) {
			ob.State(client.StateReconnecting, "register failed: port 9000 in use", time.Second)
		})
		if errOut.String() != "" {
			t.Fatalf("stderr = %q", errOut.String())
		}
	})
}

// `connect` gets none of this: no observer, no stopping, as before.
func TestNotes_ConnectIsLeftAlone(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	errOut := useStderr(t)
	s := captureStart(t)
	if err := runClient(context.Background(), viewCreds, oneTunnel, globalFlags{logLevel: "info", logFormat: "text"}); err != nil {
		t.Fatal(err)
	}
	if s.opts.Observer != nil || s.opts.StopOnRefusal {
		t.Fatalf("observer %v, stop on refusal %v", s.opts.Observer != nil, s.opts.StopOnRefusal)
	}
	if errOut.String() != "" {
		t.Fatalf("stderr = %q", errOut.String())
	}
	// Through the command, with every flag it has: no wish is ever sent.
	s = captureStart(t)
	if err := runConnect(t, "--server", "relay.example.com:7000", "--token", testToken, "--local", "127.0.0.1:3000", "--type", "http", "--name", "web"); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 || s.opts.Observer != nil || s.opts.StopOnRefusal || len(s.opts.Tunnels) != 1 ||
		s.opts.Tunnels[0] != (client.TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"}) {
		t.Fatalf("connect built %+v (observer %v, stop %v)", s.opts.Tunnels, s.opts.Observer != nil, s.opts.StopOnRefusal)
	}
	for _, f := range []string{"slug", "access"} {
		if newConnectCmd().Flags().Lookup(f) != nil {
			t.Fatalf("connect has a --%s flag", f)
		}
	}
}

// The commands after `connect` mark their run, and say whether log lines
// were asked for.
func TestForeground_MarksTheRun(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		env       map[string]string
		logsAsked bool
	}{
		{"http", []string{"http", "3000"}, nil, false},
		{"tcp", []string{"tcp", "5432"}, nil, false},
		{"--log", []string{"http", "3000", "--log", "json"}, nil, true},
		{"BURROW_LOG_FORMAT", []string{"http", "3000"}, map[string]string{"BURROW_LOG_FORMAT": "json"}, true},
		{"BURROW_LOG_LEVEL", []string{"http", "3000"}, map[string]string{"BURROW_LOG_LEVEL": "debug"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			for k, v := range tc.env {
				h.env[k] = v
			}
			d := h.deps()
			var got foregroundRun
			var marked bool
			d.run = func(ctx context.Context, _ client.Credentials, _ []client.TunnelSpec, _ globalFlags) error {
				got, marked = foregroundFrom(ctx)
				return nil
			}
			root := newRoot(d)
			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !marked || got.logsAsked != tc.logsAsked {
				t.Fatalf("marked %v, logs asked %v", marked, got.logsAsked)
			}
		})
	}
	if _, marked := foregroundFrom(context.Background()); marked {
		t.Fatal("a plain context counts as marked")
	}
}

// In the status view the notes are lines of the view: the access mode, the
// warning about the shared origin for an open http service, what was not
// applied, and a newer relay.
func TestStatusView_AccessAndNotes(t *testing.T) {
	cleanEnv(t)
	defer testutil.AssertNoGoroutineLeak(t)()
	useVersion(t, "0.7.0")
	errOut := useStderr(t)
	term := useTerminal(t, nil, 100, 40, false)
	useProbes(t)
	tunnels := []client.TunnelSpec{
		{Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "wanted", Access: "open"},
		{Name: "docs", Type: "http", LocalAddr: "127.0.0.1:3001"},
		{Name: "api", Type: "http", LocalAddr: "127.0.0.1:3002"},
		{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432"},
	}
	var opts client.Options
	useClient(t, func(_ context.Context, o client.Options) error {
		opts = o
		ob := o.Observer
		for i := 0; i < 2; i++ { // a reconnect repeats everything
			ob.State(client.StateConnected, "", 0)
			ob.(client.SessionObserver).Session(client.SessionInfo{RelayVersion: "0.8.0"})
			ob.Registered(existing)
			ob.Registered(client.RegisteredTunnel{TunnelID: "t2", Name: "docs", Type: "http", LocalAddr: "127.0.0.1:3001",
				URL: "https://relay.example.com/svc/docs/", AccessMode: "open", Created: true})
			ob.Registered(client.RegisteredTunnel{TunnelID: "t3", Name: "api", Type: "http", LocalAddr: "127.0.0.1:3002",
				URL: "https://relay.example.com/svc/api/", AccessMode: "api_key"})
			ob.Registered(client.RegisteredTunnel{TunnelID: "t4", Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000})
		}
		return nil
	})
	if err := runClient(context.Background(), viewCreds, tunnels, viewFlags); err != nil {
		t.Fatal(err)
	}
	if !opts.StopOnRefusal {
		t.Error("the view is only used by commands that stop on a refusal")
	}
	lines := term.out.lastView()
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"access: Burrow login",
		"access: open (anyone with the URL)",
		"access: API key",
		"! This app shares the dashboard's origin; expose only apps you trust.",
		"! Service my-app already exists with slug p7baeh and access login. --slug and --access apply only",
		"https://relay.example.com/services/svc-1",
		"The relay runs v0.8.0. Run: burrow update",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("%q is not in the view:\n%s", want, all)
		}
	}
	for _, once := range []string{"shares the dashboard's origin", "already exists", "burrow update"} {
		if n := strings.Count(all, once); n != 1 {
			t.Errorf("%q is in the view %d times:\n%s", once, n, all)
		}
	}
	// The header keeps naming this client's version.
	if !strings.Contains(lines[0], "v0.7.0") || strings.Contains(lines[0], "0.8.0") {
		t.Errorf("header: %q", lines[0])
	}
	// Nothing of this goes to stderr while the view is on.
	if errOut.String() != "" {
		t.Errorf("stderr used: %q", errOut.String())
	}
}

// A relay of the same or an older version, or a version that cannot be
// compared, gives no notice; a service that is not open gives no warning.
func TestStatusView_NoNoticeWithoutAReason(t *testing.T) {
	for _, relay := range []string{"0.7.0", "0.6.9", "", "develop", "9.9.9\x1b[2J"} {
		cleanEnv(t)
		useVersion(t, "0.7.0")
		term := useTerminal(t, nil, 100, 40, false)
		useProbes(t)
		useClient(t, func(_ context.Context, o client.Options) error {
			ob := o.Observer
			ob.State(client.StateConnected, "", 0)
			ob.(client.SessionObserver).Session(client.SessionInfo{RelayVersion: relay})
			ob.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000",
				URL: "https://relay.example.com/svc/abc234/", AccessMode: "burrow_login"})
			ob.Registered(client.RegisteredTunnel{TunnelID: "t2", Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000, AccessMode: "open"})
			return nil
		})
		if err := runClient(context.Background(), viewCreds, viewTunnels[:2], viewFlags); err != nil {
			t.Fatal(err)
		}
		all := strings.Join(term.out.lastView(), "\n")
		if strings.Contains(all, "burrow update") || strings.Contains(all, "origin") || strings.Contains(all, "!") {
			t.Errorf("relay version %q:\n%s", relay, all)
		}
	}
}

// The observer the commands install hands everything on, from any goroutine.
func TestRunNotes_ForwardsAndIsSafeForConcurrentUse(t *testing.T) {
	var mu sync.Mutex
	var notes []string
	n := newRunNotes(noObserver{}, "relay.example.com:7000", 2)
	n.note = func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() }
	var out bytes.Buffer
	n.unreachable = func(s string) { mu.Lock(); out.WriteString(s); mu.Unlock() }
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			n.State(client.StateReconnecting, "dial: refused", time.Second)
			n.Registered(existing)
			n.Session(client.SessionInfo{RelayVersion: "99.0.0"})
		})
	}
	wg.Wait()
	if len(notes) != 1 || strings.Count(out.String(), "Cannot reach") != 1 {
		t.Fatalf("notes %d, unreachable %q", len(notes), out.String())
	}
}
