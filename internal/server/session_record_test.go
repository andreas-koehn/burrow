package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proto"
)

type recordedSession struct {
	tokenID string
	rec     db.ClientSessionRecord
}

// sessionRecorderFake keeps every RecordClientSession call and answers err.
type sessionRecorderFake struct {
	mu    sync.Mutex
	calls []recordedSession
	err   error
}

func (f *sessionRecorderFake) RecordClientSession(_ context.Context, tokenID string, rec db.ClientSessionRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec.Served = append([]string{}, rec.Served...)
	f.calls = append(f.calls, recordedSession{tokenID: tokenID, rec: rec})
	return f.err
}

func (f *sessionRecorderFake) snapshot() []recordedSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedSession(nil), f.calls...)
}

// waitCalls waits until the recorder holds at least n calls and returns them.
func (f *sessionRecorderFake) waitCalls(t *testing.T, n int) []recordedSession {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls := f.snapshot()
		if len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d recorded sessions, got %d: %+v", n, len(calls), calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// recordedClient is a client connected to a relay that records sessions.
type recordedClient struct {
	srv  *Server
	conn net.Conn
	ctrl net.Conn
}

// connectRecorded starts a relay with the recorder and connects one client
// that says it is linux/amd64 0.9.0. http tunnels need no port.
func connectRecorded(t *testing.T, auth TokenAuthenticator, rec SessionRecorder) *recordedClient {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"),
		TLSKey: filepath.Join(dir, "dev-server-key.pem"), Auth: auth,
		PublicBind: "127.0.0.1", Services: fakeResolver{id: "svc-1", sub: "abc123"},
		SessionRecorder: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Wait() })
	waitListening(t, srv)

	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := proto.WriteMessage(conn, proto.MsgAuthRequest, proto.AuthRequest{
		ProtocolVersion: 1, Token: "secret", OS: "linux", Arch: "amd64", ClientVersion: "0.9.0",
	}); err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := proto.ReadFrame(conn, &env); err != nil || env.Type != proto.MsgAuthResponse {
		t.Fatalf("auth: %v %v", env.Type, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	sess, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	ctrl, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	return &recordedClient{srv: srv, conn: conn, ctrl: ctrl}
}

func (c *recordedClient) register(t *testing.T, name string) proto.TunnelRegisterResponse {
	t.Helper()
	rr := doRegister(t, c.ctrl, proto.TunnelRegister{Name: name, Type: "http", LocalAddr: "127.0.0.1:3000"})
	if !rr.OK {
		t.Fatalf("register %s: %+v", name, rr)
	}
	return rr
}

var recordedAuth = tokenFakeAuth{uid: "u1", id: "tok-1", name: "laptop"}

func TestSessionRecordedOnConnect(t *testing.T) {
	rec := &sessionRecorderFake{}
	c := connectRecorded(t, recordedAuth, rec)
	calls := rec.waitCalls(t, 1)
	want := recordedSession{tokenID: "tok-1", rec: db.ClientSessionRecord{
		OS: "linux", Arch: "amd64", ClientVersion: "0.9.0", RemoteAddr: c.conn.LocalAddr().String(), Served: []string{},
	}}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %+v, want one %+v", calls, want)
	}
	if ss := c.srv.SnapshotSessions(); len(ss) != 1 || ss[0].TokenID != "tok-1" || ss[0].Token != "laptop" {
		t.Fatalf("snapshot: %+v", ss)
	}
}

func TestSessionRecordedWithServedNames(t *testing.T) {
	rec := &sessionRecorderFake{}
	c := connectRecorded(t, recordedAuth, rec)
	c.register(t, "web")
	ollama := c.register(t, "ollama")
	// The record is written before the answer: no waiting.
	calls := rec.snapshot()
	if len(calls) != 3 {
		t.Fatalf("want a record at connect and one for each tunnel, got %+v", calls)
	}
	if got := calls[1].rec.Served; !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("after the first tunnel: %v", got)
	}
	last := calls[2]
	if last.tokenID != "tok-1" || last.rec.OS != "linux" || !reflect.DeepEqual(last.rec.Served, []string{"ollama", "web"}) {
		t.Fatalf("last record = %+v, want both names, sorted", last)
	}

	// A tunnel that goes away is no longer served.
	if err := proto.WriteMessage(c.ctrl, proto.MsgTunnelUnregister, proto.TunnelUnregister{TunnelID: ollama.TunnelID}); err != nil {
		t.Fatal(err)
	}
	calls = rec.waitCalls(t, 4)
	if got := calls[3].rec.Served; !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("after the unregister: %v", got)
	}
}

func TestSessionRecordedOnDisconnect(t *testing.T) {
	rec := &sessionRecorderFake{}
	c := connectRecorded(t, recordedAuth, rec)
	c.register(t, "web")
	before := len(rec.snapshot())
	if before != 2 {
		t.Fatalf("want 2 records before the disconnect, got %d", before)
	}
	_ = c.conn.Close()
	calls := rec.waitCalls(t, before+1)
	last := calls[len(calls)-1]
	if last.tokenID != "tok-1" || last.rec.ClientVersion != "0.9.0" || !reflect.DeepEqual(last.rec.Served, []string{"web"}) {
		t.Fatalf("final record = %+v, want what the session served last", last)
	}
	// The record is written before the session leaves the registry.
	deadline := time.Now().Add(3 * time.Second)
	for len(c.srv.SnapshotSessions()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the session never left the registry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(rec.snapshot()); n != before+1 {
		t.Fatalf("want exactly one record at the disconnect, got %d more", n-before)
	}
}

func TestSessionRecorderFailureDoesNotDropClient(t *testing.T) {
	rec := &sessionRecorderFake{err: errors.New("database is locked")}
	c := connectRecorded(t, recordedAuth, rec)
	rec.waitCalls(t, 1)
	c.register(t, "web")
	if err := proto.WriteMessage(c.ctrl, proto.MsgPing, proto.Ping{Nonce: "n1"}); err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	_ = c.ctrl.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := proto.ReadFrame(c.ctrl, &env); err != nil || env.Type != proto.MsgPong {
		t.Fatalf("the session must stay up: %v %v", env.Type, err)
	}
	if ss := c.srv.SnapshotSessions(); len(ss) != 1 || len(ss[0].Tunnels) != 1 {
		t.Fatalf("snapshot: %+v", ss)
	}
	if n := len(rec.snapshot()); n != 2 {
		t.Fatalf("want the recorder asked at connect and at the tunnel, got %d calls", n)
	}
}

// A session without a token id (an authenticator that names none) leaves no
// record, and a relay without a recorder runs as before.
func TestSessionNotRecordedWithoutTokenIDOrRecorder(t *testing.T) {
	rec := &sessionRecorderFake{}
	c := connectRecorded(t, fakeAuth{uid: "u1"}, rec)
	c.register(t, "web")
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("no token id, yet recorded: %+v", calls)
	}
	connectRecorded(t, recordedAuth, nil).register(t, "web")
}
