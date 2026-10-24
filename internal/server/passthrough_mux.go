package server

// passthrough_mux.go — TLS passthrough on the HTTPS port (H11,
// BURROW_TLS_PASSTHROUGH_ON_HTTPS). SplitPassthrough wraps the listener of
// the HTTPS server (the dashboard, /svc/, which terminates TLS itself) and
// peeks at the TLS ClientHello of each connection:
//
//   - its ALPN offers acme-tls/1 (a TLS-ALPN-01 challenge): terminated;
//   - its server name is one of the relay's own (the auth domain or a name
//     under it, the services domain, an ACME name, localhost, an address):
//     terminated;
//   - a custom domain covers the name (Options.CustomDomainCovers):
//     terminated;
//   - the name reaches a live tcp tunnel in mode open or ip
//     (passthroughTunnel, the names of the dedicated listener): the
//     connection goes raw to that service through admitVisitor and
//     bridgeVisitor, exactly as on the dedicated listener;
//   - anything else: terminated.
//
// Unlike the dedicated listener, a hello without a name, a name nobody has,
// a first byte that is no TLS handshake and a hello that cannot be read
// (split over records, too long, malformed) are not closed: the listener's
// Accept hands them to the TLS server with the bytes read put back. Only a
// read that times out, a full budget and a PROXY header that breaks the
// rules close a connection (without a byte, counted under muxRefusalKey
// with the reasons of the dedicated listener).
//
// A PROXY protocol v2 header is read as on the dedicated listener: required
// from Options.TrustedProxies (BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES),
// refused from anyone else. The visitor it names is the RemoteAddr of the
// connection, passed through or terminated.
//
// Each connection is peeked on a goroutine of its own, so the accept loop
// never waits; the waits have a budget of their own (muxPeek), and the
// timeout of the dedicated listener.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/proxyproto"
	"github.com/ankoehn/burrow/internal/tcpsniff"
)

// muxRefusalKey is the service key of the mux's refusals (the reasons are
// the PassthroughRefused… ones).
const muxRefusalKey = "https-mux"

// The budget of the mux's hello waits: defaultMuxPeekMax from untrusted
// peers, at most defaultMuxPeekPerPeer of them from one source, and
// defaultMuxPeekTrusted from the trusted proxies. Every HTTPS client of the
// relay passes the mux, so it has more room than the dedicated listener.
const (
	defaultMuxPeekMax     = 4096
	defaultMuxPeekPerPeer = 64
	defaultMuxPeekTrusted = 1024
)

// acmeTLSALPN is the ALPN protocol of the TLS-ALPN-01 challenge (RFC 8737).
const acmeTLSALPN = "acme-tls/1"

// customDomainTimeout bounds one CustomDomainCovers call of the mux.
const customDomainTimeout = 2 * time.Second

// SplitPassthrough returns a listener over ln for the HTTPS server: its
// Accept yields the connections the relay terminates itself (serve it with
// http.Server.ServeTLS, which makes each a *tls.Conn), while those whose
// ClientHello names a tcp service go to that service raw. It stops when ctx
// ends or when it is closed; closing it closes ln and every connection that
// still waits for its hello. It marks the relay as reaching tcp services by
// name (Options.TLSPassthrough).
func (s *Server) SplitPassthrough(ctx context.Context, ln net.Listener) net.Listener {
	s.passthroughOn.Store(true)
	m := &muxListener{
		s: s, ln: ln,
		conns:   make(chan net.Conn),
		done:    make(chan struct{}),
		pending: map[net.Conn]struct{}{},
	}
	m.wg.Add(1)
	go m.acceptLoop()
	// Registered for the life of ctx; a Close before is idempotent.
	context.AfterFunc(ctx, func() { _ = m.Close() })
	return m
}

// muxListener is the listener of SplitPassthrough.
type muxListener struct {
	s     *Server
	ln    net.Listener
	conns chan net.Conn // to terminate, for Accept
	done  chan struct{} // closed by stop

	once sync.Once
	mu   sync.Mutex
	err  error // why the inner listener stopped (Accept returns it)
	shut bool  // under mu
	// pending are the connections that wait for their hello (under mu);
	// shutting closes them.
	pending map[net.Conn]struct{}
	// wg counts the accept loop and the goroutines that have a connection
	// of the listener's (peeking, or handing it to Accept).
	wg sync.WaitGroup
}

