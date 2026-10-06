package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

// rawRelay is a relay that answers with JSON given as text, so that a test can
// stand for a relay of any age down to the byte. It keeps what clients sent.
type rawRelay struct {
	addr string
	pool *x509.CertPool
	wg   sync.WaitGroup

	// auth is the payload of the auth_response; register returns the payload
	// of the tunnel_register_response for a request.
	auth     string
	register func(req proto.TunnelRegister) string

	mu        sync.Mutex
	conns     []io.Closer
	authReqs  []string // payloads of the auth requests, as sent
	registers []string // payloads of the tunnel registrations, as sent
}

func startRawRelay(t *testing.T, auth string, register func(req proto.TunnelRegister) string) *rawRelay {
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
	r := &rawRelay{pool: x509.NewCertPool(), auth: auth, register: register}
	r.pool.AppendCertsFromPEM(caPEM)
	r.addr = ln.Addr().String()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			r.mu.Lock()
			r.conns = append(r.conns, conn)
			r.mu.Unlock()
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				defer conn.Close()
				var env proto.Envelope
				if proto.ReadFrame(conn, &env) != nil {
					return
				}
				r.mu.Lock()
				r.authReqs = append(r.authReqs, string(env.Payload))
				r.mu.Unlock()
				if proto.WriteFrame(conn, proto.Envelope{Type: proto.MsgAuthResponse, Payload: json.RawMessage(r.auth)}) != nil {
					return
				}
				var ok struct {
					OK bool `json:"ok"`
				}
				if json.Unmarshal([]byte(r.auth), &ok) != nil || !ok.OK {
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
						r.mu.Lock()
						r.registers = append(r.registers, string(env.Payload))
						r.mu.Unlock()
						var req proto.TunnelRegister
						_ = proto.DecodePayload(env, &req)
						_ = proto.WriteFrame(stream, proto.Envelope{Type: proto.MsgTunnelRegisterResp, Payload: json.RawMessage(r.register(req))})
					case proto.MsgPing:
						_ = proto.WriteFrame(stream, proto.Envelope{Type: proto.MsgPong, Payload: env.Payload})
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		r.mu.Lock()
		for _, c := range r.conns {
			_ = c.Close()
		}
		r.mu.Unlock()
		r.wg.Wait()
	})
	return r
}

func (r *rawRelay) sent() (auths, registers []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.authReqs...), append([]string(nil), r.registers...)
}

func (r *rawRelay) options(tunnels ...TunnelSpec) Options {
	return Options{
		Server: r.addr, Token: "bur_test_0000", RootCAs: r.pool, ServerName: "localhost",
		Tunnels: tunnels, Logger: slog.New(slog.DiscardHandler),
	}
}

// sessionObserver records the registrations and what the relay said of itself.
type sessionObserver struct {
	recObserver
	mu       sync.Mutex
	regs     []RegisteredTunnel
	sessions []SessionInfo
}

func (s *sessionObserver) Registered(t RegisteredTunnel) {
	s.mu.Lock()
	s.regs = append(s.regs, t)
	s.mu.Unlock()
}

func (s *sessionObserver) Session(i SessionInfo) {
	s.mu.Lock()
	s.sessions = append(s.sessions, i)
	s.mu.Unlock()
}

func (s *sessionObserver) firstReg(t *testing.T) RegisteredTunnel {
	t.Helper()
	if !waitTrue(func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.regs) > 0 }, 3*time.Second) {
		t.Fatal("no tunnel was registered")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.regs[0]
}

// The pre-change answers of a relay, byte for byte.
const (
	oldAuthOK       = `{"ok":true,"session_id":"s1"}`
	oldAuthRefused  = `{"ok":false,"error":"invalid token"}`
	oldRegisteredOK = `{"ok":true,"tunnel_id":"t1","url":"https://burrow.example.com/svc/abc234/"}`
)

func oldRegister(proto.TunnelRegister) string { return oldRegisteredOK }

