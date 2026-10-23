package server

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// Observer is told what happens to control sessions and to the traffic the
// control server carries itself (tcp tunnels). It is called on the goroutine
// of the session or the connection and must return at once; see package
// observe. cmd/server implements it once, for metrics, webhook events and
// audit entries.
type Observer interface {
	// ObserveSession receives a session's opening and closing and the
	// registration, removal and refusal of its tunnels.
	ObserveSession(observe.Session)
	// ObserveTraffic receives each visitor connection of a tcp tunnel when
	// it ends, with the tunnel's service.
	ObserveTraffic(observe.Traffic)
}

type noopObserver struct{}

func (noopObserver) ObserveSession(observe.Session) {}
func (noopObserver) ObserveTraffic(observe.Traffic) {}

// SetObserver registers o for this server's sessions and tcp traffic. Call it
// before Serve; nil means nobody is told.
func (s *Server) SetObserver(o Observer) { s.reg.SetObserver(o) }

// sessionEvent is the part every Session event of cs shares.
func sessionEvent(kind string, cs *ClientSession) observe.Session {
	return observe.Session{
		Kind: kind, SessionID: cs.SessionID, UserID: cs.UserID, TokenID: cs.TokenID,
		ConnectedAt: cs.ConnectedAt,
	}
}

// tunnelEvent describes t, a tunnel of cs.
func tunnelEvent(kind string, cs *ClientSession, t *Tunnel) observe.Session {
	ev := sessionEvent(kind, cs)
	ev.TunnelID, ev.TunnelName, ev.ServiceID = t.ID, t.Name, t.ServiceID
	ev.TunnelType = tunnelType(t.Type)
	ev.AccessMode, ev.ServiceCreated = t.accessMode, t.created
	return ev
}

// tunnelType is the type of a tunnel as an event names it: the client sends
// "" for tcp, and may send anything.
func tunnelType(typ string) string {
	switch typ {
	case "http":
		return "http"
	case "", "tcp":
		return "tcp"
	}
	return "other"
}

// refused tells the observer of reg that a registration of cs was refused
// with code.
func (s *Server) refused(reg *Registry, cs *ClientSession, tr proto.TunnelRegister, code string) {
	ev := sessionEvent(observe.TunnelRefused, cs)
	ev.TunnelName, ev.TunnelType, ev.RefusalCode = tr.Name, tunnelType(tr.Type), code
	reg.observer().ObserveSession(ev)
}

// countingStream adds what passes through a tunnel stream to the tunnel's
// byte counters: what is written to the client is BytesIn (visitor→local),
// what is read from it BytesOut. One atomic add per call, nothing else.
type countingStream struct {
	net.Conn
	in, out *atomic.Uint64
}

func (c *countingStream) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.out.Add(uint64(n))
	}
	return n, err
}

func (c *countingStream) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.in.Add(uint64(n))
	}
	return n, err
}

// tcpTraffic reports one visitor connection of a tcp tunnel that has ended:
// to the observer, and as a row of the connection log.
func (s *Server) tcpTraffic(tun *Tunnel, visitor net.Conn, started time.Time, in, out uint64, outcome, reason string) {
	ip, _, err := net.SplitHostPort(visitor.RemoteAddr().String())
	if err != nil {
		ip = visitor.RemoteAddr().String()
	}
	ended := time.Now()
	s.reg.observer().ObserveTraffic(observe.Traffic{
		Kind: observe.TrafficTCP, ServiceID: tun.ServiceID, TunnelID: tun.ID,
		UserID: tun.sess.UserID, SessionID: tun.sess.SessionID,
		Outcome: outcome, SourceIP: ip,
		BytesIn: int64(in), BytesOut: int64(out),
		Started: started, Duration: ended.Sub(started),
	})
	if s.opts.ConnLog != nil {
		s.opts.ConnLog.RecordConnection(context.Background(), ConnLogEntry{
			Kind: observe.TrafficTCP, ServiceID: tun.ServiceID, TunnelID: tun.ID,
			UserID: tun.sess.UserID, ClientSessionID: tun.sess.SessionID, SourceIP: ip,
			StartedAt: started, EndedAt: ended, BytesIn: int64(in), BytesOut: int64(out),
			Status: outcome, Reason: reason,
		})
	}
}
