package server

// passthrough.go — TLS passthrough by SNI (tcp complete, E1). An optional
// listener (BURROW_TLS_PASSTHROUGH_LISTEN) reads the server name of each
// connection's TLS ClientHello without terminating TLS and hands the
// connection to the tcp service of that name, with the bytes it read put
// back in front:
//
//   - a service's names are <slug>.<Options.TCPDomain> (when a tcp domain is
//     set) and its own host name (services.tcp_hostname), both compared in
//     tcpsniff.NormalizeHostname form;
//   - only a live tunnel in mode open or ip has names here: a private
//     service is never reached this way, whatever names it has;
//   - the connection then goes through admitVisitor and bridgeVisitor, the
//     accept checks, limits, refusal logging and connection log of the
//     service's port, unchanged;
//   - no name, a name nobody has, or a first byte that is no TLS handshake
//     is closed without a byte and counted (refusalSampler, under
//     passthroughRefusalKey);
//   - the read is bounded: helloTimeout (5 s) for the PROXY header and the
//     hello together, at most tcpsniff.MaxClientHello bytes of hello (one
//     record of 16 KiB), and at most peekMax connections (peekPerPeer from
//     one untrusted address) waiting for their hello at once.
//
// A proxy in front of the listener (Traefik's TCP router with
// tls.passthrough) would hide every visitor behind its own address, which
// the ip policy and the per-source limits then judge. Such a proxy may send
// a PROXY protocol v2 header first: it is believed from an address in
// Options.TrustedProxies only. Anyone else who starts with one is closed. A
// trusted proxy that sends none, or a LOCAL one, is the visitor itself.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/proxyproto"
	"github.com/ankoehn/burrow/internal/tcpsniff"
)

// Why a connection of the passthrough listener was closed before it reached
// a service. They are counted per minute under passthroughRefusalKey and
// summarised in the log; none writes a connection-log row, since no service
// is known. A refusal of the service itself (its ip policy, its limits) has
// the reasons of the port listener and its rows.
const (
	PassthroughRefusedNotTLS         = "not_tls"         // the first byte is no TLS handshake record
	PassthroughRefusedNoName         = "no_sni"          // a ClientHello that names no server
	PassthroughRefusedUnknownName    = "unknown_sni"     // no open or ip service has the name
	PassthroughRefusedBadHello       = "bad_hello"       // a ClientHello that cannot be read (too long, split, malformed)
	PassthroughRefusedTimeout        = "hello_timeout"   // the hello did not come within helloTimeout
	PassthroughRefusedBusy           = "busy"            // too many connections wait for their hello
	PassthroughRefusedProxyUntrusted = "proxy_untrusted" // a PROXY header from an address that is not trusted
	PassthroughRefusedProxyInvalid   = "proxy_invalid"   // a PROXY header that cannot be read
)

// passthroughRefusalKey is the service key of the refusals above.
const passthroughRefusalKey = "tls-passthrough"

// The bounds of the hello read.
const (
	defaultHelloTimeout = 5 * time.Second
	defaultPeekMax      = 1024
	defaultPeekPerPeer  = 32
)

// ListenPassthrough serves the TLS passthrough listener on addr until ctx
// ends.
func (s *Server) ListenPassthrough(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("tls passthrough listen %s: %w", addr, err)
	}
	s.log.Info("tls passthrough listening", "addr", ln.Addr().String(), "tcp_domain", s.opts.TCPDomain)
	return s.ServePassthrough(ctx, ln)
}

// ServePassthrough accepts on ln until ctx ends (it then closes ln). Each
// connection is judged on a goroutine of its own, so the accept loop never
// waits for a hello.
func (s *Server) ServePassthrough(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Warn("tls passthrough accept", "err", err)
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			time.Sleep(delay)
			continue
		}
		delay = 0
		peer := visitorAddr(conn)
		trusted := s.trustedProxy(peer)
		if !s.startPeek(peer, trusted) {
			s.refusePassthrough(conn, peer, PassthroughRefusedBusy)
			continue
		}
		go s.handlePassthrough(conn, peer, trusted)
	}
}

