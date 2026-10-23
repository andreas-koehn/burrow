package client

// burrow forward (native tcp, P05) and the access modes of tcp tunnels, against
// the real relay in process: a home client serves a private tcp service and a
// forward client reaches it through a local listener.

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/server"
)

// svcRelay keeps the tcp services of the in-process relay: it creates each in
// the mode asked for (open without a wish), keeps that mode, and lets any
// signed-in user forward to a service by its name or its slug s-<name>.
type svcRelay struct {
	mu    sync.Mutex
	modes map[string]string // name → stored mode
}

func (r *svcRelay) Resolve(context.Context, string, string, string) (string, string, error) {
	return "", "", errors.New("no http services in this test")
}

func (r *svcRelay) ResolveTCP(_ context.Context, _, name string, o server.ResolveOptions) (server.Resolved, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mode, ok := r.modes[name]
	if !ok {
		mode = o.Access
		if mode == "" {
			mode = o.AccessDefault
		}
		if mode == "" {
			mode = "open"
		}
		r.modes[name] = mode
	}
	return server.Resolved{ServiceID: "svc-" + name, Slug: "s-" + name, AccessMode: mode, Created: !ok}, nil
}

func (r *svcRelay) TCPPortReservations(context.Context) (map[int]server.TCPPortHold, error) {
	return map[int]server.TCPPortHold{}, nil
}

func (r *svcRelay) SetServiceTCPPort(context.Context, string, int, bool, string) error { return nil }

func (r *svcRelay) AuthorizeForward(_ context.Context, _, target string) (server.ForwardTarget, error) {
	name := strings.TrimPrefix(target, "s-")
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.modes[name]; !ok {
		return server.ForwardTarget{}, server.ErrForwardNotFound
	}
	return server.ForwardTarget{ServiceID: "svc-" + name, Name: name}, nil
}

func (r *svcRelay) set(name, mode string) {
	r.mu.Lock()
	r.modes[name] = mode
	r.mu.Unlock()
}

// forwardRelay is the real relay with tcp services and forward streams.
type forwardRelay struct {
	srv  *server.Server
	pool *x509.CertPool
	svcs *svcRelay
}

func startForwardRelay(t *testing.T, forwards bool) *forwardRelay {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	svcs := &svcRelay{modes: map[string]string{}}
	o := server.Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: testAuth(),
		PublicBind: "127.0.0.1", PortMin: 19610, PortMax: 19619, Services: svcs,
	}
	if forwards {
		o.Forwards = svcs
	}
	s, err := server.New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	t.Cleanup(func() { cancel(); s.Wait() })
	if !waitTrue(func() bool { return s.Addr() != "" }, 2*time.Second) {
		t.Fatal("the relay did not start")
	}
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return &forwardRelay{srv: s, pool: pool, svcs: svcs}
}

func (r *forwardRelay) options(server string, tunnels ...TunnelSpec) Options {
	if server == "" {
		server = r.srv.Addr()
	}
	return Options{
		Server: server, Token: "bur_test_0000", RootCAs: r.pool, ServerName: "localhost",
		Tunnels: tunnels, Logger: slog.New(slog.DiscardHandler),
	}
}

// start runs a client until the test ends and returns what Run returned once
// it did.
func start(t *testing.T, o Options) (*Client, <-chan error) {
	t.Helper()
	c := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	res, done := make(chan error, 1), make(chan struct{})
	go func() { res <- c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the client did not end")
		}
	})
	return c, res
}

// forwardObserver records the states of the forwards.
type forwardObserver struct {
	recObserver
	mu     sync.Mutex
	states []ForwardState
	regs   []RegisteredTunnel
	refs   []error
}

func (f *forwardObserver) Forward(s ForwardState) {
	f.mu.Lock()
	f.states = append(f.states, s)
	f.mu.Unlock()
}

func (f *forwardObserver) Registered(t RegisteredTunnel) {
	f.mu.Lock()
	f.regs = append(f.regs, t)
	f.mu.Unlock()
}