// Accept returns the next connection to terminate.
func (m *muxListener) Accept() (net.Conn, error) {
	select {
	case c := <-m.conns:
		return c, nil
	case <-m.done:
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.err != nil {
			return nil, m.err
		}
		return nil, net.ErrClosed
	}
}

// Close stops the listener: it closes ln and every connection that waits
// for its hello or for Accept, and returns when their goroutines have ended.
// Connections already handed to a tcp service are theirs.
func (m *muxListener) Close() error {
	err := m.stop(nil)
	m.wg.Wait()
	return err
}

// Addr is ln's address.
func (m *muxListener) Addr() net.Addr { return m.ln.Addr() }

// stop shuts the listener once (cause, when not nil, is what Accept then
// returns) without waiting for its goroutines.
func (m *muxListener) stop(cause error) error {
	var err error
	m.once.Do(func() {
		m.mu.Lock()
		m.shut = true
		m.err = cause
		for c := range m.pending {
			_ = c.Close()
		}
		m.pending = nil
		m.mu.Unlock()
		close(m.done)
		err = m.ln.Close()
	})
	return err
}

// track adds conn to the waiting connections, or reports that the listener
// is shut.
func (m *muxListener) track(conn net.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shut {
		return false
	}
	m.pending[conn] = struct{}{}
	return true
}

// untrack ends a wait of track.
func (m *muxListener) untrack(conn net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, conn)
}

// acceptLoop accepts on ln and starts the peek of each connection; it never
// reads itself.
func (m *muxListener) acceptLoop() {
	defer m.wg.Done()
	s := m.s
	var delay time.Duration
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				_ = m.stop(err)
				return
			}
			s.log.Warn("https mux accept", "err", err)
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			select {
			case <-time.After(delay):
			case <-m.done:
				return
			}
			continue
		}
		delay = 0
		peer := visitorAddr(conn)
		trusted := s.trustedProxy(peer)
		timeout, ok := s.muxPeek.start(peer, trusted, s.helloTimeout, s.helloTimeoutBusy)
		if !ok {
			s.refuseSNI(conn, peer, muxRefusalKey, PassthroughRefusedBusy)
			continue
		}
		if !m.track(conn) {
			s.muxPeek.end(peer, trusted)
			_ = conn.Close()
			return
		}
		m.wg.Add(1)
		go m.handle(conn, peer, trusted, timeout)
	}
}

// handle peeks at one connection and passes it on: to its tcp service, to
// Accept, or closed.
func (m *muxListener) handle(conn net.Conn, peer netip.Addr, trusted bool, timeout time.Duration) {
	s := m.s
	visitor, tun, reason := s.readMux(conn, trusted, timeout)
	s.muxPeek.end(peer, trusted)
	m.untrack(conn)
	switch {
	case reason != "":
		m.wg.Done()
		ip := peer
		if visitor != nil {
			ip = visitorAddr(visitor)
		}
		s.refuseSNI(conn, ip, muxRefusalKey, reason)
	case tun != nil:
		m.wg.Done()
		select {
		case <-m.done:
			_ = conn.Close()
			return
		default:
		}
		source, ok := s.admitVisitor(tun, visitor)
		if !ok {
			return
		}
		s.bridgeVisitor(tun, visitor, source)
	default:
		defer m.wg.Done()
		select {
		case m.conns <- visitor:
		case <-m.done:
			_ = conn.Close()
		}
	}
}

