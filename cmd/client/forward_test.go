package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
)

// recordForwards records the forwards of each run.
func (h *harness) recordForwards() *[][]client.ForwardSpec {
	var got [][]client.ForwardSpec
	h.onRun = func(ctx context.Context, _ []client.TunnelSpec) error {
		got = append(got, forwardsOf(ctx))
		return nil
	}
	return &got
}

func TestForwardCmd_BuildsTheForward(t *testing.T) {
	cases := []struct {
		args []string
		want client.ForwardSpec
	}{
		{[]string{"forward", "rdp"}, client.ForwardSpec{Target: "rdp", Addr: "127.0.0.1:13389"}},
		{[]string{"forward", "t0123456789", "2222"}, client.ForwardSpec{Target: "t0123456789", Addr: "127.0.0.1:2222"}},
		{[]string{"forward", "rdp", "--bind", "::1"}, client.ForwardSpec{Target: "rdp", Addr: "[::1]:13389"}},
		{[]string{"forward", "rdp", "3390", "--bind", "localhost"}, client.ForwardSpec{Target: "rdp", Addr: "localhost:3390"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			got := h.recordForwards()
			if code := h.exec(tc.args...); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
			if r := h.oneRun(); len(r.tunnels) != 0 {
				t.Fatalf("tunnels %+v", r.tunnels)
			}
			if !reflect.DeepEqual(*got, [][]client.ForwardSpec{{tc.want}}) {
				t.Fatalf("forwards = %+v, want %+v", *got, tc.want)
			}
			h.noToken()
		})
	}
}

func TestForwardCmd_AnotherMachineMayReachAWideBind(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	errOut := useStderr(t)
	h.recordForwards()
	if code := h.exec("forward", "rdp", "--bind", "0.0.0.0"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(errOut.String(), "WARNING") || !strings.Contains(errOut.String(), "0.0.0.0:13389") {
		t.Fatalf("no warning for a listener other machines can reach: %q", errOut.String())
	}
}

func TestForwardCmd_UsageErrors(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"forward"}, "burrow forward needs a target"},
		{[]string{"forward", "a", "1", "2"}, "burrow forward needs a target"},
		{[]string{"forward", "rdp", "abc"}, "the local port must be between 1 and 65535"},
		{[]string{"forward", "rdp", "0"}, "the local port must be between 1 and 65535"},
		{[]string{"forward", "rdp", "70000"}, "the local port must be between 1 and 65535"},
		{[]string{"forward", "rdp", "--bind", "example.com"}, "the bind address must be an IP address or localhost"},
		{[]string{"forward", strings.Repeat("x", 201)}, "target must be 1 to 200 characters"},
	}
	for _, tc := range cases {
		h := newHarness(t)
		h.signIn()
		if code := h.exec(tc.args...); code != exitUsage || !strings.Contains(h.stderr.String(), tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want %d and %q", tc.args, code, h.stderr.String(), exitUsage, tc.want)
		}
		if len(h.runs) != 0 {
			t.Errorf("%v: the client was started", tc.args)
		}
	}
}

