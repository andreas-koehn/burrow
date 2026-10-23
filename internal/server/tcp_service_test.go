package server

// A tcp tunnel is a service (native tcp, P01): its registration resolves a
// service row, keeps the service's public port across reconnects, and every
// visitor is counted live and written to the connection log.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// tcpResolver is a TCPServiceResolver over an in-memory table: one service
// per name, the holds on ports, names that belong to an http service, and
// when each service was last served (it is also the ServiceRecorder).
type tcpResolver struct {
	mu       sync.Mutex
	ids      map[string]string // name → service id
	holds    map[int]TCPPortHold
	seen     map[string]time.Time
	httpName map[string]bool
	limit    int // TCPPortLimit of every resolution
	names    []string
	listErr  error
	resErr   error
	// modes is the access mode of each service id that has another than
	// open; maxConns the cap of each. onResolve runs inside ResolveTCP.
	modes     map[string]string
	maxConns  map[string]int
	onResolve func()
}

func newTCPResolver() *tcpResolver {
	return &tcpResolver{ids: map[string]string{}, holds: map[int]TCPPortHold{}, seen: map[string]time.Time{}, httpName: map[string]bool{},
		modes: map[string]string{}, maxConns: map[string]int{}}
}

func (r *tcpResolver) Resolve(context.Context, string, string, string) (string, string, error) {
	return "", "", errors.New("Resolve must not be used for tcp")
}

