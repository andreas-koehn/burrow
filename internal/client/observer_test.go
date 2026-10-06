package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proto"
)

// recObserver records every call. delay makes each method slow.
type recObserver struct {
	delay time.Duration

	mu     sync.Mutex
	events []string
	regs   []RegisteredTunnel
	rtts   []time.Duration
	retry  []time.Duration
	connAt []time.Time
	reqAt  []time.Time
}

func (r *recObserver) add(e string, f func()) {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	if f != nil {
		f()
	}
}

func (r *recObserver) State(s ConnState, detail string, retryIn time.Duration) {
	e := "state:" + string(s)
	if detail != "" {
		e += ":" + detail
	}
	r.add(e, func() {
		if s == StateReconnecting {
			r.retry = append(r.retry, retryIn)
		}
	})
}
func (r *recObserver) Registered(t RegisteredTunnel) {
	r.add("registered:"+t.Name, func() { r.regs = append(r.regs, t) })
}
func (r *recObserver) Connection(id string, at time.Time, ip string) {
	r.add("conn:"+id+":"+ip, func() { r.connAt = append(r.connAt, at) })
}
func (r *recObserver) ConnectionClosed(id string) { r.add("closed:"+id, nil) }
func (r *recObserver) Latency(d time.Duration) {
	r.add("latency", func() { r.rtts = append(r.rtts, d) })
}
func (r *recObserver) LocalTarget(addr string, ok bool) {
	r.add(fmt.Sprintf("local:%s:%v", addr, ok), nil)
}
func (r *recObserver) Request(id string, at time.Time, method, path string, status int) {
	r.add(fmt.Sprintf("request:%s:%s:%s:%d", id, method, path, status), func() { r.reqAt = append(r.reqAt, at) })
}
func (r *recObserver) Counts(id string, open, total int) {
	r.add(fmt.Sprintf("counts:%s:%d:%d", id, open, total), nil)
}

func (r *recObserver) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// has reports whether an event with the given prefix was recorded.
func (r *recObserver) has(prefix string) bool { return r.count(prefix) > 0 }

func (r *recObserver) count(prefix string) int {
	n := 0
	for _, e := range r.snapshot() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// fakeRelay accepts control connections, authenticates everyone, answers each
// tunnel registration through answer and each ping with a pong.
type fakeRelay struct {
	ln   net.Listener
	pool *x509.CertPool
	wg   sync.WaitGroup

	mu    sync.Mutex
	conns []net.Conn
}

func startFakeRelay(t *testing.T, answer func(proto.TunnelRegister) proto.TunnelRegisterResponse) *fakeRelay {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "dev-server.pem"), filepath.Join(dir, "dev-server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	f := &fakeRelay{ln: ln, pool: x509.NewCertPool()}
	f.pool.AppendCertsFromPEM(caPEM)

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, conn)
			f.mu.Unlock()
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer conn.Close()
				var env proto.Envelope
				if proto.ReadFrame(conn, &env) != nil {
					return
				}
				if proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{OK: true, SessionID: "s"}) != nil {
					return
				}
				cfg := yamux.DefaultConfig()
				cfg.LogOutput = io.Discard
				ysess, e := yamux.Server(conn, cfg)
				if e != nil {
					return
				}
				defer ysess.Close()
				stream, e := ysess.Accept()
				if e != nil {
					return
				}
				defer stream.Close()
				for {
					if proto.ReadFrame(stream, &env) != nil {
						return
					}
					switch env.Type {
					case proto.MsgTunnelRegister:
						var reg proto.TunnelRegister
						_ = proto.DecodePayload(env, &reg)
						_ = proto.WriteMessage(stream, proto.MsgTunnelRegisterResp, answer(reg))
					case proto.MsgPing:
						var p proto.Ping
						_ = proto.DecodePayload(env, &p)
						_ = proto.WriteMessage(stream, proto.MsgPong, proto.Pong(p))
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.drop()
		f.wg.Wait()
	})
	return f
}

// drop hangs up on every client.
func (f *fakeRelay) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.Close()
	}
	f.conns = nil
}

func (f *fakeRelay) client(o Observer, tunnels ...TunnelSpec) *Client {
	return New(Options{
		Server: f.ln.Addr().String(), Token: "bur_test_0000", RootCAs: f.pool, ServerName: "localhost",
		Tunnels: tunnels, Observer: o, Logger: slog.New(slog.DiscardHandler),
	})
}

