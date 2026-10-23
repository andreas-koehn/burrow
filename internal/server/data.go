package server

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/bridge"
	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// pendingStreams pairs "a visitor is waiting for stream X" with "the client
// just opened stream X".
type pendingStreams struct {
	mu      sync.Mutex
	waiters map[string]chan *yamux.Stream
}

func newPendingStreams() *pendingStreams {
	return &pendingStreams{waiters: make(map[string]chan *yamux.Stream)}
}

// Await registers a waiter for id, runs after() (e.g. send the new_connection
// notify — done AFTER registration so Resolve can't race ahead of the waiter),
// then blocks until Resolve(id,...) or timeout.
func (p *pendingStreams) Await(id string, timeout time.Duration, after func() error) (*yamux.Stream, error) {
	ch := make(chan *yamux.Stream, 1)
	p.mu.Lock()
	p.waiters[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, id)
		p.mu.Unlock()
	}()
	if after != nil {
		if err := after(); err != nil {
			return nil, err
		}
	}
	select {
	case s := <-ch:
		return s, nil
	case <-time.After(timeout):
		return nil, errors.New("timeout waiting for client stream")
	}
}

// Resolve hands stream to a waiter for id; returns false if none was waiting.
func (p *pendingStreams) Resolve(id string, stream *yamux.Stream) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.waiters[id]
	if !ok {
		return false
	}
	ch <- stream
	return true
}

// portAllocator hands out free TCP ports in [min,max].
type portAllocator struct {
	mu       sync.Mutex
	min, max int
	used     map[int]bool
	// owner names the user and service of each port allocateTCP handed to a
	// service, until it is released: the cap counts them before the store
	// knows of them.
	owner map[int]portOwner
}

// portOwner is who a port of allocateTCP went to.
type portOwner struct{ userID, serviceID string }

func newPortAllocator(min, max int) *portAllocator {
	return &portAllocator{min: min, max: max, used: make(map[int]bool), owner: make(map[int]portOwner)}
}

// Allocate reserves requested (or, if 0, the first free port in range).
func (a *portAllocator) Allocate(requested int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if requested != 0 {
		if !a.inRange(requested) {
			return 0, fmt.Errorf("port %d out of range %d-%d", requested, a.min, a.max)
		}
		if a.used[requested] {
			return 0, portNotAvailable(requested)
		}
		a.used[requested] = true
		return requested, nil
	}
	for p := a.min; p <= a.max; p++ {
		if !a.used[p] {
			a.used[p] = true
			return p, nil
		}
	}
	return 0, errPortsFull
}

// inRange reports whether port is one the allocator hands out.
func (a *portAllocator) inRange(port int) bool { return port >= a.min && port <= a.max }

// errPortsFull is the refusal of an automatic port when none is left.
var errPortsFull = errors.New("no free ports in range")

// portNotAvailable is the refusal of a port that was asked for and cannot
// be had. It says nothing of who has it.
func portNotAvailable(port int) error { return fmt.Errorf("port %d is not available", port) }

// tcpPortPlan is the port a tcp registration got and what the service's hold
// on it becomes.
type tcpPortPlan struct {
	port     int
	pinned   bool   // the client asked for it
	takeFrom string // the offline service whose automatic hold is taken over
	reason   string // why: observe.TCPPortTakeover or observe.TCPPortReclaim
	persist  bool   // the hold changes
}

// tcpPortRequest is what a tcp registration brings to the allocator.
type tcpPortRequest struct {
	requested int    // the port the client asked for, 0: any
	serviceID string // "" for a tunnel without a service
	userID    string
	own       int  // the port the service holds, 0: none
	ownPinned bool // that hold is pinned
	limit     int  // how many ports the owner may hold, 0: no limit
	held      map[int]TCPPortHold
	serving   func(serviceID string) bool // the service has a live tunnel
}

