package server

// Access modes of tcp services (native tcp, P02): private opens no port, ip
// lets only allowed addresses reach the home client, and every public port
// has limits that are applied before the home client is told of a visitor.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// fakeTCPAccess is a TCPAccessLoader over a map; the mode of a service it
// does not name is open.
type fakeTCPAccess struct {
	mu    sync.Mutex
	acc   map[string]TCPAccess
	err   error
	calls int
}

func newFakeTCPAccess() *fakeTCPAccess { return &fakeTCPAccess{acc: map[string]TCPAccess{}} }

func (f *fakeTCPAccess) TCPAccess(_ context.Context, id string) (TCPAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return TCPAccess{}, f.err
	}
	a, ok := f.acc[id]
	if !ok {
		return TCPAccess{Mode: TCPAccessOpen}, nil
	}
	return a, nil
}

func (f *fakeTCPAccess) set(id string, a TCPAccess) {
	f.mu.Lock()
	f.acc[id] = a
	f.mu.Unlock()
}

func (f *fakeTCPAccess) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// accessRelay is a tcp relay with a resolver and an access loader, a conn
// log, and a client that echoes every visitor and counts the new_connection
// notices it is sent.
type accessRelay struct {
	*observedRelay
	res    *tcpResolver
	access *fakeTCPAccess
	log    *recConnLog
	c      *handClient
	told   *atomic.Int32
}

func newAccessRelay(t *testing.T, min, max int, mut func(*Options)) *accessRelay {
	t.Helper()
	res, access, log := newTCPResolver(), newFakeTCPAccess(), &recConnLog{}
	r := tcpRelay(t, min, max, res, func(o *Options) {
		o.TCPAccess = access
		o.ConnLog = log
		if mut != nil {
			mut(o)
		}
	})
	told := &atomic.Int32{}
	c := r.client(func(st *yamux.Stream) {
		told.Add(1)
		_, _ = io.Copy(st, st) // echo until the visitor goes
	})
	return &accessRelay{observedRelay: r, res: res, access: access, log: log, c: c, told: told}
}

// visit connects to the port, sends "hello" and reports what came back
// within d (nil, and the read error, when the connection was closed).
func visit(t *testing.T, port int, d time.Duration) ([]byte, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(d))
	_, _ = conn.Write([]byte("hello"))
	buf := make([]byte, 5)
	n, err := io.ReadFull(conn, buf)
	return buf[:n], err
}

// refusedRows are the rejected connection-log rows with reason.
func (a *accessRelay) refusedRows(reason string) []ConnLogEntry {
	var out []ConnLogEntry
	for _, e := range a.log.all() {
		if e.Status == observe.OutcomeRejected && e.Reason == reason {
			out = append(out, e)
		}
	}
	return out
}

func (a *accessRelay) rejectedTraffic(reason string) int {
	n := 0
	for _, tr := range a.traffic() {
		if tr.Outcome == observe.OutcomeRejected && tr.RejectReason == reason {
			n++
		}
	}
	return n
}

// A private tcp service gets no listener and no port: the answer says mode
// private with RemotePort 0, nothing listens on the range, and the
// service's automatic port goes back to the pool.
func TestTCPAccess_PrivateOpensNoPort(t *testing.T) {
	a := newAccessRelay(t, 19500, 19500, nil)
	// The service held a port as an open service before.
	a.res.mu.Lock()
	a.res.holds[19500] = TCPPortHold{ServiceID: "svc-rdp", UserID: "u1"}
	a.res.mu.Unlock()
	rr := a.c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"})
	if !rr.OK || rr.RemotePort != 0 || rr.AccessMode != "private" || !rr.Created || len(rr.Ignored) != 0 {
		t.Fatalf("answer: %+v", rr)
	}
	tn, ok := a.srv.LookupTunnelByServiceID("svc-rdp")
	if !ok || tn.Listener != nil || tn.RemotePort != 0 {
		t.Fatalf("tunnel: %+v %v", tn, ok)
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:19500", time.Second); err == nil {
		t.Fatal("something listens on the port of a private service")
	}
	if p := a.res.portOf("svc-rdp"); p != 0 {
		t.Fatalf("the private service still holds port %d", p)
	}
	// The one port of the range is free for another service.
	if rr := a.c.register(t, proto.TunnelRegister{Name: "ssh", Type: "tcp", LocalAddr: "127.0.0.1:22"}); !rr.OK || rr.RemotePort != 19500 {
		t.Fatalf("ssh: %+v", rr)
	}
}

