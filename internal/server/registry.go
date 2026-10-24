package server

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// controlWriteTimeout is the per-write deadline applied to control-stream
// writes (B01). Keeps a stalled-but-not-disconnected client from pinning the
// visitor goroutine and its socket indefinitely.
// Declared as a var so tests can inject a shorter value without touching the
// production constant.
var controlWriteTimeout = 10 * time.Second

// writeDeadliner is the subset of net.Conn / *yamux.Stream we need to set
// per-write deadlines. Injecting it separately lets tests use a plain
// io.Writer (e.g. bytes.Buffer) without panic.
type writeDeadliner interface {
	SetWriteDeadline(t time.Time) error
}

// Tunnel is a registered tunnel.
type Tunnel struct {
	ID         string
	Name       string
	Type       string
	RemotePort int
	LocalAddr  string

	// Listener is the public port listener (Phase 3); nil until started.
	Listener net.Listener
	// BytesIn counts visitor→local bytes; BytesOut counts local→visitor.
	BytesIn  atomic.Uint64
	BytesOut atomic.Uint64
	// sess is the owning session (for control-channel notifies).
	sess *ClientSession

	// ServiceID is the stable service identity from ServiceResolver (http)
	// or TCPServiceResolver (tcp); "" for a tunnel without a service.
	ServiceID string
	// HTTP tunnel fields (only set when IsHTTP==true).
	Subdomain string // assigned subdomain; "" for tcp tunnels
	IsHTTP    bool   // true for http-mode tunnels (no TCP port allocated)

	// What the registration reported, for the observer's TunnelOpened event.
	// accessMode is also the mode a tcp tunnel was registered in; it does
	// not change while the tunnel lives (a new mode closes the session, see
	// Server.RefreshTCPService).
	accessMode string
	created    bool

	// policy is the compiled ip policy of a tcp tunnel in mode ip (nil lets
	// nobody in), or the country rules of one in mode open (nil: none); limits are the limits of a tcp tunnel with a public port.
	policy atomic.Pointer[tcpPolicy]
	limits *tcpLimits

	// conns are the live connections of a tcp tunnel (visitors and
	// forwards), by id, under connMu.
	connMu sync.Mutex
	conns  map[string]*liveConn

	// settings are the tcp service's PROXY protocol and idle timeout as the
	// relay applies them now (see tcp_settings.go); nil is none.
	// clientProxy is the PROXY protocol the client said it writes by
	// itself (TunnelRegister.ProxyProtocol), for the dashboard.
	settings    atomic.Pointer[tcpSettings]
	clientProxy string
	// sweeping: a sweepIdle goroutine runs for the tunnel (under connMu).
	sweeping bool
	// slug is the tcp service's slug and sniName its own TLS passthrough
	// host name (see passthrough.go); the slug never changes, the host name
	// with RefreshTCPService.
	slug    string
	sniName atomic.Pointer[string]
	// guard is the tcp service's bans, bandwidth and daily volume
	// (tcp_guard.go); nil for a tunnel without a service.
	guard *tcpGuard
}