func (f *forwardObserver) Refused(_ string, err error) {
	f.mu.Lock()
	f.refs = append(f.refs, err)
	f.mu.Unlock()
}

func (f *forwardObserver) last() (ForwardState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.states) == 0 {
		return ForwardState{}, false
	}
	return f.states[len(f.states)-1], true
}

// addr waits for the forward to listen and returns its address.
func (f *forwardObserver) addr(t *testing.T) string {
	t.Helper()
	var a string
	if !waitTrue(func() bool {
		s, ok := f.last()
		a = s.Addr
		return ok && a != ""
	}, 3*time.Second) {
		t.Fatal("the forward never listened")
	}
	return a
}

// greetingEcho answers every connection with "hi " and then echoes it.
func greetingEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte("hi "))
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// through connects to the forward at addr, reads the greeting and sends
// payload back and forth.
func through(t *testing.T, addr string, payload []byte) error {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	hi := make([]byte, 3)
	if _, err := io.ReadFull(c, hi); err != nil {
		return err
	}
	if string(hi) != "hi " {
		return errors.New("greeting " + string(hi))
	}
	go func() { _, _ = c.Write(payload) }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if string(got) != string(payload) {
		return errors.New("the echo differs")
	}
	return nil
}

// home starts the client that serves rdp privately and waits until it does.
func (r *forwardRelay) home(t *testing.T) {
	t.Helper()
	o := r.options("", TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: greetingEcho(t), Access: "private"})
	c, _ := start(t, o)
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("the home client never registered")
	}
}

func TestForward_BytesGoThroughBothWays(t *testing.T) {
	r := startForwardRelay(t, true)
	r.home(t)

	obs := &forwardObserver{}
	o := r.options("")
	o.Forwards = []ForwardSpec{{Target: "s-rdp", Addr: "127.0.0.1:0"}}
	o.Observer = obs
	start(t, o)
	addr := obs.addr(t)
	if !waitTrue(func() bool { s, _ := obs.last(); return s.Connected }, 3*time.Second) {
		t.Fatal("the forward never had a session")
	}
	payload := []byte(strings.Repeat("0123456789abcdef", 64<<10/16))
	if err := through(t, addr, payload); err != nil {
		t.Fatal(err)
	}
	// One stream per local connection: two at once both get through.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { errs <- through(t, addr, []byte("again")) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !waitTrue(func() bool { s, _ := obs.last(); return s.Total == 3 && s.Open == 0 }, 3*time.Second) {
		s, _ := obs.last()
		t.Fatalf("forward state %+v, want 3 total and none open", s)
	}
}

// cutProxy stands between a client and the relay; cut hangs up every
// connection, and while it is down new ones are hung up at once.
type cutProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	down  bool
	conns []net.Conn
}

func startCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: target}
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			p.mu.Lock()
			down := p.down
			if !down {
				p.conns = append(p.conns, c)
			}
			p.mu.Unlock()
			if down {
				_ = c.Close()
				continue
			}
			go func() {
				up, e := net.Dial("tcp", target)
				if e != nil {
					_ = c.Close()
					return
				}
				p.mu.Lock()
				p.conns = append(p.conns, up)
				p.mu.Unlock()
				go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
				_, _ = io.Copy(c, up)
				_ = c.Close()
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); p.cut(false) })
	return p
}

func (p *cutProxy) cut(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *cutProxy) up() {
	p.mu.Lock()
	p.down = false
	p.mu.Unlock()
}

