package server

// forward.go — forward streams (native tcp, P04): a signed-in client opens a
// stream whose first frame is forward_open {target}, and the relay pipes it
// to the client that serves that tcp service, through the same pairing as a
// visitor of a public port. This is the only way to reach a service in mode
// private; a service in mode open or ip can be reached this way too (its
// owner and the roles it allows skip the ip policy, never the tunnel's cap).
//
// Every open is authorized again (ForwardAuthorizer): a role taken out of the
// service's policy stops the next stream. A user who is suspended, or whose
// token is revoked or expires, loses the session itself (CloseByToken), and
// with it every forward stream of the session.
//
// Each session may open forwardOpenBurst streams at once and then
// forwardOpensPerMinute a minute, with at most forwardMaxActive open; what
// fails authorization counts as well, so a session cannot probe for names.

import (
	"context"
	"errors"
	"fmt"
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

// ForwardTarget is the tcp service a forward reaches.
type ForwardTarget struct {
	ServiceID, Name string
}

// ErrForwardNotFound: no tcp service answers to the target. ErrForwardDenied:
// one does, and the caller may not reach it. The client is told the same for
// both; the relay records a refusal of the second against the service.
var (
	ErrForwardNotFound = errors.New("forward target not found")
	ErrForwardDenied   = errors.New("forward target not allowed")
)

// ForwardAuthorizer decides who may forward to what. AuthorizeForward is
// called on every forward open: it resolves target (a slug, the name of one
// of the user's own tcp services, or a service id) and checks that the user
// may reach it: the owner always, another user when their role is in the
// service's access policy (an empty policy is the owner alone). It returns
// ErrForwardNotFound or ErrForwardDenied (with the target filled in) for a
// refusal, and any other error when it could not tell.
type ForwardAuthorizer interface {
	AuthorizeForward(ctx context.Context, userID, target string) (ForwardTarget, error)
}

// Limits of the forward streams of one session.
const (
	forwardOpenBurst      = 10
	forwardOpensPerMinute = 30
	forwardMaxActive      = 16
	// forwardMaxTarget bounds the target a client may name.
	forwardMaxTarget = 200
	// forwardAuthTimeout bounds the authorization of one open.
	forwardAuthTimeout = 5 * time.Second
)

// The kinds of a live connection of a tcp service.
const (
	ConnKindVisitor = "visitor" // a visitor of its public port
	ConnKindForward = "forward" // a forward stream of a signed-in client
)

// TrafficTCPForward is the kind of a forward in the connection log and in
// observe.Traffic.
const TrafficTCPForward = observe.TrafficTCPForward

// ConnLogReasonForbidden is the reason of a refused forward to a service the
// caller may not reach.
const ConnLogReasonForbidden = "forbidden"

// forwardLimits are the limits of one session's forward streams.
type forwardLimits struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	active int
	now    func() time.Time
}

func newForwardLimits() *forwardLimits {
	return &forwardLimits{tokens: forwardOpenBurst, now: time.Now}
}

// admit takes one open: false when the session has forwardMaxActive streams
// or has used up its bucket. An admitted open ends with done.
func (l *forwardLimits) admit() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.last.IsZero() {
		if el := now.Sub(l.last); el > 0 {
			l.tokens += el.Minutes() * forwardOpensPerMinute
			if l.tokens > forwardOpenBurst {
				l.tokens = forwardOpenBurst
			}
		}
	}
	l.last = now
	if l.active >= forwardMaxActive || l.tokens < 1 {
		return false
	}
	l.tokens--
	l.active++
	return true
}

func (l *forwardLimits) done() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.mu.Unlock()
}

// liveConn is one connection of a tcp service the relay carries now: a
// visitor of its public port or a forward stream. Its counters move while it
// is open.
type liveConn struct {
	id, kind  string
	sourceIP  string // the address it comes from, without the port
	userID    string // forward: the user who forwards
	tokenName string // forward: the name of their client token
	sessionID string // forward: their session
	serviceID string
	service   string // the service's name (forward)
	started   time.Time
	in, out   atomic.Uint64
	closeOnce sync.Once
	closeFn   func()
}

func (c *liveConn) close() { c.closeOnce.Do(c.closeFn) }