// ClientSession is one authenticated client connection.
type ClientSession struct {
	SessionID  string
	UserID     string
	RemoteAddr string
	// LocalAddr is the relay's address the session connected to: the
	// destination of the PROXY protocol header of its forward streams.
	LocalAddr string
	// proxyV2: the client writes a PROXY protocol v2 header when asked
	// (proto.CapProxyProtocolV2).
	proxyV2 bool
	// Handshake metadata (best-effort, from AuthRequest). Zero values when a
	// client predates these fields. Hostname is reserved (v0.3) and unused.
	OS            string
	Arch          string
	ClientVersion string
	TokenName     string
	// TokenID is the id of the client token the session logged in with. Empty
	// when the authenticator names none (see HandleHandshakeWith); such a
	// session is never recorded on a token.
	TokenID string
	// Scope is the scope of the token the session logged in with (the
	// db.TokenScope… values). Only those values allow anything; the handshake
	// sets db.TokenScopeAll for an authenticator that has no scopes (a test
	// double; the relay's own is a ScopedAuthenticator).
	Scope string
	// ConnectedAt is when the handshake succeeded.
	ConnectedAt time.Time
	Yamux       *yamux.Session
	mu          sync.Mutex
	Tunnels     map[string]*Tunnel

	pending       *pendingStreams
	ctrlMu        sync.Mutex
	ctrl          io.Writer
	ctrlDeadliner writeDeadliner // non-nil when ctrl also supports SetWriteDeadline

	// summaries holds the request summaries that wait to be written to the
	// client. nil: the client did not announce proto.CapRequestSummaries and
	// is sent none. See offerSummary and runSummaries.
	summaries        chan proto.RequestSummary
	summariesDropped atomic.Uint64
	summariesOff     atomic.Bool // the sender has ended

	// fwdLimits limit the forward streams this session opens; forwards are
	// the open ones, by id (both under mu).
	fwdLimits *forwardLimits
	forwards  map[string]*liveConn
	// waitingFirst counts the client-opened streams whose first frame has
	// not been read yet (see maxStreamsWaiting).
	waitingFirst atomic.Int32
}

// summaryQueue is how many request summaries may wait for one client.
const summaryQueue = 256

// WantsRequestSummaries reports whether the client announced that it reads
// request summaries.
func (cs *ClientSession) WantsRequestSummaries() bool { return cs.summaries != nil }

// SummariesDropped is how many request summaries were not sent to this client
// because it did not take them fast enough.
func (cs *ClientSession) SummariesDropped() uint64 { return cs.summariesDropped.Load() }

// offerSummary queues a summary for the client and reports whether it was
// taken. It never waits: the caller is the goroutine of a visitor's request.
// When summaryQueue summaries wait already, this one is dropped and counted.
func (cs *ClientSession) offerSummary(s proto.RequestSummary) bool {
	if cs.summaries == nil {
		return false
	}
	if !cs.summariesOff.Load() {
		select {
		case cs.summaries <- s:
			return true
		default:
		}
	}
	cs.summariesDropped.Add(1)
	return false
}

// runSummaries writes the queued summaries to the client, one at a time, until
// done is closed or a write fails. It is the only place that waits for the
// control stream on behalf of summaries. A failed write (the stream is closed,
// or the client took nothing for controlWriteTimeout) ends it for the session:
// writing on would hold the control stream for the same time again and again.
func (cs *ClientSession) runSummaries(done <-chan struct{}) {
	defer cs.summariesOff.Store(true)
	for {
		select {
		case <-done:
			return
		case s := <-cs.summaries:
			if cs.SendControl(proto.MsgRequestSummary, s) != nil {
				return
			}
		}
	}
}

// Registry tracks live sessions and their tunnels (in-memory, mutex-guarded).
//
// tunnelIndex (v0.5.2 BACKLOG #1) is a parallel map keyed by tunnel runtime ID
// pointing at the owning session. It is maintained on AddTunnel/RemoveTunnel/
// RemoveSession and lets LookupSessionByTunnelID do an O(1) probe instead of
// the O(N sessions x M tunnels) scan the previous SnapshotSessions-based
// lookup performed once per proxied request.
type Registry struct {
	mu          sync.RWMutex
	sessions    map[string]*ClientSession
	tunnelIndex map[string]*ClientSession
	obs         Observer // nil: nobody is told
}

// SetObserver registers the observer of this registry's sessions and tunnels.
func (r *Registry) SetObserver(o Observer) {
	r.mu.Lock()
	r.obs = o
	r.mu.Unlock()
}

// observer returns the registered observer, or one that does nothing.
func (r *Registry) observer() Observer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.obs == nil {
		return noopObserver{}
	}
	return r.obs
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		sessions:    make(map[string]*ClientSession),
		tunnelIndex: make(map[string]*ClientSession),
	}
}

// AddSession registers a session (initialising its tunnel map).
func (r *Registry) AddSession(cs *ClientSession) {
	if cs.Tunnels == nil {
		cs.Tunnels = make(map[string]*Tunnel)
	}
	r.mu.Lock()
	r.sessions[cs.SessionID] = cs
	r.mu.Unlock()
	r.observer().ObserveSession(sessionEvent(observe.SessionOpened, cs))
}