func TestForward_TheListenerSurvivesAReconnect(t *testing.T) {
	r := startForwardRelay(t, true)
	r.home(t)
	proxy := startCutProxy(t, r.srv.Addr())

	obs := &forwardObserver{}
	o := r.options(proxy.ln.Addr().String())
	o.Forwards = []ForwardSpec{{Target: "rdp", Addr: "127.0.0.1:0"}}
	o.Observer = obs
	start(t, o)
	addr := obs.addr(t)
	connected := func() bool { s, _ := obs.last(); return s.Connected }
	if !waitTrue(connected, 3*time.Second) {
		t.Fatal("the forward never had a session")
	}
	if err := through(t, addr, []byte("before")); err != nil {
		t.Fatal(err)
	}

	// While the relay cannot be reached, a local connection is taken and
	// closed at once: nothing waits for the relay to come back.
	proxy.cut(true)
	if !waitTrue(func() bool { return !connected() }, 3*time.Second) {
		t.Fatal("the forward did not notice the lost session")
	}
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("the listener is gone while disconnected: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a connection while disconnected: read %d, %v; want it closed at once", n, err)
	}
	_ = c.Close()
	if !waitTrue(func() bool { s, _ := obs.last(); return strings.Contains(s.Problem, "not connected") }, 2*time.Second) {
		t.Fatal("the closed connection was not told")
	}

	// Back: the same listener forwards again.
	proxy.up()
	if !waitTrue(connected, 10*time.Second) {
		t.Fatal("the forward did not reconnect")
	}
	if s, _ := obs.last(); s.Problem != "" {
		t.Fatalf("still told %q after the reconnect", s.Problem)
	}
	if err := through(t, addr, []byte("after")); err != nil {
		t.Fatal(err)
	}
}

func TestForward_ARelayWithoutTheCapabilityIsTooOld(t *testing.T) {
	r := startForwardRelay(t, false) // keeps tcp services, takes no forwards
	obs := &forwardObserver{}
	o := r.options("")
	o.Forwards = []ForwardSpec{{Target: "rdp", Addr: "127.0.0.1:0"}}
	o.Observer = obs
	_, res := start(t, o)
	var err error
	select {
	case err = <-res:
	case <-time.After(5 * time.Second):
		t.Fatal("the forward kept running against a relay that cannot forward")
	}
	var fu *ForwardUnsupportedError
	if !errors.As(err, &fu) || !strings.Contains(err.Error(), "relay too old") {
		t.Fatalf("error = %v, want a ForwardUnsupportedError that says relay too old", err)
	}
	s, _ := obs.last()
	if !s.Stopped || !strings.Contains(s.Problem, "relay too old") {
		t.Fatalf("forward state %+v", s)
	}
	if c, err := net.DialTimeout("tcp", s.Addr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the listener is still open")
	}
}