// readMux is the bounded read of the mux. It returns the connection with
// the bytes read put back (and, behind a trusted proxy, the addresses its
// header names) and the tunnel it goes to raw, nil to terminate it; or the
// reason it is closed (visitor is set when the PROXY header was read, for
// the address to count). The read deadline is cleared on a hand-over.
func (s *Server) readMux(conn net.Conn, trusted bool, timeout time.Duration) (net.Conn, *Tunnel, string) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	br := bufio.NewReaderSize(conn, proxyproto.MaxHeader+tcpsniff.MaxClientHello)
	isTimeout := func(err error) bool {
		var ne net.Error
		return errors.As(err, &ne) && ne.Timeout()
	}
	visitor := conn
	if trusted {
		// As on the dedicated listener: a trusted proxy names every visitor.
		first, err := br.Peek(1)
		switch {
		case err != nil && isTimeout(err):
			return nil, nil, PassthroughRefusedTimeout
		case err != nil || !proxyproto.MayBeHeader(first):
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
	}
	// handOver puts back what was read, for the TLS server or the service.
	handOver := func(tun *Tunnel) (net.Conn, *Tunnel, string) {
		_ = conn.SetReadDeadline(time.Time{})
		rest := make([]byte, br.Buffered())
		_, _ = io.ReadFull(br, rest)
		return &replayConn{Conn: visitor, r: io.MultiReader(bytes.NewReader(rest), conn)}, tun, ""
	}
	// failed: a read that timed out closes; any other error (the peer
	// closed or reset) is the TLS server's to see.
	failed := func(err error) (net.Conn, *Tunnel, string) {
		if isTimeout(err) {
			return visitor, nil, PassthroughRefusedTimeout
		}
		return handOver(nil)
	}
	first, err := br.Peek(1)
	if err != nil {
		return failed(err)
	}
	if !trusted && proxyproto.MayBeHeader(first) {
		// Read the whole signature (within the deadline): a header from
		// anyone but a trusted proxy is closed; bytes that only start like
		// one are the TLS server's.
		b, err := br.Peek(len(proxyproto.Signature))
		if err == nil && proxyproto.MayBeHeader(b) {
			return nil, nil, PassthroughRefusedProxyUntrusted
		}
		if err != nil {
			return failed(err)
		}
	}
	if first[0] != 0x16 {
		return handOver(nil)
	}
	hdr, err := br.Peek(5)
	if err != nil {
		return failed(err)
	}
	n := 5 + (int(hdr[3])<<8 | int(hdr[4]))
	if n > tcpsniff.MaxClientHello {
		return handOver(nil)
	}
	rec, err := br.Peek(n)
	if err != nil {
		return failed(err)
	}
	hello, err := tcpsniff.ParseClientHello(rec)
	if err != nil {
		return handOver(nil)
	}
	return handOver(s.muxRoute(hello))
}

// muxRoute is the tcp tunnel a ClientHello goes to raw, nil to terminate it
// (see the order at the top of this file).
func (s *Server) muxRoute(h tcpsniff.Hello) *Tunnel {
	if h.Offers(acmeTLSALPN) || h.ServerName == "" {
		return nil
	}
	name := h.ServerName
	if s.relayOwnsName(name) {
		if s.passthroughTunnel(name) != nil {
			s.warnShadowed(name, "relay")
		}
		return nil
	}
	tun := s.passthroughTunnel(name)
	if tun == nil {
		return nil
	}
	if covers := s.opts.CustomDomainCovers; covers != nil {
		ctx, cancel := context.WithTimeout(context.Background(), customDomainTimeout)
		ok, err := covers(ctx, name)
		cancel()
		if err != nil {
			s.log.Warn("https mux: custom domain lookup failed; the relay terminates the name", "name", name, "err", err)
			return nil
		}
		if ok {
			s.warnShadowed(name, "custom_domain")
			return nil
		}
	}
	return tun
}

// relayOwnsName reports whether name (normalised) is one the relay serves
// itself on the HTTPS port: the auth domain or a name under it, the
// services domain, an ACME name, localhost or an address.
func (s *Server) relayOwnsName(name string) bool {
	if name == "localhost" {
		return true
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}
	norm := func(d string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".") }
	auth := norm(s.opts.AuthDomain)
	if auth != "" && (name == auth || strings.HasSuffix(name, "."+auth)) {
		return true
	}
	if d := norm(s.opts.ServicesDomain); d != "" && name == d {
		return true
	}
	for _, d := range s.opts.CertNames {
		if norm(d) == name {
			return true
		}
	}
	return false
}

// warnShadowed logs once per name that a tcp service's passthrough name is
// served by the relay's HTTPS instead (by: relay, custom_domain).
func (s *Server) warnShadowed(name, by string) {
	if _, seen := s.shadowWarned.LoadOrStore(name, struct{}{}); seen {
		return
	}
	s.log.Warn("tls passthrough name is terminated on the https port: the http side owns it",
		"name", name, "shadowed_by", by)
}
