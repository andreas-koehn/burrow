package server

// TLS passthrough by SNI (tcp complete, E1): the dedicated listener reads the
// ClientHello's server name without terminating TLS and hands the connection,
// with the bytes it read, to the tcp service of that name, through the same
// accept checks as the service's port.

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxyproto"
)

const testTCPDomain = "tcp.example.com"

// passRelay is an accessRelay (its client c echoes raw bytes) with a TLS
// passthrough listener at addr.
type passRelay struct {
	*accessRelay
	addr string
}

// newPassRelay starts the relay with BURROW_TCP_DOMAIN set; tweak changes the
// server before the passthrough listener runs.
func newPassRelay(t *testing.T, min, max int, mut func(*Options), tweak func(*Server)) *passRelay {
	t.Helper()
	a := newAccessRelay(t, min, max, func(o *Options) {
		o.TCPDomain = testTCPDomain
		if mut != nil {
			mut(o)
		}
	})
	if tweak != nil {
		tweak(a.srv)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.srv.ServePassthrough(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &passRelay{accessRelay: a, addr: ln.Addr().String()}
}

// testCert is a self-signed certificate for any name.
func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "echo"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"*.example.com", "*.tcp.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tlsHome is a home client whose target terminates TLS: it greets every
// connection with "<name>\n" and then echoes it. told counts the
// connections it was asked to open.
func (r *passRelay) tlsHome(t *testing.T, name string, told *atomic.Int32) *handClient {
	t.Helper()
	cfg := &tls.Config{Certificates: []tls.Certificate{testCert(t)}, MinVersion: tls.VersionTLS12}
	return r.client(func(st *yamux.Stream) {
		told.Add(1)
		tc := tls.Server(st, cfg)
		defer tc.Close()
		if tc.Handshake() != nil {
			return
		}
		_, _ = tc.Write([]byte(name + "\n"))
		_, _ = io.Copy(tc, tc)
	})
}

// tlsVisit is a real TLS client through the passthrough listener with the
// server name sni: it reads the target's greeting and an echo of "ping".
func tlsVisit(addr, sni string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	tc := tls.Client(conn, &tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // the test target's certificate is self-signed
	br := bufio.NewReader(tc)
	greeting, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	if _, err := tc.Write([]byte("ping")); err != nil {
		return "", err
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "ping" {
		return "", errors.New("no echo")
	}
	return strings.TrimSuffix(greeting, "\n"), nil
}

// clientHello is the first flight of a Go TLS client that names sni ("" for
// none).
func clientHello(t *testing.T, sni string) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		_ = tls.Client(a, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake() //nolint:gosec // only the hello is wanted
	}()
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(b, hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[3:5]))
	if _, err := io.ReadFull(b, body); err != nil {
		t.Fatal(err)
	}
	return append(hdr, body...)
}

// proxyV2 is a PROXY protocol v2 header from src to dst.
func proxyV2(src, dst string) []byte {
	s, d := netip.MustParseAddrPort(src), netip.MustParseAddrPort(dst)
	b := append([]byte{}, proxyproto.Signature[:]...)
	b = append(b, 0x21, 0x11, 0, 12)
	sa, da := s.Addr().As4(), d.Addr().As4()
	b = append(append(b, sa[:]...), da[:]...)
	b = binary.BigEndian.AppendUint16(b, s.Port())
	return binary.BigEndian.AppendUint16(b, d.Port())
}

// rawVisit writes b and reads back as many bytes from the echo, within d.
func rawVisit(addr string, b []byte, d time.Duration) ([]byte, error) {
	return rawVisitN(addr, b, len(b), d)
}

// rawVisitN writes b and reads back n bytes, within d.
func rawVisitN(addr string, b []byte, n int, d time.Duration) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(d))
	if _, err := conn.Write(b); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	got, err := io.ReadFull(conn, buf)
	return buf[:got], err
}

