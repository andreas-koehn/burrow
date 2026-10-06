package client

import (
	"sync"
	"time"
)

// ConnState is the state of the control connection.
type ConnState string

// The states a client goes through.
const (
	StateConnecting   ConnState = "connecting"
	StateConnected    ConnState = "connected"
	StateReconnecting ConnState = "reconnecting"
)

// RegisteredTunnel is a tunnel the relay accepted.
type RegisteredTunnel struct {
	TunnelID, Name, Type, LocalAddr string
	// URL is the public address of an http tunnel; an older relay reports only
	// a hostname, which is then what URL holds.
	URL string
	// RemotePort is the public port of a tcp tunnel.
	RemotePort int

	// What follows is known only from a relay that reports it; an older one
	// leaves it empty.
	//
	// AccessMode is the relay's access mode of an http service: open, api_key,
	// burrow_login or mtls.
	AccessMode string
	// Created says that this registration created the service.
	Created bool
	// DashboardURL is the page of the service in the dashboard.
	DashboardURL string
	// Ignored names the wishes (TunnelSpec.Slug, .Access) the relay did not
	// apply because the service existed with other values.
	Ignored OptionSet
	// SlugUnacknowledged: a slug was wished for and the relay said nothing
	// about it. It is older and never saw the wish, so the service has the
	// slug the relay gave it. (An access mode the relay did not acknowledge
	// ends the client instead: see AccessNotAppliedError.)
	SlugUnacknowledged bool
}

// OptionSet names some of the two things a client can wish for a new service.
type OptionSet struct{ Slug, Access bool }

// Any reports whether the set names anything.
func (s OptionSet) Any() bool { return s.Slug || s.Access }

// SessionInfo is what the relay says of itself after a successful sign-in. An
// older relay says nothing: RelayVersion is "".
type SessionInfo struct {
	RelayVersion string
}

// SessionObserver is an Observer that also wants to know what the relay said
// when the control connection was accepted. It is optional: an Observer
// without the method is not told.
type SessionObserver interface {
	// Session is called after State(StateConnected), once per connection.
	Session(info SessionInfo)
}

// CountObserver is an Observer that is also told how many visitor connections
// a tunnel has. It is optional: an Observer without the method is not told.
//
// The calls for single connections (Connection, ConnectionClosed) are dropped
// first when the observer cannot keep up, so counting them leaves connections
// open that have ended. Counts are not dropped: of each tunnel the newest
// numbers are always delivered.
type CountObserver interface {
	// Counts reports the visitor connections of a tunnel: how many are open
	// now and how many there were since the tunnel was registered.
	Counts(tunnelID string, open, total int)
}

// Observer receives what the client does.
//
// The client never waits for an observer: calls are queued, made one at a time
// from a goroutine of their own and in the order in which things happened.
// When more than maxPendingEvents wait, the oldest call for a single
// connection or request (Connection, ConnectionClosed, Request) is dropped;
// the other calls are dropped, oldest first, only when no such call waits. A
// method should still return quickly, because a slow one delays the calls
// behind it.
type Observer interface {
	// State reports the control connection: connecting, connected, or
	// reconnecting with the error that ended the last attempt and the delay
	// before the next one.
	State(s ConnState, detail string, retryIn time.Duration)
	// Registered reports a tunnel the relay accepted. It is called again for
	// every tunnel after each reconnect, with new tunnel ids.
	Registered(t RegisteredTunnel)
	// Connection reports a visitor connection on a tunnel.
	Connection(tunnelID string, at time.Time, sourceIP string)
	// ConnectionClosed reports that a visitor connection has ended.
	ConnectionClosed(tunnelID string)
	// Latency reports the round-trip time of a ping on the control connection.
	Latency(rtt time.Duration)
	// LocalTarget reports whether the local service at localAddr accepted the
	// connection made for a visitor. It is called when that changes.
	LocalTarget(localAddr string, reachable bool)
	// Request reports a request to an http tunnel that the relay has answered:
	// when it arrived (local time), its method, its path without the query
	// string, and the status of the answer. It is called only when
	// Options.RequestSummaries is set and the relay sends summaries. Method
	// and path are the visitor's words, bounded and without control
	// characters (proto.SummaryMethod, proto.SummaryPath).
	Request(tunnelID string, at time.Time, method, path string, status int)
}