// ConnView is a live connection of a tcp service as the API shows it.
type ConnView struct {
	ID, Kind, SourceIP string
	UserID, TokenName  string // forward only
	SessionID          string // forward only: the forwarding session
	Since              time.Time
	BytesIn, BytesOut  uint64
}

// ForwardView is an open forward stream of a session.
type ForwardView struct {
	ID, ServiceID, Service string
	Since                  time.Time
	BytesIn, BytesOut      uint64
}

func (c *liveConn) view() ConnView {
	return ConnView{
		ID: c.id, Kind: c.kind, SourceIP: c.sourceIP, UserID: c.userID, TokenName: c.tokenName,
		SessionID: c.sessionID, Since: c.started, BytesIn: c.in.Load(), BytesOut: c.out.Load(),
	}
}

// addConn and dropConn keep the tunnel's table of live connections.
func (t *Tunnel) addConn(c *liveConn) {
	t.connMu.Lock()
	if t.conns == nil {
		t.conns = map[string]*liveConn{}
	}
	t.conns[c.id] = c
	t.connMu.Unlock()
}

func (t *Tunnel) dropConn(id string) {
	t.connMu.Lock()
	delete(t.conns, id)
	t.connMu.Unlock()
}

func (t *Tunnel) liveConns() []*liveConn {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	out := make([]*liveConn, 0, len(t.conns))
	for _, c := range t.conns {
		out = append(out, c)
	}
	return out
}

// forwardLimits returns the session's limits of forward streams.
func (cs *ClientSession) forwardLimits() *forwardLimits {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.fwdLimits == nil {
		cs.fwdLimits = newForwardLimits()
	}
	return cs.fwdLimits
}

func (cs *ClientSession) addForward(c *liveConn) {
	cs.mu.Lock()
	if cs.forwards == nil {
		cs.forwards = map[string]*liveConn{}
	}
	cs.forwards[c.id] = c
	cs.mu.Unlock()
}

func (cs *ClientSession) dropForward(id string) {
	cs.mu.Lock()
	delete(cs.forwards, id)
	cs.mu.Unlock()
}

// forwardViews lists the session's open forward streams.
func (cs *ClientSession) forwardViews() []ForwardView {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]ForwardView, 0, len(cs.forwards))
	for _, c := range cs.forwards {
		out = append(out, ForwardView{
			ID: c.id, ServiceID: c.serviceID, Service: c.service, Since: c.started,
			BytesIn: c.in.Load(), BytesOut: c.out.Load(),
		})
	}
	return out
}

// ServiceConnections lists the live connections of a tcp service: the
// visitors of its public port and the forward streams to it. nil when no
// tunnel serves it.
func (s *Server) ServiceConnections(serviceID string) []ConnView {
	tn, ok := s.LookupTunnelByServiceID(serviceID)
	if !ok || tn.IsHTTP {
		return nil
	}
	conns := tn.liveConns()
	out := make([]ConnView, 0, len(conns))
	for _, c := range conns {
		out = append(out, c.view())
	}
	return out
}

// CloseServiceConnection closes one live connection of a tcp service and
// reports whether there was one of that id. Its connection-log row is
// written when its goroutine sees it end.
func (s *Server) CloseServiceConnection(serviceID, connID string) bool {
	tn, ok := s.LookupTunnelByServiceID(serviceID)
	if !ok || tn.IsHTTP {
		return false
	}
	tn.connMu.Lock()
	c := tn.conns[connID]
	tn.connMu.Unlock()
	if c == nil {
		return false
	}
	c.close()
	return true
}

// endStream ends a yamux stream for both directions at once: Close alone
// only ends what this side writes, and a read would wait for the peer. A
// deadline in the past wakes the reads and writes that wait.
func endStream(st *yamux.Stream) {
	_ = st.SetDeadline(time.Now().Add(-time.Second))
	_ = st.Close()
}

