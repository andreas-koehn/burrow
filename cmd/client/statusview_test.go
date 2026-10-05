package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/testutil"
)

// lockedBuffer is a terminal that takes everything at once.
type lockedBuffer struct {
	mu   sync.Mutex
	b    strings.Builder
	last string // the last write: one drawing of the view
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = string(p)
	return l.b.Write(p)
}

// lastView returns the lines of the view that was drawn last, without escape
// sequences.
func (l *lockedBuffer) lastView() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	plain := escapeRe.ReplaceAllString(l.last, "")
	return strings.Split(strings.TrimSuffix(strings.TrimPrefix(plain, "\r"), "\r\n"), "\r\n")
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fakeTerminal replaces the terminal of the status view for the test.
type fakeTerminal struct {
	out      *lockedBuffer
	opened   int
	restored int
}

func useTerminal(t *testing.T, w io.Writer, cols, rows int, colour bool) *fakeTerminal {
	t.Helper()
	f := &fakeTerminal{out: &lockedBuffer{}}
	if w == nil {
		w = f.out
	}
	prev := openTerminal
	openTerminal = func() (terminal, bool) {
		f.opened++
		return terminal{w: w, size: func() (int, int) { return cols, rows }, colour: colour, restore: func() { f.restored++ }}, true
	}
	t.Cleanup(func() { openTerminal = prev })
	return f
}

// noTerminal fails the test when the status view asks for the terminal.
func noTerminal(t *testing.T) {
	t.Helper()
	prev := openTerminal
	openTerminal = func() (terminal, bool) {
		t.Error("the status view was started")
		return terminal{}, false
	}
	t.Cleanup(func() { openTerminal = prev })
}

// probes records what the view asks to be probed and lets the test report.
type probes struct {
	mu      sync.Mutex
	targets []string
	ctx     map[string]context.Context
	report  func(string, bool)
}

func useProbes(t *testing.T) *probes {
	t.Helper()
	p := &probes{ctx: map[string]context.Context{}}
	prev := startProbe
	startProbe = func(ctx context.Context, targets []string, interval time.Duration, report func(string, bool)) {
		if interval != 5*time.Second {
			t.Errorf("probe interval = %v, want 5 s", interval)
		}
		p.mu.Lock()
		p.targets = append(p.targets, targets...)
		for _, a := range targets {
			p.ctx[a] = ctx
		}
		p.report = report
		p.mu.Unlock()
		<-ctx.Done()
	}
	t.Cleanup(func() { startProbe = prev })
	return p
}

func (p *probes) wait(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := len(p.targets)
		p.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("fewer than %d probes were started", n)
}

// client replaces startClient with f.
func useClient(t *testing.T, f func(ctx context.Context, o client.Options) error) {
	t.Helper()
	prev := startClient
	startClient = f
	t.Cleanup(func() { startClient = prev })
}

