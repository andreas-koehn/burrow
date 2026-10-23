package server

// The observer seen through a real Server: a handshake, a control stream and
// tunnels, for the ways a session can end and the ways a registration can be
// refused.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// observedRelay is a Server that reports to a recObserver, with its client
// side helpers.
type observedRelay struct {
	t      *testing.T
	srv    *Server
	obs    *recObserver
	pool   *x509.CertPool
	cancel context.CancelFunc
}

// newObservedRelay serves on one public port, port; mut changes the options.
func newObservedRelay(t *testing.T, port int, mut func(*Options)) *observedRelay {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	o := Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: fakeAuth{uid: "u1"},
		PublicBind: "127.0.0.1", PortMin: port, PortMax: port,
		Services: fakeResolver{id: "svc-1", sub: "abc123"},
	}
	if mut != nil {
		mut(&o)
	}
	srv, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	obs := &recObserver{}
	srv.SetObserver(obs)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	waitListening(t, srv)
	caPEM, err := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	r := &observedRelay{t: t, srv: srv, obs: obs, pool: pool, cancel: cancel}
	t.Cleanup(func() { cancel(); srv.Wait() })
	return r
}

// events names what the observer was told about sessions, in order:
// kind[:tunnel type][:refusal code].
func (r *observedRelay) events() []string {
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	out := make([]string, 0, len(r.obs.sessions))
	for _, e := range r.obs.sessions {
		s := e.Kind
		if e.TunnelType != "" {
			s += ":" + e.TunnelType
		}
		if e.RefusalCode != "" {
			s += ":" + e.RefusalCode
		}
		out = append(out, s)
	}
	return out
}

func (r *observedRelay) traffic() []observe.Traffic {
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	return append([]observe.Traffic(nil), r.obs.traffic...)
}

func (r *observedRelay) count(kind string) int {
	n := 0
	for _, e := range r.events() {
		if strings.HasPrefix(e, kind) {
			n++
		}
	}
	return n
}