// closedWithoutAByte reports whether the relay closed conn without writing
// to it, within d (a timeout is not a close).
func closedWithoutAByte(conn net.Conn, d time.Duration) error {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	var b [1]byte
	n, err := conn.Read(b[:])
	switch {
	case n > 0:
		return errors.New("the relay wrote a byte")
	case err == nil:
		return errors.New("no close")
	case errors.Is(err, os.ErrDeadlineExceeded):
		return errors.New("still open")
	}
	return nil
}

// count is how many refusals of (service, reason) the current minute saw.
func (r *refusalSampler) count(service, reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, c := range r.seen {
		if k.service == service && k.reason == reason {
			n += c
		}
	}
	return n
}

// A real TLS client reaches the service its server name names, the slug
// name under BURROW_TCP_DOMAIN or the service's own host name, case and a
// trailing dot aside. TLS runs end to end: the relay terminates nothing. The
// row of the connection says tls and the name.
func TestPassthrough_RoutesByServerName(t *testing.T) {
	r := newPassRelay(t, 19570, 19572, nil, nil)
	r.res.mu.Lock()
	r.res.hosts["svc-web"] = "rdp.example.com"
	r.res.mu.Unlock()
	var toldWeb, toldDB atomic.Int32
	web, db := r.tlsHome(t, "web", &toldWeb), r.tlsHome(t, "db", &toldDB)
	if rr := web.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register web: %+v", rr)
	}
	if rr := db.register(t, proto.TunnelRegister{Name: "db", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK || rr.Slug != "s-db" {
		t.Fatalf("register db: %+v", rr)
	}
	for sni, want := range map[string]string{
		"rdp.example.com":       "web",
		"s-web.tcp.example.com": "web",
		"s-db.tcp.example.com":  "db",
		"S-DB.TCP.Example.COM":  "db",
	} {
		got, err := tlsVisit(r.addr, sni)
		if err != nil || got != want {
			t.Fatalf("%s: %q %v, want %q", sni, got, err, want)
		}
	}
	if toldWeb.Load() != 2 || toldDB.Load() != 2 {
		t.Fatalf("connections web %d db %d", toldWeb.Load(), toldDB.Load())
	}
	row := r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.SNI == "rdp.example.com" })
	if row.ServiceID != "svc-web" || row.Protocol != "tls" || row.SourceIP != "127.0.0.1" || row.Status != observe.OutcomeClean {
		t.Fatalf("row %+v", row)
	}
	// The port of the service still works as it did.
	if got, err := tlsVisit("127.0.0.1:19570", "anything.example.com"); err != nil || got != "web" {
		t.Fatalf("port: %q %v", got, err)
	}
}