func TestForward_NotFoundStopsTheForwardAndOfflineDoesNot(t *testing.T) {
	r := startForwardRelay(t, true)
	obs := &forwardObserver{}
	o := r.options("")
	o.Forwards = []ForwardSpec{{Target: "nope", Addr: "127.0.0.1:0"}, {Target: "rdp", Addr: "127.0.0.1:0"}}
	o.Observer = obs
	_, res := start(t, o)
	if !waitTrue(func() bool {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		n := 0
		for _, s := range obs.states {
			if s.Connected && s.Addr != "" {
				n |= 1 << s.Index
			}
		}
		return n == 3
	}, 3*time.Second) {
		t.Fatal("the forwards never had a session")
	}
	addrOf := func(i int) string {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		for _, s := range obs.states {
			if s.Index == i && s.Addr != "" {
				return s.Addr
			}
		}
		return ""
	}
	stateOf := func(i int) ForwardState {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		var out ForwardState
		for _, s := range obs.states {
			if s.Index == i {
				out = s
			}
		}
		return out
	}

	// rdp exists but nobody serves it: offline, and the forward stays.
	r.svcs.set("rdp", "private")
	if err := through(t, addrOf(1), []byte("x")); err == nil {
		t.Fatal("an offline service answered")
	}
	if !waitTrue(func() bool { return strings.Contains(stateOf(1).Problem, "offline") }, 3*time.Second) {
		t.Fatalf("state %+v", stateOf(1))
	}
	if stateOf(1).Stopped {
		t.Fatal("offline stopped the forward")
	}

	// nope is no service: not found ends that forward alone.
	_ = through(t, addrOf(0), []byte("x"))
	if !waitTrue(func() bool { return stateOf(0).Stopped }, 3*time.Second) {
		t.Fatalf("state %+v", stateOf(0))
	}
	if p := stateOf(0).Problem; !strings.Contains(p, `"nope"`) || !strings.Contains(p, "not found") {
		t.Fatalf("problem %q", p)
	}
	select {
	case err := <-res:
		t.Fatalf("the run ended with one forward left: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// The other forward still works once its service is served.
	r.home(t)
	if err := through(t, addrOf(1), []byte("works")); err != nil {
		t.Fatal(err)
	}
}

func TestForward_TheLastForwardEndingEndsTheRun(t *testing.T) {
	r := startForwardRelay(t, true)
	obs := &forwardObserver{}
	o := r.options("")
	o.Forwards = []ForwardSpec{{Target: "nope", Addr: "127.0.0.1:0"}}
	o.Observer = obs
	_, res := start(t, o)
	addr := obs.addr(t)
	if !waitTrue(func() bool { s, _ := obs.last(); return s.Connected }, 3*time.Second) {
		t.Fatal("no session")
	}
	_ = through(t, addr, []byte("x"))
	select {
	case err := <-res:
		var fr *ForwardRefusedError
		if !errors.As(err, &fr) || fr.Code != proto.CodeNotFound || fr.Target != "nope" {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
	}
}

func TestForward_AListenerThatCannotOpenEndsTheRun(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	o := Options{Server: "127.0.0.1:1", Token: "bur_test_0000", Logger: slog.New(slog.DiscardHandler),
		Forwards: []ForwardSpec{{Target: "rdp", Addr: taken.Addr().String()}}}
	err, ended := runFor(t, o, 3*time.Second)
	if !ended || err == nil || !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Fatalf("error = %v, ended %v", err, ended)
	}
}

// --- access modes of tcp tunnels (D9 and the wish against the stored mode) ---

func TestTCPAccess_APrivateWishFacingAnOldRelayServesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, auth, answer string
	}{
		// No capability: the registration is never sent, so no port opens.
		{"no capability", oldAuthOK, `{"ok":true,"tunnel_id":"t1","remote_port":9001,"ignored":["access"]}`},
		// A relay that says it can and then does not answer the mode.
		{"no access mode", `{"ok":true,"session_id":"s1","capabilities":["tcp_access"]}`, `{"ok":true,"tunnel_id":"t1","remote_port":9001}`},
		{"access ignored", `{"ok":true,"session_id":"s1","capabilities":["tcp_access"]}`, `{"ok":true,"tunnel_id":"t1","remote_port":9001,"access_mode":"open","ignored":["access"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, access := range []string{"private", "ip"} {
				r := startRawRelay(t, tc.auth, func(proto.TunnelRegister) string { return tc.answer })
				o := r.options(TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: access})
				obs := &forwardObserver{}
				o.Observer = obs
				err, ended := runFor(t, o, 3*time.Second)
				var na *AccessNotAppliedError
				if !ended || !errors.As(err, &na) || na.Type != "tcp" || na.Access != access {
					t.Fatalf("%s: error = %v, ended %v", access, err, ended)
				}
				auths, regs := r.sent()
				if len(auths) != 1 {
					t.Fatalf("%s: %d sessions; want one and no retry", access, len(auths))
				}
				if tc.auth == oldAuthOK {
					if len(regs) != 0 || !na.NotSent {
						t.Fatalf("%s: registrations sent to an old relay: %v", access, regs)
					}
				}
				obs.mu.Lock()
				served := len(obs.regs)
				obs.mu.Unlock()
				if served != 0 {
					t.Fatalf("%s: the observer was told of a served tunnel", access)
				}
			}
		})
	}
}

func TestTCPAccess_OnAnOldRelayTheOtherTunnelsKeepRunning(t *testing.T) {
	r := startRawRelay(t, oldAuthOK, func(req proto.TunnelRegister) string {
		return `{"ok":true,"tunnel_id":"t-` + req.Name + `","remote_port":9001,"ignored":["access"]}`
	})
	obs := &forwardObserver{}
	o := r.options(
		TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"},
		TunnelSpec{Name: "web", Type: "tcp", LocalAddr: "127.0.0.1:8080"},
	)
	o.Observer = obs
	if err, ended := runFor(t, o, 2*time.Second); ended {
		t.Fatalf("the run ended: %v", err)
	}
	if _, regs := r.sent(); len(regs) != 1 || !strings.Contains(regs[0], `"web"`) {
		t.Fatalf("registrations %v; want web alone", regs)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	var na *AccessNotAppliedError
	if len(obs.refs) != 1 || !errors.As(obs.refs[0], &na) || na.Name != "rdp" {
		t.Fatalf("refusals told: %v", obs.refs)
	}
}

func TestTCPAccess_AStricterWishIsRefusedWithTheWayOut(t *testing.T) {
	r := startForwardRelay(t, true)
	r.svcs.set("rdp", "open")
	o := r.options("", TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"})
	o.StopOnRefusal = true
	err, ended := runFor(t, o, 3*time.Second)
	var re *RefusedError
	if !ended || !errors.As(err, &re) || re.Code != proto.CodeAccessInvalid ||
		!strings.Contains(re.Message, "restart with --access open") {
		t.Fatalf("error = %v, ended %v", err, ended)
	}
}

func TestTCPAccess_ALooserWishIsServedInTheStoredMode(t *testing.T) {
	r := startForwardRelay(t, true)
	r.svcs.set("rdp", "private")
	obs := &forwardObserver{}
	o := r.options("", TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: greetingEcho(t), Access: "open"})
	o.Observer = obs
	c, _ := start(t, o)
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("never registered")
	}
	if !waitTrue(func() bool { obs.mu.Lock(); defer obs.mu.Unlock(); return len(obs.regs) == 1 }, 2*time.Second) {
		t.Fatal("the observer was not told")
	}
	obs.mu.Lock()
	reg := obs.regs[0]
	obs.mu.Unlock()
	if reg.AccessMode != "private" || reg.RemotePort != 0 || reg.Slug != "s-rdp" || reg.Wished != "open" {
		t.Fatalf("registered %+v", reg)
	}
}

// A default (burrow tcp without --access, a burrow.yaml entry without
// access:) is sent and applied by the relay only to a new service: an
// existing open service is served open and not refused.
func TestTCPAccess_TheDefaultIsAppliedOnlyToANewService(t *testing.T) {
	r := startForwardRelay(t, true)
	r.svcs.set("ssh", "open")
	obs := &forwardObserver{}
	o := r.options("",
		TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: greetingEcho(t), AccessDefault: "private"},
		TunnelSpec{Name: "ssh", Type: "tcp", LocalAddr: greetingEcho(t), AccessDefault: "private"},
	)
	o.Observer = obs
	c, _ := start(t, o)
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("never registered")
	}
	if !waitTrue(func() bool { obs.mu.Lock(); defer obs.mu.Unlock(); return len(obs.regs) == 2 }, 2*time.Second) {
		t.Fatalf("registered %v, refused %v", obs.regs, obs.refs)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	modes := map[string]string{}
	for _, reg := range obs.regs {
		modes[reg.Name] = reg.AccessMode
		if reg.Wished != "" {
			t.Errorf("%s: a default is no wish: %+v", reg.Name, reg)
		}
	}
	if modes["rdp"] != "private" || modes["ssh"] != "open" {
		t.Fatalf("modes %v", modes)
	}
}

// A private default facing an old relay is never sent: the relay would
// create the service open.
func TestTCPAccess_APrivateDefaultFacingAnOldRelayIsNotSent(t *testing.T) {
	r := startRawRelay(t, oldAuthOK, func(proto.TunnelRegister) string {
		return `{"ok":true,"tunnel_id":"t1","remote_port":9001}`
	})
	o := r.options(TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", AccessDefault: "private"})
	err, ended := runFor(t, o, 3*time.Second)
	var na *AccessNotAppliedError
	if !ended || !errors.As(err, &na) || !na.NotSent || na.Access != "private" {
		t.Fatalf("error = %v, ended %v", err, ended)
	}
	if _, regs := r.sent(); len(regs) != 0 {
		t.Fatalf("registrations sent to an old relay: %v", regs)
	}
}

// Defence in depth: a relay that answers a mode less strict than the wish,
// or than the default of a service it says it created, is not served.
func TestTCPAccess_AModeLessStrictThanAskedForStops(t *testing.T) {
	const auth = `{"ok":true,"session_id":"s1","capabilities":["tcp_access"]}`
	for _, tc := range []struct {
		name   string
		spec   TunnelSpec
		answer string
		stop   bool
	}{
		{"wish private, open", TunnelSpec{Access: "private"}, `{"ok":true,"tunnel_id":"t1","remote_port":9001,"access_mode":"open"}`, true},
		{"wish private, ip", TunnelSpec{Access: "private"}, `{"ok":true,"tunnel_id":"t1","remote_port":9001,"access_mode":"ip"}`, true},
		{"wish ip, open", TunnelSpec{Access: "ip"}, `{"ok":true,"tunnel_id":"t1","remote_port":9001,"access_mode":"open"}`, true},
		{"default private, created open", TunnelSpec{AccessDefault: "private"}, `{"ok":true,"tunnel_id":"t1","remote_port":9001,"access_mode":"open","created":true}`, true},
		{"default private, existing open", TunnelSpec{AccessDefault: "private"}, `{"ok":true,"tunnel_id":"t1","remote_port":9001,"access_mode":"open"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startRawRelay(t, auth, func(proto.TunnelRegister) string { return tc.answer })
			spec := tc.spec
			spec.Name, spec.Type, spec.LocalAddr = "rdp", "tcp", "127.0.0.1:3389"
			err, ended := runFor(t, r.options(spec), 2*time.Second)
			var na *AccessNotAppliedError
			if tc.stop && (!ended || !errors.As(err, &na) || na.Type != "tcp") {
				t.Fatalf("error = %v, ended %v", err, ended)
			}
			if !tc.stop && ended {
				t.Fatalf("the run ended: %v", err)
			}
		})
	}
}