// maxPendingEvents is how many observer calls may wait.
const maxPendingEvents = 512

// event is one waiting observer call.
type event struct {
	call func(Observer)
	// line: the call tells of a single connection or request. Nothing else
	// depends on it, so it is the first to go when the queue is full.
	line bool
	// counts: the call delivers the newest counts of this tunnel (see
	// notifier.counts). It is never dropped; there is at most one per tunnel.
	counts string
}

// notifier hands events to an Observer without ever making the caller wait.
// A nil notifier drops everything: that is a client without an observer.
type notifier struct {
	obs  Observer
	wake chan struct{}

	mu      sync.Mutex
	pending []event
	latest  map[string][2]int // tunnel id → open, total; waiting to be told
	stopped bool
}

func newNotifier(o Observer) *notifier {
	return &notifier{obs: o, wake: make(chan struct{}, 1), latest: map[string][2]int{}}
}

// emit queues one call. It takes a lock that is only ever held for a few
// instructions, never while the observer runs.
func (n *notifier) emit(f func(Observer)) { n.queue(event{call: f}) }

// emitLine queues a call that tells of a single connection or request.
func (n *notifier) emitLine(f func(Observer)) { n.queue(event{call: f, line: true}) }

// counts tells a CountObserver the newest counts of a tunnel. While the
// numbers of a tunnel wait to be told, newer ones replace them, so a burst of
// connections costs one call and no memory.
func (n *notifier) counts(tunnelID string, open, total int) {
	if n == nil {
		return
	}
	if _, ok := n.obs.(CountObserver); !ok {
		return
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	_, waiting := n.latest[tunnelID]
	n.latest[tunnelID] = [2]int{open, total}
	n.mu.Unlock()
	if waiting {
		return
	}
	n.queue(event{counts: tunnelID, call: func(o Observer) {
		n.mu.Lock()
		c, ok := n.latest[tunnelID]
		delete(n.latest, tunnelID)
		n.mu.Unlock()
		if co, is := o.(CountObserver); ok && is {
			co.Counts(tunnelID, c[0], c[1])
		}
	}})
}

func (n *notifier) queue(e event) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	if len(n.pending) >= maxPendingEvents {
		n.makeRoom()
	}
	n.pending = append(n.pending, e)
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// makeRoom drops one waiting call: the oldest line, or without one the oldest
// call that is not the counts of a tunnel. n.mu is held.
func (n *notifier) makeRoom() {
	victim := -1
	for i, e := range n.pending {
		if e.line {
			victim = i
			break
		}
		if victim < 0 && e.counts == "" {
			victim = i
		}
	}
	if victim < 0 {
		return // counts only: one per tunnel, which bounds them
	}
	copy(n.pending[victim:], n.pending[victim+1:])
	n.pending[len(n.pending)-1] = event{}
	n.pending = n.pending[:len(n.pending)-1]
}

// start delivers queued calls until the returned function is called. Stopping
// does not wait for an observer that is still in a call; what is queued then is
// dropped.
func (n *notifier) start() (stop func()) {
	n.mu.Lock()
	n.stopped = false
	n.mu.Unlock()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			var next func(Observer)
			n.mu.Lock()
			if len(n.pending) > 0 {
				next = n.pending[0].call
				n.pending[0] = event{}
				n.pending = n.pending[1:]
			}
			n.mu.Unlock()
			if next != nil {
				n.call(next)
				continue
			}
			select {
			case <-done:
				return
			case <-n.wake:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			n.mu.Lock()
			n.stopped = true
			n.pending = nil
			clear(n.latest)
			n.mu.Unlock()
			close(done)
		})
	}
}

// call makes one observer call. A panic in the observer ends that call and
// nothing else: what shows the tunnels must not be able to take them down.
func (n *notifier) call(f func(Observer)) {
	defer func() { _ = recover() }()
	f(n.obs)
}