// The bytes the relay read to find the name reach the target unchanged, and
// so does everything after them.
func TestPassthrough_ReplaysWhatItRead(t *testing.T) {
	r := newPassRelay(t, 19573, 19573, nil, nil)
	if rr := r.c.register(t, proto.TunnelRegister{Name: "raw", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	hello := clientHello(t, "s-raw.tcp.example.com")
	msg := append(append([]byte{}, hello...), "and more"...)
	got, err := rawVisit(r.addr, msg, 5*time.Second)
	if err != nil || string(got) != string(msg) {
		t.Fatalf("echo %d of %d bytes: %v", len(got), len(msg), err)
	}
}

// What has no service of its name is closed without a byte, before the home
// client is told: a name nobody has, a hello without a name, bytes that are
// no TLS, and a private service, by either of its names. Each is counted.
func TestPassthrough_RefusesWithoutAByte(t *testing.T) {
	r := newPassRelay(t, 19574, 19575, nil, nil)
	r.res.mu.Lock()
	r.res.hosts["svc-vault"] = "vault.example.com"
	r.res.mu.Unlock()
	if rr := r.c.register(t, proto.TunnelRegister{Name: "open", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if rr := r.c.register(t, proto.TunnelRegister{Name: "vault", Type: "tcp", LocalAddr: "x", Access: "private"}); !rr.OK || rr.RemotePort != 0 {
		t.Fatalf("register private: %+v", rr)
	}
	for name, first := range map[string][]byte{
		"unknown name":       clientHello(t, "nobody.example.com"),
		"no name":            clientHello(t, ""),
		"not tls":            []byte("GET / HTTP/1.1\r\nHost: s-open.tcp.example.com\r\n\r\n"),
		"one byte, not tls":  {'x'},
		"private by name":    clientHello(t, "vault.example.com"),
		"private by slug":    clientHello(t, "s-vault.tcp.example.com"),
		"a name under none":  clientHello(t, "s-open.other.example.com"),
		"the domain itself":  clientHello(t, testTCPDomain),
		"a deeper slug name": clientHello(t, "x.s-open.tcp.example.com"),
	} {
		conn, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write(first)
		if err := closedWithoutAByte(conn, 3*time.Second); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		_ = conn.Close()
	}
	settle()
	if n := r.told.Load(); n != 0 {
		t.Fatalf("the home client was told of %d connections", n)
	}
	for _, reason := range []string{PassthroughRefusedUnknownName, PassthroughRefusedNoName, PassthroughRefusedNotTLS} {
		if n := r.srv.refusals.count(passthroughRefusalKey, reason); n == 0 {
			t.Errorf("no %s refusal counted", reason)
		}
	}
	// A host name under the tcp domain (stored before the domain was set)
	// is no name: those are the slugs'.
	r.access.set("svc-open", TCPAccess{Mode: TCPAccessOpen, Hostname: "s-vault.tcp.example.com"})
	if err := r.srv.RefreshTCPService(context.Background(), "svc-open"); err != nil {
		t.Fatal(err)
	}
	if _, err := rawVisit(r.addr, clientHello(t, "s-vault.tcp.example.com"), 3*time.Second); err == nil {
		t.Fatal("a host name under the tcp domain reached a service")
	}
	// The name of the open service works.
	if got, err := rawVisit(r.addr, clientHello(t, "s-open.tcp.example.com"), 5*time.Second); err != nil || len(got) == 0 {
		t.Fatalf("open: %v", err)
	}
}

// Mode ip judges a passthrough visitor as it judges one of the port: the
// same policy, the same rejected row, and a change applies at once.
func TestPassthrough_IPModeAllowsAndDenies(t *testing.T) {
	r := newPassRelay(t, 19576, 19576, nil, nil)
	r.access.set("svc-guard", TCPAccess{Mode: TCPAccessIP, Hostname: "guard.example.com"})
	r.res.mu.Lock()
	r.res.hosts["svc-guard"] = "guard.example.com"
	r.res.mu.Unlock()
	if rr := r.c.register(t, proto.TunnelRegister{Name: "guard", Type: "tcp", LocalAddr: "x", Access: "ip"}); !rr.OK || rr.AccessMode != "ip" {
		t.Fatalf("register: %+v", rr)
	}
	hello := clientHello(t, "guard.example.com")
	if _, err := rawVisit(r.addr, hello, 3*time.Second); err == nil {
		t.Fatal("denied visitor got through")
	}
	r.waitFor("a rejected row", func() bool { return len(r.refusedRows(observe.RejectedIPGeo)) == 1 })
	if row := r.refusedRows(observe.RejectedIPGeo)[0]; row.ServiceID != "svc-guard" || row.SourceIP != "127.0.0.1" {
		t.Fatalf("row %+v", row)
	}
	if r.told.Load() != 0 {
		t.Fatal("the home client was told of a denied visitor")
	}
	r.access.set("svc-guard", TCPAccess{Mode: TCPAccessIP, Hostname: "guard.example.com", AllowCIDRs: []string{"127.0.0.1"}})
	if err := r.srv.RefreshTCPService(context.Background(), "svc-guard"); err != nil {
		t.Fatal(err)
	}
	if got, err := rawVisit(r.addr, hello, 5*time.Second); err != nil || string(got) != string(hello) {
		t.Fatalf("allowed visitor: %v", err)
	}
}

// A new host name applies without a new registration; the old one is gone
// at once.
func TestPassthrough_ANewHostNameApplies(t *testing.T) {
	r := newPassRelay(t, 19577, 19577, func(o *Options) { o.TCPDomain = "" }, nil)
	r.res.mu.Lock()
	r.res.hosts["svc-app"] = "old.example.com"
	r.res.mu.Unlock()
	if rr := r.c.register(t, proto.TunnelRegister{Name: "app", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if _, err := rawVisit(r.addr, clientHello(t, "old.example.com"), 5*time.Second); err != nil {
		t.Fatalf("old name: %v", err)
	}
	// Without BURROW_TCP_DOMAIN the slug has no name.
	if _, err := rawVisit(r.addr, clientHello(t, "s-app.tcp.example.com"), 3*time.Second); err == nil {
		t.Fatal("a slug name without a tcp domain")
	}
	r.access.set("svc-app", TCPAccess{Mode: TCPAccessOpen, Hostname: "new.example.com"})
	if err := r.srv.RefreshTCPService(context.Background(), "svc-app"); err != nil {
		t.Fatal(err)
	}
	if _, err := rawVisit(r.addr, clientHello(t, "old.example.com"), 3*time.Second); err == nil {
		t.Fatal("the old name still works")
	}
	if _, err := rawVisit(r.addr, clientHello(t, "new.example.com"), 5*time.Second); err != nil {
		t.Fatalf("new name: %v", err)
	}
}

// A PROXY protocol v2 header is believed from a trusted proxy only: the
// visitor it names is the one the policy judges, the home client is told
// of and the row keeps. A trusted proxy that sends none (or a LOCAL one) is
// the visitor itself; anyone else who sends one is closed.
func TestPassthrough_ProxyHeaderFromTrustedProxiesOnly(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	r := newPassRelay(t, 19578, 19578, func(o *Options) { o.TrustedProxies = trusted }, func(s *Server) { s.helloTimeout = time.Second })
	r.access.set("svc-guard", TCPAccess{Mode: TCPAccessIP, AllowCIDRs: []string{"198.51.100.0/24"}})
	if rr := r.c.register(t, proto.TunnelRegister{Name: "guard", Type: "tcp", LocalAddr: "x", Access: "ip"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	hello := clientHello(t, "s-guard.tcp.example.com")
	header := proxyV2("198.51.100.7:4242", "10.0.0.2:443")
	got, err := rawVisitN(r.addr, append(append([]byte{}, header...), hello...), len(hello), 5*time.Second)
	if err != nil || string(got) != string(hello) {
		t.Fatalf("through the proxy: %v", err)
	}
	if nc := r.c.notice(t); nc.SourceIP != "198.51.100.7:4242" || nc.DestAddr != "10.0.0.2:443" {
		t.Fatalf("new_connection %+v", nc)
	}
	row := r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.Status == observe.OutcomeClean })
	if row.SourceIP != "198.51.100.7" || row.SNI != "s-guard.tcp.example.com" {
		t.Fatalf("row %+v", row)
	}
	// No header: the proxy itself, which the policy does not allow.
	if _, err := rawVisit(r.addr, hello, 3*time.Second); err == nil {
		t.Fatal("the proxy's own address got through")
	}
	r.waitFor("a rejected row of the proxy", func() bool {
		rows := r.refusedRows(observe.RejectedIPGeo)
		return len(rows) == 1 && rows[0].SourceIP == "127.0.0.1"
	})
	local := append(append([]byte{}, proxyproto.Signature[:]...), 0x20, 0x00, 0, 0)
	if _, err := rawVisit(r.addr, append(local, hello...), 3*time.Second); err == nil {
		t.Fatal("a LOCAL header got through as someone else")
	}
	// A trusted proxy that stops in its header is closed when the read
	// times out.
	stalled, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stalled.Write(proxyproto.Signature[:6])
	start := time.Now()
	if err := closedWithoutAByte(stalled, 3*time.Second); err != nil || time.Since(start) < 800*time.Millisecond {
		t.Fatalf("a stalled header: %v after %v", err, time.Since(start))
	}
	_ = stalled.Close()

	// A relay that does not trust this peer.
	u := newPassRelay(t, 19579, 19579, func(o *Options) {
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	}, nil)
	if rr := u.c.register(t, proto.TunnelRegister{Name: "open", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	conn, err := net.Dial("tcp", u.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write(append(proxyV2("198.51.100.7:4242", "10.0.0.2:443"), clientHello(t, "s-open.tcp.example.com")...))
	if err := closedWithoutAByte(conn, 3*time.Second); err != nil {
		t.Fatalf("untrusted header: %v", err)
	}
	if n := u.srv.refusals.count(passthroughRefusalKey, PassthroughRefusedProxyUntrusted); n != 1 {
		t.Fatalf("%d untrusted refusals", n)
	}
	// Without a header the same peer is served.
	if _, err := rawVisit(u.addr, clientHello(t, "s-open.tcp.example.com"), 5*time.Second); err != nil {
		t.Fatalf("no header: %v", err)
	}
	if u.told.Load() != 1 {
		t.Fatalf("told %d", u.told.Load())
	}
}

// The ClientHello read is bounded: a visitor that sends nothing, or a part
// of a hello, is closed when the read times out; a record longer than 16 KiB
// is closed at once; and only so many connections wait for their hello at a
// time.
func TestPassthrough_TheHelloReadIsBounded(t *testing.T) {
	const timeout = time.Second
	r := newPassRelay(t, 19580, 19580, nil, func(s *Server) {
		s.helloTimeout = timeout
		s.peekMax, s.peekPerPeer = 3, 2
	})
	if rr := r.c.register(t, proto.TunnelRegister{Name: "open", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	timed := func(name string, first []byte, atLeast, atMost time.Duration) {
		t.Helper()
		conn, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		start := time.Now()
		if len(first) > 0 {
			_, _ = conn.Write(first)
		}
		if err := closedWithoutAByte(conn, atMost+time.Second); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if took := time.Since(start); took < atLeast || took > atMost {
			t.Fatalf("%s: closed after %v, want %v..%v", name, took, atLeast, atMost)
		}
	}
	hello := clientHello(t, "s-open.tcp.example.com")
	timed("nothing", nil, timeout-100*time.Millisecond, timeout+time.Second)
	timed("half a hello", hello[:len(hello)/2], timeout-100*time.Millisecond, timeout+time.Second)
	timed("a record of 16 KiB and one byte", []byte{0x16, 0x03, 0x01, 0x40, 0x01, 0x01}, 0, timeout/2)
	if n := r.srv.refusals.count(passthroughRefusalKey, PassthroughRefusedTimeout); n == 0 {
		t.Fatal("no timeout counted")
	}

	// Two silent connections from this peer wait; a third one is closed at
	// once, and so is one beyond the relay's total.
	var held []net.Conn
	for range 2 {
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	time.Sleep(100 * time.Millisecond)
	timed("a third from one peer", nil, 0, timeout/2)
	if n := r.srv.refusals.count(passthroughRefusalKey, PassthroughRefusedBusy); n == 0 {
		t.Fatal("no busy refusal counted")
	}
	for _, c := range held {
		_ = c.Close()
	}
	r.waitFor("the waits end", func() bool { return r.srv.peeking() == 0 })
	if got, err := rawVisit(r.addr, hello, 5*time.Second); err != nil || string(got) != string(hello) {
		t.Fatalf("after the waits: %v", err)
	}
}
