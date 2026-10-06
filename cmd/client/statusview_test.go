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

// `connect` never shows the view, terminal or not, and never asks the relay
// for request summaries: what it sends is what it always sent.
func TestStatusView_ConnectNeverUsesIt(t *testing.T) {
	cleanEnv(t)
	noTerminal(t)
	yaml := writeFile(t, "burrow.yaml", twoServices)
	prevShows := stdoutShowsView
	t.Cleanup(func() { stdoutShowsView = prevShows })
	for _, onTerminal := range []bool{false, true} {
		stdoutShowsView = func() bool { return onTerminal }
		for _, args := range [][]string{
			{"--server", "relay.example.com:7000", "--token", "tok"},
			{"--server", "relay.example.com:7000", "--token", "tok", "--type", "http"},
			{"--config", yaml},
		} {
			s := captureStart(t)
			if err := runConnect(t, args...); err != nil {
				t.Fatal(err)
			}
			if s.calls != 1 || s.opts.Observer != nil || s.opts.RequestSummaries {
				t.Fatalf("terminal %v, %v: calls %d, observer set: %v, summaries asked: %v",
					onTerminal, args, s.calls, s.opts.Observer != nil, s.opts.RequestSummaries)
			}
			logShape(t, s.opts.Logger) // a text or json logger with info on, as before

			// The same through the root command, whose other commands would
			// show the view on this terminal.
			h := newHarness(t)
			h.viewTerm = onTerminal
			if code := h.exec(append([]string{"connect"}, args...)...); code != 0 {
				t.Fatalf("terminal %v, connect %v: exit %d: %s", onTerminal, args, code, h.stderr.String())
			}
			if s.calls != 2 || s.opts.Observer != nil || s.opts.RequestSummaries {
				t.Fatalf("terminal %v, connect %v through the root: calls %d, observer set: %v, summaries asked: %v",
					onTerminal, args, s.calls, s.opts.Observer != nil, s.opts.RequestSummaries)
			}
			if out := h.stdout.String() + h.stderr.String(); strings.ContainsRune(out, 0x1b) {
				t.Fatalf("ESC in the output of connect: %q", out)
			}
		}
	}
}

// Request summaries are for the view. A run that prints log lines does not ask
// the relay for them, so its log has the lines it always had.
func TestStatusView_SummariesAreAskedForOnlyWithTheView(t *testing.T) {
	cleanEnv(t)
	t.Run("log lines", func(t *testing.T) {
		noTerminal(t)
		for _, ctx := range []context.Context{
			context.Background(),
			foregroundContext(context.Background(), true),  // http --log json
			foregroundContext(context.Background(), false), // http, stdout is a pipe
		} {
			s := captureStart(t)
			if err := runClient(ctx, viewCreds, viewTunnels, globalFlags{logLevel: "info", logFormat: "json"}); err != nil {
				t.Fatal(err)
			}
			if s.opts.RequestSummaries {
				t.Fatal("a run without the view asks for request summaries")
			}
		}
	})
	t.Run("view", func(t *testing.T) {
		useProbes(t)
		useTerminal(t, nil, 100, 30, false)
		asked := false
		useClient(t, func(_ context.Context, o client.Options) error {
			asked = o.RequestSummaries
			return nil
		})
		if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
			t.Fatal(err)
		}
		if !asked {
			t.Fatal("the view does not ask for request summaries")
		}
	})
	t.Run("the terminal cannot show the view", func(t *testing.T) {
		prev := openTerminal
		openTerminal = func() (terminal, bool) { return terminal{}, false }
		t.Cleanup(func() { openTerminal = prev })
		s := captureStart(t)
		if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
			t.Fatal(err)
		}
		if s.opts.RequestSummaries {
			t.Fatal("log lines, and request summaries were asked for")
		}
	})
}