// RemoveSession drops a session and clears its tunnel-index entries.
func (r *Registry) RemoveSession(cs *ClientSession) {
	// Snapshot the session's tunnel IDs under cs.mu so we can clear them from
	// the registry-level tunnelIndex without holding two locks in nested order.
	cs.mu.Lock()
	ids := make([]string, 0, len(cs.Tunnels))
	open := make([]*Tunnel, 0, len(cs.Tunnels))
	for id, t := range cs.Tunnels {
		ids = append(ids, id)
		open = append(open, t)
	}
	cs.mu.Unlock()

	r.mu.Lock()
	_, registered := r.sessions[cs.SessionID]
	delete(r.sessions, cs.SessionID)
	for _, id := range ids {
		// Only clear if the index still points at THIS session (a fast
		// re-add under a different session would be tolerated).
		if r.tunnelIndex[id] == cs {
			delete(r.tunnelIndex, id)
		}
	}
	r.mu.Unlock()

	// Told once: a session that is not registered (any more) ends nothing.
	if !registered {
		return
	}
	obs := r.observer()
	for _, t := range open {
		obs.ObserveSession(tunnelEvent(observe.TunnelClosed, cs, t))
	}
	ev := sessionEvent(observe.SessionClosed, cs)
	if !cs.ConnectedAt.IsZero() {
		ev.Duration = time.Since(cs.ConnectedAt)
	}
	obs.ObserveSession(ev)
}

// AddTunnel records a tunnel under a session and indexes it on the registry.
func (r *Registry) AddTunnel(cs *ClientSession, t *Tunnel) {
	cs.mu.Lock()
	if cs.Tunnels == nil {
		cs.Tunnels = make(map[string]*Tunnel)
	}
	cs.Tunnels[t.ID] = t
	cs.mu.Unlock()

	r.mu.Lock()
	r.tunnelIndex[t.ID] = cs
	r.mu.Unlock()
	r.observer().ObserveSession(tunnelEvent(observe.TunnelOpened, cs, t))
}

// RemoveTunnel drops a tunnel from a session and from the registry index.
func (r *Registry) RemoveTunnel(cs *ClientSession, tunnelID string) {
	cs.mu.Lock()
	t := cs.Tunnels[tunnelID]
	delete(cs.Tunnels, tunnelID)
	cs.mu.Unlock()

	r.mu.Lock()
	if r.tunnelIndex[tunnelID] == cs {
		delete(r.tunnelIndex, tunnelID)
	}
	r.mu.Unlock()
	if t != nil {
		r.observer().ObserveSession(tunnelEvent(observe.TunnelClosed, cs, t))
	}
}

// SessionByTunnelID probes the tunnel-ID index. Returns (cs, true) when the
// tunnel is live, (nil, false) otherwise. O(1).
func (r *Registry) SessionByTunnelID(tunnelID string) (*ClientSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cs, ok := r.tunnelIndex[tunnelID]
	return cs, ok
}

// Sessions returns a snapshot slice of live sessions.
func (r *Registry) Sessions() []*ClientSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ClientSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// CloseByToken ends every live session that logged in with the client token
// and returns how many there were. It closes the session's yamux session,
// which is how a session ends everywhere else (a lost connection, a
// shutdown): the handler of the connection then closes the tunnels'
// listeners, frees their ports, writes the last session record and takes the
// session out of the registry. An empty id closes nothing.
func (r *Registry) CloseByToken(tokenID string) int {
	if tokenID == "" {
		return 0
	}
	n := 0
	for _, cs := range r.Sessions() {
		if cs.TokenID != tokenID || cs.Yamux == nil {
			continue
		}
		_ = cs.Yamux.Close()
		n++
	}
	return n
}

// CloseByToken ends the live sessions of a client token (see
// Registry.CloseByToken). The API calls it when the token is revoked and when
// its user is suspended or deleted.
func (s *Server) CloseByToken(tokenID string) int { return s.reg.CloseByToken(tokenID) }