// allocateTCP picks the public port of a tcp tunnel and reserves it in the
// allocator. The rules keep a service on its port and off the ports of
// others:
//
//   - a port asked for is given unless another service's hold is pinned or
//     that service is online; an automatic hold of an offline service is
//     taken over (that service gets another port next time). The hold
//     becomes pinned.
//   - otherwise the service's own port, when it is in range.
//   - otherwise the lowest free port that no service holds; when there is
//     none, the automatic hold of the offline service seen least recently is
//     reclaimed. Pinned holds are never reclaimed.
//
// A service that holds no port yet gets one only while its owner holds fewer
// than limit, counting the ports handed out that the store does not know
// yet. A tunnel without a service (an old client that sends no name) follows
// the same rules and holds nothing itself: a hold it takes over or reclaims
// is cleared.
func (a *portAllocator) allocateTCP(q tcpPortRequest) (tcpPortPlan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	named := q.serviceID != ""
	other := func(p int) (TCPPortHold, bool) {
		h, ok := q.held[p]
		return h, ok && h.ServiceID != q.serviceID
	}
	free := func(p int) bool { return a.inRange(p) && !a.used[p] }
	take := func(p int) {
		a.used[p] = true
		if named {
			a.owner[p] = portOwner{userID: q.userID, serviceID: q.serviceID}
		}
	}
	if named && q.own == 0 && q.limit > 0 {
		holders := map[string]bool{}
		for _, h := range q.held {
			if h.UserID == q.userID && h.ServiceID != q.serviceID {
				holders[h.ServiceID] = true
			}
		}
		for _, o := range a.owner {
			if o.userID == q.userID && o.serviceID != q.serviceID {
				holders[o.serviceID] = true
			}
		}
		if len(holders) >= q.limit {
			return tcpPortPlan{}, fmt.Errorf("port limit reached (%d); delete an unused tcp service", q.limit)
		}
	}

	if q.requested != 0 {
		if !a.inRange(q.requested) {
			return tcpPortPlan{}, fmt.Errorf("port %d out of range %d-%d", q.requested, a.min, a.max)
		}
		plan := tcpPortPlan{port: q.requested, pinned: named}
		if h, ok := other(q.requested); ok {
			if h.Pinned || q.serving(h.ServiceID) {
				return tcpPortPlan{}, portNotAvailable(q.requested)
			}
			plan.takeFrom, plan.reason = h.ServiceID, observe.TCPPortTakeover
		}
		if !free(q.requested) {
			return tcpPortPlan{}, portNotAvailable(q.requested)
		}
		take(q.requested)
		plan.persist = named && (q.requested != q.own || !q.ownPinned || plan.takeFrom != "")
		return plan, nil
	}
	if named && q.own != 0 && a.inRange(q.own) {
		if !free(q.own) {
			return tcpPortPlan{}, portNotAvailable(q.own)
		}
		take(q.own)
		return tcpPortPlan{port: q.own, pinned: q.ownPinned}, nil
	}
	for p := a.min; p <= a.max; p++ {
		if _, held := q.held[p]; free(p) && !held {
			take(p)
			return tcpPortPlan{port: p, persist: named}, nil
		}
	}
	best, bestSeen := 0, time.Time{}
	for p := a.min; p <= a.max; p++ {
		h, ok := other(p)
		if !ok || h.Pinned || !free(p) || q.serving(h.ServiceID) {
			continue
		}
		if best == 0 || h.LastSeen.Before(bestSeen) {
			best, bestSeen = p, h.LastSeen
		}
	}
	if best == 0 {
		return tcpPortPlan{}, errPortsFull
	}
	take(best)
	return tcpPortPlan{port: best, takeFrom: q.held[best].ServiceID, reason: observe.TCPPortReclaim, persist: named}, nil
}

// Release returns a port to the pool.
func (a *portAllocator) Release(port int) {
	a.mu.Lock()
	delete(a.used, port)
	delete(a.owner, port)
	a.mu.Unlock()
}

// startPublicListener binds tun.RemotePort and bridges each visitor that
// admitVisitor lets in. The judgement runs on the accept loop, before any
// goroutine is started for the visitor and before the home client is told:
// a refused visitor costs one accept and one close.
func (s *Server) startPublicListener(tun *Tunnel) error {
	addr := fmt.Sprintf("%s:%d", s.opts.PublicBind, tun.RemotePort)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	tun.Listener = l
	go func() {
		// B11: capped exponential backoff on transient accept errors (EMFILE etc.)
		// mirrors the net/http tempDelay pattern. Exits immediately on net.ErrClosed.
		var delay time.Duration
		for {
			visitor, err := l.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				s.log.Warn("public accept", "tunnel_id", tun.ID, "err", err)
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				if delay > time.Second {
					delay = time.Second
				}
				time.Sleep(delay)
				continue
			}
			delay = 0 // reset on success
			source, ok := s.admitVisitor(tun, visitor)
			if !ok {
				continue
			}
			go s.bridgeVisitor(tun, visitor, source)
		}
	}()
	return nil
}