func TestStatusView_ShowsRequests(t *testing.T) {
	cleanEnv(t)
	defer testutil.AssertNoGoroutineLeak(t)()
	term := useTerminal(t, nil, 100, 40, true)
	pr := useProbes(t)
	day := func(s int) time.Time { return time.Date(2026, 10, 5, 14, 2, s, 0, time.UTC) }
	useClient(t, func(_ context.Context, o client.Options) error {
		ob := o.Observer
		pr.wait(t, 2)
		ob.State(client.StateConnected, "", 0)
		ob.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", URL: "https://relay.example.com/svc/p7baeh/"})
		ob.Registered(client.RegisteredTunnel{TunnelID: "t2", Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000})
		ob.Connection("t1", day(11), "203.0.113.7:4711")
		ob.Request("t1", day(11), "GET", "/api/users", 200)
		ob.Connection("t1", day(12), "203.0.113.7:4712")
		ob.Request("t1", day(12), "POST", "/api/login", 401)
		ob.Request("t1", day(13), "GET", "/a\x1b[2Jb", 502)
		ob.Connection("t2", day(14), "203.0.113.9:1")
		ob.ConnectionClosed("t1")
		// The counts come from the client, whatever became of the single calls.
		co, ok := ob.(client.CountObserver)
		if !ok {
			t.Error("the view's observer does not take counts")
			return nil
		}
		co.Counts("t1", 0, 7)
		return nil
	})
	if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
		t.Fatal(err)
	}
	final := strings.Join(term.out.lastView(), "\n")
	for _, w := range []string{
		"  14:02:11  GET   /api/users      200",
		"  14:02:12  POST  /api/login      401",
		"  14:02:13  GET   /a?[2Jb         502",
		"0 open, 7 total",
		"  14:02:14  connection from 203.0.113.9",
	} {
		if !strings.Contains(final, w) {
			t.Errorf("%q is missing in the final view:\n%s", w, final)
		}
	}
	if strings.Contains(final, "203.0.113.7") {
		t.Errorf("a service with request lines shows its connections too:\n%s", final)
	}
	out := term.out.String()
	if !strings.Contains(out, "\x1b[33m401\x1b[0m") || !strings.Contains(out, "\x1b[31m502\x1b[0m") || strings.Contains(out, "\x1b[33m200") {
		t.Error("the statuses are not coloured as they should be")
	}
	if strings.Contains(out, "\x1b[2J") {
		t.Error("an escape sequence of a visitor reached the terminal")
	}
}

// timedTerminal is a terminal that notes when it was written to.
type timedTerminal struct {
	lockedBuffer
	tmu    sync.Mutex
	writes []time.Time
	cols   int
	rows   int
}

func (tt *timedTerminal) Write(p []byte) (int, error) {
	tt.tmu.Lock()
	tt.writes = append(tt.writes, time.Now())
	tt.tmu.Unlock()
	return tt.lockedBuffer.Write(p)
}

func (tt *timedTerminal) size() (int, int) {
	tt.tmu.Lock()
	defer tt.tmu.Unlock()
	return tt.cols, tt.rows
}

func (tt *timedTerminal) resize(cols, rows int) {
	tt.tmu.Lock()
	tt.cols, tt.rows = cols, rows
	tt.tmu.Unlock()
}

// A thousand requests a second for three seconds, and the terminal is resized
// in the middle: the view is drawn at most four times a second, each drawing
// fits the terminal as it is then, the client is never kept waiting, and the
// last ten requests are what stays on the screen.
func TestStatusView_BurstOfRequestSummaries(t *testing.T) {
	if testing.Short() {
		t.Skip("three seconds of requests")
	}
	cleanEnv(t)
	useProbes(t)
	tt := &timedTerminal{cols: 100, rows: 30}
	prev := openTerminal
	openTerminal = func() (terminal, bool) {
		return terminal{w: tt, size: tt.size, colour: true, restore: func() {}}, true
	}
	t.Cleanup(func() { openTerminal = prev })

	const total = 3000
	var slowest, took time.Duration
	useClient(t, func(_ context.Context, o client.Options) error {
		ob := o.Observer
		ob.State(client.StateConnected, "", 0)
		ob.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", URL: "https://relay.example.com/svc/p7baeh/"})
		start := time.Now()
		for i := 0; i < total; i++ {
			if i == total/2 {
				tt.resize(50, 20)
			}
			call := time.Now()
			ob.Connection("t1", call, "203.0.113.7:1")
			ob.Request("t1", call, "GET", fmt.Sprintf("/n/%d/%s", i, strings.Repeat("x", i%90)), 200+i%400)
			ob.ConnectionClosed("t1")
			if d := time.Since(call); d > slowest {
				slowest = d
			}
			// a thousand a second
			if wait := time.Duration(i+1)*time.Millisecond - time.Since(start); wait > 0 {
				time.Sleep(wait)
			}
		}
		took = time.Since(start)
		return nil
	})
	if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
		t.Fatal(err)
	}
	if slowest > 100*time.Millisecond {
		t.Errorf("one report to the view took %v", slowest)
	}
	if took > 6*time.Second {
		t.Errorf("3000 requests at 1000 a second took %v with the view on", took)
	}
	tt.tmu.Lock()
	writes := append([]time.Time(nil), tt.writes...)
	tt.tmu.Unlock()
	// Four a second, and the last one when the command ends.
	if most := int(took/(250*time.Millisecond)) + 3; len(writes) > most {
		t.Errorf("%d drawings in %v, at most %d are allowed", len(writes), took, most)
	}
	for i := 1; i < len(writes)-1; i++ {
		if gap := writes[i].Sub(writes[i-1]); gap < 200*time.Millisecond {
			t.Errorf("drawings %d and %d are %v apart", i-1, i, gap)
		}
	}
	lines := tt.lastView()
	if len(lines) > 19 {
		t.Errorf("%d lines on 20 rows after the resize", len(lines))
	}
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > 50 {
			t.Errorf("line of %d characters on 50 columns: %q", n, l)
		}
	}
	final := strings.Join(lines, "\n")
	if !strings.Contains(final, fmt.Sprintf("/n/%d/", total-1)) || strings.Contains(final, "/n/100/") {
		t.Errorf("the final view does not show the newest requests:\n%s", final)
	}
	if !strings.Contains(final, "0 open, 3000 total") {
		t.Errorf("counts after the burst:\n%s", final)
	}
	// Every drawing is whole: it starts at the left edge and ends on a new
	// line, and after the resize the old view is cleared first.
	all := tt.String()
	if strings.Count(all, "\x1b[?7l") != strings.Count(all, "\x1b[?7h") {
		t.Error("line wrapping was not turned on again as often as it was turned off")
	}
}