// trustedProxy reports whether a may send a PROXY protocol header.
func (s *Server) trustedProxy(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range s.opts.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// startPeek counts a connection that waits for its hello, or reports that
// there is no room: peekMax in all, peekPerPeer from one address unless it
// is a trusted proxy (all of whose visitors share its address).
func (s *Server) startPeek(peer netip.Addr, trusted bool) bool {
	s.peekMu.Lock()
	defer s.peekMu.Unlock()
	if s.peekTotal >= s.peekMax {
		return false
	}
	key := sourceKey(peer)
	if !trusted && s.peekBy[key] >= s.peekPerPeer {
		return false
	}
	if s.peekBy == nil {
		s.peekBy = map[string]int{}
	}
	s.peekTotal++
	s.peekBy[key]++
	return true
}

// endPeek ends a wait of startPeek.
func (s *Server) endPeek(peer netip.Addr) {
	s.peekMu.Lock()
	defer s.peekMu.Unlock()
	s.peekTotal--
	key := sourceKey(peer)
	if s.peekBy[key] <= 1 {
		delete(s.peekBy, key)
	} else {
		s.peekBy[key]--
	}
}

// peeking is how many connections wait for their hello now.
func (s *Server) peeking() int {
	s.peekMu.Lock()
	defer s.peekMu.Unlock()
	return s.peekTotal
}

// handlePassthrough reads what comes before the TLS hello (a PROXY header of
// a trusted proxy) and the hello, finds the service of its name and hands the
// connection to the port listener's path.
func (s *Server) handlePassthrough(conn net.Conn, peer netip.Addr, trusted bool) {
	visitor, tun, reason := s.readPassthrough(conn, peer, trusted)
	s.endPeek(peer)
	if tun == nil {
		ip := peer
		if visitor != nil {
			ip = visitorAddr(visitor)
		}
		s.refusePassthrough(conn, ip, reason)
		return
	}
	source, ok := s.admitVisitor(tun, visitor)
	if !ok {
		return
	}
	s.bridgeVisitor(tun, visitor, source)
}

// readPassthrough is the bounded read of handlePassthrough. It returns the
// visitor (the connection with the bytes read put back and, behind a trusted
// proxy, the addresses its header names) and the tunnel of the hello's name;
// on a refusal the tunnel is nil and reason says why (visitor is set when
// the PROXY header was read, for the address to count).
func (s *Server) readPassthrough(conn net.Conn, peer netip.Addr, trusted bool) (net.Conn, *Tunnel, string) {
	_ = conn.SetReadDeadline(time.Now().Add(s.helloTimeout))
	br := bufio.NewReaderSize(conn, proxyproto.MaxHeader+tcpsniff.MaxClientHello)
	timedOut := func(err error) string {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return PassthroughRefusedTimeout
		}
		return PassthroughRefusedBadHello
	}
	first, err := br.Peek(1)
	if err != nil {
		return nil, nil, timedOut(err)
	}
	visitor := conn
	if proxyproto.MayBeHeader(first) {
		if !trusted {
			if b, _ := br.Peek(br.Buffered()); proxyproto.MayBeHeader(b) && len(b) >= 2 {
				return nil, nil, PassthroughRefusedProxyUntrusted
			}
			return nil, nil, PassthroughRefusedNotTLS
		}
		h, n, perr := s.readProxyHeader(br)
		if perr != "" {
			return nil, nil, perr
		}
		if !h.Local {
			visitor = &proxiedConn{Conn: conn,
				remote: net.TCPAddrFromAddrPort(h.Source), local: net.TCPAddrFromAddrPort(h.Dest)}
		}
		_, _ = br.Discard(n)
	}
	name, reason := readServerName(br, timedOut)
	if reason != "" {
		return visitor, nil, reason
	}
	tun := s.passthroughTunnel(name)
	if tun == nil {
		return visitor, nil, PassthroughRefusedUnknownName
	}
	_ = conn.SetReadDeadline(time.Time{})
	// Put back what was read: the hello, and whatever came with it.
	rest := make([]byte, br.Buffered())
	_, _ = io.ReadFull(br, rest)
	return &replayConn{Conn: visitor, r: io.MultiReader(bytes.NewReader(rest), conn)}, tun, ""
}