func (r *observedRelay) waitFor(what string, ok func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out: %s; events %v", what, r.events())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settle gives an event that would be told twice the time to arrive.
func settle() { time.Sleep(200 * time.Millisecond) }

func (r *observedRelay) dial(token, version string) (net.Conn, proto.AuthResponse) {
	r.t.Helper()
	conn, err := tls.Dial("tcp", r.srv.Addr(), &tls.Config{RootCAs: r.pool, ServerName: "localhost"})
	if err != nil {
		r.t.Fatalf("dial: %v", err)
	}
	if err := proto.WriteMessage(conn, proto.MsgAuthRequest, proto.AuthRequest{ProtocolVersion: 1, Token: token, ClientVersion: version}); err != nil {
		r.t.Fatalf("auth request: %v", err)
	}
	var env proto.Envelope
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ar proto.AuthResponse
	if err := proto.ReadFrame(conn, &env); err == nil {
		_ = proto.DecodePayload(env, &ar)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, ar
}

// handClient is a hand-made client: the handshake, the control stream, and a
// reader that hands register answers to regs and serves every new_connection
// with onConn (none: the stream is never opened).
type handClient struct {
	conn net.Conn
	ctrl *yamux.Stream
	regs chan proto.TunnelRegisterResponse
}

func (r *observedRelay) client(onConn func(st *yamux.Stream)) *handClient {
	r.t.Helper()
	return r.clientWith("secret", onConn)
}

// clientWith is client with the given token.
func (r *observedRelay) clientWith(token string, onConn func(st *yamux.Stream)) *handClient {
	r.t.Helper()
	conn, ar := r.dial(token, "")
	if !ar.OK {
		r.t.Fatalf("auth: %+v", ar)
	}
	sess, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		r.t.Fatal(err)
	}
	ctrl, err := sess.OpenStream()
	if err != nil {
		r.t.Fatal(err)
	}
	c := &handClient{conn: conn, ctrl: ctrl, regs: make(chan proto.TunnelRegisterResponse, 8)}
	go func() {
		for {
			var env proto.Envelope
			if proto.ReadFrame(ctrl, &env) != nil {
				return
			}
			switch env.Type {
			case proto.MsgTunnelRegisterResp:
				var rr proto.TunnelRegisterResponse
				_ = proto.DecodePayload(env, &rr)
				c.regs <- rr
			case proto.MsgNewConnection:
				var nc proto.NewConnection
				if proto.DecodePayload(env, &nc) != nil || onConn == nil {
					continue
				}
				go func() {
					st, e := sess.OpenStream()
					if e != nil {
						return
					}
					defer st.Close()
					if proto.WriteMessage(st, proto.MsgStreamOpen, proto.StreamHeader{StreamID: nc.StreamID, TunnelID: nc.TunnelID}) != nil {
						return
					}
					onConn(st)
				}()
			}
		}
	}()
	return c
}

func (c *handClient) register(t *testing.T, tr proto.TunnelRegister) proto.TunnelRegisterResponse {
	t.Helper()
	if err := proto.WriteMessage(c.ctrl, proto.MsgTunnelRegister, tr); err != nil {
		t.Fatalf("register: %v", err)
	}
	select {
	case rr := <-c.regs:
		return rr
	case <-time.After(5 * time.Second):
		t.Fatal("no register answer")
	}
	return proto.TunnelRegisterResponse{}
}

// echo5 reads five bytes, answers "echo:" and them (ten bytes), and waits for
// the other side to end the stream.
func echo5(st *yamux.Stream) {
	buf := make([]byte, 5)
	if _, err := io.ReadFull(st, buf); err != nil {
		return
	}
	_, _ = st.Write(append([]byte("echo:"), buf...))
	_, _ = io.Copy(io.Discard, st)
}

func requireEvents(t *testing.T, got, want []string) {
	t.Helper()
	if !sameStrings(got, want) {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
}

// A client that registers tunnels and disconnects: its life is told in order,
// each part once.
func TestServer_ObserverClientDisconnect(t *testing.T) {
	r := newObservedRelay(t, 19300, nil)
	c := r.client(nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "raw", Type: "tcp", RemotePort: 19300, LocalAddr: "x"}); !rr.OK {
		t.Fatalf("tcp register: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("http register: %+v", rr)
	}
	_ = c.conn.Close()
	r.waitFor("the session is closed", func() bool { return r.count(observe.SessionClosed) >= 1 })
	settle()
	got := r.events()
	if len(got) != 6 || got[0] != observe.SessionOpened || got[len(got)-1] != observe.SessionClosed {
		t.Fatalf("events %v", got)
	}
	have := map[string]int{}
	for _, e := range got {
		have[e]++
	}
	for _, k := range []string{
		observe.TunnelOpened + ":tcp", observe.TunnelOpened + ":http",
		observe.TunnelClosed + ":tcp", observe.TunnelClosed + ":http",
	} {
		if have[k] != 1 {
			t.Errorf("%s told %d times, want once: %v", k, have[k], got)
		}
	}
}

// A connection that authenticates but never opens the control stream is a
// session: one opened, one closed.
func TestServer_ObserverAuthenticatedWithoutControlStream(t *testing.T) {
	r := newObservedRelay(t, 19300, nil)
	conn, ar := r.dial("secret", "")
	if !ar.OK {
		t.Fatalf("auth: %+v", ar)
	}
	_ = conn.Close()
	r.waitFor("closed", func() bool { return r.count(observe.SessionClosed) >= 1 })
	settle()
	requireEvents(t, r.events(), []string{observe.SessionOpened, observe.SessionClosed})
}

// A handshake that fails — a token the authenticator refuses, a client that
// is too old — tells nothing.
func TestServer_ObserverFailedHandshakesTellNothing(t *testing.T) {
	r := newObservedRelay(t, 19300, func(o *Options) { o.MinClientVersion = "2.0.0" })
	conn, ar := r.dial("", "3.0.0")
	if ar.OK {
		t.Fatal("empty token accepted")
	}
	_ = conn.Close()
	conn, ar = r.dial("secret", "1.0.0")
	if ar.OK || ar.Code != proto.CodeClientTooOld {
		t.Fatalf("old client: %+v", ar)
	}
	_ = conn.Close()
	// A good one after them, so that there is something to wait for.
	conn, ar = r.dial("secret", "3.0.0")
	if !ar.OK {
		t.Fatalf("auth: %+v", ar)
	}
	_ = conn.Close()
	r.waitFor("closed", func() bool { return r.count(observe.SessionClosed) >= 1 })
	settle()
	requireEvents(t, r.events(), []string{observe.SessionOpened, observe.SessionClosed})
}

// The relay shuts down with a live session: its tunnel and the session are
// closed once, before Wait returns.
func TestServer_ObserverShutdownWithLiveSession(t *testing.T) {
	r := newObservedRelay(t, 19300, nil)
	c := r.client(nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "raw", Type: "tcp", RemotePort: 19300, LocalAddr: "x"}); !rr.OK {
		t.Fatalf("tcp register: %+v", rr)
	}
	r.cancel()
	r.srv.Wait()
	requireEvents(t, r.events(), []string{
		observe.SessionOpened, observe.TunnelOpened + ":tcp", observe.TunnelClosed + ":tcp", observe.SessionClosed,
	})
	_ = c.conn.Close()
}

// Two sessions of one token both live (there is no takeover): each is opened
// and closed once, and opened minus closed ends at zero for the user.
func TestServer_ObserverTwoSessionsOfOneToken(t *testing.T) {
	r := newObservedRelay(t, 19300, func(o *Options) { o.PortMax = 19301 })
	a := r.client(nil)
	if rr := a.register(t, proto.TunnelRegister{Name: "raw", Type: "tcp", RemotePort: 19300, LocalAddr: "x"}); !rr.OK {
		t.Fatalf("a: %+v", rr)
	}
	b := r.client(nil)
	if rr := b.register(t, proto.TunnelRegister{Name: "raw", Type: "tcp", RemotePort: 19301, LocalAddr: "x"}); !rr.OK {
		t.Fatalf("b: %+v", rr)
	}
	_ = a.conn.Close()
	r.waitFor("a closed", func() bool { return r.count(observe.SessionClosed) >= 1 })
	settle()
	if n := r.count(observe.SessionClosed); n != 1 {
		t.Fatalf("%d closed after the first left", n)
	}
	_ = b.conn.Close()
	r.waitFor("b closed", func() bool { return r.count(observe.SessionClosed) >= 2 })
	settle()
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	per := map[string]int{}
	open := 0
	for _, e := range r.obs.sessions {
		per[e.Kind+":"+e.SessionID]++
		switch e.Kind {
		case observe.SessionOpened:
			open++
		case observe.SessionClosed:
			open--
		}
	}
	if len(r.obs.sessions) != 8 {
		t.Fatalf("%d events, want 8", len(r.obs.sessions))
	}
	for k, v := range per {
		if v != 1 {
			t.Fatalf("%s told %d times", k, v)
		}
	}
	if len(per) != 8 || open != 0 {
		t.Fatalf("%d distinct events, %d sessions still open", len(per), open)
	}
}

// Each of the five places that refuse a registration announces it once, with
// the code the client was answered with.
func TestServer_ObserverAnnouncesEachRefusalOnce(t *testing.T) {
	taken := mustListen(t) // a port the relay is given but cannot listen on
	takenPort := taken.Addr().(*net.TCPAddr).Port
	cases := []struct {
		name string
		mut  func(*Options)
		port int
		pre  *proto.TunnelRegister // registered first, must succeed
		tr   proto.TunnelRegister
		code string
		typ  string
	}{
		{name: "http not configured", mut: func(o *Options) { o.Services = nil }, port: 19310,
			tr: proto.TunnelRegister{Name: "w", Type: "http", LocalAddr: "x"}, code: proto.CodeInternal, typ: "http"},
		{name: "resolver fails", mut: func(o *Options) { o.Services = fakeResolver{err: errors.New("boom")} }, port: 19311,
			tr: proto.TunnelRegister{Name: "w", Type: "http", LocalAddr: "x"}, code: proto.CodeInternal, typ: "http"},
		{name: "resolver refuses", mut: func(o *Options) {
			o.Services = fakeResolver{err: &RefusalError{Code: proto.CodeBadRequest, Message: "no"}}
		}, port: 19312,
			tr: proto.TunnelRegister{Name: "w", Type: "http", LocalAddr: "x"}, code: proto.CodeBadRequest, typ: "http"},
		{name: "port taken", port: 19313,
			pre:  &proto.TunnelRegister{Name: "a", Type: "tcp", RemotePort: 19313, LocalAddr: "x"},
			tr:   proto.TunnelRegister{Name: "b", Type: "tcp", RemotePort: 19313, LocalAddr: "x"},
			code: proto.CodePortUnavailable, typ: "tcp"},
		{name: "cannot listen", port: takenPort,
			tr:   proto.TunnelRegister{Name: "b", Type: "tcp", RemotePort: takenPort, LocalAddr: "x"},
			code: proto.CodePortUnavailable, typ: "tcp"},
		{name: "unknown type", port: 19315,
			tr: proto.TunnelRegister{Name: "b", Type: "udp", LocalAddr: "x"}, code: proto.CodeUnknownTunnelType, typ: "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newObservedRelay(t, tc.port, tc.mut)
			c := r.client(nil)
			want := []string{observe.SessionOpened}
			if tc.pre != nil {
				if rr := c.register(t, *tc.pre); !rr.OK {
					t.Fatalf("first register: %+v", rr)
				}
				want = append(want, observe.TunnelOpened+":tcp")
			}
			rr := c.register(t, tc.tr)
			if rr.OK || rr.Code != tc.code {
				t.Fatalf("answer %+v, want a refusal with code %s", rr, tc.code)
			}
			r.waitFor("the refusal is announced", func() bool { return r.count(observe.TunnelRefused) >= 1 })
			settle()
			want = append(want, observe.TunnelRefused+":"+tc.typ+":"+tc.code)
			requireEvents(t, r.events(), want)
			if ev := r.obs.sessions[len(r.obs.sessions)-1]; ev.TunnelName != tc.tr.Name || ev.UserID != "u1" || ev.SessionID == "" {
				t.Fatalf("refusal event: %+v", ev)
			}
			_ = c.conn.Close()
		})
	}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// A tcp visitor is announced once, with its bytes in the right direction: 5
// sent to the service are BytesIn, the 10 it answered BytesOut. The tunnel's
// counters agree, and so do the counters of an http tunnel's stream, which
// announces nothing.
func TestServer_ObserverTCPTrafficAndHTTPStreamBytes(t *testing.T) {
	r := newObservedRelay(t, 19300, nil)
	c := r.client(echo5)
	if rr := c.register(t, proto.TunnelRegister{Name: "raw", Type: "tcp", RemotePort: 19300, LocalAddr: "x"}); !rr.OK {
		t.Fatalf("tcp register: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("http register: %+v", rr)
	}

	vc, err := net.DialTimeout("tcp", "127.0.0.1:19300", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = vc.Write([]byte("HELLO"))
	_ = vc.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp := make([]byte, 10)
	if _, err := io.ReadFull(vc, resp); err != nil || string(resp) != "echo:HELLO" {
		t.Fatalf("visitor got %q %v", resp, err)
	}
	_ = vc.Close()
	r.waitFor("the tcp connection is reported", func() bool { return len(r.traffic()) >= 1 })
	settle()
	tr := r.traffic()
	if len(tr) != 1 {
		t.Fatalf("%d traffic events, want 1: %+v", len(tr), tr)
	}
	if e := tr[0]; e.BytesIn != 5 || e.BytesOut != 10 || e.Outcome != observe.OutcomeClean || e.Kind != observe.TrafficTCP ||
		e.SourceIP != "127.0.0.1" || e.UserID != "u1" || e.ServiceID != "" || e.TunnelID == "" || e.SessionID == "" {
		t.Fatalf("tcp traffic event: %+v", e)
	}
	for _, v := range r.srv.ListUserTunnels("u1") {
		if v.Name == "raw" && (v.BytesIn != 5 || v.BytesOut != 10) {
			t.Fatalf("tcp tunnel counters in=%d out=%d, want 5 and 10", v.BytesIn, v.BytesOut)
		}
	}

	tn, ok := r.srv.LookupHTTPTunnel("abc123")
	if !ok {
		t.Fatal("no http tunnel")
	}
	st, err := r.srv.OpenTunnelStream(context.Background(), tn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Write([]byte("HELLO")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(st, resp); err != nil || string(resp) != "echo:HELLO" {
		t.Fatalf("stream got %q %v", resp, err)
	}
	if in, out := tn.BytesIn.Load(), tn.BytesOut.Load(); in != 5 || out != 10 {
		t.Fatalf("http tunnel counters in=%d out=%d, want 5 and 10", in, out)
	}
	if n := len(r.traffic()); n != 1 {
		t.Fatalf("the http stream announced traffic: %d events", n)
	}
}

// A visitor whose client never opens the stream is announced once as an error
// with no bytes.
func TestServer_ObserverTCPStreamNeverOpened(t *testing.T) {
	r := newObservedRelay(t, 19300, nil)
	c := r.client(nil) // ignores new_connection
	if rr := c.register(t, proto.TunnelRegister{Name: "raw", Type: "tcp", RemotePort: 19300, LocalAddr: "x"}); !rr.OK {
		t.Fatalf("tcp register: %+v", rr)
	}
	vc, err := net.DialTimeout("tcp", "127.0.0.1:19300", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer vc.Close()
	r.waitFor("the failed connection is reported", func() bool { return len(r.traffic()) >= 1 })
	settle()
	tr := r.traffic()
	if len(tr) != 1 || tr[0].Outcome != observe.OutcomeError || tr[0].BytesIn != 0 || tr[0].BytesOut != 0 {
		t.Fatalf("traffic %+v", tr)
	}
	_ = c.conn.Close()
}