func TestClient_SendsSlugAndAccessOnlyWhenAsked(t *testing.T) {
	r := startRawRelay(t, oldAuthOK, func(proto.TunnelRegister) string {
		return `{"ok":true,"tunnel_id":"t1","url":"https://burrow.example.com/svc/abc234/","access_mode":"burrow_login"}`
	})
	plain := TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"}
	wished := TunnelSpec{Name: "app", Type: "http", LocalAddr: "127.0.0.1:3001", Slug: "my-app", Access: "burrow_login"}
	c := New(r.options(plain, wished))
	runUntilDone(t, c)
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("never registered")
	}
	auths, regs := r.sent()
	if len(auths) != 1 || len(regs) != 2 {
		t.Fatalf("%d auth requests, %d registrations", len(auths), len(regs))
	}
	// Without the wishes the registration is what it has always been.
	if want := `{"name":"web","type":"http","remote_port":0,"local_addr":"127.0.0.1:3000"}`; regs[0] != want {
		t.Errorf("registration without wishes:\n got %s\nwant %s", regs[0], want)
	}
	if want := `{"name":"app","type":"http","remote_port":0,"local_addr":"127.0.0.1:3001","slug":"my-app","access":"burrow_login"}`; regs[1] != want {
		t.Errorf("registration with wishes:\n got %s\nwant %s", regs[1], want)
	}
	// No capability exists yet, so none is announced: the request has the
	// fields it has always had.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(auths[0]), &keys); err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		switch k {
		case "protocol_version", "token", "client_version", "os", "arch":
		default:
			t.Errorf("auth request has the new field %q", k)
		}
	}
	if len(keys) != 5 {
		t.Errorf("auth request has %d fields, want 5", len(keys))
	}
}