// A client never gets a public port it did not ask for: a mode the service
// does not have, a mode on a tunnel without a name, a mode on a relay without
// tcp services and a mode that is no tcp mode are refused, and nothing is
// opened.
func TestTCPAccess_NoPortThatWasNotAskedFor(t *testing.T) {
	a := newAccessRelay(t, 19501, 19501, nil)
	a.res.setMode("legacy", "open")
	cases := []struct {
		name string
		tr   proto.TunnelRegister
		want string
	}{
		{"mode differs", proto.TunnelRegister{Name: "legacy", Type: "tcp", LocalAddr: "x", Access: "private"}, "is in access mode open, not private"},
		{"no name", proto.TunnelRegister{Type: "tcp", LocalAddr: "x", Access: "private"}, "needs a name"},
		{"http mode", proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "api_key"}, "open, ip, private"},
	}
	for _, c := range cases {
		rr := a.c.register(t, c.tr)
		if rr.OK || rr.Code != proto.CodeAccessInvalid || !strings.Contains(rr.Error, c.want) {
			t.Errorf("%s: %+v", c.name, rr)
		}
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:19501", time.Second); err == nil {
		t.Fatal("a refused registration opened the port")
	}
	if n := len(a.srv.Tunnels()); n != 0 {
		t.Fatalf("%d tunnels", n)
	}
	// Asking for the mode the service has is fine.
	if rr := a.c.register(t, proto.TunnelRegister{Name: "legacy", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK || rr.AccessMode != "open" || rr.RemotePort != 19501 {
		t.Fatalf("open: %+v", rr)
	}

	// A relay without tcp services cannot apply a mode.
	old := newObservedRelay(t, 19502, nil)
	c := old.client(nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x", Access: "private"}); rr.OK || rr.Code != proto.CodeAccessInvalid {
		t.Fatalf("relay without tcp services: %+v", rr)
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:19502", time.Second); err == nil {
		t.Fatal("the port was opened")
	}
	// Without a wish it is the tunnel it has always been.
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19502 || rr.AccessMode != "" {
		t.Fatalf("relay without tcp services, no wish: %+v", rr)
	}
}

// In mode ip a visitor whose address the policy does not allow is closed
// without a byte and the home client is never told; an allowed one reaches
// it. Grants are read again when the relay is told of a change, and an
// expired grant stops new visitors while a connected one stays.
func TestTCPAccess_IPModeAllowsAndDenies(t *testing.T) {
	a := newAccessRelay(t, 19503, 19503, nil)
	id := a.res.setMode("rdp", "ip")
	a.access.set(id, TCPAccess{Mode: TCPAccessIP, AllowCIDRs: []string{"10.0.0.0/8"}})
	rr := a.c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"})
	if !rr.OK || rr.AccessMode != "ip" || rr.RemotePort != 19503 {
		t.Fatalf("answer: %+v", rr)
	}

	got, err := visit(t, 19503, 2*time.Second)
	if len(got) != 0 || err == nil {
		t.Fatalf("a denied visitor got %q, %v", got, err)
	}
	settle()
	if n := a.told.Load(); n != 0 {
		t.Fatalf("the home client was told of %d denied visitors", n)
	}
	rows := a.refusedRows(observe.RejectedIPGeo)
	if len(rows) != 1 || rows[0].SourceIP != "127.0.0.1" || rows[0].ServiceID != id {
		t.Fatalf("rejected rows %+v", rows)
	}
	if a.rejectedTraffic(observe.RejectedIPGeo) != 1 {
		t.Fatalf("observer: %+v", a.traffic())
	}

	// A grant for this address, then the relay is told.
	a.access.set(id, TCPAccess{Mode: TCPAccessIP, AllowCIDRs: []string{"10.0.0.0/8"},
		Grants: []TCPGrant{{CIDR: "127.0.0.1", ExpiresAt: time.Now().Add(700 * time.Millisecond)}}})
	if err := a.srv.RefreshTCPService(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Connected while the grant holds; it stays connected after it expires.
	conn, err := net.Dial("tcp", "127.0.0.1:19503")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	echo := func(msg string) error {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte(msg)); err != nil {
			return err
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return err
		}
		if string(buf) != msg {
			return fmt.Errorf("got %q", buf)
		}
		return nil
	}
	if err := echo("hello"); err != nil {
		t.Fatalf("granted visitor: %v", err)
	}
	time.Sleep(900 * time.Millisecond)
	if got, err := visit(t, 19503, 2*time.Second); len(got) != 0 || err == nil {
		t.Fatalf("a visitor after the grant expired got %q, %v", got, err)
	}
	if err := echo("still"); err != nil {
		t.Fatalf("the connected visitor was cut when its grant expired: %v", err)
	}
	if a.access.calls != 2 {
		t.Fatalf("the policy was read %d times, want 2 (no read per visitor)", a.access.calls)
	}
}

// The policy: block entries win, an empty allow list lets nobody in, IPv4
// written as IPv6 is IPv4, IPv6 ranges hold IPv6 visitors only.
func TestTCPPolicy_Allows(t *testing.T) {
	now := time.Now()
	p, err := compileTCPPolicy(TCPAccess{
		AllowCIDRs: []string{"192.0.2.0/24", "2001:db8::/32", "::ffff:198.51.100.7"},
		BlockCIDRs: []string{"192.0.2.66"},
		Grants: []TCPGrant{
			{CIDR: "203.0.113.9", ExpiresAt: now.Add(time.Hour)},
			{CIDR: "203.0.113.10", ExpiresAt: now.Add(-time.Second)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"192.0.2.1": true, "::ffff:192.0.2.1": true, "192.0.2.66": false, "192.0.3.1": false,
		"2001:db8::1": true, "2001:db9::1": false, "198.51.100.7": true,
		"203.0.113.9": true, "203.0.113.10": false, "0.0.0.0": false,
	} {
		if got := p.allows(netip.MustParseAddr(ip), now); got != want {
			t.Errorf("%s: %v, want %v", ip, got, want)
		}
	}
	empty, _ := compileTCPPolicy(TCPAccess{})
	if empty.allows(netip.MustParseAddr("192.0.2.1"), now) {
		t.Error("an empty policy let a visitor in")
	}
	var none *tcpPolicy
	if none.allows(netip.MustParseAddr("192.0.2.1"), now) {
		t.Error("no policy let a visitor in")
	}
	if _, err := compileTCPPolicy(TCPAccess{BlockCIDRs: []string{"nonsense"}}); err == nil {
		t.Error("a block entry that cannot be read was left out")
	}
	for in, want := range map[string]string{"10.1.2.3/8": "10.0.0.0/8", "2001:db8::1": "2001:db8::1/128", "::ffff:10.0.0.0/104": "10.0.0.0/8"} {
		if p, err := ParseTCPPrefix(in); err != nil || p.String() != want {
			t.Errorf("ParseTCPPrefix(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	if k := sourceKey(netip.MustParseAddr("2001:db8:1:2:3:4:5:6")); k != "2001:db8:1:2::/64" {
		t.Errorf("source of an IPv6 visitor: %s", k)
	}
	if k := sourceKey(netip.MustParseAddr("::ffff:192.0.2.1")); k != "192.0.2.1" {
		t.Errorf("source of a mapped IPv4 visitor: %s", k)
	}
}

// A new mode on a live tunnel closes its session, so that the client
// registers again (D14); so does an access the relay cannot read.
func TestTCPAccess_ChangesCloseOrApply(t *testing.T) {
	a := newAccessRelay(t, 19504, 19504, nil)
	id := a.res.setMode("rdp", "ip")
	a.access.set(id, TCPAccess{Mode: TCPAccessIP, AllowCIDRs: []string{"127.0.0.1"}})
	if rr := a.c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("answer: %+v", rr)
	}
	// A new cap applies, the session stays.
	a.access.set(id, TCPAccess{Mode: TCPAccessIP, AllowCIDRs: []string{"127.0.0.1"}, MaxConnections: 3})
	if err := a.srv.RefreshTCPService(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	tn, _ := a.srv.LookupTunnelByServiceID(id)
	if tn == nil || tn.limits.max != 3 {
		t.Fatalf("cap not applied: %+v", tn)
	}
	a.access.set(id, TCPAccess{Mode: TCPAccessOpen})
	if err := a.srv.RefreshTCPService(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	waitTunnelsGone(t, a.observedRelay)

	b := newAccessRelay(t, 19505, 19505, nil)
	if rr := b.c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("answer: %+v", rr)
	}
	b.access.setErr(errors.New("database is gone"))
	if err := b.srv.RefreshTCPService(context.Background(), "svc-rdp"); err == nil {
		t.Fatal("no error")
	}
	waitTunnelsGone(t, b.observedRelay)
}

// A change the relay is told of while a registration reads the service is
// not lost: the registration reads it again once its tunnel is there.
func TestTCPAccess_ChangeDuringRegistration(t *testing.T) {
	a := newAccessRelay(t, 19506, 19506, nil)
	var once sync.Once
	a.res.onResolve = func() {
		once.Do(func() {
			a.access.set("svc-rdp", TCPAccess{Mode: TCPAccessPrivate})
			_ = a.srv.RefreshTCPService(context.Background(), "svc-rdp") // no tunnel yet
		})
	}
	if rr := a.c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("answer: %+v", rr)
	}
	waitTunnelsGone(t, a.observedRelay)
}

// In mode ip a relay that cannot read policies opens nothing.
func TestTCPAccess_IPModeWithoutLoaderIsRefused(t *testing.T) {
	a := newAccessRelay(t, 19507, 19507, func(o *Options) { o.TCPAccess = nil })
	a.res.setMode("rdp", "ip")
	if rr := a.c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); rr.OK || rr.Code != proto.CodeAccessInvalid {
		t.Fatalf("answer: %+v", rr)
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:19507", time.Second); err == nil {
		t.Fatal("the port was opened")
	}
}

// Over the cap of the service a visitor is closed at once, and the home
// client is not told of it.
func TestTCPAccess_TunnelCapRefusesWithoutTellingTheClient(t *testing.T) {
	a := newAccessRelay(t, 19508, 19508, nil)
	a.res.maxConns["svc-rdp"] = 2
	if rr := a.c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("answer: %+v", rr)
	}
	var held []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", "127.0.0.1:19508")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte("hi")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
			t.Fatalf("visitor %d: %v", i, err)
		}
		held = append(held, c)
	}
	if got, err := visit(t, 19508, 2*time.Second); len(got) != 0 || err == nil {
		t.Fatalf("the third visitor got %q, %v", got, err)
	}
	settle()
	if n := a.told.Load(); n != 2 {
		t.Fatalf("the home client was told of %d visitors, want 2", n)
	}
	if len(a.refusedRows(TCPRefusedTunnelFull)) != 1 {
		t.Fatalf("rows %+v", a.log.all())
	}
	_ = held[0].Close()
	a.waitFor("a place is free", func() bool {
		tn, _ := a.srv.LookupTunnelByServiceID("svc-rdp")
		n, _ := tn.limits.counts()
		return n == 1
	})
	if got, err := visit(t, 19508, 2*time.Second); string(got) != "hello" {
		t.Fatalf("a visitor after a place came free got %q, %v", got, err)
	}
}

// The limits per source and per tunnel, on a clock of the test's own.
func TestTCPLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTCPLimits(0)
	l.now = func() time.Time { return now }
	if l.max != DefaultTCPMaxConnections || newTCPLimits(100000).max != MaxTCPMaxConnections {
		t.Fatalf("caps: %d %d", l.max, newTCPLimits(100000).max)
	}
	// Four at once from one source; the fifth waits for one to end.
	for i := 0; i < tcpSourceConcurrent; i++ {
		if r := l.admit("a"); r != "" {
			t.Fatalf("visitor %d: %s", i, r)
		}
		l.paired()
	}
	if r := l.admit("a"); r != TCPRefusedSourceLimit {
		t.Fatalf("fifth concurrent: %q", r)
	}
	l.done("a")
	if r := l.admit("a"); r != "" { // the fifth token of the burst
		t.Fatalf("after one ended: %q", r)
	}
	l.paired()
	for i := 0; i < tcpSourceConcurrent; i++ {
		l.done("a")
	}
	// The burst is used up: one more per six seconds.
	if r := l.admit("a"); r != TCPRefusedRateLimited {
		t.Fatalf("beyond the burst: %q", r)
	}
	now = now.Add(time.Minute / tcpSourcePerMinute)
	if r := l.admit("a"); r != "" {
		t.Fatalf("after the refill: %q", r)
	}
	l.paired()
	l.done("a")
	// Another source is not held back by the first.
	if r := l.admit("b"); r != "" {
		t.Fatalf("source b: %q", r)
	}
	l.done("b")

	// Pending visitors and the cap of the tunnel.
	p := newTCPLimits(3)
	p.now = l.now
	for i := 0; i < 3; i++ {
		if r := p.admit(fmt.Sprintf("s%d", i)); r != "" {
			t.Fatal(r)
		}
	}
	if r := p.admit("s9"); r != TCPRefusedTunnelFull {
		t.Fatalf("over the cap: %q", r)
	}
	q := newTCPLimits(MaxTCPMaxConnections)
	q.now = l.now
	for i := 0; i < tcpMaxPending; i++ {
		if r := q.admit(fmt.Sprintf("s%d", i)); r != "" {
			t.Fatal(r)
		}
	}
	if r := q.admit("s99"); r != TCPRefusedPendingFull {
		t.Fatalf("over the pending cap: %q", r)
	}
	q.paired()
	if r := q.admit("s99"); r != "" {
		t.Fatalf("after one was paired: %q", r)
	}

	// The sources it remembers are bounded: idle ones are forgotten, and
	// a new source is refused while none can be.
	m := newTCPLimits(MaxTCPMaxConnections)
	m.now = l.now
	for i := 0; i < tcpSourcesTracked; i++ {
		m.sources[fmt.Sprintf("x%d", i)] = &tcpSource{tokens: 0, last: now, active: 1}
	}
	if r := m.admit("new"); r != TCPRefusedRateLimited {
		t.Fatalf("a new source while the table is full: %q", r)
	}
	now = now.Add(time.Hour)
	for _, s := range m.sources {
		s.active = 0
	}
	if r := m.admit("new"); r != "" || len(m.sources) != 1 {
		t.Fatalf("after the idle sources could go: %q, %d sources", r, len(m.sources))
	}
}