var (
	viewCreds   = client.Credentials{Control: "relay.example.com:7000", Token: testToken, TokenName: "kohns-laptop", Source: client.SourceUserConfig}
	viewTunnels = []client.TunnelSpec{
		{Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000"},
		{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432"},
		{Name: "again", Type: "tcp", LocalAddr: "127.0.0.1:3000"},
	}
	viewFlags = globalFlags{logLevel: "info", logFormat: "text", view: true}
	colourRe  = regexp.MustCompile("\x1b\\[[0-9;]*m")
	escapeRe  = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
)

func TestStatusView_ChosenOnATerminalWithoutLogSettings(t *testing.T) {
	yaml := writeFile(t, "burrow.yaml", "services:\n  - { name: app, local: 127.0.0.1:3000 }\n")
	commands := [][]string{{"http", "3000"}, {"tcp", "5432"}, {"up", "--file", yaml}}
	cases := []struct {
		name     string
		terminal bool
		args     []string
		env      map[string]string
		want     bool
	}{
		{"terminal", true, nil, nil, true},
		{"not a terminal", false, nil, nil, false},
		{"--log text", true, []string{"--log", "text"}, nil, false},
		{"--log json", true, []string{"--log", "json"}, nil, false},
		{"BURROW_LOG_FORMAT", true, nil, map[string]string{"BURROW_LOG_FORMAT": "json"}, false},
		{"BURROW_LOG_LEVEL", true, nil, map[string]string{"BURROW_LOG_LEVEL": "debug"}, false},
		{"not a terminal, --log json", false, []string{"--log", "json"}, nil, false},
	}
	for _, c := range cases {
		for _, command := range commands {
			t.Run(c.name+"/"+command[0], func(t *testing.T) {
				h := newHarness(t)
				h.signIn()
				h.viewTerm = c.terminal
				for k, v := range c.env {
					h.env[k] = v
				}
				if code := h.exec(append(append([]string{}, command...), c.args...)...); code != 0 {
					t.Fatalf("exit %d: %s", code, h.stderr.String())
				}
				if got := h.oneRun().g.view; got != c.want {
					t.Fatalf("status view = %v, want %v", got, c.want)
				}
				if strings.ContainsRune(h.stdout.String(), 0x1b) {
					t.Fatal("ESC on stdout")
				}
			})
		}
	}
}

// Without the view the client gets exactly what it got before there was one.
func TestStatusView_OffMeansLogLinesAsBefore(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	for _, format := range []string{"text", "json"} {
		s := captureStart(t)
		g := globalFlags{logLevel: "info", logFormat: format}
		if err := runClient(context.Background(), viewCreds, viewTunnels, g); err != nil {
			t.Fatal(err)
		}
		if s.opts.Observer != nil {
			t.Fatal("an observer was set without the status view")
		}
		if f, _ := logShape(t, s.opts.Logger); f != format {
			t.Fatalf("logger = %s, want %s", f, format)
		}
	}
}

// `connect` never shows the view, terminal or not.
func TestStatusView_ConnectNeverUsesIt(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	yaml := writeFile(t, "burrow.yaml", twoServices)
	for _, args := range [][]string{
		{"--server", "relay.example.com:7000", "--token", "tok"},
		{"--server", "relay.example.com:7000", "--token", "tok", "--type", "http"},
		{"--config", yaml},
	} {
		s := captureStart(t)
		if err := runConnect(t, args...); err != nil {
			t.Fatal(err)
		}
		if s.calls != 1 || s.opts.Observer != nil {
			t.Fatalf("%v: calls %d, observer set: %v", args, s.calls, s.opts.Observer != nil)
		}
		logShape(t, s.opts.Logger) // a text or json logger with info on, as before
	}
}

func TestStatusView_ShowsWhatTheClientReports(t *testing.T) {
	cleanEnv(t)
	defer testutil.AssertNoGoroutineLeak(t)()
	term := useTerminal(t, nil, 100, 30, false)
	pr := useProbes(t)
	var opts client.Options
	useClient(t, func(ctx context.Context, o client.Options) error {
		opts = o
		ob := o.Observer
		pr.wait(t, 2)
		ob.State(client.StateConnecting, "", 0)
		ob.State(client.StateConnected, "", 0)
		ob.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", URL: "https://relay.example.com/svc/p7baeh/"})
		ob.Registered(client.RegisteredTunnel{TunnelID: "t2", Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000})
		ob.Latency(12 * time.Millisecond)
		ob.Connection("t1", time.Date(2026, 10, 5, 14, 2, 11, 0, time.UTC), "203.0.113.7")
		// the probe finds nothing on either port
		pr.report("127.0.0.1:3000", false)
		pr.report("127.0.0.1:5432", false)
		// a visitor then reaches the first one: its probe ends, and what a
		// probe that was still under way reports is not shown any more
		ob.LocalTarget("127.0.0.1:3000", true)
		select {
		case <-pr.ctx["127.0.0.1:3000"].Done():
		case <-time.After(2 * time.Second):
			t.Error("the probe of a target a visitor reached was not stopped")
		}
		if pr.ctx["127.0.0.1:5432"].Err() != nil {
			t.Error("the probe of the other target was stopped too")
		}
		pr.report("127.0.0.1:3000", false)
		return nil
	})

	if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
		t.Fatal(err)
	}
	if term.opened != 1 || term.restored != 1 {
		t.Fatalf("terminal opened %d times, restored %d times", term.opened, term.restored)
	}
	// Each local address is probed once, however many services use it.
	sort.Strings(pr.targets)
	if got := strings.Join(pr.targets, " "); got != "127.0.0.1:3000 127.0.0.1:5432" {
		t.Fatalf("probed %q", got)
	}
	// The client logs nothing while the view is up.
	for _, l := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if opts.Logger == nil || opts.Logger.Enabled(context.Background(), l) {
			t.Fatalf("the client's logger is on at level %v", l)
		}
	}
	if opts.Server != "relay.example.com:7000" || opts.Token != testToken || len(opts.Tunnels) != 3 {
		t.Fatal("the client did not get the options of the command")
	}

	out := term.out.String()
	want := []string{
		"burrow  ●  connected to relay.example.com",
		"12 ms",
		"  my-app   https://relay.example.com/svc/p7baeh/  →  127.0.0.1:3000",
		"1 open, 1 total",
		"  14:02:11  connection from 203.0.113.7",
		"  pg   relay.example.com:9000  →  127.0.0.1:5432",
		"  ! nothing is listening on 127.0.0.1:5432",
	}
	final := strings.Join(term.out.lastView(), "\n")
	for _, w := range want {
		if !strings.Contains(final, w) {
			t.Errorf("%q is missing in the final view:\n%s", w, final)
		}
	}
	if strings.Contains(final, "nothing is listening on 127.0.0.1:3000") {
		t.Errorf("the warning for a target a visitor reached is still shown:\n%s", final)
	}
	if !strings.HasSuffix(out, "\r\n") {
		t.Errorf("the cursor is not left below the view: %q", out[max(0, len(out)-20):])
	}
	// Nothing of the token, and nothing that names it.
	if strings.Contains(out, testToken) || strings.Contains(out, testToken[len(testToken)-4:]) || strings.Contains(out, "kohns-laptop") {
		t.Error("the view shows something of the token")
	}
	if colourRe.MatchString(out) {
		t.Error("colour sequences although colour is off")
	}
	for _, bad := range []string{"\x1b[?1049", "\x1b[?25l"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q was written", bad)
		}
	}
}