func TestClient_ReportsWhatTheRelaySaysAboutTheService(t *testing.T) {
	r := startRawRelay(t,
		`{"ok":true,"session_id":"s1","relay_version":"0.8.0","user_email":"owner@example.com"}`,
		func(req proto.TunnelRegister) string {
			if req.Name == "new" {
				return `{"ok":true,"tunnel_id":"t1","url":"https://burrow.example.com/svc/my-app/","access_mode":"burrow_login","created":true,"dashboard_url":"https://burrow.example.com/services/svc-1"}`
			}
			return `{"ok":true,"tunnel_id":"t2","url":"https://burrow.example.com/svc/abc234/","access_mode":"open","dashboard_url":"https://burrow.example.com/services/svc-2","ignored":["access","slug"]}`
		})
	obs := &sessionObserver{}
	o := r.options(
		TunnelSpec{Name: "new", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app", Access: "burrow_login"},
		TunnelSpec{Name: "old", Type: "http", LocalAddr: "127.0.0.1:3001", Slug: "other", Access: "api_key"},
	)
	o.Observer = obs
	c := New(o)
	runUntilDone(t, c)
	if !waitTrue(func() bool { obs.mu.Lock(); defer obs.mu.Unlock(); return len(obs.regs) == 2 }, 3*time.Second) {
		t.Fatal("two registrations were expected")
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if want := (RegisteredTunnel{TunnelID: "t1", Name: "new", Type: "http", LocalAddr: "127.0.0.1:3000",
		URL: "https://burrow.example.com/svc/my-app/", AccessMode: "burrow_login", Created: true,
		DashboardURL: "https://burrow.example.com/services/svc-1"}); obs.regs[0] != want {
		t.Errorf("created service:\n got %+v\nwant %+v", obs.regs[0], want)
	}
	if want := (RegisteredTunnel{TunnelID: "t2", Name: "old", Type: "http", LocalAddr: "127.0.0.1:3001",
		URL: "https://burrow.example.com/svc/abc234/", AccessMode: "open",
		DashboardURL: "https://burrow.example.com/services/svc-2", Ignored: OptionSet{Slug: true, Access: true}}); obs.regs[1] != want {
		t.Errorf("existing service:\n got %+v\nwant %+v", obs.regs[1], want)
	}
	if len(obs.sessions) != 1 || obs.sessions[0] != (SessionInfo{RelayVersion: "0.8.0"}) {
		t.Errorf("session info: %+v", obs.sessions)
	}
}

// An older relay ignores a wished slug and does not say so. The client works,
// and it knows that nothing was said about the slug.
func TestClient_OlderRelay(t *testing.T) {
	r := startRawRelay(t, oldAuthOK, oldRegister)
	obs := &sessionObserver{}
	o := r.options(
		TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app"},
		TunnelSpec{Name: "plain", Type: "http", LocalAddr: "127.0.0.1:3001"},
		// The relay's default is open: a wish for it needs no answer.
		TunnelSpec{Name: "open", Type: "http", LocalAddr: "127.0.0.1:3002", Access: "open"},
		TunnelSpec{Name: "both", Type: "http", LocalAddr: "127.0.0.1:3003", Slug: "pub", Access: "open"},
	)
	o.Observer = obs
	c := New(o)
	runUntilDone(t, c)
	if !waitTrue(func() bool { obs.mu.Lock(); defer obs.mu.Unlock(); return len(obs.regs) == 4 }, 3*time.Second) {
		t.Fatal("four registrations were expected")
	}
	if !c.Registered() {
		t.Fatal("the client does not count as registered")
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if want := (RegisteredTunnel{TunnelID: "t1", Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000",
		URL: "https://burrow.example.com/svc/abc234/", SlugUnacknowledged: true}); obs.regs[0] != want {
		t.Errorf("with a slug:\n got %+v\nwant %+v", obs.regs[0], want)
	}
	if obs.regs[1].SlugUnacknowledged || obs.regs[1].AccessMode != "" {
		t.Errorf("without wishes: %+v", obs.regs[1])
	}
	if obs.regs[2].SlugUnacknowledged {
		t.Errorf("access open alone: %+v", obs.regs[2])
	}
	if !obs.regs[3].SlugUnacknowledged {
		t.Errorf("slug and access open: %+v", obs.regs[3])
	}
	// An older relay says nothing of itself.
	if len(obs.sessions) != 1 || obs.sessions[0] != (SessionInfo{}) {
		t.Errorf("session info: %+v", obs.sessions)
	}
}

// An older relay ignores a wished access mode and makes the service open. A
// client that asked for login or an API key must not serve it like that: Run
// ends, with or without StopOnRefusal, and the tunnel is never reported.
func TestClient_OlderRelayCannotRestrictAccess(t *testing.T) {
	for _, spec := range []TunnelSpec{
		{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Access: "burrow_login"},
		{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Access: "api_key"},
		{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app", Access: "burrow_login"},
	} {
		for _, stop := range []bool{true, false} {
			r := startRawRelay(t, oldAuthOK, oldRegister)
			obs := &sessionObserver{}
			o := r.options(TunnelSpec{Name: "first", Type: "http", LocalAddr: "127.0.0.1:2999"}, spec)
			o.Observer, o.StopOnRefusal = obs, stop
			err, ended := runFor(t, o, 3*time.Second)
			if !ended {
				t.Fatalf("%+v: the client kept the tunnel", spec)
			}
			var ae *AccessNotAppliedError
			if !errors.As(err, &ae) {
				t.Fatalf("%+v: Run returned %v", spec, err)
			}
			if *ae != (AccessNotAppliedError{Name: "web", Access: spec.Access, URL: "https://burrow.example.com/svc/abc234/"}) {
				t.Fatalf("%+v: error %+v", spec, *ae)
			}
			if auths, _ := r.sent(); len(auths) != 1 {
				t.Fatalf("%d connection attempts, want 1", len(auths))
			}
			obs.mu.Lock()
			for _, reg := range obs.regs {
				if reg.Name == "web" {
					t.Fatalf("the tunnel was reported as registered: %+v", reg)
				}
			}
			obs.mu.Unlock()
		}
	}
}

// runFor runs the client and returns what Run returned, or nil and false when
// it was still running after d.
func runFor(t *testing.T, o Options, d time.Duration) (error, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- New(o).Run(ctx) }()
	select {
	case err := <-res:
		cancel()
		return err, true
	case <-time.After(d):
		cancel()
		<-res
		return nil, false
	}
}

func TestRun_StopsOnARefusalThatWillNotChange(t *testing.T) {
	registerRefusal := func(payload string) (string, func(proto.TunnelRegister) string) {
		return oldAuthOK, func(proto.TunnelRegister) string { return payload }
	}
	type relay struct {
		auth     string
		register func(proto.TunnelRegister) string
	}
	mk := func(auth string, reg func(proto.TunnelRegister) string) relay { return relay{auth, reg} }
	cases := []struct {
		name    string
		relay   relay
		code    string
		message string
		text    string // what Error() says: the text the client has always logged
	}{
		{"invalid token", mk(`{"ok":false,"error":"invalid token","code":"invalid_token"}`, oldRegister),
			proto.CodeInvalidToken, "invalid token", "auth failed: invalid token"},
		{"invalid token, older relay", mk(oldAuthRefused, oldRegister),
			proto.CodeInvalidToken, "invalid token", "auth failed: invalid token"},
		{"client too old", mk(`{"ok":false,"error":"client too old: this relay needs burrow 0.9.0 or newer","code":"client_too_old"}`, oldRegister),
			proto.CodeClientTooOld, "client too old: this relay needs burrow 0.9.0 or newer", "auth failed: client too old: this relay needs burrow 0.9.0 or newer"},
		{"slug taken", mk(registerRefusal(`{"ok":false,"error":"slug already in use; try: abc234","code":"slug_taken"}`)),
			proto.CodeSlugTaken, "slug already in use; try: abc234", "register failed: slug already in use; try: abc234"},
		{"slug invalid", mk(registerRefusal(`{"ok":false,"error":"slug must be 3-40 characters","code":"slug_invalid"}`)),
			proto.CodeSlugInvalid, "slug must be 3-40 characters", "register failed: slug must be 3-40 characters"},
		{"access invalid", mk(registerRefusal(`{"ok":false,"error":"burrow_login requires a configured auth_domain","code":"access_invalid"}`)),
			proto.CodeAccessInvalid, "burrow_login requires a configured auth_domain", "register failed: burrow_login requires a configured auth_domain"},
		{"forbidden", mk(registerRefusal(`{"ok":false,"error":"your role may not choose a slug","code":"forbidden"}`)),
			proto.CodeForbidden, "your role may not choose a slug", "register failed: your role may not choose a slug"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRawRelay(t, c.relay.auth, c.relay.register)
			o := r.options(TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
			o.StopOnRefusal = true
			err, ended := runFor(t, o, 3*time.Second)
			if !ended {
				t.Fatal("the client kept reconnecting")
			}
			var re *RefusedError
			if !errors.As(err, &re) {
				t.Fatalf("Run returned %v, want a *RefusedError", err)
			}
			if re.Code != c.code || re.Message != c.message || err.Error() != c.text {
				t.Fatalf("code %q message %q text %q", re.Code, re.Message, err.Error())
			}
			if auths, _ := r.sent(); len(auths) != 1 {
				t.Fatalf("%d connection attempts, want 1", len(auths))
			}
			if strings.Contains(err.Error(), "bur_test_0000") {
				t.Fatal("the error carries the token")
			}
		})
	}
}

// `burrow connect` does not ask for it and keeps reconnecting, as it always has.
func TestRun_WithoutStopOnRefusalKeepsReconnecting(t *testing.T) {
	r := startRawRelay(t, `{"ok":false,"error":"invalid token","code":"invalid_token"}`, oldRegister)
	o := r.options(TunnelSpec{Name: "web", Type: "tcp", LocalAddr: "127.0.0.1:3000"})
	var logs syncBuffer
	o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(o).Run(ctx) }()
	ok := waitTrue(func() bool { a, _ := r.sent(); return len(a) >= 2 }, 5*time.Second)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	if !ok {
		t.Fatal("the client did not try again")
	}
	if !strings.Contains(logs.String(), `msg="connection ended" err="auth failed: invalid token"`) {
		t.Fatalf("the log line changed: %s", logs.String())
	}
}

// A refusal that may pass (a port that is taken, a relay in trouble) is tried
// again, with or without StopOnRefusal.
func TestRun_OtherRefusalsAreTriedAgain(t *testing.T) {
	for _, payload := range []string{
		`{"ok":false,"error":"port 9000 in use"}`,
		`{"ok":false,"error":"http tunnels not configured"}`,
		`{"ok":false,"error":"port 9000 in use","code":"port_unavailable"}`,
		`{"ok":false,"error":"resolve service: db down","code":"internal"}`,
		`{"ok":false,"error":"something new","code":"a_code_of_the_future"}`,
	} {
		r := startRawRelay(t, oldAuthOK, func(proto.TunnelRegister) string { return payload })
		o := r.options(TunnelSpec{Name: "web", Type: "tcp", LocalAddr: "127.0.0.1:3000"})
		o.StopOnRefusal = true
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- New(o).Run(ctx) }()
		ok := waitTrue(func() bool { a, _ := r.sent(); return len(a) >= 2 }, 5*time.Second)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || !ok {
			t.Fatalf("%s: tried again %v, Run returned %v", payload, ok, err)
		}
	}
}

func TestCheckAuth_CodeAndRelayVersion(t *testing.T) {
	r := startRawRelay(t, `{"ok":true,"session_id":"s1","relay_version":"0.8.0"}`, oldRegister)
	res, err := CheckAuth(context.Background(), r.options())
	if err != nil || !res.OK || res.RelayVersion != "0.8.0" || res.Code != "" {
		t.Fatalf("%+v %v", res, err)
	}
	r = startRawRelay(t, `{"ok":false,"error":"client too old: this relay needs burrow 0.9.0 or newer","code":"client_too_old"}`, oldRegister)
	res, err = CheckAuth(context.Background(), r.options())
	if err != nil || res.OK || res.Code != proto.CodeClientTooOld || !strings.Contains(res.Error, "0.9.0") {
		t.Fatalf("%+v %v", res, err)
	}
	// An older relay sends the text alone; the code is derived from it.
	r = startRawRelay(t, oldAuthRefused, oldRegister)
	res, err = CheckAuth(context.Background(), r.options())
	if err != nil || res.OK || res.Code != proto.CodeInvalidToken || res.Error != "invalid token" {
		t.Fatalf("%+v %v", res, err)
	}
}
