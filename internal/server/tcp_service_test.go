package server

// A tcp tunnel is a service (native tcp, P01): its registration resolves a
// service row, keeps the service's public port across reconnects, and every
// visitor is counted live and written to the connection log.

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// tcpResolver is a TCPServiceResolver over an in-memory table: one service
// per name, the ports the services hold, and names that belong to an http
// service.
type tcpResolver struct {
	mu       sync.Mutex
	ids      map[string]string // name → service id
	ports    map[int]string    // port → service id
	httpName map[string]bool
	setPorts []string // "svc:port" for every SetServiceTCPPort
	listErr  error
}

func newTCPResolver() *tcpResolver {
	return &tcpResolver{ids: map[string]string{}, ports: map[int]string{}, httpName: map[string]bool{}}
}

func (r *tcpResolver) Resolve(context.Context, string, string, string) (string, string, error) {
	return "", "", errors.New("Resolve must not be used for tcp")
}

func (r *tcpResolver) ResolveTCP(_ context.Context, _, name string, _ ResolveOptions) (Resolved, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.httpName[name] {
		return Resolved{}, &RefusalError{Code: proto.CodeNameInUse, Message: "the name " + name + " is used by an http service"}
	}
	id, ok := r.ids[name]
	if !ok {
		id = "svc-" + name
		r.ids[name] = id
	}
	res := Resolved{ServiceID: id, AccessMode: "open", Created: !ok}
	for p, sid := range r.ports {
		if sid == id {
			res.TCPPort = p
		}
	}
	return res, nil
}

func (r *tcpResolver) TCPPortReservations(context.Context) (map[int]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := map[int]string{}
	for p, id := range r.ports {
		out[p] = id
	}
	return out, nil
}

func (r *tcpResolver) SetServiceTCPPort(_ context.Context, id string, port int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, sid := range r.ports {
		if sid == id {
			delete(r.ports, p)
		}
	}
	if port != 0 {
		r.ports[port] = id
	}
	r.setPorts = append(r.setPorts, id+":"+itoa(port))
	return nil
}

func (r *tcpResolver) portOf(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, sid := range r.ports {
		if sid == id {
			return p
		}
	}
	return 0
}

func itoa(n int) string { return (&net.TCPAddr{Port: n}).String()[1:] }

// recConnLog records the connection-log entries it is given.
type recConnLog struct {
	mu      sync.Mutex
	entries []ConnLogEntry
}

func (l *recConnLog) RecordConnection(_ context.Context, e ConnLogEntry) {
	l.mu.Lock()
	l.entries = append(l.entries, e)
	l.mu.Unlock()
}

func (l *recConnLog) all() []ConnLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ConnLogEntry(nil), l.entries...)
}

// recServiceStamps records which services were stamped.
type recServiceStamps struct {
	mu  sync.Mutex
	ids []string
}

func (r *recServiceStamps) TouchServiceLastSeen(_ context.Context, id, _ string) error {
	r.mu.Lock()
	r.ids = append(r.ids, id)
	r.mu.Unlock()
	return nil
}

func (r *recServiceStamps) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// tcpRelay serves the ports [min, max] with a tcpResolver.
func tcpRelay(t *testing.T, min, max int, res *tcpResolver, mut func(*Options)) *observedRelay {
	t.Helper()
	return newObservedRelay(t, min, func(o *Options) {
		o.PortMax = max
		o.Services = res
		if mut != nil {
			mut(o)
		}
	})
}

func (c *handClient) close() { _ = c.conn.Close() }

func waitTunnelsGone(t *testing.T, r *observedRelay) {
	t.Helper()
	r.waitFor("the session ends", func() bool { return len(r.srv.reg.Sessions()) == 0 })
}