func TestStatusView_ColourOnlyWhenAllowed(t *testing.T) {
	cleanEnv(t)
	useProbes(t)
	term := useTerminal(t, nil, 100, 30, true)
	useClient(t, func(_ context.Context, o client.Options) error {
		o.Observer.State(client.StateConnected, "", 0)
		return nil
	})
	if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
		t.Fatal(err)
	}
	if out := term.out.String(); !strings.Contains(out, "\x1b[32m●\x1b[0m") {
		t.Fatalf("no green dot: %q", out)
	}
}

func TestStatusView_TheClientsErrorIsReturned(t *testing.T) {
	cleanEnv(t)
	useProbes(t)
	term := useTerminal(t, nil, 100, 30, false)
	useClient(t, func(context.Context, client.Options) error { return io.ErrUnexpectedEOF })
	if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v", err)
	}
	if term.restored != 1 {
		t.Fatal("the terminal was not restored")
	}
}

// When the console cannot show the view, the client logs as it always did.
func TestStatusView_FallsBackToLogLines(t *testing.T) {
	cleanEnv(t)
	prev := openTerminal
	openTerminal = func() (terminal, bool) { return terminal{}, false }
	t.Cleanup(func() { openTerminal = prev })
	s := captureStart(t)
	if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
		t.Fatal(err)
	}
	if s.opts.Observer != nil {
		t.Fatal("observer without a view")
	}
	if f, _ := logShape(t, s.opts.Logger); f != "text" {
		t.Fatalf("logger = %s", f)
	}
}

