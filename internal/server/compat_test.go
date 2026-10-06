package server

// The compatibility matrix of the control protocol after slug, access and the
// error codes were added. Each case is a real handshake over TLS, a
// registration and one byte through a tunnel.
//
//	client  relay   how the older side is played
//	new     new     —
//	old     new     a client in this file that writes the JSON of before the change, as text
//	new     old     a relay in this file that answers the JSON of before the change, as text
//	old     old     the tests that existed before; they are unchanged and still pass

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/version"
)

// The answers of a relay from before the change decode into these.
type (
	oldAuthResponse struct {
		OK        bool   `json:"ok"`
		SessionID string `json:"session_id,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	oldTunnelRegisterResponse struct {
		OK         bool   `json:"ok"`
		TunnelID   string `json:"tunnel_id,omitempty"`
		RemotePort int    `json:"remote_port,omitempty"`
		URL        string `json:"url,omitempty"`
		Hostname   string `json:"hostname,omitempty"`
		Error      string `json:"error,omitempty"`
	}
)

// compatResolver is the relay's service store for these tests: one user's
// services by name, created with the wished slug and mode.
type compatResolver struct {
	mu       sync.Mutex
	services map[string]Resolved
}

func (c *compatResolver) Resolve(ctx context.Context, userID, name, typ string) (string, string, error) {
	r, err := c.ResolveWithOptions(ctx, userID, name, typ, ResolveOptions{})
	return r.ServiceID, r.Slug, err
}

func (c *compatResolver) ResolveWithOptions(_ context.Context, _, name, _ string, o ResolveOptions) (Resolved, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if o.Slug == "taken" {
		return Resolved{}, &RefusalError{Code: proto.CodeSlugTaken, Message: "slug already in use; try: abc234"}
	}
	if r, ok := c.services[name]; ok {
		r.Created = false
		if o.Access != "" && o.Access != r.AccessMode {
			r.Ignored = append(r.Ignored, "access")
		}
		if o.Slug != "" && o.Slug != r.Slug {
			r.Ignored = append(r.Ignored, "slug")
		}
		return r, nil
	}
	r := Resolved{ServiceID: "svc-" + name, Slug: "gen234", AccessMode: "open", Created: true}
	if o.Slug != "" {
		r.Slug = o.Slug
	}
	if o.Access != "" {
		r.AccessMode = o.Access
	}
	if c.services == nil {
		c.services = map[string]Resolved{}
	}
	c.services[name] = r
	return r, nil
}

// newRelay starts the relay of this code base and returns it with the CA its
// certificate is signed by.
func newRelay(t *testing.T, minClient string) (*Server, *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	auth := AuthFunc(func(_ context.Context, tok string) (string, error) {
		if tok != "bur_test_0000" {
			return "", errors.New("unknown token")
		}
		return "u1", nil
	})
	s, err := New(Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"), TLSKey: filepath.Join(dir, "dev-server-key.pem"),
		Auth: auth, PublicBind: "127.0.0.1", PortMin: 18300, PortMax: 18399,
		Logger:   slog.New(slog.DiscardHandler),
		Services: &compatResolver{}, AuthDomain: "burrow.example.com",
		MinClientVersion: minClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	t.Cleanup(func() { cancel(); s.Wait() })
	waitListening(t, s)
	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return s, pool
}

// echoOnce listens on a free port and echoes what every connection sends.
func echoOnce(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// throughPort sends one byte to a public tcp port and expects it back.
func throughPort(t *testing.T, port int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != 'x' {
		t.Fatalf("the byte did not come back: %q %v", b, err)
	}
}

// regObserver keeps the registrations and what the relay said of itself.
type regObserver struct {
	mu       sync.Mutex
	regs     map[string]client.RegisteredTunnel
	sessions []client.SessionInfo
}

func (r *regObserver) State(client.ConnState, string, time.Duration) {}
func (r *regObserver) Connection(string, time.Time, string)          {}
func (r *regObserver) ConnectionClosed(string)                       {}
func (r *regObserver) Latency(time.Duration)                         {}
func (r *regObserver) LocalTarget(string, bool)                      {}
func (r *regObserver) Registered(t client.RegisteredTunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.regs == nil {
		r.regs = map[string]client.RegisteredTunnel{}
	}
	r.regs[t.Name] = t
}
func (r *regObserver) Session(i client.SessionInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions = append(r.sessions, i)
}

// wait returns the registrations once there are n of them.
func (r *regObserver) wait(t *testing.T, n int) map[string]client.RegisteredTunnel {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.regs) >= n {
			out := map[string]client.RegisteredTunnel{}
			for k, v := range r.regs {
				out[k] = v
			}
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d tunnels were registered", n)
	return nil
}

// runNewClient runs the client of this code base until the test ends.
func runNewClient(t *testing.T, addr string, pool *x509.CertPool, obs client.Observer, tunnels ...client.TunnelSpec) {
	t.Helper()
	c := client.New(client.Options{
		Server: addr, Token: "bur_test_0000", RootCAs: pool, ServerName: "localhost",
		Tunnels: tunnels, Observer: obs, Logger: slog.New(slog.DiscardHandler), StopOnRefusal: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestCompat_NewClientNewRelay(t *testing.T) {
	s, pool := newRelay(t, "")
	local := echoOnce(t)
	obs := &regObserver{}
	runNewClient(t, s.Addr(), pool, obs,
		client.TunnelSpec{Name: "app", Type: "http", LocalAddr: local, Slug: "my-app", Access: "burrow_login"},
		client.TunnelSpec{Name: "plain", Type: "http", LocalAddr: local},
		client.TunnelSpec{Name: "echo", Type: "tcp", LocalAddr: local},
	)
	regs := obs.wait(t, 3)

	app := regs["app"]
	if app.URL != "https://burrow.example.com/svc/my-app/" || app.AccessMode != "burrow_login" || !app.Created ||
		app.DashboardURL != "https://burrow.example.com/services/svc-app" || app.Ignored.Any() || app.SlugUnacknowledged {
		t.Errorf("service created with slug and access: %+v", app)
	}
	plain := regs["plain"]
	if plain.URL != "https://burrow.example.com/svc/gen234/" || plain.AccessMode != "open" || !plain.Created || plain.SlugUnacknowledged {
		t.Errorf("service created without wishes: %+v", plain)
	}
	obs.mu.Lock()
	if len(obs.sessions) != 1 || obs.sessions[0] != (client.SessionInfo{RelayVersion: version.Version}) {
		t.Errorf("session info: %+v", obs.sessions)
	}
	obs.mu.Unlock()
	throughPort(t, regs["echo"].RemotePort)

	// A second client asks for other values for the service that now exists.
	again := &regObserver{}
	runNewClient(t, s.Addr(), pool, again, client.TunnelSpec{Name: "app", Type: "http", LocalAddr: local, Slug: "other", Access: "open"})
	got := again.wait(t, 1)["app"]
	if got.URL != "https://burrow.example.com/svc/my-app/" || got.AccessMode != "burrow_login" || got.Created ||
		got.Ignored != (client.OptionSet{Slug: true, Access: true}) {
		t.Errorf("existing service: %+v", got)
	}
}

// A refusal with a code ends the new client, with the relay's reason.
func TestCompat_NewClientNewRelay_Refusals(t *testing.T) {
	s, pool := newRelay(t, "")
	run := func(o client.Options) error {
		o.Server, o.RootCAs, o.ServerName, o.StopOnRefusal = s.Addr(), pool, "localhost", true
		o.Logger = slog.New(slog.DiscardHandler)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return client.New(o).Run(ctx)
	}
	var re *client.RefusedError
	err := run(client.Options{Token: "bur_test_bad0", Tunnels: []client.TunnelSpec{{Name: "x", Type: "tcp", LocalAddr: "127.0.0.1:1"}}})
	if !errors.As(err, &re) || re.Code != proto.CodeInvalidToken || err.Error() != "auth failed: invalid token" {
		t.Fatalf("bad token: %v", err)
	}
	err = run(client.Options{Token: "bur_test_0000", Tunnels: []client.TunnelSpec{{Name: "new", Type: "http", LocalAddr: "127.0.0.1:1", Slug: "taken"}}})
	if !errors.As(err, &re) || re.Code != proto.CodeSlugTaken || re.Message != "slug already in use; try: abc234" {
		t.Fatalf("taken slug: %v", err)
	}
}

// oldClient is a client of before the change. Everything it sends is JSON
// written out here; what it reads it decodes into the old message shapes.
type oldClient struct {
	t    *testing.T
	conn net.Conn
	sess *yamux.Session
	ctrl net.Conn
}

func sendRaw(t *testing.T, w io.Writer, typ proto.MessageType, payload string) {
	t.Helper()
	if err := proto.WriteFrame(w, proto.Envelope{Type: typ, Payload: json.RawMessage(payload)}); err != nil {
		t.Fatal(err)
	}
}

func readRaw(t *testing.T, r net.Conn, want proto.MessageType) string {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer func() { _ = r.SetReadDeadline(time.Time{}) }()
	var env proto.Envelope
	if err := proto.ReadFrame(r, &env); err != nil {
		t.Fatalf("reading %s: %v", want, err)
	}
	if env.Type != want {
		t.Fatalf("got %s, want %s", env.Type, want)
	}
	return string(env.Payload)
}

// dialOld connects and sends the auth request of a client of version v
// (the request has exactly the fields it had before the change).
func dialOld(t *testing.T, s *Server, pool *x509.CertPool, token, v string) (*oldClient, string) {
	t.Helper()
	conn, err := tls.Dial("tcp", s.Addr(), &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	sendRaw(t, conn, proto.MsgAuthRequest,
		`{"protocol_version":1,"token":"`+token+`","client_version":"`+v+`","os":"linux","arch":"amd64"}`)
	return &oldClient{t: t, conn: conn}, readRaw(t, conn, proto.MsgAuthResponse)
}

func (c *oldClient) openControl() {
	c.t.Helper()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Client(c.conn, cfg)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = sess.Close() })
	ctrl, err := sess.OpenStream()
	if err != nil {
		c.t.Fatal(err)
	}
	c.sess, c.ctrl = sess, ctrl
}

func (c *oldClient) register(payload string) (string, oldTunnelRegisterResponse) {
	c.t.Helper()
	sendRaw(c.t, c.ctrl, proto.MsgTunnelRegister, payload)
	raw := readRaw(c.t, c.ctrl, proto.MsgTunnelRegisterResp)
	var rr oldTunnelRegisterResponse
	if err := json.Unmarshal([]byte(raw), &rr); err != nil {
		c.t.Fatalf("the old client cannot read %s: %v", raw, err)
	}
	return raw, rr
}

func TestCompat_OldClientNewRelay(t *testing.T) {
	// A minimum is set and the old client meets it.
	s, pool := newRelay(t, "0.5.0")

	c, raw := dialOld(t, s, pool, "bur_test_0000", "0.6.0")
	var ar oldAuthResponse
	if err := json.Unmarshal([]byte(raw), &ar); err != nil {
		t.Fatalf("the old client cannot read %s: %v", raw, err)
	}
	if !ar.OK || ar.SessionID == "" || ar.Error != "" {
		t.Fatalf("auth response as the old client reads it: %+v", ar)
	}
	// The fields the old client knows come first and are written as before.
	if want := `{"ok":true,"session_id":"` + ar.SessionID + `"`; !strings.HasPrefix(raw, want) {
		t.Fatalf("auth response\n got %s\nwant it to start with %s", raw, want)
	}
	c.openControl()

	// http, as `burrow connect --type http` registers it.
	raw, rr := c.register(`{"name":"web","type":"http","remote_port":0,"local_addr":"127.0.0.1:3000"}`)
	if rr != (oldTunnelRegisterResponse{OK: true, TunnelID: rr.TunnelID, URL: "https://burrow.example.com/svc/gen234/"}) || rr.TunnelID == "" {
		t.Fatalf("http registration as the old client reads it: %+v", rr)
	}
	if want := `{"ok":true,"tunnel_id":"` + rr.TunnelID + `","url":"https://burrow.example.com/svc/gen234/"`; !strings.HasPrefix(raw, want) {
		t.Fatalf("http registration\n got %s\nwant it to start with %s", raw, want)
	}
	// The same name again is the same service, as it always was.
	if _, again := c.register(`{"name":"web","type":"http","remote_port":0,"local_addr":"127.0.0.1:3000"}`); again.URL != rr.URL || !again.OK {
		t.Fatalf("second registration: %+v", again)
	}

	// tcp, and one byte through it.
	raw, tcp := c.register(`{"name":"echo","type":"tcp","remote_port":0,"local_addr":"127.0.0.1:9"}`)
	if !tcp.OK || tcp.TunnelID == "" || tcp.RemotePort < 18300 || tcp.RemotePort > 18399 || tcp.URL != "" || tcp.Error != "" {
		t.Fatalf("tcp registration as the old client reads it: %+v", tcp)
	}
	// Without a wish there is nothing to report: the answer is, to the byte,
	// the one of before the change.
	if want := `{"ok":true,"tunnel_id":"` + tcp.TunnelID + `","remote_port":` + strconv.Itoa(tcp.RemotePort) + `}`; raw != want {
		t.Fatalf("tcp registration\n got %s\nwant %s", raw, want)
	}
	visited := make(chan struct{})
	go func() { defer close(visited); throughPort(t, tcp.RemotePort) }()
	var nc struct {
		TunnelID string `json:"tunnel_id"`
		StreamID string `json:"stream_id"`
		SourceIP string `json:"source_ip"`
	}
	if err := json.Unmarshal([]byte(readRaw(t, c.ctrl, proto.MsgNewConnection)), &nc); err != nil || nc.TunnelID != tcp.TunnelID || nc.StreamID == "" {
		t.Fatalf("new_connection: %+v %v", nc, err)
	}
	data, err := c.sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	sendRaw(t, data, proto.MsgStreamOpen, `{"stream_id":"`+nc.StreamID+`","tunnel_id":"`+nc.TunnelID+`"}`)
	b := make([]byte, 1)
	_ = data.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(data, b); err != nil {
		t.Fatalf("the visitor's byte did not arrive: %v", err)
	}
	if _, err := data.Write(b); err != nil {
		t.Fatal(err)
	}
	select {
	case <-visited:
	case <-time.After(5 * time.Second):
		t.Fatal("the visitor got no answer")
	}

	// ping and pong.
	sendRaw(t, c.ctrl, proto.MsgPing, `{"nonce":"hb"}`)
	if got := readRaw(t, c.ctrl, proto.MsgPong); got != `{"nonce":"hb"}` {
		t.Fatalf("pong = %s", got)
	}

	// Refusals keep their text, written first and as before.
	raw, refused := c.register(`{"name":"x","type":"udp","remote_port":0,"local_addr":"127.0.0.1:9"}`)
	if refused.OK || refused.Error != `unknown tunnel type "udp"` || !strings.HasPrefix(raw, `{"ok":false,"error":"unknown tunnel type \"udp\""`) {
		t.Fatalf("unknown type: %s", raw)
	}
}

func TestCompat_OldClientNewRelay_RefusedAtTheHandshake(t *testing.T) {
	s, pool := newRelay(t, "0.7.0")

	_, raw := dialOld(t, s, pool, "bur_test_bad0", "0.6.0")
	if want := `{"ok":false,"error":"invalid token"`; !strings.HasPrefix(raw, want) {
		t.Fatalf("bad token\n got %s\nwant it to start with %s", raw, want)
	}
	var ar oldAuthResponse
	if err := json.Unmarshal([]byte(raw), &ar); err != nil || ar != (oldAuthResponse{Error: "invalid token"}) {
		t.Fatalf("bad token as the old client reads it: %+v %v", ar, err)
	}

	// Older than the minimum: the old client prints the relay's text, which
	// says what to do.
	_, raw = dialOld(t, s, pool, "bur_test_0000", "0.6.0")
	ar = oldAuthResponse{}
	if err := json.Unmarshal([]byte(raw), &ar); err != nil || ar.OK || ar.SessionID != "" ||
		ar.Error != "client too old: this relay needs burrow 0.7.0 or newer" {
		t.Fatalf("too old as the old client reads it: %+v %v", ar, err)
	}

	// The rolling build names no release and is let in.
	_, raw = dialOld(t, s, pool, "bur_test_0000", "develop")
	ar = oldAuthResponse{}
	if err := json.Unmarshal([]byte(raw), &ar); err != nil || !ar.OK {
		t.Fatalf("develop: %s", raw)
	}
}

// oldRelay is a relay of before the change: it answers with the JSON written
// out here and does not look at fields it never had. visit sends one byte
// through the tunnel that was registered last and waits for it to come back.
type oldRelay struct {
	addr  string
	pool  *x509.CertPool
	visit chan chan error

	mu        sync.Mutex
	registers []string
}

func startOldRelay(t *testing.T) *oldRelay {
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
	r := &oldRelay{addr: ln.Addr().String(), pool: x509.NewCertPool(), visit: make(chan chan error)}
	r.pool.AppendCertsFromPEM(caPEM)

	var wg sync.WaitGroup
	var conns []net.Conn
	var mu sync.Mutex
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Go(func() { defer conn.Close(); r.serve(t, conn) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return r
}

func (r *oldRelay) serve(t *testing.T, conn net.Conn) {
	var env proto.Envelope
	if proto.ReadFrame(conn, &env) != nil || env.Type != proto.MsgAuthRequest {
		return
	}
	if proto.WriteFrame(conn, proto.Envelope{Type: proto.MsgAuthResponse, Payload: json.RawMessage(`{"ok":true,"session_id":"old-session"}`)}) != nil {
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		return
	}
	defer sess.Close()
	ctrl, err := sess.Accept()
	if err != nil {
		return
	}
	defer ctrl.Close()

	// Messages of the client arrive on one goroutine; visits are made from
	// another, as in the relay.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case res := <-r.visit:
				res <- func() error {
					if err := proto.WriteFrame(ctrl, proto.Envelope{Type: proto.MsgNewConnection,
						Payload: json.RawMessage(`{"tunnel_id":"old-tunnel","stream_id":"old-stream","source_ip":"203.0.113.7:4711"}`)}); err != nil {
						return err
					}
					data, err := sess.Accept()
					if err != nil {
						return err
					}
					defer data.Close()
					_ = data.SetDeadline(time.Now().Add(3 * time.Second))
					var hdr proto.Envelope
					if err := proto.ReadFrame(data, &hdr); err != nil {
						return err
					}
					if hdr.Type != proto.MsgStreamOpen || string(hdr.Payload) != `{"stream_id":"old-stream","tunnel_id":"old-tunnel"}` {
						return errors.New("stream header: " + string(hdr.Payload))
					}
					if _, err := data.Write([]byte("x")); err != nil {
						return err
					}
					b := make([]byte, 1)
					if _, err := io.ReadFull(data, b); err != nil {
						return err
					}
					if b[0] != 'x' {
						return errors.New("another byte came back")
					}
					return nil
				}()
			}
		}
	}()

	for {
		if proto.ReadFrame(ctrl, &env) != nil {
			return
		}
		switch env.Type {
		case proto.MsgTunnelRegister:
			r.mu.Lock()
			r.registers = append(r.registers, string(env.Payload))
			r.mu.Unlock()
			// The old relay reads the four fields it knows and answers.
			var req struct {
				Name       string `json:"name"`
				Type       string `json:"type"`
				RemotePort int    `json:"remote_port"`
				LocalAddr  string `json:"local_addr"`
			}
			if json.Unmarshal(env.Payload, &req) != nil {
				return
			}
			answer := `{"ok":true,"tunnel_id":"old-tunnel","remote_port":9000}`
			if req.Type == "http" {
				answer = `{"ok":true,"tunnel_id":"old-tunnel","url":"https://old.example.com/svc/k7p2qx/"}`
			}
			_ = proto.WriteFrame(ctrl, proto.Envelope{Type: proto.MsgTunnelRegisterResp, Payload: json.RawMessage(answer)})
		case proto.MsgPing:
			_ = proto.WriteFrame(ctrl, proto.Envelope{Type: proto.MsgPong, Payload: env.Payload})
		}
	}
}

func (r *oldRelay) throughTunnel(t *testing.T) {
	t.Helper()
	res := make(chan error, 1)
	select {
	case r.visit <- res:
	case <-time.After(3 * time.Second):
		t.Fatal("the old relay has no client to visit")
	}
	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("one byte through the tunnel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the byte did not come back")
	}
}

// `burrow http 3000` against a relay of before the change.
func TestCompat_NewClientOldRelay(t *testing.T) {
	r := startOldRelay(t)
	local := echoOnce(t)
	obs := &regObserver{}
	runNewClient(t, r.addr, r.pool, obs, client.TunnelSpec{Name: "web", Type: "http", LocalAddr: local})
	got := obs.wait(t, 1)["web"]
	if got.TunnelID != "old-tunnel" || got.URL != "https://old.example.com/svc/k7p2qx/" || got.AccessMode != "" ||
		got.Created || got.DashboardURL != "" || got.Ignored.Any() || got.SlugUnacknowledged {
		t.Fatalf("registration: %+v", got)
	}
	r.throughTunnel(t)
	obs.mu.Lock()
	if len(obs.sessions) != 1 || obs.sessions[0] != (client.SessionInfo{}) {
		t.Errorf("an older relay says nothing of itself: %+v", obs.sessions)
	}
	obs.mu.Unlock()
	// What the client sent is what a client of before the change sent.
	r.mu.Lock()
	sent := r.registers[0]
	r.mu.Unlock()
	if want := `{"name":"web","type":"http","remote_port":0,"local_addr":"` + local + `"}`; sent != want {
		t.Fatalf("registration sent\n got %s\nwant %s", sent, want)
	}
}

// `burrow http 3000 --access login` (or api-key) against a relay of before the
// change. That relay ignores the wish and serves the service to everyone, so
// the client must not keep the tunnel: it ends at once, no visitor is served,
// and the error names the service and its address.
func TestCompat_NewClientOldRelay_AccessThatWasNotAppliedEndsTheClient(t *testing.T) {
	for _, spec := range []client.TunnelSpec{
		{Name: "web", Type: "http", Access: "burrow_login"},
		{Name: "web", Type: "http", Access: "api_key"},
		{Name: "web", Type: "http", Slug: "my-app", Access: "burrow_login"},
	} {
		r := startOldRelay(t)
		spec.LocalAddr = echoOnce(t)
		obs := &regObserver{}
		c := client.New(client.Options{
			Server: r.addr, Token: "bur_test_0000", RootCAs: r.pool, ServerName: "localhost",
			Tunnels: []client.TunnelSpec{spec}, Observer: obs, Logger: slog.New(slog.DiscardHandler), StopOnRefusal: true,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.Run(ctx)
		cancel()
		var ae *client.AccessNotAppliedError
		if !errors.As(err, &ae) {
			t.Fatalf("%+v: Run returned %v", spec, err)
		}
		if *ae != (client.AccessNotAppliedError{Name: "web", Access: spec.Access, URL: "https://old.example.com/svc/k7p2qx/"}) {
			t.Fatalf("%+v: %+v", spec, *ae)
		}
		obs.mu.Lock()
		if len(obs.regs) != 0 {
			t.Fatalf("%+v: the tunnel was reported: %+v", spec, obs.regs)
		}
		obs.mu.Unlock()
		// The client has hung up. Until the relay has noticed, a visit it
		// starts must fail; then it has no session left to send one to.
		gone := false
		for deadline := time.Now().Add(3 * time.Second); !gone && time.Now().Before(deadline); {
			res := make(chan error, 1)
			select {
			case r.visit <- res:
				if err := <-res; err == nil {
					t.Fatalf("%+v: a visitor was served", spec)
				}
			case <-time.After(100 * time.Millisecond):
				gone = true
			}
		}
		if !gone {
			t.Fatalf("%+v: the relay still has a session", spec)
		}
		r.mu.Lock()
		n := len(r.registers)
		r.mu.Unlock()
		if n != 1 {
			t.Fatalf("%+v: %d registrations, want one and no retry", spec, n)
		}
	}
}

// --slug alone, or with --access open, against a relay of before the change:
// the tunnel works; the client knows the relay said nothing about the slug, so
// that it can warn. The relay's default is open, so that wish needs no word.
func TestCompat_NewClientOldRelay_SlugIsNotSilentlyLost(t *testing.T) {
	for _, tc := range []struct {
		spec client.TunnelSpec
		slug bool
	}{
		{client.TunnelSpec{Name: "web", Type: "http", Slug: "my-app"}, true},
		{client.TunnelSpec{Name: "web", Type: "http", Slug: "my-app", Access: "open"}, true},
		{client.TunnelSpec{Name: "web", Type: "http", Access: "open"}, false},
	} {
		r := startOldRelay(t)
		tc.spec.LocalAddr = echoOnce(t)
		obs := &regObserver{}
		runNewClient(t, r.addr, r.pool, obs, tc.spec)
		got := obs.wait(t, 1)["web"]
		if got.URL != "https://old.example.com/svc/k7p2qx/" || got.SlugUnacknowledged != tc.slug || got.Ignored.Any() {
			t.Fatalf("%+v: registration %+v", tc.spec, got)
		}
		r.throughTunnel(t)
	}
}
