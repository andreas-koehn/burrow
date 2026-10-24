package client

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
)

const newAuthUpstream = `{"ok":true,"session_id":"s1","capabilities":["upstream_protocol"]}`

func registerFields(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The client names the protocol only when its user chose one, and a relay that
// announces the capability applies it.
func TestUpstream_SentWhenChosen(t *testing.T) {
	r := startRawRelay(t, newAuthUpstream, newRegister)
	c := New(r.options(
		TunnelSpec{Name: "api", Type: "http", LocalAddr: "127.0.0.1:50051", UpstreamProtocol: proto.UpstreamH2C},
		TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"},
		TunnelSpec{Name: "one", Type: "http", LocalAddr: "127.0.0.1:3001", UpstreamProtocol: proto.UpstreamHTTP1},
	))
	runUntilDone(t, c)
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("never registered")
	}
	_, regs := r.sent()
	if len(regs) != 3 {
		t.Fatalf("registrations: %v", regs)
	}
	for i, want := range []string{"h2c", "", "http1"} {
		got := ""
		if v, ok := registerFields(t, regs[i])["upstream_protocol"]; ok {
			_ = json.Unmarshal(v, &got)
		}
		if got != want {
			t.Errorf("registration %d: upstream_protocol %q, want %q (%s)", i, got, want, regs[i])
		}
	}
}

// A relay that does not announce the capability would speak HTTP/1.1 to a gRPC
// server: the registration is not sent, and the run ends without trying again.
func TestUpstream_OldRelayRefusesNonDefault(t *testing.T) {
	for _, p := range []string{proto.UpstreamH2C, proto.UpstreamH2, proto.UpstreamHTTPS} {
		r := startRawRelay(t, oldAuthOK, newRegister)
		err, ended := runFor(t, r.options(TunnelSpec{Name: "api", Type: "http", LocalAddr: "127.0.0.1:50051", UpstreamProtocol: p}), 3*time.Second)
		var ue *UpstreamNotAppliedError
		if !ended || !errors.As(err, &ue) || ue.Protocol != p || ue.Name != "api" {
			t.Fatalf("%s: err = %v, ended %v", p, err, ended)
		}
		for _, w := range []string{p, "api", "update the relay"} {
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(w)) {
				t.Errorf("%s: message %q lacks %q", p, err, w)
			}
		}
		auths, regs := r.sent()
		if len(auths) != 1 || len(regs) != 0 {
			t.Fatalf("%s: %d sessions, registrations sent: %v", p, len(auths), regs)
		}
	}
}

// The default, an unchosen protocol and an explicit http1 all work on an old
// relay, and nothing about the protocol is sent to it.
func TestUpstream_OldRelayServesHTTP1(t *testing.T) {
	r := startRawRelay(t, oldAuthOK, newRegister)
	c := New(r.options(
		TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"},
		TunnelSpec{Name: "one", Type: "http", LocalAddr: "127.0.0.1:3001", UpstreamProtocol: proto.UpstreamHTTP1},
	))
	runUntilDone(t, c)
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("never registered")
	}
	_, regs := r.sent()
	for _, raw := range regs {
		if _, ok := registerFields(t, raw)["upstream_protocol"]; ok {
			t.Errorf("an old relay was sent upstream_protocol: %s", raw)
		}
	}
}

// With several services, the one that cannot be served is skipped and the
// others run.
func TestUpstream_OldRelaySkipsOnlyThatService(t *testing.T) {
	r := startRawRelay(t, oldAuthOK, newRegister)
	c := New(r.options(
		TunnelSpec{Name: "api", Type: "http", LocalAddr: "127.0.0.1:50051", UpstreamProtocol: proto.UpstreamH2C},
		TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"},
	))
	runUntilDone(t, c)
	if !waitTrue(c.Registered, 3*time.Second) {
		t.Fatal("web never registered")
	}
	if _, regs := r.sent(); len(regs) != 1 || !strings.Contains(regs[0], `"web"`) {
		t.Fatalf("registrations: %v", regs)
	}
}

// grpcObserver records the grpc-status an observer that wants it is told.
type grpcObserver struct {
	recObserver
	gmu   sync.Mutex
	calls []string
}

func (g *grpcObserver) RequestGRPC(id string, at time.Time, method, path string, status int, grpc *int) {
	s := "none"
	if grpc != nil {
		s = itoa(*grpc)
	}
	g.gmu.Lock()
	g.calls = append(g.calls, id+" "+method+" "+path+" "+itoa(status)+" grpc="+s)
	g.gmu.Unlock()
}

func (g *grpcObserver) got() []string {
	g.gmu.Lock()
	defer g.gmu.Unlock()
	return append([]string(nil), g.calls...)
}

func TestSummaries_GRPCStatusReachesAnObserverThatWantsIt(t *testing.T) {
	r := startRawRelay(t, oldAuthOK, newRegister)
	obs := &grpcObserver{}
	o := r.options(TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
	o.Observer, o.RequestSummaries = obs, true
	c := New(o)
	runUntilDone(t, c)
	if !waitTrue(func() bool { return c.Registered() && obs.has("latency") }, 3*time.Second) {
		t.Fatalf("not ready: %v", obs.snapshot())
	}
	r.push(t, proto.MsgRequestSummary, `{"tunnel_id":"t-web","time":"2026-10-05T14:02:11Z","method":"POST","path":"/pkg.S/M","status":200,"duration_ms":3,"grpc_status":14}`)
	r.push(t, proto.MsgRequestSummary, `{"tunnel_id":"t-web","time":"2026-10-05T14:02:12Z","method":"GET","path":"/","status":200,"duration_ms":3}`)
	if !waitTrue(func() bool { return len(obs.got()) == 2 }, 3*time.Second) {
		t.Fatalf("calls: %v", obs.got())
	}
	got := obs.got()
	if got[0] != "t-web POST /pkg.S/M 200 grpc=14" || got[1] != "t-web GET / 200 grpc=none" {
		t.Fatalf("calls: %v", got)
	}
	// An observer that does not know the field is told as before.
	if obs.has("request:") {
		t.Fatalf("both calls went to Request too: %v", obs.snapshot())
	}
}