func (r *tcpResolver) ResolveTCP(_ context.Context, userID, name string, o ResolveOptions) (Resolved, error) {
	if r.onResolve != nil {
		r.onResolve()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
	if r.resErr != nil {
		return Resolved{}, r.resErr
	}
	if r.httpName[name] {
		return Resolved{}, &RefusalError{Code: proto.CodeNameInUse, Message: "the name " + name + " is used by an http service"}
	}
	id, ok := r.ids[name]
	if !ok {
		id = "svc-" + name
		r.ids[name] = id
		want := o.Access
		if want == "" {
			want = o.AccessDefault
		}
		if want != "" && want != "open" {
			r.modes[id] = want
		}
	}
	_, served := r.seen[id]
	mode := "open"
	if m, ok := r.modes[id]; ok {
		mode = m
	}
	res := Resolved{ServiceID: id, Slug: "s-" + name, AccessMode: mode, Created: !served, TCPPortLimit: r.limit, MaxConnections: r.maxConns[id]}
	for p, h := range r.holds {
		if h.ServiceID == id {
			res.TCPPort, res.TCPPortPinned = p, h.Pinned
		}
	}
	return res, nil
}

func (r *tcpResolver) TCPPortReservations(context.Context) (map[int]TCPPortHold, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := map[int]TCPPortHold{}
	for p, h := range r.holds {
		if h.UserID == "" {
			h.UserID = "u1"
		}
		h.LastSeen = r.seen[h.ServiceID]
		out[p] = h
	}
	return out, nil
}

func (r *tcpResolver) SetServiceTCPPort(_ context.Context, id string, port int, pinned bool, takeFrom string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.holds[port]; ok && h.ServiceID != id {
		if h.ServiceID != takeFrom || h.Pinned {
			return ErrTCPPortTaken
		}
		delete(r.holds, port)
	}
	for p, h := range r.holds {
		if h.ServiceID == id {
			delete(r.holds, p)
		}
	}
	if port != 0 {
		r.holds[port] = TCPPortHold{ServiceID: id, UserID: "u1", Pinned: pinned}
	}
	return nil
}

func (r *tcpResolver) TouchServiceLastSeen(_ context.Context, id, _ string) error {
	r.mu.Lock()
	r.seen[id] = time.Now()
	r.mu.Unlock()
	return nil
}

func (r *tcpResolver) hold(id string) (port int, pinned bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, h := range r.holds {
		if h.ServiceID == id {
			return p, h.Pinned
		}
	}
	return 0, false
}

func (r *tcpResolver) portOf(id string) int { p, _ := r.hold(id); return p }

// setMode gives the service of name (created when it is not there) a mode.
func (r *tcpResolver) setMode(name, mode string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.ids[name]
	if !ok {
		id = "svc-" + name
		r.ids[name] = id
	}
	r.modes[id] = mode
	return id
}

func (r *tcpResolver) resolved() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

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
	// The answer names the service's mode (P02); a tcp service has no URL.
	if rr.AccessMode != "open" || !rr.Created || rr.URL != "" || len(rr.Ignored) != 0 {
		t.Fatalf("answer: %+v", rr)
	}
	if p := res.portOf("svc-rdp"); p != 19401 {
		t.Fatalf("stored port %d, want 19401", p)
	}
	var serviceIDs []string
	for _, v := range r.srv.ListUserTunnels("u1") {
		serviceIDs = append(serviceIDs, v.ServiceID)
	}
	slices.Sort(serviceIDs) // the tunnels of a session come in no order
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

// A pinned port of another service is never handed out: asked for, it is
// "not available", and an automatic port skips it, also when the pool is
// full. An automatic hold of an offline service is taken over by a request
// for its port, and reclaimed when no port is left, the least recently seen
// first.
func TestRegisterTCP_PinnedHoldsSurviveAutomaticOnesAreReclaimed(t *testing.T) {
	res := newTCPResolver()
	now := time.Now()
	res.holds[19410] = TCPPortHold{ServiceID: "svc-pin", Pinned: true}
	res.holds[19411] = TCPPortHold{ServiceID: "svc-newer"}
	res.holds[19412] = TCPPortHold{ServiceID: "svc-older"}
	res.seen["svc-pin"] = now.Add(-3 * time.Hour)
	res.seen["svc-newer"] = now.Add(-time.Hour)
	res.seen["svc-older"] = now.Add(-2 * time.Hour)
	r := tcpRelay(t, 19410, 19412, res, nil)
	c := r.client(nil)
	defer c.close()
	rr := c.register(t, proto.TunnelRegister{Name: "pg", Type: "tcp", RemotePort: 19410, LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodePortUnavailable || rr.Error != "port 19410 is not available" {
		t.Fatalf("a pinned port: %+v", rr)
	}
	// The pool is full: the automatic hold seen longest ago goes.
	if rr := c.register(t, proto.TunnelRegister{Name: "pg", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19412 {
		t.Fatalf("reclaimed: %+v", rr)
	}
	if p, pinned := res.hold("svc-pg"); p != 19412 || pinned {
		t.Fatalf("pg holds %d pinned %v", p, pinned)
	}
	if p := res.portOf("svc-older"); p != 0 {
		t.Fatalf("the displaced service still holds %d", p)
	}
	if rr := c.register(t, proto.TunnelRegister{Name: "ssh", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19411 {
		t.Fatalf("second reclaim: %+v", rr)
	}
	rr = c.register(t, proto.TunnelRegister{Name: "vnc", Type: "tcp", LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodePortUnavailable {
		t.Fatalf("only a pinned hold is left: %+v", rr)
	}
	if p, pinned := res.hold("svc-pin"); p != 19410 || !pinned {
		t.Fatalf("the pinned hold: %d %v", p, pinned)
	}
}

// The restart race: an automatic service takes port N while the client that
// asks for N is away. While the automatic one is online N is not available;
// once it is offline the fixed client gets N, pinned, and the automatic one
// moves on its next registration.
func TestRegisterTCP_FixedRequestTakesOverAnOfflineAutomaticHold(t *testing.T) {
	res := newTCPResolver()
	r := tcpRelay(t, 19480, 19482, res, func(o *Options) { o.ServiceRecorder = res })
	auto := r.client(nil)
	if rr := auto.register(t, proto.TunnelRegister{Name: "auto", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19480 {
		t.Fatalf("auto: %+v", rr)
	}
	fixed := r.client(nil)
	rr := fixed.register(t, proto.TunnelRegister{Name: "fixed", Type: "tcp", RemotePort: 19480, LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodePortUnavailable || rr.Error != "port 19480 is not available" {
		t.Fatalf("fixed while auto is online: %+v", rr)
	}
	auto.close()
	r.waitFor("auto leaves", func() bool { _, ok := r.srv.LookupTunnelByServiceID("svc-auto"); return !ok })
	if rr := fixed.register(t, proto.TunnelRegister{Name: "fixed", Type: "tcp", RemotePort: 19480, LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19480 {
		t.Fatalf("fixed while auto is offline: %+v", rr)
	}
	if p, pinned := res.hold("svc-fixed"); p != 19480 || !pinned {
		t.Fatalf("fixed holds %d pinned %v", p, pinned)
	}
	auto = r.client(nil)
	defer auto.close()
	if rr := auto.register(t, proto.TunnelRegister{Name: "auto", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19481 {
		t.Fatalf("auto moved: %+v", rr)
	}
	fixed.close()
}

// A user holds at most TCPPortLimit ports: a service that holds none yet is
// refused beyond it; the services that hold one keep registering.
func TestRegisterTCP_PortsPerUserCap(t *testing.T) {
	res := newTCPResolver()
	res.limit = 2
	r := tcpRelay(t, 19485, 19489, res, nil)
	c := r.client(nil)
	for _, n := range []string{"a", "b"} {
		if rr := c.register(t, proto.TunnelRegister{Name: n, Type: "tcp", LocalAddr: "x"}); !rr.OK {
			t.Fatalf("%s: %+v", n, rr)
		}
	}
	for _, rp := range []int{0, 19489} {
		rr := c.register(t, proto.TunnelRegister{Name: "c", Type: "tcp", RemotePort: rp, LocalAddr: "x"})
		if rr.OK || rr.Code != proto.CodePortUnavailable || rr.Error != "port limit reached (2); delete an unused tcp service" {
			t.Fatalf("over the cap (remote %d): %+v", rp, rr)
		}
	}
	c.close()
	waitTunnelsGone(t, r)
	c = r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "b", Type: "tcp", RemotePort: 19488, LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19488 {
		t.Fatalf("a service that holds a port moves: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Name: "a", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("a service that holds a port: %+v", rr)
	}
}

// A port asked for moves the service to it. A stored port outside the range
// (the operator changed it) is replaced by a free one.
func TestRegisterTCP_ChosenPortAndPortOutOfRange(t *testing.T) {
	res := newTCPResolver()
	res.ids["rdp"] = "svc-rdp"
	res.holds[18000] = TCPPortHold{ServiceID: "svc-rdp"}
	r := tcpRelay(t, 19420, 19422, res, nil)
	c := r.client(nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19420 {
		t.Fatalf("out of range: %+v", rr)
	}
	if p, pinned := res.hold("svc-rdp"); p != 19420 || pinned {
		t.Fatalf("stored port %d pinned %v, want 19420 automatic", p, pinned)
	}
	c.close()
	waitTunnelsGone(t, r)
	c = r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", RemotePort: 19422, LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19422 {
		t.Fatalf("chosen port: %+v", rr)
	}
	if p, pinned := res.hold("svc-rdp"); p != 19422 || !pinned {
		t.Fatalf("stored port %d pinned %v, want 19422 pinned", p, pinned)
	}
}

// A tcp service is served by one client at a time: a second registration is
// refused with service_busy (tried again: it may be the last session of the
// same client, not reaped yet) and succeeds once the first is gone.
func TestRegisterTCP_SecondLiveTunnelIsRefused(t *testing.T) {
	res := newTCPResolver()
	r := tcpRelay(t, 19430, 19432, res, nil)
	a := r.client(nil)
	if rr := a.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19430 {
		t.Fatalf("first client: %+v", rr)
	}
	b := r.client(nil)
	defer b.close()
	rr := b.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", RemotePort: 19431, LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodeServiceBusy || rr.Error != `the service "rdp" is already served by another client` {
		t.Fatalf("second client: %+v", rr)
	}
	if p := res.portOf("svc-rdp"); p != 19430 {
		t.Fatalf("stored port %d, want 19430", p)
	}
	a.close()
	r.waitFor("the first leaves", func() bool { _, ok := r.srv.LookupTunnelByServiceID("svc-rdp"); return !ok })
	if rr := b.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19430 {
		t.Fatalf("second client after the first left: %+v", rr)
	}
	// Unregistering frees the service for the next registration.
	if err := proto.WriteMessage(b.ctrl, proto.MsgTunnelUnregister, proto.TunnelUnregister{TunnelID: r.srv.Tunnels()[0].ID}); err != nil {
		t.Fatal(err)
	}
	r.waitFor("unregistered", func() bool { return len(r.srv.Tunnels()) == 0 })
	if rr := b.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("after unregister: %+v", rr)
	}
}

// An old client without --name sends no name: such tunnels have no service,
// as before, and each gets its own port, freed when it goes.
func TestRegisterTCP_UnnamedTunnelsKeepTheOldPath(t *testing.T) {
	res := newTCPResolver()
	res.holds[19435] = TCPPortHold{ServiceID: "svc-pin", Pinned: true}
	r := tcpRelay(t, 19435, 19437, res, nil)
	c := r.client(nil)
	ports := map[int]bool{}
	for i := 0; i < 2; i++ {
		rr := c.register(t, proto.TunnelRegister{Type: "tcp", LocalAddr: "x"})
		if !rr.OK || rr.RemotePort == 19435 {
			t.Fatalf("unnamed %d: %+v", i, rr)
		}
		ports[rr.RemotePort] = true
	}
	if len(ports) != 2 {
		t.Fatalf("ports %v", ports)
	}
	for _, tn := range r.srv.Tunnels() {
		if tn.ServiceID != "" {
			t.Fatalf("an unnamed tunnel has service %q", tn.ServiceID)
		}
	}
	if got := res.resolved(); len(got) != 0 {
		t.Fatalf("resolved %v", got)
	}
	if len(res.holds) != 1 {
		t.Fatalf("holds %v", res.holds)
	}
	c.close()
	waitTunnelsGone(t, r)
	c = r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19436 {
		t.Fatalf("the ports were freed: %+v", rr)
	}
}

// Created is reported until a registration of the service succeeds: one that
// fails for want of a port does not use it up.
func TestRegisterTCP_CreatedUntilServed(t *testing.T) {
	res := newTCPResolver()
	res.holds[19438] = TCPPortHold{ServiceID: "svc-pin", Pinned: true}
	r := tcpRelay(t, 19438, 19438, res, func(o *Options) { o.ServiceRecorder = res })
	c := r.client(nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); rr.OK {
		t.Fatalf("no port: %+v", rr)
	}
	res.mu.Lock()
	delete(res.holds, 19438)
	res.mu.Unlock()
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	c.close()
	waitTunnelsGone(t, r)
	c = r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("again: %+v", rr)
	}
	var created []bool
	r.obs.mu.Lock()
	for _, e := range r.obs.sessions {
		if e.Kind == observe.TunnelOpened {
			created = append(created, e.ServiceCreated)
		}
	}
	r.obs.mu.Unlock()
	if !reflect.DeepEqual(created, []bool{true, false}) {
		t.Fatalf("ServiceCreated of the openings: %v", created)
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

// A failure of the store is refused with a text that names nothing of it,
// and tried again: without the holds a port of another service could be
// handed out.
func TestRegisterTCP_StoreFailuresSayNothingOfTheStore(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*tcpResolver)
	}{
		{"holds unreadable", func(r *tcpResolver) { r.listErr = errors.New("pq: connection refused at 10.0.0.5:5432") }},
		{"resolve failed", func(r *tcpResolver) { r.resErr = errors.New("resolve service: list: pq: connection refused") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := newTCPResolver()
			c.edit(res)
			r := tcpRelay(t, 19445, 19445, res, nil)
			cl := r.client(nil)
			defer cl.close()
			rr := cl.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"})
			if rr.OK || rr.Code != proto.CodeInternal || rr.Error != "the relay could not look up the service; try again later" {
				t.Fatalf("answer: %+v", rr)
			}
		})
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

// tokenAuth names a client token after the token itself: every token is
// valid, for user u1.
type tokenAuth struct{}

func (tokenAuth) Authenticate(context.Context, string) (string, error) { return "u1", nil }
func (tokenAuth) AuthenticateToken(_ context.Context, tok string) (string, string, string, error) {
	return "u1", "tok-" + tok, tok, nil
}

// reclaims are the TCPPortReclaimed events: "taker<from:port:reason".
func (r *observedRelay) reclaims() []string {
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	var out []string
	for _, e := range r.obs.sessions {
		if e.Kind == observe.TCPPortReclaimed {
			out = append(out, fmt.Sprintf("%s<%s:%d:%s", e.ServiceID, e.FromServiceID, e.Port, e.Reason))
		}
	}
	return out
}

// A second registration of a served service with the same client token is
// probably that client's own session the relay has not seen end: it is
// refused with service_busy and tried again. With another token it is
// another machine: name_in_use, which that client does not retry.
func TestRegisterTCP_ServedByAnotherTokenIsNameInUse(t *testing.T) {
	res := newTCPResolver()
	r := tcpRelay(t, 19500, 19502, res, func(o *Options) { o.Auth = tokenAuth{} })
	laptop := r.clientWith("laptop", nil)
	defer laptop.close()
	if rr := laptop.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("laptop: %+v", rr)
	}
	desk := r.clientWith("desk", nil)
	defer desk.close()
	rr := desk.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodeNameInUse || rr.Error != `the service "rdp" is already served by another client` {
		t.Fatalf("another token: %+v", rr)
	}
	// The desk's other service is not affected.
	if rr := desk.register(t, proto.TunnelRegister{Name: "ssh", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("desk's other service: %+v", rr)
	}
	again := r.clientWith("laptop", nil)
	defer again.close()
	rr = again.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"})
	if rr.OK || rr.Code != proto.CodeServiceBusy {
		t.Fatalf("the same token: %+v", rr)
	}
}

// A tunnel without a name may take over an offline automatic hold it asks
// for, and reclaim one when the pool is full; the holder loses its hold and
// the tunnel holds nothing. Pinned holds stay. Every take-over and reclaim is
// told, with both services, the port and the reason.
func TestRegisterTCP_UnnamedTakesOverAndReclaimsAutomaticHolds(t *testing.T) {
	res := newTCPResolver()
	res.holds[19505] = TCPPortHold{ServiceID: "svc-away1"}
	res.holds[19506] = TCPPortHold{ServiceID: "svc-pin", Pinned: true}
	res.holds[19507] = TCPPortHold{ServiceID: "svc-away2"}
	r := tcpRelay(t, 19505, 19508, res, nil)
	c := r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Type: "tcp", RemotePort: 19505, LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19505 {
		t.Fatalf("take over: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Type: "tcp", RemotePort: 19506, LocalAddr: "x"}); rr.OK {
		t.Fatalf("a pinned hold: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19508 {
		t.Fatalf("a free port: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19507 {
		t.Fatalf("reclaim: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Type: "tcp", LocalAddr: "x"}); rr.OK {
		t.Fatalf("only the pinned hold is left: %+v", rr)
	}
	res.mu.Lock()
	holds := len(res.holds)
	_, pin := res.holds[19506]
	res.mu.Unlock()
	if holds != 1 || !pin {
		t.Fatalf("holds %v", res.holds)
	}
	if got := r.reclaims(); !reflect.DeepEqual(got, []string{"<svc-away1:19505:takeover", "<svc-away2:19507:reclaim"}) {
		t.Fatalf("reclaims %v", got)
	}
}

// A named take-over and reclaim are told too.
func TestRegisterTCP_NamedReclaimsAreTold(t *testing.T) {
	res := newTCPResolver()
	res.holds[19510] = TCPPortHold{ServiceID: "svc-away1"}
	res.holds[19511] = TCPPortHold{ServiceID: "svc-away2"}
	r := tcpRelay(t, 19510, 19511, res, nil)
	c := r.client(nil)
	defer c.close()
	if rr := c.register(t, proto.TunnelRegister{Name: "fixed", Type: "tcp", RemotePort: 19510, LocalAddr: "x"}); !rr.OK {
		t.Fatalf("take over: %+v", rr)
	}
	if rr := c.register(t, proto.TunnelRegister{Name: "auto", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.RemotePort != 19511 {
		t.Fatalf("reclaim: %+v", rr)
	}
	if got := r.reclaims(); !reflect.DeepEqual(got, []string{"svc-fixed<svc-away1:19510:takeover", "svc-auto<svc-away2:19511:reclaim"}) {
		t.Fatalf("reclaims %v", got)
	}
}

// The cap counts the ports the allocator handed out and the store does not
// know yet: two registrations that read the same holds cannot both pass it.
func TestAllocateTCP_CapCountsPortsNotYetKept(t *testing.T) {
	a := newPortAllocator(19515, 19519)
	q := tcpPortRequest{userID: "u1", limit: 1, held: map[int]TCPPortHold{}, serving: func(string) bool { return false }}
	q.serviceID = "svc-a"
	if _, err := a.allocateTCP(q); err != nil {
		t.Fatal(err)
	}
	q.serviceID = "svc-b"
	if _, err := a.allocateTCP(q); err == nil || err.Error() != "port limit reached (1); delete an unused tcp service" {
		t.Fatalf("second service over the cap: %v", err)
	}
	// Another user is not counted against u1, and a released port is not
	// counted at all.
	q.userID = "u2"
	if _, err := a.allocateTCP(q); err != nil {
		t.Fatalf("another user: %v", err)
	}
	a.Release(19515)
	q.userID, q.serviceID = "u1", "svc-c"
	if _, err := a.allocateTCP(q); err != nil {
		t.Fatalf("after release: %v", err)
	}
}