// flakyListener fails its first Accepts with an error that is no timeout.
type flakyListener struct {
	net.Listener
	mu    sync.Mutex
	fails int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	fail := l.fails > 0
	if fail {
		l.fails--
	}
	l.mu.Unlock()
	if fail {
		return nil, errors.New("accept: too many open files")
	}
	return l.Listener.Accept()
}

func TestForward_AcceptErrorsAreRetried(t *testing.T) {
	prev := listenTCP
	listenTCP = func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if err != nil {
			return nil, err
		}
		return &flakyListener{Listener: ln, fails: 3}, nil
	}
	t.Cleanup(func() { listenTCP = prev })

	r := startForwardRelay(t, true)
	r.home(t)
	obs := &forwardObserver{}
	o := r.options("")
	o.Forwards = []ForwardSpec{{Target: "rdp", Addr: "127.0.0.1:0"}}
	o.Observer = obs
	start(t, o)
	addr := obs.addr(t)
	if !waitTrue(func() bool { s, _ := obs.last(); return s.Connected }, 3*time.Second) {
		t.Fatal("no session")
	}
	if err := through(t, addr, []byte("still accepting")); err != nil {
		t.Fatal(err)
	}
}

// closedSoon reports whether the connection is closed by the other side
// within d.
func closedSoon(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 64)
	for {
		if _, err := c.Read(buf); err != nil {
			return !errors.Is(err, os.ErrDeadlineExceeded)
		}
	}
}