// CloseByService ends every live session that serves a tunnel of the service
// and returns how many there were. Like CloseByToken it closes the yamux
// session and leaves the cleanup to the handler of the connection. An empty
// id closes nothing.
func (r *Registry) CloseByService(serviceID string) int {
	if serviceID == "" {
		return 0
	}
	n := 0
	for _, cs := range r.Sessions() {
		if cs.Yamux == nil {
			continue
		}
		for _, t := range r.snapshotTunnels(cs) {
			if t.ServiceID == serviceID {
				_ = cs.Yamux.Close()
				n++
				break
			}
		}
	}
	return n
}

// CloseByService ends the live sessions that serve a service (see
// Registry.CloseByService). The API calls it when the service is deleted.
func (s *Server) CloseByService(serviceID string) int { return s.reg.CloseByService(serviceID) }

// Tunnel returns the named tunnel for a session, or nil.
func (r *Registry) Tunnel(cs *ClientSession, id string) *Tunnel {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.Tunnels[id]
}

// snapshotTunnels returns a copy of a session's tunnels (safe to range after).
func (r *Registry) snapshotTunnels(cs *ClientSession) []*Tunnel {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]*Tunnel, 0, len(cs.Tunnels))
	for _, t := range cs.Tunnels {
		out = append(out, t)
	}
	return out
}

// snapshotTunnelsForTest exposes a session's tunnels for white-box tests.
func (cs *ClientSession) snapshotTunnelsForTest() []*Tunnel {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]*Tunnel, 0, len(cs.Tunnels))
	for _, t := range cs.Tunnels {
		out = append(out, t)
	}
	return out
}

// SetControl records the control stream used for serialized server→client writes.
// If w also implements writeDeadliner (e.g. *yamux.Stream, net.Conn), per-write
// deadlines will be applied automatically by SendControl.
func (cs *ClientSession) SetControl(w io.Writer) {
	cs.ctrlMu.Lock()
	if d, ok := w.(writeDeadliner); ok {
		cs.ctrlDeadliner = d
	} else {
		cs.ctrlDeadliner = nil
	}
	cs.ctrl = w
	cs.ctrlMu.Unlock()
}

// SendControl writes one control message to the client, serialized against all
// other senders (control-loop replies and visitor notifies).
// B01: each write is bounded by controlWriteTimeout so a stalled client cannot
// pin the caller's goroutine/socket indefinitely. The deadline is cleared after
// each write so it is per-write, not cumulative.
func (cs *ClientSession) SendControl(typ proto.MessageType, payload any) error {
	cs.ctrlMu.Lock()
	defer cs.ctrlMu.Unlock()
	if cs.ctrl == nil {
		return io.ErrClosedPipe
	}
	if cs.ctrlDeadliner != nil {
		_ = cs.ctrlDeadliner.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
		defer func() { _ = cs.ctrlDeadliner.SetWriteDeadline(time.Time{}) }()
	}
	return proto.WriteMessage(cs.ctrl, typ, payload)
}

// Pending returns the session's pending-stream registry (lazily created).
func (cs *ClientSession) Pending() *pendingStreams {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.pending == nil {
		cs.pending = newPendingStreams()
	}
	return cs.pending
}

// Texts of a refusal for the scope of the token. They name the scope and say
// what to do, and travel with proto.CodeScopeDenied.
const (
	errScopeNoPublish = "this token is limited to forwarding (scope forward) and may not register tunnels; use a token with scope all or publish"
	errScopeNoForward = "this token is limited to publishing (scope publish) and may not open forwards; use a token with scope all or forward"
)

// MayPublish reports whether the session's token may register tunnels. Only
// the scopes all and publish allow it: an empty or unknown scope denies.
func (cs *ClientSession) MayPublish() bool {
	return cs.Scope == db.TokenScopeAll || cs.Scope == db.TokenScopePublish
}

// MayForward reports whether the session's token may open forwards. Only the
// scopes all and forward allow it: an empty or unknown scope denies.
func (cs *ClientSession) MayForward() bool {
	return cs.Scope == db.TokenScopeAll || cs.Scope == db.TokenScopeForward
}
