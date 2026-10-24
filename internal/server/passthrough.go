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
//     one untrusted IPv4 address or IPv6 /48) waiting for their hello at
//     once, peekTrustedMax more from the trusted proxies; a budget over three
//     quarters full shortens the read to helloTimeoutBusy (2 s).
//
// A proxy in front of the listener (Traefik's TCP router with
// tls.passthrough) would hide every visitor behind its own address, which
// the ip policy and the per-source limits then judge. Such a proxy must send
// a PROXY protocol v2 header first (Traefik: proxyProtocol version 2 on the
// HostSNI router's service). Options.TrustedProxies
// (BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES, not BURROW_TRUSTED_PROXIES) are
// those proxies: a connection from one without a header naming a visitor
// (none, or LOCAL) is closed, and anyone else who starts with a header is
// closed. With no trusted proxies no header is believed, and the TCP peer is
// the visitor.

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
	PassthroughRefusedProxyMissing   = "proxy_missing"   // a trusted proxy sent no PROXY header naming a visitor
)

// passthroughRefusalKey is the service key of the refusals above.
const passthroughRefusalKey = "tls-passthrough"

// The bounds of the hello read. Untrusted peers share defaultPeekMax waits,
// at most defaultPeekPerPeer from one source (an IPv4 address or an IPv6
// /48); the trusted proxies have a budget of their own, defaultPeekTrusted,
// so that a flood from the open internet cannot starve the proxy's visitors
// and the other way round. When a budget is more than three quarters full,
// a new connection gets defaultHelloTimeoutBusy instead of
// defaultHelloTimeout.
const (
	defaultHelloTimeout     = 5 * time.Second
	defaultHelloTimeoutBusy = 2 * time.Second
	defaultPeekMax          = 1024
	defaultPeekPerPeer      = 32
	defaultPeekTrusted      = 1024
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
	s.passthroughOn.Store(true)
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
		timeout, ok := s.startPeek(peer, trusted)
		if !ok {
			s.refusePassthrough(conn, peer, PassthroughRefusedBusy)
			continue
		}
		go s.handlePassthrough(conn, peer, trusted, timeout)
	}
}

// trustedProxy reports whether a is a proxy in front of the listener
// (Options.TrustedProxies): it must send a PROXY protocol header, and only it
// may.
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

// peekKey is what the per-peer budget counts an untrusted peer by: its
// IPv4 address, or the /48 of its IPv6 address (one site usually has one).
func peekKey(a netip.Addr) string {
	if a.Is6() && !a.Is4In6() {
		p, _ := a.Prefix(48)
		return p.String()
	}
	return a.Unmap().String()
}

// startPeek counts a connection that waits for its hello and returns how
// long it may take, or reports that there is no room. A trusted proxy counts
// against peekTrustedMax only; anyone else against peekMax and peekPerPeer.
// The wait is helloTimeoutBusy once the budget is over three quarters full.
func (s *Server) startPeek(peer netip.Addr, trusted bool) (time.Duration, bool) {
	s.peekMu.Lock()
	defer s.peekMu.Unlock()
	busy := func(n, max int) time.Duration {
		if 4*n > 3*max {
			return min(s.helloTimeoutBusy, s.helloTimeout)
		}
		return s.helloTimeout
	}
	if trusted {
		if s.peekTrusted >= s.peekTrustedMax {
			return 0, false
		}
		s.peekTrusted++
		return busy(s.peekTrusted, s.peekTrustedMax), true
	}
	if s.peekTotal >= s.peekMax {
		return 0, false
	}
	key := peekKey(peer)
	if s.peekBy[key] >= s.peekPerPeer {
		return 0, false
	}
	if s.peekBy == nil {
		s.peekBy = map[string]int{}
	}
	s.peekTotal++
	s.peekBy[key]++
	return busy(s.peekTotal, s.peekMax), true
}

// endPeek ends a wait of startPeek.
func (s *Server) endPeek(peer netip.Addr, trusted bool) {
	s.peekMu.Lock()
	defer s.peekMu.Unlock()
	if trusted {
		s.peekTrusted--
		return
	}
	s.peekTotal--
	key := peekKey(peer)
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
	return s.peekTotal + s.peekTrusted
}

// handlePassthrough reads what comes before the TLS hello (a PROXY header of
// a trusted proxy) and the hello, finds the service of its name and hands the
// connection to the port listener's path.
func (s *Server) handlePassthrough(conn net.Conn, peer netip.Addr, trusted bool, timeout time.Duration) {
	visitor, tun, reason := s.readPassthrough(conn, trusted, timeout)
	s.endPeek(peer, trusted)
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
func (s *Server) readPassthrough(conn net.Conn, trusted bool, timeout time.Duration) (net.Conn, *Tunnel, string) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
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
	switch {
	case trusted:
		// A trusted proxy names every visitor: one that sends no header, or
		// a LOCAL one, would be let in as the proxy's own address, which ip
		// rules and limits cannot tell from all the others.
		if !proxyproto.MayBeHeader(first) {
			return nil, nil, PassthroughRefusedProxyMissing
		}
		h, n, perr := s.readProxyHeader(br)
		if perr != "" {
			return nil, nil, perr
		}
		if h.Local {
			return nil, nil, PassthroughRefusedProxyMissing
		}
		visitor = &proxiedConn{Conn: conn,
			remote: net.TCPAddrFromAddrPort(h.Source), local: net.TCPAddrFromAddrPort(h.Dest)}
		_, _ = br.Discard(n)
	case proxyproto.MayBeHeader(first):
		// Read the whole signature (within the deadline) to tell a header
		// from bytes that only start like one; either way it is closed.
		if b, err := br.Peek(len(proxyproto.Signature)); err == nil && proxyproto.MayBeHeader(b) {
			return nil, nil, PassthroughRefusedProxyUntrusted
		}
		return nil, nil, PassthroughRefusedNotTLS
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

// reachableByName reports whether a TLS client reaches tn by a host name on
// the passthrough listener: the listener runs and the tunnel has a name
// there (<slug>.<TCPDomain>, or its own). The mode is the caller's to check.
func (s *Server) reachableByName(tn *Tunnel) bool {
	return s.passthroughOn.Load() && ((s.opts.TCPDomain != "" && tn.slug != "") || tn.hostname() != "")
}

// refusePassthrough closes a connection of the passthrough listener without a
// byte and counts it; the first refusal of a reason and address in a minute
// is logged at debug, the minute's totals at info (refusalSampler).
func (s *Server) refusePassthrough(conn net.Conn, ip netip.Addr, reason string) {
	// Counted before the close, so that whoever sees the close sees the count.
	defer conn.Close()
	addr := ""
	if ip.IsValid() {
		addr = ip.String()
	}
	if s.passRefusals.sample(passthroughRefusalKey, reason, addr, time.Now()) {
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