// hostOf is the address of addr without its port.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// handleForward serves a stream whose first frame was forward_open: it
// answers with forward_open_response and, when the open is accepted, pipes
// the stream to the client that serves the service until either side ends.
func (s *Server) handleForward(cs *ClientSession, st *yamux.Stream, env proto.Envelope) {
	refuse := func(code, msg string) {
		_ = st.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
		_ = proto.WriteMessage(st, proto.MsgForwardOpenResp, proto.ForwardOpenResponse{Code: code, Error: msg})
		_ = st.Close()
	}
	var fo proto.ForwardOpen
	if proto.DecodePayload(env, &fo) != nil || fo.Target == "" || len(fo.Target) > forwardMaxTarget {
		refuse(proto.CodeBadRequest, "a forward needs a target: the slug, name or id of a tcp service")
		return
	}
	limits := cs.forwardLimits()
	if !limits.admit() {
		s.log.Info("forward refused: too many opens", "session_id", cs.SessionID, "user_id", cs.UserID)
		refuse(proto.CodeRateLimited, "too many forward streams from this session; try again in a moment")
		return
	}
	defer limits.done()

	notFound := fmt.Sprintf("no tcp service %q that you may reach", fo.Target)
	ctx, cancel := context.WithTimeout(context.Background(), forwardAuthTimeout)
	target, err := s.opts.Forwards.AuthorizeForward(ctx, cs.UserID, fo.Target)
	cancel()
	switch {
	case errors.Is(err, ErrForwardDenied):
		s.log.Info("forward refused: not allowed", "session_id", cs.SessionID, "user_id", cs.UserID, "service_id", target.ServiceID)
		s.refuseForward(cs, target.ServiceID, ConnLogReasonForbidden)
		refuse(proto.CodeNotFound, notFound)
		return
	case errors.Is(err, ErrForwardNotFound):
		s.log.Info("forward refused: no such service", "session_id", cs.SessionID, "user_id", cs.UserID)
		refuse(proto.CodeNotFound, notFound)
		return
	case err != nil:
		s.log.Warn("forward authorization failed", "session_id", cs.SessionID, "user_id", cs.UserID, "err", err)
		refuse(proto.CodeInternal, errTCPLookup)
		return
	}
	tn, ok := s.LookupTunnelByServiceID(target.ServiceID)
	if !ok || tn.IsHTTP {
		refuse(proto.CodeOffline, fmt.Sprintf("the service %q is offline: no client serves it now", target.Name))
		return
	}
	if tn.limits != nil {
		if reason := tn.limits.admitForward(); reason != "" {
			s.refuseForward(cs, target.ServiceID, reason)
			refuse(proto.CodeRateLimited, fmt.Sprintf("the service %q has too many connections; try again later", target.Name))
			return
		}
		defer tn.limits.done("")
	}

	started := time.Now()
	streamID := uuid.NewString()
	home, err := tn.sess.Pending().Await(streamID, tcpPairTimeout, func() error {
		return tn.sess.SendControl(proto.MsgNewConnection, proto.NewConnection{
			TunnelID: tn.ID, StreamID: streamID, SourceIP: "forward:" + cs.RemoteAddr,
		})
	})
	if tn.limits != nil {
		tn.limits.paired()
	}
	c := &liveConn{
		id: uuid.NewString(), kind: ConnKindForward, sourceIP: hostOf(cs.RemoteAddr),
		userID: cs.UserID, tokenName: cs.TokenName, sessionID: cs.SessionID,
		serviceID: target.ServiceID, service: target.Name, started: started,
	}
	if err != nil {
		s.log.Warn("client did not open stream for a forward", "tunnel_id", tn.ID, "err", err)
		s.forwardTraffic(tn, c, observe.OutcomeError, ConnLogReasonNoStream)
		refuse(proto.CodeOffline, fmt.Sprintf("the client that serves %q did not answer; try again", target.Name))
		return
	}
	defer home.Close()
	_ = st.SetReadDeadline(time.Time{})
	_ = st.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
	if err := proto.WriteMessage(st, proto.MsgForwardOpenResp, proto.ForwardOpenResponse{
		OK: true, ServiceID: target.ServiceID, Name: target.Name,
	}); err != nil {
		_ = st.Close()
		s.forwardTraffic(tn, c, observe.OutcomeError, "")
		return
	}
	_ = st.SetWriteDeadline(time.Time{})
	c.closeFn = func() { endStream(st); endStream(home) }
	tn.addConn(c)
	cs.addForward(c)
	s.log.Info("forward opened", "service_id", target.ServiceID, "tunnel_id", tn.ID, "user_id", cs.UserID,
		"token_name", cs.TokenName, "session_id", cs.SessionID, "source_ip", c.sourceIP)
	ev := sessionEvent(observe.ForwardOpened, cs)
	ev.ServiceID, ev.TunnelID, ev.TunnelType, ev.TokenName, ev.SourceIP = target.ServiceID, tn.ID, "tcp", cs.TokenName, c.sourceIP
	ev.OwnerID = tn.sess.UserID
	s.reg.observer().ObserveSession(ev)
	s.forwardsChanged(cs, tn)

	// What the forwarder writes goes to the home client (BytesIn); the
	// service's tunnel counts it, and so does the connection.
	var in, out atomic.Uint64
	bridge.Pipe(st, &countingStream{
		Conn: &countingStream{Conn: home, in: &tn.BytesIn, out: &tn.BytesOut},
		in:   &c.in, out: &c.out,
	}, &in, &out)
	tn.dropConn(c.id)
	cs.dropForward(c.id)
	s.forwardTraffic(tn, c, observe.OutcomeClean, "")
	s.forwardsChanged(cs, tn)
}