// D13: a forward carries a private service; under --insecure the relay is
// not verified, so it runs only when --insecure-forward accepts that.
func TestForwardCmd_InsecureNeedsInsecureForward(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code := h.exec("--insecure", "forward", "rdp"); code != exitUsage || !strings.Contains(h.stderr.String(), "--insecure-forward") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if len(h.runs) != 0 {
		t.Fatal("the client was started")
	}
	got := h.recordForwards()
	if code := h.exec("--insecure", "--insecure-forward", "forward", "rdp"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if len(*got) != 1 {
		t.Fatal("the forward did not run")
	}
	// `up` with forwards in the file is held to the same rule.
	dir := t.TempDir()
	file := filepath.Join(dir, "burrow.yaml")
	writeAt(t, file, "forwards:\n  - { target: rdp }\n")
	h.runs = nil
	if code := h.exec("--insecure", "up", "--file", file); code != exitUsage || !strings.Contains(h.stderr.String(), "--insecure-forward") {
		t.Fatalf("up: exit %d: %s", code, h.stderr.String())
	}
	if len(h.runs) != 0 {
		t.Fatal("up was started")
	}
}

func TestUp_RunsTheForwardsOfTheFile(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	got := h.recordForwards()
	dir := t.TempDir()
	file := filepath.Join(dir, "burrow.yaml")
	writeAt(t, file, "services:\n  - { name: rdp, local: 127.0.0.1:3389, access: private }\n"+
		"forwards:\n  - { target: db, local_port: 15432 }\n")
	if code := h.exec("up", "--file", file); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	r := h.oneRun()
	if len(r.tunnels) != 1 || r.tunnels[0].Access != "private" {
		t.Fatalf("tunnels %+v", r.tunnels)
	}
	if !reflect.DeepEqual(*got, [][]client.ForwardSpec{{{Target: "db", Addr: "127.0.0.1:15432"}}}) {
		t.Fatalf("forwards %+v", *got)
	}
}

// runClient hands the forwards of the command to the client.
func TestRunClient_PassesTheForwards(t *testing.T) {
	cleanEnv(t)
	s := captureStart(t)
	fw := []client.ForwardSpec{{Target: "rdp", Addr: "127.0.0.1:13389"}}
	if err := runClient(withForwards(context.Background(), fw), viewCreds, nil, globalFlags{logLevel: "info", logFormat: "text"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.opts.Forwards, fw) {
		t.Fatalf("forwards %+v", s.opts.Forwards)
	}
}

// D2: `burrow tcp` keeps a new service private unless --access says other.
func TestTCP_AccessModes(t *testing.T) {
	cases := []struct {
		args []string
		want client.TunnelSpec
	}{
		{[]string{"tcp", "3389"}, client.TunnelSpec{Name: "kohns-laptop-3389", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"}},
		{[]string{"tcp", "3389", "--access", "ip"}, client.TunnelSpec{Name: "kohns-laptop-3389", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "ip"}},
		{[]string{"tcp", "5432", "--access", "open", "--remote", "9000", "--name", "pg"}, client.TunnelSpec{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000, Access: "open"}},
	}
	for _, tc := range cases {
		h := newHarness(t)
		h.signIn()
		if code := h.exec(tc.args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, h.stderr.String())
		}
		if r := h.oneRun(); !reflect.DeepEqual(r.tunnels, []client.TunnelSpec{tc.want}) {
			t.Fatalf("%v: tunnels %+v, want %+v", tc.args, r.tunnels, tc.want)
		}
	}
	bad := []struct {
		args []string
		want string
	}{
		{[]string{"tcp", "3389", "--access", "login"}, "--access must be one of: open, ip, private"},
		{[]string{"http", "3000", "--access", "private"}, "--access must be one of: open, login, api-key"},
		{[]string{"tcp", "3389", "--remote", "9000"}, "--remote needs --access open or ip"},
		{[]string{"tcp", "3389", "--access", "private", "--remote", "9000"}, "--remote needs --access open or ip"},
	}
	for _, tc := range bad {
		h := newHarness(t)
		h.signIn()
		if code := h.exec(tc.args...); code != exitUsage || !strings.Contains(h.stderr.String(), tc.want) {
			t.Errorf("%v: exit %d, stderr %q", tc.args, code, h.stderr.String())
		}
	}
}

// What a tcp registration means for the person, said once per run.
func TestNotes_TCPServices(t *testing.T) {
	private := client.RegisteredTunnel{TunnelID: "t1", Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", AccessMode: "private", Slug: "t0123456789", Wished: "private"}
	if got := tcpNote(private, "relay.example.com", true); got != "This service is private: the relay opened no public port. "+
		"Reach it from another machine with: burrow forward t0123456789 13389" {
		t.Errorf("private: %s", got)
	}
	open := client.RegisteredTunnel{TunnelID: "t1", Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", AccessMode: "open", RemotePort: 9001, Wished: "open"}
	if got := tcpNote(open, "relay.example.com", true); !strings.HasPrefix(got, "This service is open to anyone at relay.example.com:9001. ") ||
		!strings.Contains(got, "Port 3389 (remote desktop)") || !strings.Contains(got, "--access private") {
		t.Errorf("open rdp: %s", got)
	}
	ip := open
	ip.AccessMode, ip.LocalAddr, ip.Name = "ip", "127.0.0.1:22", "ssh"
	if got := tcpNote(ip, "relay.example.com", false); !strings.HasPrefix(got, "Service ssh is reachable at relay.example.com:9001 from the addresses allowed in the dashboard. ") ||
		!strings.Contains(got, "Port 22 (SSH)") {
		t.Errorf("ip ssh: %s", got)
	}
	web := open
	web.LocalAddr = "127.0.0.1:8080"
	if got := tcpNote(web, "relay.example.com", true); got != "This service is open to anyone at relay.example.com:9001." {
		t.Errorf("open web: %s", got)
	}
	// Asked for open, kept private in the dashboard.
	stricter := private
	stricter.Wished = "open"
	if got := tcpNote(stricter, "relay.example.com", true); !strings.Contains(got, "--access open was asked for; the dashboard keeps the service private.") {
		t.Errorf("stricter: %s", got)
	}
	// What the relay says goes to a terminal.
	evil := private
	evil.Slug = "t\x1b[2J"
	if got := tcpNote(evil, "relay.example.com", true); strings.ContainsAny(got, "\x1b") || !strings.Contains(got, "burrow forward rdp") {
		t.Errorf("evil slug: %q", got)
	}

	// In log lines, once.
	cleanEnv(t)
	noTerminal(t)
	errOut := useStderr(t)
	fgRun(t, true, globalFlags{logLevel: "info", logFormat: "text"}, []client.TunnelSpec{{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"}}, func(ob client.Observer) {
		for range 2 {
			ob.State(client.StateConnected, "", 0)
			ob.Registered(private)
			ob.State(client.StateReconnecting, "session closed", time.Second)
		}
	})
	if n := strings.Count(errOut.String(), "burrow forward t0123456789 13389"); n != 1 {
		t.Fatalf("the hint was printed %d times:\n%s", n, errOut.String())
	}
}

func TestReport_TCPAccessAndForwards(t *testing.T) {
	var out strings.Builder
	code := report(&out, &client.AccessNotAppliedError{Name: "rdp", Access: "private", Type: "tcp", NotSent: true})
	if code != exitGeneral || !strings.Contains(out.String(), "This relay is too old to keep a tcp service private") ||
		!strings.Contains(out.String(), "no port was opened") {
		t.Fatalf("not sent: %d %s", code, out.String())
	}
	out.Reset()
	report(&out, &client.AccessNotAppliedError{Name: "rdp", Access: "ip", Type: "tcp", RemotePort: 9001})
	if !strings.Contains(out.String(), "did not confirm access mode ip") || !strings.Contains(out.String(), "port 9001") {
		t.Fatalf("unconfirmed: %s", out.String())
	}
	out.Reset()
	if code := report(&out, &client.ForwardUnsupportedError{Target: "rdp"}); code != exitGeneral || !strings.Contains(out.String(), "relay too old") || strings.HasPrefix(out.String(), "error:") {
		t.Fatalf("too old: %d %s", code, out.String())
	}
	out.Reset()
	report(&out, &client.ForwardRefusedError{Target: "rdp", Code: "not_found", Message: `tcp service "rdp" not found, or you may not reach it`})
	if strings.HasPrefix(out.String(), "error:") || !strings.Contains(out.String(), `"rdp" not found`) {
		t.Fatalf("not found: %s", out.String())
	}
}