// When a visitor's connection finds the local service gone after its probe
// has ended, the address is probed again: otherwise nothing but another
// visitor would take the warning back.
func TestStatusView_ProbesAgainWhenAVisitorFindsTheServiceGone(t *testing.T) {
	cleanEnv(t)
	defer testutil.AssertNoGoroutineLeak(t)()
	term := useTerminal(t, nil, 100, 30, false)
	pr := useProbes(t)
	count := func(addr string) int {
		pr.mu.Lock()
		defer pr.mu.Unlock()
		n := 0
		for _, a := range pr.targets {
			if a == addr {
				n++
			}
		}
		return n
	}
	var during string
	useClient(t, func(_ context.Context, o client.Options) error {
		ob := o.Observer
		pr.wait(t, 2)
		ob.State(client.StateConnected, "", 0)
		ob.Registered(client.RegisteredTunnel{TunnelID: "t1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", URL: "https://relay.example.com/svc/p7baeh/"})
		pr.report("127.0.0.1:3000", true)
		ob.LocalTarget("127.0.0.1:3000", true) // a visitor got through: the probe ends
		first := pr.ctx["127.0.0.1:3000"]
		select {
		case <-first.Done():
		case <-time.After(2 * time.Second):
			t.Error("the first probe was not stopped")
		}
		// The service goes away and a visitor finds out.
		ob.LocalTarget("127.0.0.1:3000", false)
		pr.wait(t, 3)
		if n := count("127.0.0.1:3000"); n != 2 {
			t.Errorf("the address was probed %d times, want a second probe", n)
		}
		pr.mu.Lock()
		second := pr.ctx["127.0.0.1:3000"]
		pr.mu.Unlock()
		if second == first || second.Err() != nil {
			t.Error("no running probe for the address")
		}
		// Another failed visitor does not start a third.
		ob.LocalTarget("127.0.0.1:3000", false)
		time.Sleep(600 * time.Millisecond) // the view draws the warning
		during = strings.Join(term.out.lastView(), "\n")
		if n := count("127.0.0.1:3000"); n != 2 {
			t.Errorf("probed %d times", n)
		}
		// The service is back; the probe sees it before any visitor does.
		pr.report("127.0.0.1:3000", true)
		return nil
	})
	if err := runClient(context.Background(), viewCreds, viewTunnels, viewFlags); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(during, "nothing is listening on 127.0.0.1:3000") {
		t.Errorf("the warning was not shown while the service was gone:\n%s", during)
	}
	if final := strings.Join(term.out.lastView(), "\n"); strings.Contains(final, "nothing is listening on 127.0.0.1:3000") {
		t.Errorf("the warning stayed after the probe found the service again:\n%s", final)
	}
	for addr, ctx := range pr.ctx {
		if ctx.Err() == nil {
			t.Errorf("the probe of %s outlived the command", addr)
		}
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
					ob.Request("t1", time.Now(), "GET", "/burst", 200)
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
		t.Fatalf("160000 events took %v behind a blocked terminal", burst)
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