// A tcp registration creates the service and binds the tunnel to it; the port
// is stored on the service, and a reconnect that asks for no port (an old
// client) gets the same one, also when a lower port is free.
func TestRegisterTCP_ServiceAndStickyPort(t *testing.T) {
	res := newTCPResolver()
	stamps := &recServiceStamps{}
	r := tcpRelay(t, 19400, 19405, res, func(o *Options) { o.ServiceRecorder = stamps })

	c := r.client(nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "first", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19400 {
		t.Fatalf("first: %+v", rr)
	}
	rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389"})
	if !rr.OK || rr.RemotePort != 19401 {
		t.Fatalf("rdp: %+v", rr)
	}
	// The answer is the one tcp has always had: no access mode yet (P02).
	if rr.AccessMode != "" || rr.Created || rr.URL != "" {
		t.Fatalf("answer: %+v", rr)
	}
	if p := res.portOf("svc-rdp"); p != 19401 {
		t.Fatalf("stored port %d, want 19401", p)
	}
	var serviceIDs []string
	for _, v := range r.srv.ListUserTunnels("u1") {
		serviceIDs = append(serviceIDs, v.ServiceID)
	}
	if !sameStrings(serviceIDs, []string{"svc-first", "svc-rdp"}) {
		t.Fatalf("tunnel service ids %v", serviceIDs)
	}
	if tn, ok := r.srv.LookupTunnelByServiceID("svc-rdp"); !ok || tn.RemotePort != 19401 {
		t.Fatalf("LookupTunnelByServiceID: %v %v", tn, ok)
	}
	if _, ok := r.srv.LookupHTTPTunnelByServiceID("svc-rdp"); ok {
		t.Fatal("a tcp tunnel was found as an http tunnel")
	}
	if got := stamps.seen(); !sameStrings(got, []string{"svc-first", "svc-rdp"}) {
		t.Fatalf("stamped %v", got)
	}
	c.close()
	waitTunnelsGone(t, r)
	if got := stamps.seen(); len(got) != 4 {
		t.Fatalf("stamps after the session ended: %v", got)
	}

	// 19400 is free now, and rdp still gets 19401.
	c = r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389"}); !rr.OK || rr.RemotePort != 19401 {
		t.Fatalf("rdp after a reconnect: %+v", rr)
	}
	// A new service does not get first's port either, while first is away.
	if rr := c.register(t, proto.TunnelRegister{Name: "other", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19402 {
		t.Fatalf("other: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Name: "first", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19400 {
		t.Fatalf("first after a reconnect: %+v", rr)
	}
}

// A port another service holds is never handed out: asked for, it is
// refused, and an automatic port skips it. When every free port is held, a
// new service gets none.
func TestRegisterTCP_PortsOfOtherServicesAreKept(t *testing.T) {
	res := newTCPResolver()
	res.ports[19410] = "svc-away"
	r := tcpRelay(t, 19410, 19411, res, nil)
	c := r.client(nil)
	defer c.close()
	rr := c.register(t, proto.TunnelRegister{Name: "pg", Type: "tcp", RemotePort: 19410, LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodePortUnavailable {
		t.Fatalf("a held port: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Name: "pg", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19411 {
		t.Fatalf("automatic port: %+v", rr)
	}
	rr = c.register(t, proto.TunnelRegister{Name: "ssh", Type: "tcp", LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodePortUnavailable {
		t.Fatalf("no unheld port left: %+v", rr)
	}
	if p := res.portOf("svc-away"); p != 19410 {
		t.Fatalf("the away service lost its port: %d", p)
	}
}

// A port asked for moves the service to it. A stored port outside the range
// (the operator changed it) is replaced by a free one.
func TestRegisterTCP_ChosenPortAndPortOutOfRange(t *testing.T) {
	res := newTCPResolver()
	res.ids["rdp"] = "svc-rdp"
	res.ports[18000] = "svc-rdp"
	r := tcpRelay(t, 19420, 19422, res, nil)
	c := r.client(nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19420 {
		t.Fatalf("out of range: %+v", rr)
	}
	if p := res.portOf("svc-rdp"); p != 19420 {
		t.Fatalf("stored port %d, want 19420", p)
	}
	c.close()
	waitTunnelsGone(t, r)
	c = r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", RemotePort: 19422, LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19422 {
		t.Fatalf("chosen port: %+v", rr)
	}
	if p := res.portOf("svc-rdp"); p != 19422 {
		t.Fatalf("stored port %d, want 19422", p)
	}
}

// While the service's port is taken by its own tunnel of another session (a
// second client, or the last session before it is reaped), a registration
// that asks for no port is refused and tried again: the port does not hop.
func TestRegisterTCP_OwnPortStillTaken(t *testing.T) {
	res := newTCPResolver()
	r := tcpRelay(t, 19430, 19432, res, nil)
	a := r.client(nil)
	defer a.close()
	if rr := a.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19430 {
		t.Fatalf("first client: %+v", rr)
	}
	b := r.client(nil)
	defer b.close()
	rr := b.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodePortUnavailable {
		t.Fatalf("second client: %+v", rr)
	}
	if p := res.portOf("svc-rdp"); p != 19430 {
		t.Fatalf("stored port %d, want 19430", p)
	}
}

// A name that belongs to a service of another type is refused with
// name_in_use, and nothing listens.
func TestRegisterTCP_NameInUse(t *testing.T) {
	res := newTCPResolver()
	res.httpName["web"] = true
	r := tcpRelay(t, 19440, 19440, res, nil)
	c := r.client(nil)
	defer c.close()
	rr := c.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodeNameInUse || rr.Error != "the name web is used by an http service" {
		t.Fatalf("answer: %+v", rr)
	}
	if n := len(r.srv.ListUserTunnels("u1")); n != 0 {
		t.Fatalf("%d tunnels", n)
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:19440", 500*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("the port listens")
	}
	r.waitFor("the refusal is told", func() bool { return r.count(observe.TunnelRefused+":tcp:"+proto.CodeNameInUse) == 1 })
}

// Without the port reservations a registration cannot know which ports are
// held: it is refused, and tried again.
func TestRegisterTCP_ReservationsUnreadable(t *testing.T) {
	res := newTCPResolver()
	res.listErr = errors.New("db down")
	r := tcpRelay(t, 19445, 19445, res, nil)
	c := r.client(nil)
	defer c.close()
	rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodeInternal {
		t.Fatalf("answer: %+v", rr)
	}
}

// echoHold answers five bytes with ten and holds the stream open until the
// visitor ends it.
func echoHold(st *yamux.Stream) { echo5(st) }

// The bytes of a tcp visitor are on the tunnel while the connection is open;
// when it ends, one connection-log row names the service, the tunnel, the
// session, the source and the bytes, and the traffic event names the service.
func TestTCPVisitor_LiveBytesAndConnectionLog(t *testing.T) {
	res := newTCPResolver()
	cl := &recConnLog{}
	r := tcpRelay(t, 19450, 19450, res, func(o *Options) { o.ConnLog = cl })
	c := r.client(echoHold)
	defer c.close()
	rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"})
	if !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	vc, err := net.DialTimeout("tcp", "127.0.0.1:19450", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = vc.Write([]byte("HELLO"))
	_ = vc.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp := make([]byte, 10)
	if _, err := io.ReadFull(vc, resp); err != nil || string(resp) != "echo:HELLO" {
		t.Fatalf("visitor got %q %v", resp, err)
	}
	// Still open.
	tn, ok := r.srv.LookupTunnelByServiceID("svc-rdp")
	if !ok {
		t.Fatal("no tunnel")
	}
	r.waitFor("live bytes", func() bool { return tn.BytesIn.Load() == 5 && tn.BytesOut.Load() == 10 })
	if n := len(cl.all()); n != 0 {
		t.Fatalf("%d rows before the connection ended", n)
	}
	time.Sleep(50 * time.Millisecond)
	_ = vc.Close()

	r.waitFor("the connection is logged", func() bool { return len(cl.all()) >= 1 })
	settle()
	rows := cl.all()
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1: %+v", len(rows), rows)
	}
	e := rows[0]
	if e.Kind != observe.TrafficTCP || e.ServiceID != "svc-rdp" || e.TunnelID != rr.TunnelID || e.UserID != "u1" ||
		e.ClientSessionID == "" || e.SourceIP != "127.0.0.1" || e.BytesIn != 5 || e.BytesOut != 10 ||
		e.Status != observe.OutcomeClean || e.Reason != "" {
		t.Fatalf("row %+v", e)
	}
	if e.StartedAt.IsZero() || e.EndedAt.Before(e.StartedAt) || e.EndedAt.Sub(e.StartedAt) < 50*time.Millisecond {
		t.Fatalf("times %v → %v", e.StartedAt, e.EndedAt)
	}
	// Counted once: the totals are not added again at the end.
	if in, out := tn.BytesIn.Load(), tn.BytesOut.Load(); in != 5 || out != 10 {
		t.Fatalf("tunnel counters in=%d out=%d, want 5 and 10", in, out)
	}
	tr := r.traffic()
	if len(tr) != 1 || tr[0].ServiceID != "svc-rdp" {
		t.Fatalf("traffic %+v", tr)
	}
}

// A visitor whose client never opens the stream is logged as an error.
func TestTCPVisitor_StreamNeverOpenedIsLogged(t *testing.T) {
	res := newTCPResolver()
	cl := &recConnLog{}
	r := tcpRelay(t, 19455, 19455, res, func(o *Options) { o.ConnLog = cl })
	c := r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	vc, err := net.DialTimeout("tcp", "127.0.0.1:19455", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer vc.Close()
	r.waitFor("the failed connection is logged", func() bool { return len(cl.all()) >= 1 })
	rows := cl.all()
	if len(rows) != 1 || rows[0].Status != observe.OutcomeError || rows[0].Reason != ConnLogReasonNoStream ||
		rows[0].ServiceID != "svc-rdp" || rows[0].BytesIn != 0 || rows[0].BytesOut != 0 {
		t.Fatalf("rows %+v", rows)
	}
}

// CloseByService ends the session that serves a tcp service.
func TestCloseByService_TCP(t *testing.T) {
	res := newTCPResolver()
	r := tcpRelay(t, 19460, 19460, res, nil)
	c := r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if n := r.srv.CloseByService("svc-rdp"); n != 1 {
		t.Fatalf("closed %d sessions", n)
	}
	waitTunnelsGone(t, r)
}

// Tunnels lists the live tunnels of every type.
func TestTunnels_AllTypes(t *testing.T) {
	res := newTCPResolver()
	r := tcpRelay(t, 19465, 19465, res, nil)
	c := r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	var got []string
	for _, tn := range r.srv.Tunnels() {
		got = append(got, tn.Name+":"+tn.ServiceID)
	}
	if !reflect.DeepEqual(got, []string{"rdp:svc-rdp"}) {
		t.Fatalf("Tunnels() = %v", got)
	}
}