// A refused visitor is written down once per service, reason and address a
// minute, however often it comes; the table of a minute is bounded.
func TestRefusalSampler(t *testing.T) {
	r := &refusalSampler{}
	now := time.Unix(1_700_000_000, 0)
	if !r.sample("s", "ip_geo", "192.0.2.1", now) {
		t.Fatal("first refusal not sampled")
	}
	for i := 0; i < 100; i++ {
		if r.sample("s", "ip_geo", "192.0.2.1", now.Add(time.Second)) {
			t.Fatal("a repeat was sampled")
		}
	}
	if !r.sample("s", "rate_limited", "192.0.2.1", now) || !r.sample("s", "ip_geo", "192.0.2.2", now) {
		t.Fatal("another reason or address was not sampled")
	}
	if !r.sample("s", "ip_geo", "192.0.2.1", now.Add(time.Minute)) {
		t.Fatal("the next minute was not sampled")
	}
	for i := 0; i < refusalKeysPerMinute+10; i++ {
		r.sample("s", "ip_geo", fmt.Sprintf("10.0.%d.%d", i/256, i%256), now.Add(time.Minute))
	}
	if len(r.seen) != refusalKeysPerMinute || r.dropped == 0 {
		t.Fatalf("table %d, dropped %d", len(r.seen), r.dropped)
	}
}