// forwardsChanged tells the dashboards of the forwarder and of the service's
// owner that the live connections changed.
func (s *Server) forwardsChanged(cs *ClientSession, tn *Tunnel) {
	s.opts.Events.PublishTunnelsChanged(tn.sess.UserID)
	if cs.UserID != tn.sess.UserID {
		s.opts.Events.PublishTunnelsChanged(cs.UserID)
	}
}

// forwardTraffic reports one forward stream that has ended: to the observer,
// and as a row of the connection log with the forwarding user, their token
// name and their address.
func (s *Server) forwardTraffic(tn *Tunnel, c *liveConn, outcome, reason string) {
	ended := time.Now()
	in, out := int64(c.in.Load()), int64(c.out.Load())
	s.reg.observer().ObserveTraffic(observe.Traffic{
		Kind: TrafficTCPForward, ServiceID: c.serviceID, TunnelID: tn.ID,
		UserID: c.userID, SessionID: c.sessionID, Outcome: outcome, SourceIP: c.sourceIP,
		BytesIn: in, BytesOut: out, Started: c.started, Duration: ended.Sub(c.started),
	})
	if s.opts.ConnLog != nil {
		s.opts.ConnLog.RecordConnection(context.Background(), ConnLogEntry{
			Kind: TrafficTCPForward, ServiceID: c.serviceID, TunnelID: tn.ID,
			UserID: c.userID, ClientSessionID: c.sessionID, SourceIP: c.sourceIP, TokenName: c.tokenName,
			StartedAt: c.started, EndedAt: ended, BytesIn: in, BytesOut: out,
			Status: outcome, Reason: reason,
		})
	}
}

// refuseForward counts a forward refused for a service that exists (not
// allowed, or over the tunnel's cap): every one is told to the observer, and
// the first of each (service, reason, address) in a minute is a rejected row
// of the connection log, as for the visitors of a public port.
func (s *Server) refuseForward(cs *ClientSession, serviceID, reason string) {
	now := time.Now()
	ip := hostOf(cs.RemoteAddr)
	s.reg.observer().ObserveTraffic(observe.Traffic{
		Kind: TrafficTCPForward, ServiceID: serviceID, UserID: cs.UserID, SessionID: cs.SessionID,
		Outcome: observe.OutcomeRejected, RejectReason: reason, SourceIP: ip, Started: now,
	})
	if serviceID == "" || !s.refusals.sample(serviceID, "forward:"+reason, ip, now) || s.opts.ConnLog == nil {
		return
	}
	s.opts.ConnLog.RecordConnection(context.Background(), ConnLogEntry{
		Kind: TrafficTCPForward, ServiceID: serviceID, UserID: cs.UserID, ClientSessionID: cs.SessionID,
		SourceIP: ip, TokenName: cs.TokenName, StartedAt: now, EndedAt: now,
		Status: observe.OutcomeRejected, Reason: reason,
	})
}