func TestForward_ACloseOnEitherSideEndsTheOther(t *testing.T) {
	r := startForwardRelay(t, true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	served := make(chan net.Conn, 4)
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			served <- c
		}
	}()
	home, _ := start(t, r.options("", TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: ln.Addr().String(), Access: "private"}))
	if !waitTrue(home.Registered, 3*time.Second) {
		t.Fatal("home never registered")
	}
	obs := &forwardObserver{}
	o := r.options("")
	o.Forwards = []ForwardSpec{{Target: "rdp", Addr: "127.0.0.1:0"}}
	o.Observer = obs
	start(t, o)
	addr := obs.addr(t)
	if !waitTrue(func() bool { s, _ := obs.last(); return s.Connected }, 3*time.Second) {
		t.Fatal("no session")
	}
	open := func() (local, remote net.Conn) {
		t.Helper()
		local, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = local.Close() })
		_, _ = local.Write([]byte("x")) // the service sees the connection
		select {
		case remote = <-served:
		case <-time.After(5 * time.Second):
			t.Fatal("the service was not reached")
		}
		t.Cleanup(func() { _ = remote.Close() })
		return local, remote
	}

	// The local program hangs up: the service's connection ends.
	local, remote := open()
	_ = local.Close()
	if !closedSoon(remote, 5*time.Second) {
		t.Fatal("the service's connection stayed open after the local close")
	}
	// The service hangs up: the local connection ends.
	local, remote = open()
	_ = remote.Close()
	if !closedSoon(local, 5*time.Second) {
		t.Fatal("the local connection stayed open after the service closed")
	}
}