// Refusals are counted per source and the samples land in the connection
// log; the access.denied event is the observer's (cmd/server).
func TestTCPAccess_RepeatedRefusalsWriteOneRow(t *testing.T) {
	a := newAccessRelay(t, 19509, 19509, nil)
	id := a.res.setMode("rdp", "ip")
	a.access.set(id, TCPAccess{Mode: TCPAccessIP})
	if rr := a.c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("answer: %+v", rr)
	}
	for i := 0; i < 5; i++ {
		_, _ = visit(t, 19509, time.Second)
	}
	a.waitFor("five refusals observed", func() bool { return a.rejectedTraffic(observe.RejectedIPGeo) == 5 })
	if rows := a.refusedRows(observe.RejectedIPGeo); len(rows) != 1 {
		t.Fatalf("%d rejected rows, want one", len(rows))
	}
}

// The control port answers a source that keeps sending wrong tokens with
// rate_limited until its bucket refills, but a client with a valid token
// from that source (the same NAT, the same Docker gateway) still connects:
// the token is checked first. A failure of the store, not of the token, is
// no failed sign-in.
func TestControl_FailedSignInsAreLimited(t *testing.T) {
	r := newObservedRelay(t, 19510, nil)
	now := time.Unix(1_700_000_000, 0)
	// The clock is read under the limiter's lock, so it is changed under it.
	af := r.srv.authFails
	af.mu.Lock()
	af.now = func() time.Time { return now }
	af.mu.Unlock()
	for i := 0; i < authFailBurst; i++ {
		conn, ar := r.dial("", "")
		_ = conn.Close()
		if ar.OK || ar.Code != proto.CodeInvalidToken {
			t.Fatalf("attempt %d: %+v", i, ar)
		}
	}
	// The attacker keeps the source limited.
	for i := 0; i < 3; i++ {
		conn, ar := r.dial("", "")
		_ = conn.Close()
		if ar.OK || ar.Code != proto.CodeRateLimited {
			t.Fatalf("a bad token from a limited source: %+v", ar)
		}
	}
	conn, ar := r.dial("secret", "")
	_ = conn.Close()
	if !ar.OK {
		t.Fatalf("a good token from a limited source: %+v", ar)
	}
	if r.srv.authFails.limited("192.0.2.1") {
		t.Fatal("another source is limited")
	}
	af.mu.Lock()
	now = now.Add(authFailRefill)
	af.mu.Unlock()
	conn, ar = r.dial("", "")
	_ = conn.Close()
	if ar.Code != proto.CodeInvalidToken {
		t.Fatalf("after the refill a bad token is told it is invalid: %+v", ar)
	}
}

