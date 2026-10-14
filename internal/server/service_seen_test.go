package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proto"
)

type serviceTouch struct{ serviceID, tokenName string }

// serviceRecorderFake keeps every TouchServiceLastSeen call and answers err.
type serviceRecorderFake struct {
	mu    sync.Mutex
	calls []serviceTouch
	err   error
}

func (f *serviceRecorderFake) TouchServiceLastSeen(_ context.Context, serviceID, tokenName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, serviceTouch{serviceID: serviceID, tokenName: tokenName})
	return f.err
}

func (f *serviceRecorderFake) snapshot() []serviceTouch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]serviceTouch(nil), f.calls...)
}

// waitCalls waits until the recorder holds at least n calls and returns them.
func (f *serviceRecorderFake) waitCalls(t *testing.T, n int) []serviceTouch {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls := f.snapshot()
		if len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d service stamps, got %d: %+v", n, len(calls), calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// nameResolver binds the http tunnel named n to the service "svc-n".
type nameResolver struct{}

func (nameResolver) Resolve(_ context.Context, _, name, _ string) (string, string, error) {
	return "svc-" + name, "slug-" + name, nil
}

// connectStamped starts a relay that stamps services on rec and connects one
// client that logged in with the token "laptop".
func connectStamped(t *testing.T, rec ServiceRecorder, set func(*Options)) *recordedClient {
	t.Helper()
	return connectRelay(t, recordedAuth, func(o *Options) {
		o.Services = nameResolver{}
		o.ServiceRecorder = rec
		if set != nil {
			set(o)
		}
	})
}

func TestServiceStampedOnRegister(t *testing.T) {
	rec := &serviceRecorderFake{}
	c := connectStamped(t, rec, nil)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("a client that serves nothing stamps nothing: %+v", calls)
	}
	c.register(t, "web")
	// The stamp is written before the answer: no waiting.
	if calls, want := rec.snapshot(), []serviceTouch{{"svc-web", "laptop"}}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
}

func TestServiceStampedOnUnregister(t *testing.T) {
	rec := &serviceRecorderFake{}
	c := connectStamped(t, rec, nil)
	c.register(t, "web")
	ollama := c.register(t, "ollama")
	if err := proto.WriteMessage(c.ctrl, proto.MsgTunnelUnregister, proto.TunnelUnregister{TunnelID: ollama.TunnelID}); err != nil {
		t.Fatal(err)
	}
	calls := rec.waitCalls(t, 3)
	want := []serviceTouch{{"svc-web", "laptop"}, {"svc-ollama", "laptop"}, {"svc-ollama", "laptop"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %+v, want one for each register and one for the unregister: %+v", calls, want)
	}

	// A tunnel this session does not have stamps nothing.
	if err := proto.WriteMessage(c.ctrl, proto.MsgTunnelUnregister, proto.TunnelUnregister{TunnelID: "no-such-tunnel"}); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteMessage(c.ctrl, proto.MsgPing, proto.Ping{Nonce: "n1"}); err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	_ = c.ctrl.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := proto.ReadFrame(c.ctrl, &env); err != nil || env.Type != proto.MsgPong {
		t.Fatalf("pong: %v %v", env.Type, err)
	}
	if n := len(rec.snapshot()); n != 3 {
		t.Fatalf("an unknown tunnel was stamped: %+v", rec.snapshot())
	}
}

func TestServiceStampedForEveryTunnelOpenAtDisconnect(t *testing.T) {
	rec := &serviceRecorderFake{}
	c := connectStamped(t, rec, nil)
	c.register(t, "web")
	c.register(t, "ollama")
	gone := c.register(t, "gone")
	if err := proto.WriteMessage(c.ctrl, proto.MsgTunnelUnregister, proto.TunnelUnregister{TunnelID: gone.TunnelID}); err != nil {
		t.Fatal(err)
	}
	before := len(rec.waitCalls(t, 4))
	if before != 4 {
		t.Fatalf("want 4 stamps before the disconnect, got %+v", rec.snapshot())
	}
	_ = c.conn.Close()
	rec.waitCalls(t, before+2)
	waitNoSessions(t, c.srv)
	calls := rec.snapshot()
	if len(calls) != before+2 {
		t.Fatalf("want one stamp for each tunnel still open, got %+v", calls[before:])
	}
	final := []string{calls[before].serviceID, calls[before+1].serviceID}
	slices.Sort(final)
	if !reflect.DeepEqual(final, []string{"svc-ollama", "svc-web"}) || calls[before].tokenName != "laptop" || calls[before+1].tokenName != "laptop" {
		t.Fatalf("final stamps = %+v, want svc-web and svc-ollama by laptop", calls[before:])
	}
}

// A tcp tunnel has no durable service: nothing is stamped for it.
func TestTCPTunnelStampsNoService(t *testing.T) {
	rec := &serviceRecorderFake{}
	c := connectStamped(t, rec, func(o *Options) { o.PortMin, o.PortMax = 18400, 18449 })
	rr := doRegister(t, c.ctrl, proto.TunnelRegister{Name: "db", Type: "tcp", LocalAddr: "127.0.0.1:5432"})
	if !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if err := proto.WriteMessage(c.ctrl, proto.MsgTunnelUnregister, proto.TunnelUnregister{TunnelID: rr.TunnelID}); err != nil {
		t.Fatal(err)
	}
	if rr = doRegister(t, c.ctrl, proto.TunnelRegister{Name: "db", Type: "tcp", LocalAddr: "127.0.0.1:5432"}); !rr.OK {
		t.Fatalf("second register: %+v", rr)
	}
	_ = c.conn.Close()
	waitNoSessions(t, c.srv)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("a tcp tunnel was stamped: %+v", calls)
	}
}

