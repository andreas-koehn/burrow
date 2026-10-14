package server

import (
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
}

func newPortAllocator(min, max int) *portAllocator {
	return &portAllocator{min: min, max: max, used: make(map[int]bool)}
}

// Allocate reserves requested (or, if 0, the first free port in range).
func (a *portAllocator) Allocate(requested int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if requested != 0 {
		if requested < a.min || requested > a.max {
			return 0, fmt.Errorf("port %d out of range %d-%d", requested, a.min, a.max)
		}
		if a.used[requested] {
			return 0, fmt.Errorf("port %d already in use", requested)
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
	return 0, errors.New("no free ports in range")
}

// Release returns a port to the pool.
func (a *portAllocator) Release(port int) {
	a.mu.Lock()
	delete(a.used, port)
	a.mu.Unlock()
}

// startPublicListener binds tun.RemotePort and bridges each visitor.
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
			go s.bridgeVisitor(tun, visitor)
		}
	}()
	return nil
}

func (s *Server) bridgeVisitor(tun *Tunnel, visitor net.Conn) {
	defer visitor.Close()
	started := time.Now()
	streamID := uuid.NewString()
	stream, err := tun.sess.Pending().Await(streamID, 10*time.Second, func() error {
		return tun.sess.SendControl(proto.MsgNewConnection, proto.NewConnection{
			TunnelID: tun.ID, StreamID: streamID, SourceIP: visitor.RemoteAddr().String(),
		})
	})
	if err != nil {
		s.log.Warn("client did not open stream", "tunnel_id", tun.ID, "err", err)
		s.tcpTraffic(tun, visitor, started, 0, 0, observe.OutcomeError)
		return
	}
	defer stream.Close()
	// The connection's own totals, added to the tunnel's when it ends: the
	// copy itself counts nothing (see bridge.Pipe).
	var in, out atomic.Uint64
	bridge.Pipe(visitor, stream, &in, &out)
	tun.BytesIn.Add(in.Load())
	tun.BytesOut.Add(out.Load())
	s.tcpTraffic(tun, visitor, started, in.Load(), out.Load(), observe.OutcomeClean)
}

// handleDataStream reads a freshly-accepted client stream's StreamHeader and
// hands it to the waiting visitor; closes it if nobody is waiting.
func (s *Server) handleDataStream(cs *ClientSession, st *yamux.Stream) {
	var env proto.Envelope
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := proto.ReadFrame(st, &env); err != nil || env.Type != proto.MsgStreamOpen {
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