// runUntilDone runs c until the test ends.
func runUntilDone(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestObserver_StatesAndRegistrations(t *testing.T) {
	relay := startFakeRelay(t, func(reg proto.TunnelRegister) proto.TunnelRegisterResponse {
		if reg.Type == "http" {
			return proto.TunnelRegisterResponse{OK: true, TunnelID: "t-" + reg.Name, URL: "https://burrow.example.com/svc/p7baeh/"}
		}
		return proto.TunnelRegisterResponse{OK: true, TunnelID: "t-" + reg.Name, RemotePort: 9000}
	})
	obs := &recObserver{}
	logs := &syncBuffer{}
	c := relay.client(obs,
		TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"},
		TunnelSpec{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432"},
	)
	c.log = slog.New(slog.NewTextHandler(logs, nil))
	runUntilDone(t, c)

	if !waitTrue(func() bool { return obs.count("registered:") == 2 }, 3*time.Second) {
		t.Fatalf("events: %v", obs.snapshot())
	}
	got := obs.snapshot()
	want := []string{"state:connecting", "state:connected", "registered:web", "registered:pg"}
	if len(got) < len(want) || strings.Join(got[:4], " ") != strings.Join(want, " ") {
		t.Fatalf("events = %v, want %v first", got, want)
	}
	obs.mu.Lock()
	regs := append([]RegisteredTunnel(nil), obs.regs...)
	obs.mu.Unlock()
	if regs[0] != (RegisteredTunnel{TunnelID: "t-web", Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", URL: "https://burrow.example.com/svc/p7baeh/"}) {
		t.Fatalf("http tunnel = %+v", regs[0])
	}
	if regs[1] != (RegisteredTunnel{TunnelID: "t-pg", Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000}) {
		t.Fatalf("tcp tunnel = %+v", regs[1])
	}
	// The log lines are those a client without an observer prints.
	out := logs.String()
	for _, line := range []string{
		`msg=connected session_id=s`,
		`msg="tunnel registered" name=web tunnel_id=t-web url=https://burrow.example.com/svc/p7baeh/`,
		`msg="tunnel registered" tunnel_id=t-pg remote_port=9000`,
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("log line %q missing in:\n%s", line, out)
		}
	}
}

func TestObserver_HostnameOfAnOlderRelay(t *testing.T) {
	relay := startFakeRelay(t, func(proto.TunnelRegister) proto.TunnelRegisterResponse {
		return proto.TunnelRegisterResponse{OK: true, TunnelID: "t1", Hostname: "k7p2qx.tunnels.example.com"}
	})
	obs := &recObserver{}
	runUntilDone(t, relay.client(obs, TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"}))
	if !waitTrue(func() bool { return obs.has("registered:") }, 3*time.Second) {
		t.Fatalf("events: %v", obs.snapshot())
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.regs[0].URL != "k7p2qx.tunnels.example.com" {
		t.Fatalf("URL = %q", obs.regs[0].URL)
	}
}

func TestObserver_ReconnectingAfterTheRelayHangsUp(t *testing.T) {
	relay := startFakeRelay(t, func(proto.TunnelRegister) proto.TunnelRegisterResponse {
		return proto.TunnelRegisterResponse{OK: true, TunnelID: "t1", RemotePort: 9000}
	})
	obs := &recObserver{}
	runUntilDone(t, relay.client(obs, TunnelSpec{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432"}))
	if !waitTrue(func() bool { return obs.has("registered:") }, 3*time.Second) {
		t.Fatalf("events: %v", obs.snapshot())
	}
	relay.drop()
	if !waitTrue(func() bool { return obs.count("registered:") == 2 }, 5*time.Second) {
		t.Fatalf("the client did not come back; events: %v", obs.snapshot())
	}
	var got []string
	for _, e := range obs.snapshot() {
		if e != "latency" { // round trips are measured in between
			got = append(got, e)
		}
	}
	// connecting, connected, registered, reconnecting:<error>, connecting, connected, registered
	i := 3
	if !strings.HasPrefix(got[i], "state:reconnecting:") || len(got[i]) == len("state:reconnecting:") {
		t.Fatalf("event %d = %q, want reconnecting with the error text; all: %v", i, got[i], got)
	}
	if got[i+1] != "state:connecting" || got[i+2] != "state:connected" {
		t.Fatalf("after reconnecting: %v", got[i+1:])
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.retry) == 0 || obs.retry[0] < 0 || obs.retry[0] > 30*time.Second {
		t.Fatalf("retry delays = %v", obs.retry)
	}
}

func TestObserver_Latency(t *testing.T) {
	relay := startFakeRelay(t, func(proto.TunnelRegister) proto.TunnelRegisterResponse {
		return proto.TunnelRegisterResponse{OK: true, TunnelID: "t1", RemotePort: 9000}
	})
	obs := &recObserver{}
	c := relay.client(obs, TunnelSpec{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432"})
	c.pingInterval = 30 * time.Millisecond
	runUntilDone(t, c)
	if !waitTrue(func() bool { return obs.count("latency") >= 2 }, 3*time.Second) {
		t.Fatalf("no round trip was reported; events: %v", obs.snapshot())
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	for _, d := range obs.rtts {
		if d <= 0 || d >= time.Second {
			t.Fatalf("round trip = %v", d)
		}
	}
}

// echoListener answers every connection with what it reads.
func echoListener(t *testing.T) net.Listener {
	t.Helper()
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ls.Close() })
	go func() {
		for {
			c, e := ls.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ls
}

// realRelayClient starts the real control server and a client with one tcp
// tunnel to local.
func realRelayClient(t *testing.T, obs Observer, local string) *Client {
	t.Helper()
	dir := t.TempDir()
	s, cancel := startServer(t, dir, "secret")
	t.Cleanup(func() { cancel(); s.Wait() })
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	c := New(Options{
		Server: s.Addr(), Token: "bur_test_0000", RootCAs: pool, ServerName: "localhost",
		Tunnels:  []TunnelSpec{{Name: "echo", Type: "tcp", LocalAddr: local}},
		Observer: obs, Logger: slog.New(slog.DiscardHandler),
	})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	t.Cleanup(func() { stop(); <-done })
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("client never registered")
	}
	return c
}

// visit sends four bytes through the tunnel and returns how long the echo took.
func visit(t *testing.T, port int) time.Duration {
	t.Helper()
	start := time.Now()
	vc, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer vc.Close()
	_, _ = vc.Write([]byte("abcd"))
	_ = vc.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 4)
	if _, err := io.ReadFull(vc, b); err != nil || string(b) != "abcd" {
		t.Fatalf("got %q err=%v", b, err)
	}
	return time.Since(start)
}

func TestObserver_VisitorConnections(t *testing.T) {
	ls := echoListener(t)
	local := ls.Addr().String()
	obs := &recObserver{}
	before := time.Now()
	c := realRelayClient(t, obs, local)
	port := c.lastRemotePortForTest()

	visit(t, port)
	visit(t, port)
	if !waitTrue(func() bool { return obs.count("closed:") == 2 }, 3*time.Second) {
		t.Fatalf("events: %v", obs.snapshot())
	}
	obs.mu.Lock()
	id := obs.regs[0].TunnelID
	at := obs.connAt[0]
	obs.mu.Unlock()
	if id == "" {
		t.Fatal("no tunnel id")
	}
	if n := obs.count("conn:" + id + ":127.0.0.1"); n != 2 {
		t.Fatalf("%d connections with tunnel id and source address, want 2; events: %v", n, obs.snapshot())
	}
	if obs.count("closed:"+id) != 2 {
		t.Fatalf("events: %v", obs.snapshot())
	}
	if at.Before(before) || at.After(time.Now()) {
		t.Fatalf("connection time %v is not between %v and now", at, before)
	}
	// The local target is reported when it changes, not for every visitor.
	if n := obs.count("local:" + local + ":true"); n != 1 {
		t.Fatalf("local target reported %d times, want once; events: %v", n, obs.snapshot())
	}
	if obs.has("local:" + local + ":false") {
		t.Fatalf("events: %v", obs.snapshot())
	}
}

func TestObserver_LocalTargetDown(t *testing.T) {
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := ls.Addr().String()
	_ = ls.Close() // nothing listens there now
	obs := &recObserver{}
	c := realRelayClient(t, obs, local)

	vc, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(c.lastRemotePortForTest()), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer vc.Close()
	if !waitTrue(func() bool { return obs.has("local:"+local+":false") && obs.has("closed:") }, 3*time.Second) {
		t.Fatalf("events: %v", obs.snapshot())
	}
	if obs.has("local:" + local + ":true") {
		t.Fatalf("events: %v", obs.snapshot())
	}
}

// A slow observer must not hold up the traffic of a tunnel.
func TestObserver_SlowObserverDoesNotDelayTraffic(t *testing.T) {
	ls := echoListener(t)
	obs := &recObserver{delay: 300 * time.Millisecond}
	c := realRelayClient(t, obs, ls.Addr().String())
	port := c.lastRemotePortForTest()
	for i := 0; i < 3; i++ {
		if d := visit(t, port); d > 150*time.Millisecond {
			t.Fatalf("visit %d took %v behind an observer that needs 300 ms per call", i, d)
		}
	}
}

func TestNotifier_NeverBlocksAndKeepsTheNewest(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	var got []int
	n := newNotifier(blockingObserver{release: release, entered: entered})
	stop := n.start()
	defer stop()

	// The observer must hang in a call before the burst starts. Emitted in
	// the middle of the burst, the call can be dropped from a full queue
	// before it is delivered, and then nothing blocks at all.
	n.emit(func(o Observer) { o.Latency(0) })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the observer was never called")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			i := i
			n.emit(func(Observer) { mu.Lock(); got = append(got, i); mu.Unlock() })
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked behind an observer that does not return")
	}
	close(release)
	if !waitTrue(func() bool { mu.Lock(); defer mu.Unlock(); return len(got) > 0 && got[len(got)-1] == 4999 }, 3*time.Second) {
		t.Fatal("the newest event was not delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) > maxPendingEvents {
		t.Fatalf("%d events were delivered, the queue holds at most %d", len(got), maxPendingEvents)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("events out of order: %d after %d", got[i], got[i-1])
		}
	}
}

// blockingObserver hangs in Latency until release is closed. It closes
// entered, when there is one, as soon as it is inside that call.
type blockingObserver struct{ release, entered chan struct{} }

func (blockingObserver) State(ConnState, string, time.Duration) {}
func (blockingObserver) Registered(RegisteredTunnel)            {}
func (blockingObserver) Connection(string, time.Time, string)   {}
func (blockingObserver) ConnectionClosed(string)                {}
func (b blockingObserver) Latency(time.Duration) {
	if b.entered != nil {
		close(b.entered)
	}
	<-b.release
}
func (blockingObserver) LocalTarget(string, bool) {}
func (blockingObserver) Request(string, time.Time, string, string, int) {
}

func TestNotifier_StopEndsItsGoroutineAndAPanicDoesNotSpread(t *testing.T) {
	n := newNotifier(blockingObserver{})
	stop := n.start()
	ran := make(chan struct{})
	n.emit(func(Observer) { panic("observer bug") })
	n.emit(func(Observer) { close(ran) })
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the event after a panicking one was not delivered")
	}
	stop()
	stop() // twice is fine
	n.emit(func(Observer) { t.Error("delivered after stop") })
	time.Sleep(50 * time.Millisecond)

	var none *notifier
	none.emit(func(Observer) { t.Error("a client without an observer delivered an event") })
}

// Lines — one connection, one request — are what a burst consists of. When
// the queue is full they give way, oldest first, and what the view cannot do
// without stays: a state, a registration, the local target.
func TestNotifier_LinesGiveWayToEverythingElse(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var got []string
	rec := func(s string) func(Observer) {
		return func(Observer) { mu.Lock(); got = append(got, s); mu.Unlock() }
	}
	n := newNotifier(blockingObserver{release: release})
	stop := n.start()
	defer stop()
	n.emit(func(o Observer) { o.Latency(0) }) // the observer hangs in this one
	n.emit(rec("state"))
	for i := 0; i < 5000; i++ {
		n.emitLine(rec("line"))
		if i == 2500 {
			n.emit(rec("registered"))
		}
	}
	n.emit(rec("local"))
	n.mu.Lock()
	waiting := len(n.pending)
	n.mu.Unlock()
	if waiting > maxPendingEvents {
		t.Fatalf("%d calls wait, the queue holds %d", waiting, maxPendingEvents)
	}
	close(release)
	if !waitTrue(func() bool { mu.Lock(); defer mu.Unlock(); return len(got) > 0 && got[len(got)-1] == "local" }, 3*time.Second) {
		t.Fatal("the last call was not delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	var kept []string
	lines := 0
	for _, g := range got {
		if g == "line" {
			lines++
		} else {
			kept = append(kept, g)
		}
	}
	if strings.Join(kept, ",") != "state,registered,local" {
		t.Fatalf("delivered besides the lines: %v", kept)
	}
	if lines == 0 || lines > maxPendingEvents {
		t.Fatalf("%d lines were delivered", lines)
	}
}

// countingObserver hangs like blockingObserver and keeps the counts it is told.
type countingObserver struct {
	blockingObserver
	mu     sync.Mutex
	counts []string
	opened int
	closed int
}

func (c *countingObserver) Connection(string, time.Time, string) {
	c.mu.Lock()
	c.opened++
	c.mu.Unlock()
}
func (c *countingObserver) ConnectionClosed(string) { c.mu.Lock(); c.closed++; c.mu.Unlock() }
func (c *countingObserver) Counts(id string, open, total int) {
	c.mu.Lock()
	c.counts = append(c.counts, fmt.Sprintf("%s:%d:%d", id, open, total))
	c.mu.Unlock()
}

// The calls for single connections may be dropped; how many are open and how
// many there were is told apart from them, the newest numbers of each tunnel
// only, and is never dropped. A view that counted the calls would show
// connections as open that have ended.
func TestNotifier_CountsSurviveABurst(t *testing.T) {
	release := make(chan struct{})
	obs := &countingObserver{blockingObserver: blockingObserver{release: release}}
	n := newNotifier(obs)
	stop := n.start()
	defer stop()
	n.emit(func(o Observer) { o.Latency(0) })
	for i := 1; i <= 5000; i++ {
		n.emitLine(func(o Observer) { o.Connection("t1", time.Time{}, "") })
		n.counts("t1", 1, i)
		if i%2 == 0 {
			n.counts("t2", 0, i/2)
		}
		n.emitLine(func(o Observer) { o.ConnectionClosed("t1") })
		n.counts("t1", 0, i)
	}
	n.mu.Lock()
	waiting, latest := len(n.pending), len(n.latest)
	n.mu.Unlock()
	if waiting > maxPendingEvents || latest > 2 {
		t.Fatalf("%d calls and the counts of %d tunnels wait", waiting, latest)
	}
	close(release)
	last := func(id string) string {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		for i := len(obs.counts) - 1; i >= 0; i-- {
			if strings.HasPrefix(obs.counts[i], id+":") {
				return obs.counts[i]
			}
		}
		return ""
	}
	if !waitTrue(func() bool { return last("t1") == "t1:0:5000" && last("t2") == "t2:0:2500" }, 3*time.Second) {
		t.Fatalf("last counts: %q %q", last("t1"), last("t2"))
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.opened >= 5000 || obs.closed >= 5000 {
		t.Fatalf("nothing was dropped (%d, %d): the test did not fill the queue", obs.opened, obs.closed)
	}
	if len(obs.counts) > 20 {
		t.Fatalf("the counts were told %d times", len(obs.counts))
	}
}

// The client tells the counts of each tunnel after every visitor connection.
func TestObserver_CountsOfVisitorConnections(t *testing.T) {
	ls := echoListener(t)
	obs := &recObserver{}
	c := realRelayClient(t, obs, ls.Addr().String())
	port := c.lastRemotePortForTest()
	visit(t, port)
	visit(t, port)
	obs.mu.Lock()
	id := obs.regs[0].TunnelID
	obs.mu.Unlock()
	if !waitTrue(func() bool { return obs.has("counts:" + id + ":0:2") }, 3*time.Second) {
		t.Fatalf("events: %v", obs.snapshot())
	}
	ev := obs.snapshot()
	if last := ev[len(ev)-1]; last != "counts:"+id+":0:2" && !strings.HasPrefix(last, "closed:") && !strings.HasPrefix(last, "latency") {
		t.Fatalf("last event %q of %v", last, ev)
	}
}

// Many visitor connections of one tunnel open and close at the same moment.
// Each change tells the observer the counts; were two changes able to hand
// their numbers over in another order than they were made in, the older
// numbers would be the last the view hears, and it would show connections as
// open that have ended. The last counts told are the true ones, every time.
func TestClient_CountsArriveInTheOrderTheyWereMade(t *testing.T) {
	const conns = 200
	for round := 0; round < 40; round++ {
		obs := &recObserver{}
		c := New(Options{Server: "127.0.0.1:1", Observer: obs, Logger: slog.New(slog.DiscardHandler)})
		stop := c.events.start()
		c.mu.Lock()
		c.live = map[string]*liveTunnel{"t1": {http: true}}
		c.mu.Unlock()
		var wg sync.WaitGroup
		for i := 0; i < conns; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.countConnection("t1", 1)
				c.countConnection("t1", -1)
			}()
		}
		wg.Wait()
		// Everything that was queued has been delivered.
		if !waitTrue(func() bool {
			c.events.mu.Lock()
			defer c.events.mu.Unlock()
			return len(c.events.pending) == 0 && len(c.events.latest) == 0
		}, 3*time.Second) {
			t.Fatal("the queue did not drain")
		}
		want := fmt.Sprintf("counts:t1:0:%d", conns)
		if !waitTrue(func() bool { ev := obs.snapshot(); return len(ev) > 0 && ev[len(ev)-1] == want }, time.Second) {
			ev := obs.snapshot()
			stop()
			t.Fatalf("round %d: the last counts told are %q, want %q", round, ev[len(ev)-1], want)
		}
		stop()
	}
}