// A failed stamp never fails the tunnel, and a relay without a service
// recorder runs as before.
func TestServiceStampFailureDoesNotFailTunnel(t *testing.T) {
	rec := &serviceRecorderFake{err: errors.New("database is locked")}
	c := connectStamped(t, rec, nil)
	web := c.register(t, "web")
	if err := proto.WriteMessage(c.ctrl, proto.MsgTunnelUnregister, proto.TunnelUnregister{TunnelID: web.TunnelID}); err != nil {
		t.Fatal(err)
	}
	c.register(t, "web")
	if ss := c.srv.SnapshotSessions(); len(ss) != 1 || len(ss[0].Tunnels) != 1 {
		t.Fatalf("snapshot: %+v", ss)
	}
	if n := len(rec.snapshot()); n != 3 {
		t.Fatalf("want the recorder asked at both registers and the unregister, got %d calls", n)
	}
	connectStamped(t, nil, nil).register(t, "web")
}

// logLines collects what a relay logs, one line per record.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logLines) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// with returns the lines that contain every one of the parts.
func (l *logLines) with(parts ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
lines:
	for _, line := range strings.Split(l.buf.String(), "\n") {
		for _, p := range parts {
			if !strings.Contains(line, p) {
				continue lines
			}
		}
		out = append(out, line)
	}
	return out
}

// A token revoked or a service deleted while its client is connected is an
// expected state: the write that finds no row is logged at Debug. Any other
// failure stays a warning.
func TestRecorderNotFoundIsLoggedAtDebug(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		level string
	}{
		{"not found", fmt.Errorf("record: %w", db.ErrNotFound), "level=DEBUG"},
		{"another failure", errors.New("database is locked"), "level=WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &logLines{}
			sessions := &sessionRecorderFake{err: tc.err}
			services := &serviceRecorderFake{err: tc.err}
			c := connectStamped(t, services, func(o *Options) {
				o.SessionRecorder = sessions
				o.Logger = logs.logger()
			})
			c.register(t, "web")
			for _, msg := range []string{"record client session failed", "record service last seen failed"} {
				lines := logs.with(msg)
				if len(lines) == 0 {
					t.Fatalf("%q was not logged", msg)
				}
				for _, line := range lines {
					if !strings.Contains(line, tc.level) {
						t.Errorf("want %s, got: %s", tc.level, line)
					}
				}
			}
		})
	}
}

// portProbe is a recorder that notes, at every write, whether the tcp port of
// the test is free again.
type portProbe struct {
	mu   sync.Mutex
	port int
	free []bool
}

func (p *portProbe) probe() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.port == 0 {
		return
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port)))
	if err == nil {
		_ = ln.Close()
	}
	p.free = append(p.free, err == nil)
}

func (p *portProbe) RecordClientSession(context.Context, string, db.ClientSessionRecord) error {
	p.probe()
	return nil
}

func (p *portProbe) TouchServiceLastSeen(context.Context, string, string) error {
	p.probe()
	return nil
}

// At a disconnect the public listeners are closed and the ports released
// first: a slow database must not keep a port taken. The final session
// record and the final service stamps follow, before the session leaves the
// registry.
func TestFinalRecordsAreWrittenAfterThePortsAreFree(t *testing.T) {
	probe := &portProbe{}
	c := connectStamped(t, probe, func(o *Options) {
		o.SessionRecorder = probe
		o.PortMin, o.PortMax = 18450, 18499
	})
	c.register(t, "web")
	rr := doRegister(t, c.ctrl, proto.TunnelRegister{Name: "db", Type: "tcp", LocalAddr: "127.0.0.1:5432"})
	if !rr.OK || rr.RemotePort == 0 {
		t.Fatalf("register: %+v", rr)
	}
	probe.mu.Lock()
	probe.port = rr.RemotePort
	probe.mu.Unlock()

	_ = c.conn.Close()
	waitNoSessions(t, c.srv)
	probe.mu.Lock()
	defer probe.mu.Unlock()
	// The final session record and the stamp of the one http service.
	if want := []bool{true, true}; !reflect.DeepEqual(probe.free, want) {
		t.Fatalf("port free at the final writes = %v, want %v", probe.free, want)
	}
}