// A terminal that takes no output (stopped with Ctrl-S, a hanging ssh) must
// neither hold up the client nor keep the command from ending.
func TestStatusView_BlockedTerminalAndABurstOfEvents(t *testing.T) {
	cleanEnv(t)
	useProbes(t)
	prevWait := viewCloseWait
	viewCloseWait = 100 * time.Millisecond
	t.Cleanup(func() { viewCloseWait = prevWait })

	r, w := io.Pipe() // nobody reads: every write blocks
	t.Cleanup(func() { _ = r.Close() })
	useTerminal(t, w, 80, 24, true)

	var burst time.Duration
	useClient(t, func(_ context.Context, o client.Options) error {
		ob := o.Observer
		start := time.Now()
		ob.State(client.StateConnected, "", 0)
		ob.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", URL: "https://relay.example.com/svc/p7baeh/"})
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 5000; i++ {
					ob.Connection("t1", time.Now(), "203.0.113.7")
					ob.LocalTarget("127.0.0.1:3000", i%2 == 0)
					ob.ConnectionClosed("t1")
				}
			}()
		}
		wg.Wait()
		burst = time.Since(start)
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- runClient(context.Background(), viewCreds, viewTunnels, viewFlags) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not end behind a terminal that takes no output")
	}
	if burst > 5*time.Second {
		t.Fatalf("120000 events took %v behind a blocked terminal", burst)
	}
}

func TestStatusView_NarrowAndUnknownSizes(t *testing.T) {
	cleanEnv(t)
	for _, size := range [][2]int{{20, 10}, {20, 0}, {0, 0}, {1, 1}, {300, 3}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			useProbes(t)
			term := useTerminal(t, nil, size[0], size[1], false)
			useClient(t, func(_ context.Context, o client.Options) error {
				ob := o.Observer
				ob.State(client.StateConnected, "", 0)
				ob.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", URL: "https://relay.example.com/svc/p7baeh/"})
				for i := 0; i < 12; i++ {
					ob.Connection("t1", time.Now(), "203.0.113.7")
				}
				ob.State(client.StateReconnecting, "dial: dial tcp 203.0.113.9:7000: connect: connection refused", 4*time.Second)
				return nil
			})
			if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
				t.Fatal(err)
			}
			width := size[0]
			if width <= 0 {
				width = 80
			}
			lines := term.out.lastView()
			if !strings.Contains(strings.Join(lines, " "), "reconnecting") && size[0] != 1 {
				t.Errorf("the last state is not what was drawn last: %q", lines)
			}
			for _, l := range lines {
				if strings.ContainsAny(l, "\r\n") {
					t.Errorf("line break inside %q", l)
				}
				if n := utf8.RuneCountInString(l); n > width {
					t.Errorf("line of %d characters on %d columns: %q", n, width, l)
				}
			}
			if rows := size[1]; rows > 0 && len(lines) > max(rows-1, 1) {
				t.Errorf("%d lines on %d rows", len(lines), rows)
			}
		})
	}
}

// A fault in the view ends the view, not the tunnels.
func TestStatusView_APanicInTheViewDoesNotEndTheClient(t *testing.T) {
	cleanEnv(t)
	useProbes(t)
	errOut := &lockedBuffer{}
	prevErr := viewErr
	viewErr = errOut
	t.Cleanup(func() { viewErr = prevErr })

	restored := 0
	prev := openTerminal
	openTerminal = func() (terminal, bool) {
		return terminal{w: io.Discard, size: func() (int, int) { panic("size failed") }, restore: func() { restored++ }}, true
	}
	t.Cleanup(func() { openTerminal = prev })

	ran := false
	useClient(t, func(_ context.Context, o client.Options) error {
		for i := 0; i < 100; i++ {
			o.Observer.State(client.StateConnected, "", 0)
			time.Sleep(time.Millisecond)
		}
		ran = true
		return nil
	})
	if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
		t.Fatal(err)
	}
	if !ran || restored != 1 {
		t.Fatalf("client ran to its end: %v, terminal restored %d times", ran, restored)
	}
	if !strings.Contains(errOut.String(), "status view") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}
