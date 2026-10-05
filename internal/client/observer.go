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
}

// Observer receives what the client does.
//
// The client never waits for an observer: calls are queued, made one at a time
// from a goroutine of their own and in the order in which things happened, and
// the oldest waiting call is dropped when more than maxPendingEvents are
// queued. A method should still return quickly, because a slow one delays the
// calls behind it.
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
}

// maxPendingEvents is how many observer calls may wait.
const maxPendingEvents = 512

// notifier hands events to an Observer without ever making the caller wait.
// A nil notifier drops everything: that is a client without an observer.
type notifier struct {
	obs  Observer
	wake chan struct{}

	mu      sync.Mutex
	pending []func(Observer)
	stopped bool
}

func newNotifier(o Observer) *notifier {
	return &notifier{obs: o, wake: make(chan struct{}, 1)}
}

// emit queues one call. It takes a lock that is only ever held for a few
// instructions, never while the observer runs.
func (n *notifier) emit(f func(Observer)) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	if len(n.pending) >= maxPendingEvents {
		copy(n.pending, n.pending[1:])
		n.pending[len(n.pending)-1] = nil
		n.pending = n.pending[:len(n.pending)-1]
	}
	n.pending = append(n.pending, f)
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
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
				next = n.pending[0]
				n.pending[0] = nil
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