func TestForward_RateLimitedHoldsOffNewConnections(t *testing.T) {
	prev := forwardCooldown
	forwardCooldown = 700 * time.Millisecond
	t.Cleanup(func() { forwardCooldown = prev })

	r := startForwardRelay(t, true)
	r.svcs.set("rdp", "private") // exists, nobody serves it: every open is answered offline
	obs := &forwardObserver{}
	o := r.options("")
	o.Forwards = []ForwardSpec{{Target: "rdp", Addr: "127.0.0.1:0"}}
	o.Observer = obs
	start(t, o)
	addr := obs.addr(t)
	if !waitTrue(func() bool { s, _ := obs.last(); return s.Connected }, 3*time.Second) {
		t.Fatal("no session")
	}
	problem := func() string { s, _ := obs.last(); return s.Problem }
	dial := func() {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if !closedSoon(c, 5*time.Second) {
			t.Fatal("a refused forward kept the local connection open")
		}
	}
	// The relay takes a burst of 10 opens; the eleventh is rate limited.
	limited := false
	for i := 0; i < 14 && !limited; i++ {
		dial()
		limited = waitTrue(func() bool { return strings.Contains(problem(), "limits forwards") }, 300*time.Millisecond)
	}
	if !limited {
		t.Fatalf("never rate limited; problem %q", problem())
	}
	// Right after, a local connection is closed without asking the relay.
	dial()
	if !waitTrue(func() bool { return strings.Contains(problem(), "waiting") }, 2*time.Second) {
		t.Fatalf("problem %q, want the hold-off", problem())
	}
	// After the hold-off the relay is asked again.
	time.Sleep(forwardCooldown + 100*time.Millisecond)
	dial()
	if !waitTrue(func() bool { return !strings.Contains(problem(), "waiting") }, 2*time.Second) {
		t.Fatalf("still holding off: %q", problem())
	}
}

func TestForward_AListenAddressInUseSaysSo(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	_, port, _ := net.SplitHostPort(taken.Addr().String())
	o := Options{Server: "127.0.0.1:1", Token: "bur_test_0000", Logger: slog.New(slog.DiscardHandler),
		Forwards: []ForwardSpec{{Target: "rdp", Addr: taken.Addr().String()}}}
	err, _ = runFor(t, o, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "local port "+port+" is in use; pick another") {
		t.Fatalf("error = %v", err)
	}
}