// A store that fails (not the token) is not counted against the source.
func TestControl_StoreFailuresAreNoFailedSignIns(t *testing.T) {
	r := newObservedRelay(t, 19513, func(o *Options) { o.Auth = fakeAuth{err: errors.New("database is locked")} })
	for i := 0; i < 2*authFailBurst; i++ {
		conn, _ := r.dial("secret", "")
		_ = conn.Close()
	}
	if r.srv.authFails.limited("127.0.0.1") {
		t.Fatal("store failures limited the source")
	}
}

// The relay announces tcp_access when it keeps tcp services, and nothing
// otherwise; forward comes with the forward streams.
func TestControl_RelayCapabilities(t *testing.T) {
	a := newAccessRelay(t, 19511, 19511, nil)
	conn, ar := a.dial("secret", "")
	_ = conn.Close()
	if !ar.OK || !slices.Equal(ar.Capabilities, []string{proto.CapTCPAccess}) {
		t.Fatalf("relay with tcp services: %+v", ar)
	}
	old := newObservedRelay(t, 19512, nil)
	conn, ar = old.dial("secret", "")
	_ = conn.Close()
	if !ar.OK || ar.Capabilities != nil {
		t.Fatalf("relay without tcp services: %+v", ar)
	}
	refused, ar := old.dial("", "")
	_ = refused.Close()
	if ar.OK || ar.Capabilities != nil {
		t.Fatalf("a refused client learns the capabilities: %+v", ar)
	}
}

// An unnamed tunnel that asks for open gets open: nothing is reported as
// ignored (a slug still is). A relay without tcp services still reports the
// access (TestRegisterTCP_SlugAndAccessAreReportedAsIgnored).
func TestTCPAccess_UnnamedOpenIsNotIgnored(t *testing.T) {
	a := newAccessRelay(t, 19514, 19515, nil)
	if rr := a.c.register(t, proto.TunnelRegister{Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK || len(rr.Ignored) != 0 {
		t.Fatalf("unnamed open: %+v", rr)
	}
	if rr := a.c.register(t, proto.TunnelRegister{Type: "tcp", LocalAddr: "x", Access: "open", Slug: "nope"}); !rr.OK || !slices.Equal(rr.Ignored, []string{"slug"}) {
		t.Fatalf("unnamed open with a slug: %+v", rr)
	}
}
