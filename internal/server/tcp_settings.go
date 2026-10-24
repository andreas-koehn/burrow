package server

// tcp_settings.go — per-service settings of a tcp tunnel that shape its
// connections rather than who may make them (tcp complete):
//
//   - E2: the PROXY protocol header. The relay only asks for it: every
//     new_connection carries the service's setting, the visitor's address
//     (or "forward:" and the forwarder's) and the address it connected to,
//     and the home client writes the header to its target.
//   - E3: the protocol and the TLS server name of each connection, from the
//     first bytes its visitor or forwarder sends. They are watched as they
//     pass (tcpsniff.Sniffer), so nothing waits for them: no byte is held
//     back, and a protocol whose server speaks first is simply unknown
//     until, and unless, its client sends something the sniffer knows.
//   - E4: the idle timeout. A connection that carried no byte in either
//     direction for that long is closed, with reason idle_timeout. One
//     sweeper per tunnel looks while the tunnel has live connections.
//
// A change of a setting applies without a new registration
// (RefreshTCPService): the PROXY protocol to the next connection, the idle
// timeout to the open ones as well.

import (
	"net"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/tcpsniff"
)

// ConnLogReasonIdleTimeout is the reason of a connection the relay closed
// because it carried no byte for its service's idle timeout.
const ConnLogReasonIdleTimeout = "idle_timeout"

// The idle timeout a tcp service may set: 0 (never) or MinTCPIdleTimeout to
// MaxTCPIdleTimeout. The relay applies any positive value it is given; the
// API and the store keep to the range.
const (
	MinTCPIdleTimeout = time.Minute
	MaxTCPIdleTimeout = 24 * time.Hour
)

// defaultIdleTick is how often a live connection is checked against its idle
// timeout: a quiet connection is closed between the timeout and the timeout
// plus one tick.
const defaultIdleTick = 5 * time.Second

// ProxyProtocolV2 is the PROXY protocol a tcp service may ask for.
const ProxyProtocolV2 = proto.ProxyProtocolV2

// tcpSettings are a tunnel's settings as the relay applies them now.
type tcpSettings struct {
	proxyProtocol string // proto.ProxyProtocolV2 or ""
	idle          time.Duration
}

// setTCPSettings applies a service's settings to its live tunnel. A PROXY
// protocol other than v2 is none, and a negative timeout never.
func (t *Tunnel) setTCPSettings(proxyProtocol string, idle time.Duration) {
	st := tcpSettings{idle: max(idle, 0)}
	if proxyProtocol == proto.ProxyProtocolV2 {
		st.proxyProtocol = proto.ProxyProtocolV2
	}
	t.settings.Store(&st)
}

// tcpSettings are the settings applied now.
func (t *Tunnel) tcpSettings() tcpSettings {
	if p := t.settings.Load(); p != nil {
		return *p
	}
	return tcpSettings{}
}

// ClientProxyProtocol is the PROXY protocol the client that serves a tcp
// service writes by its own configuration (burrow.yaml, --proxy-protocol):
// proto.ProxyProtocolV2, or "" when it said nothing or no client serves the
// service. The header is written when either this or the service's setting
// asks for it.
func (s *Server) ClientProxyProtocol(serviceID string) string {
	tn, ok := s.LookupTunnelByServiceID(serviceID)
	if !ok || tn.IsHTTP {
		return ""
	}
	return tn.clientProxy
}

// sniffConn hands every byte read from the connection to a sniffer, until it
// has seen enough.
type sniffConn struct {
	net.Conn
	sniff *tcpsniff.Sniffer
}

func (c sniffConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && !c.sniff.Done() {
		c.sniff.Observe(p[:n])
	}
	return n, err
}

// watched marks a live connection whose bytes the sniffer sees from now on
// and returns the side of the connection to read them from.
func (c *liveConn) watched(conn net.Conn) net.Conn {
	c.sniffed.Store(true)
	return sniffConn{Conn: conn, sniff: &c.sniff}
}

// protocol is what the sniffer tells of the connection: "" and "" while its
// bytes were never looked at.
func (c *liveConn) protocol() (protocol, sni string) {
	if !c.sniffed.Load() {
		return "", ""
	}
	return c.sniff.Result()
}

// TCPRefusedProxyUnsupported: the service asks for a PROXY protocol header
// and the client that serves it cannot write one (it does not announce
// proto.CapProxyProtocolV2).
const TCPRefusedProxyUnsupported = "proxy_unsupported"

// canServeProxy reports whether the tunnel's client can serve a connection
// as the service's settings ask: it writes the PROXY header when one is
// asked for.
func (t *Tunnel) canServeProxy() bool {
	return t.tcpSettings().proxyProtocol == "" || t.sess.proxyV2
}

// ClientProxySupported reports whether the client that serves a tcp service
// writes a PROXY protocol v2 header when asked (supported), and whether a
// client serves it at all (online).
func (s *Server) ClientProxySupported(serviceID string) (supported, online bool) {
	tn, ok := s.LookupTunnelByServiceID(serviceID)
	if !ok || tn.IsHTTP {
		return false, false
	}
	return tn.sess.proxyV2, true
}

// addLiveConn makes c a live connection of tn and sees that the tunnel's
// idle sweeper runs while it has any.
func (s *Server) addLiveConn(tn *Tunnel, c *liveConn) {
	c.idleSince = time.Now()
	tn.connMu.Lock()
	if tn.conns == nil {
		tn.conns = map[string]*liveConn{}
	}
	tn.conns[c.id] = c
	start := !tn.sweeping
	tn.sweeping = true
	tn.connMu.Unlock()
	if start {
		go s.sweepIdle(tn)
	}
}

// sweepIdle is the one idle watch of a tunnel: on every tick it closes, with
// reason idle_timeout, the live connections whose bytes in and out have not
// moved for the tunnel's idle timeout, which it reads again each time so
// that a change applies to open connections. It ends when the tunnel has no
// live connection left; addLiveConn starts it again.
func (s *Server) sweepIdle(tn *Tunnel) {
	t := time.NewTicker(s.idleTick)
	defer t.Stop()
	for now := range t.C {
		tn.connMu.Lock()
		if len(tn.conns) == 0 {
			tn.sweeping = false
			tn.connMu.Unlock()
			return
		}
		conns := make([]*liveConn, 0, len(tn.conns))
		for _, c := range tn.conns {
			conns = append(conns, c)
		}
		tn.connMu.Unlock()
		d := tn.tcpSettings().idle
		for _, c := range conns {
			if n := c.in.Load() + c.out.Load(); n != c.idleBytes {
				c.idleBytes, c.idleSince = n, now
				continue
			}
			if d > 0 && now.Sub(c.idleSince) >= d {
				s.log.Info("tcp connection closed: idle", "service_id", c.serviceID, "conn_id", c.id,
					"kind", c.kind, "idle", d.String())
				c.closeWith(ConnLogReasonIdleTimeout)
			}
		}
	}
}