// admitVisitor decides whether a visitor of a tcp tunnel may reach the home
// client: in mode ip its address must be allowed by the tunnel's policy, and
// in every mode it must fit the tunnel's limits. A refused visitor is closed
// at once, without a byte, and counted (see refuseVisitor). On success it
// returns the source the limits count it by; the caller must end it with
// tun.limits.done.
//
// The address is the connection's own (RemoteAddr): behind Docker's
// userland proxy it is the bridge gateway, and that is what is checked and
// recorded.
func (s *Server) admitVisitor(tun *Tunnel, visitor net.Conn) (string, bool) {
	ip := visitorAddr(visitor)
	if !ip.IsValid() {
		s.refuseVisitor(tun, visitor, "", TCPRefusedNoAddress)
		return "", false
	}
	if tun.accessMode == TCPAccessIP && !tun.policy.Load().allows(ip, time.Now()) {
		s.refuseVisitor(tun, visitor, ip.String(), observe.RejectedIPGeo)
		return "", false
	}
	source := sourceKey(ip)
	if tun.limits != nil {
		if reason := tun.limits.admit(source); reason != "" {
			s.refuseVisitor(tun, visitor, ip.String(), reason)
			return "", false
		}
	}
	return source, true
}

func (s *Server) bridgeVisitor(tun *Tunnel, visitor net.Conn, source string) {
	defer visitor.Close()
	if tun.limits != nil {
		defer tun.limits.done(source)
	}
	started := time.Now()
	streamID := uuid.NewString()
	stream, err := tun.sess.Pending().Await(streamID, tcpPairTimeout, func() error {
		return tun.sess.SendControl(proto.MsgNewConnection, proto.NewConnection{
			TunnelID: tun.ID, StreamID: streamID, SourceIP: visitor.RemoteAddr().String(),
		})
	})
	if tun.limits != nil {
		tun.limits.paired()
	}
	if err != nil {
		s.log.Warn("client did not open stream", "tunnel_id", tun.ID, "err", err)
		s.tcpTraffic(tun, visitor, started, 0, 0, observe.OutcomeError, ConnLogReasonNoStream)
		return
	}
	defer stream.Close()
	// The tunnel's counters move while the connection is open (the stream
	// counts what passes, as for http), and so do the connection's own,
	// which the API shows while it lasts and its log row keeps.
	c := &liveConn{
		id: uuid.NewString(), kind: ConnKindVisitor, sourceIP: hostOf(visitor.RemoteAddr().String()),
		serviceID: tun.ServiceID, started: started,
	}
	c.closeFn = func() { _ = visitor.Close(); endStream(stream) }
	tun.addConn(c)
	var in, out atomic.Uint64
	bridge.Pipe(visitor, &countingStream{
		Conn: &countingStream{Conn: stream, in: &tun.BytesIn, out: &tun.BytesOut},
		in:   &c.in, out: &c.out,
	}, &in, &out)
	tun.dropConn(c.id)
	s.tcpTraffic(tun, visitor, started, c.in.Load(), c.out.Load(), observe.OutcomeClean, "")
}

// The first frame of a client-opened stream (stream_open or forward_open) is
// small: one that declares more than maxFirstFrame bytes is closed before
// anything is allocated for it. A session may have maxStreamsWaiting streams
// whose first frame has not come yet; one more is closed at once.
const (
	maxFirstFrame     = 4 << 10
	maxStreamsWaiting = 32
)

// readFirstFrame reads one frame of at most maxFirstFrame bytes: its length
// is checked before its body is read or allocated.
func readFirstFrame(r io.Reader, env *proto.Envelope) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFirstFrame {
		return fmt.Errorf("first frame of %d bytes, at most %d", n, maxFirstFrame)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, env)
}

// forwardsOf is how many forward streams of user (not the owner) the tunnel
// has open (tests).
func (l *tcpLimits) forwardsOf(user string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.perUser[user]
}

// handleDataStream reads the first frame of a freshly-accepted client
// stream. A StreamHeader hands it to the waiting visitor (it is closed if
// nobody is waiting); a forward_open is a forward stream (see handleForward)
// on a relay that takes them. Anything else is closed, which is also what an
// older relay does with a forward_open.
//
// The caller counted the stream in cs.waitingFirst; it is counted out once
// the first frame is read or has failed.
func (s *Server) handleDataStream(cs *ClientSession, st *yamux.Stream) {
	var env proto.Envelope
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	err := readFirstFrame(st, &env)
	cs.waitingFirst.Add(-1)
	if err != nil {
		_ = st.Close()
		return
	}
	if env.Type == proto.MsgForwardOpen && s.forwarding() {
		s.handleForward(cs, st, env)
		return
	}
	if env.Type != proto.MsgStreamOpen {
		_ = st.Close()
		return
	}
	var sh proto.StreamHeader
	if proto.DecodePayload(env, &sh) != nil {
		_ = st.Close()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	if !cs.Pending().Resolve(sh.StreamID, st) {
		_ = st.Close()
	}
}
