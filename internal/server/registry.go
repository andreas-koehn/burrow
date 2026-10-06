package server

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

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

	// HTTP tunnel fields (only set when IsHTTP==true).
	Subdomain string // assigned subdomain; "" for tcp tunnels
	ServiceID string // stable service identity from ServiceResolver; "" for tcp tunnels
	IsHTTP    bool   // true for http-mode tunnels (no TCP port allocated)
}

// ClientSession is one authenticated client connection.
type ClientSession struct {
	SessionID  string
	UserID     string
	RemoteAddr string
	// Handshake metadata (best-effort, from AuthRequest). Zero values when a
	// client predates these fields. Hostname is reserved (v0.3) and unused.
	OS            string
	Arch          string
	ClientVersion string
	TokenName     string
	Yamux         *yamux.Session
	mu            sync.Mutex
	Tunnels       map[string]*Tunnel

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
}

// RemoveSession drops a session and clears its tunnel-index entries.
func (r *Registry) RemoveSession(cs *ClientSession) {
	// Snapshot the session's tunnel IDs under cs.mu so we can clear them from
	// the registry-level tunnelIndex without holding two locks in nested order.
	cs.mu.Lock()
	ids := make([]string, 0, len(cs.Tunnels))
	for id := range cs.Tunnels {
		ids = append(ids, id)
	}
	cs.mu.Unlock()

	r.mu.Lock()
	delete(r.sessions, cs.SessionID)
	for _, id := range ids {
		// Only clear if the index still points at THIS session (a fast
		// re-add under a different session would be tolerated).
		if r.tunnelIndex[id] == cs {
			delete(r.tunnelIndex, id)
		}
	}
	r.mu.Unlock()
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
}

// RemoveTunnel drops a tunnel from a session and from the registry index.
func (r *Registry) RemoveTunnel(cs *ClientSession, tunnelID string) {
	cs.mu.Lock()
	delete(cs.Tunnels, tunnelID)
	cs.mu.Unlock()

	r.mu.Lock()
	if r.tunnelIndex[tunnelID] == cs {
		delete(r.tunnelIndex, tunnelID)
	}
	r.mu.Unlock()
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