// readProxyHeader peeks the PROXY v2 header at the start of br and returns
// it with its length, or the reason of a refusal.
func (s *Server) readProxyHeader(br *bufio.Reader) (proxyproto.Header, int, string) {
	for want := 16; ; {
		b, err := br.Peek(want)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return proxyproto.Header{}, 0, PassthroughRefusedTimeout
			}
			return proxyproto.Header{}, 0, PassthroughRefusedProxyInvalid
		}
		h, n, perr := proxyproto.Parse(b)
		switch {
		case perr == nil:
			return h, n, ""
		case errors.Is(perr, proxyproto.ErrShort) && want == 16:
			want = 16 + (int(b[14])<<8 | int(b[15]))
			if want > proxyproto.MaxHeader {
				return proxyproto.Header{}, 0, PassthroughRefusedProxyInvalid
			}
		default:
			return proxyproto.Header{}, 0, PassthroughRefusedProxyInvalid
		}
	}
}

// readServerName peeks the TLS record of the ClientHello at the start of br
// (at most tcpsniff.MaxClientHello bytes) and returns its server name, or the
// reason of a refusal.
func readServerName(br *bufio.Reader, failed func(error) string) (string, string) {
	hdr, err := br.Peek(1)
	if err != nil {
		return "", failed(err)
	}
	if hdr[0] != 0x16 {
		return "", PassthroughRefusedNotTLS
	}
	if hdr, err = br.Peek(5); err != nil {
		return "", failed(err)
	}
	if _, err := tcpsniff.ServerName(hdr); errors.Is(err, tcpsniff.ErrNotClientHello) {
		return "", PassthroughRefusedNotTLS
	}
	n := 5 + (int(hdr[3])<<8 | int(hdr[4]))
	if n > tcpsniff.MaxClientHello {
		return "", PassthroughRefusedBadHello
	}
	rec, err := br.Peek(n)
	if err != nil {
		return "", failed(err)
	}
	name, err := tcpsniff.ServerName(rec)
	switch {
	case errors.Is(err, tcpsniff.ErrNotClientHello):
		return "", PassthroughRefusedNotTLS
	case err != nil:
		return "", PassthroughRefusedBadHello
	case name == "":
		return "", PassthroughRefusedNoName
	}
	return name, ""
}

// passthroughTunnel is the live tcp tunnel in mode open or ip that has name
// (normalised), or nil. A private tunnel has no names here. The names under
// the tcp domain are the slugs' alone: a service's own host name there (one
// stored before the domain was set; the API refuses new ones) never
// matches, so that one name never names two services.
func (s *Server) passthroughTunnel(name string) *Tunnel {
	slug, underDomain := "", false
	if d := s.opts.TCPDomain; d != "" {
		label, ok := strings.CutSuffix(name, "."+d)
		underDomain = ok || name == d
		if ok && label != "" && !strings.Contains(label, ".") {
			slug = label
		}
	}
	for _, tn := range s.Tunnels() {
		if tn.IsHTTP || tn.ServiceID == "" || (tn.accessMode != TCPAccessOpen && tn.accessMode != TCPAccessIP) {
			continue
		}
		if underDomain {
			if slug != "" && tn.slug == slug {
				return tn
			}
			continue
		}
		if h := tn.hostname(); h != "" && h == name {
			return tn
		}
	}
	return nil
}

// refusePassthrough closes a connection of the passthrough listener without a
// byte and counts it; the first refusal of a reason and address in a minute
// is logged at debug, the minute's totals at info (refusalSampler).
func (s *Server) refusePassthrough(conn net.Conn, ip netip.Addr, reason string) {
	_ = conn.Close()
	addr := ""
	if ip.IsValid() {
		addr = ip.String()
	}
	if s.refusals.sample(passthroughRefusalKey, reason, addr, time.Now()) {
		s.log.Debug("tls passthrough connection refused", "reason", reason, "source_ip", addr)
	}
}

// replayConn reads the bytes the relay read first, then the connection.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// proxiedConn is a connection whose addresses are the ones a trusted
// proxy's PROXY header named.
type proxiedConn struct {
	net.Conn
	remote, local *net.TCPAddr
}

func (c *proxiedConn) RemoteAddr() net.Addr { return c.remote }
func (c *proxiedConn) LocalAddr() net.Addr  { return c.local }

// setHostname is the tunnel's own TLS passthrough host name now
// (services.tcp_hostname), "" for none; one that is no host name is none.
func (t *Tunnel) setHostname(h string) {
	n, ok := tcpsniff.NormalizeHostname(h)
	if !ok {
		n = ""
	}
	t.sniName.Store(&n)
}

// hostname is the tunnel's own TLS passthrough host name, "" for none.
func (t *Tunnel) hostname() string {
	if p := t.sniName.Load(); p != nil {
		return *p
	}
	return ""
}
