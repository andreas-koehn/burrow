package client

// PROXY protocol v2 (tcp complete, E2): the home client writes the header to
// its local target before any byte of the connection. The test target below
// parses the header as a server that speaks the protocol does (HAProxy's
// spec, section 2.2), independently of the code that writes it.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
)

// proxyHeader is a parsed PROXY v2 header.
type proxyHeader struct {
	command  string // "PROXY" or "LOCAL"
	family   string // "TCP4", "TCP6" or "UNSPEC"
	src, dst netip.AddrPort
}

var proxySig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// readProxyV2 reads one PROXY v2 header from r, as a server would.
func readProxyV2(r io.Reader) (proxyHeader, error) {
	var h proxyHeader
	fixed := make([]byte, 16)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return h, err
	}
	if !bytes.Equal(fixed[:12], proxySig) {
		return h, fmt.Errorf("no PROXY v2 signature: %q", fixed)
	}
	if fixed[12]>>4 != 2 {
		return h, fmt.Errorf("version %d", fixed[12]>>4)
	}
	switch fixed[12] & 0x0f {
	case 0:
		h.command = "LOCAL"
	case 1:
		h.command = "PROXY"
	default:
		return h, fmt.Errorf("command %d", fixed[12]&0x0f)
	}
	body := make([]byte, binary.BigEndian.Uint16(fixed[14:16]))
	if _, err := io.ReadFull(r, body); err != nil {
		return h, err
	}
	switch fixed[13] {
	case 0x00:
		h.family = "UNSPEC"
	case 0x11:
		if len(body) < 12 {
			return h, errors.New("short TCP4 addresses")
		}
		h.family = "TCP4"
		h.src = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[0:4])), binary.BigEndian.Uint16(body[8:10]))
		h.dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[4:8])), binary.BigEndian.Uint16(body[10:12]))
	case 0x21:
		if len(body) < 36 {
			return h, errors.New("short TCP6 addresses")
		}
		h.family = "TCP6"
		h.src = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[0:16])), binary.BigEndian.Uint16(body[32:34]))
		h.dst = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[16:32])), binary.BigEndian.Uint16(body[34:36]))
	default:
		return h, fmt.Errorf("family %#x", fixed[13])
	}
	return h, nil
}

func TestProxyV2Header(t *testing.T) {
	for _, c := range []struct {
		name, src, dst string
		want           proxyHeader
	}{
		{"ipv4", "203.0.113.7:50000", "198.51.100.1:9001",
			proxyHeader{"PROXY", "TCP4", netip.MustParseAddrPort("203.0.113.7:50000"), netip.MustParseAddrPort("198.51.100.1:9001")}},
		{"ipv6", "[2001:db8::7]:50000", "[2001:db8::1]:9001",
			proxyHeader{"PROXY", "TCP6", netip.MustParseAddrPort("[2001:db8::7]:50000"), netip.MustParseAddrPort("[2001:db8::1]:9001")}},
		{"a forward: the forwarder's address", "forward:192.0.2.4:4000", "198.51.100.1:7000",
			proxyHeader{"PROXY", "TCP4", netip.MustParseAddrPort("192.0.2.4:4000"), netip.MustParseAddrPort("198.51.100.1:7000")}},
		{"ipv4 written as ipv6 is ipv4", "[::ffff:203.0.113.7]:50000", "[::ffff:198.51.100.1]:9001",
			proxyHeader{"PROXY", "TCP4", netip.MustParseAddrPort("203.0.113.7:50000"), netip.MustParseAddrPort("198.51.100.1:9001")}},
		{"mixed families are ipv6", "203.0.113.7:50000", "[2001:db8::1]:9001",
			proxyHeader{"PROXY", "TCP6", netip.MustParseAddrPort("[::ffff:203.0.113.7]:50000"), netip.MustParseAddrPort("[2001:db8::1]:9001")}},
		{"an older relay sends no destination", "203.0.113.7:50000", "",
			proxyHeader{"PROXY", "TCP4", netip.MustParseAddrPort("203.0.113.7:50000"), netip.MustParseAddrPort("0.0.0.0:0")}},
		{"a source that cannot be read is LOCAL", "pipe", "198.51.100.1:9001", proxyHeader{"LOCAL", "UNSPEC", netip.AddrPort{}, netip.AddrPort{}}},
		{"a zone is dropped", "[fe80::1%eth0]:50000", "[fe80::2%eth0]:9001",
			proxyHeader{"PROXY", "TCP6", netip.MustParseAddrPort("[fe80::1]:50000"), netip.MustParseAddrPort("[fe80::2]:9001")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := proxyV2Header(c.src, c.dst)
			got, err := readProxyV2(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("parse %x: %v", b, err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// proxyTarget is a local service that expects a PROXY v2 header on every
// connection: it records the header, then echoes. A connection without one
// is recorded as an error and closed.
type proxyTarget struct {
	addr string
	mu   sync.Mutex
	hdrs []proxyHeader
	errs []error
}

func newProxyTarget(t *testing.T) *proxyTarget {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := &proxyTarget{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				h, err := readProxyV2(c)
				p.mu.Lock()
				if err != nil {
					p.errs = append(p.errs, err)
				} else {
					p.hdrs = append(p.hdrs, h)
				}
				p.mu.Unlock()
				if err != nil {
					return
				}
				_ = c.SetReadDeadline(time.Time{})
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return p
}

func (p *proxyTarget) headers() ([]proxyHeader, []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxyHeader(nil), p.hdrs...), append([]error(nil), p.errs...)
}

// echoOnce writes s to addr and reads it back.
func echoOnce(t *testing.T, addr, s string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(s))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != s {
		t.Fatalf("echo: %q %v", buf, err)
	}
	return conn
}

// The client's own setting (burrow.yaml, --proxy-protocol): a visitor of the
// public port reaches the target behind a header that names the visitor's
// address and the port it connected to, and the bytes after it are the
// visitor's. The relay is told that the client writes the header.
func TestProxyProtocol_TheClientsOwnSetting(t *testing.T) {
	r := startForwardRelay(t, true)
	target := newProxyTarget(t)
	obs := &forwardObserver{}
	o := r.options("", TunnelSpec{Name: "web", Type: "tcp", Access: "open", LocalAddr: target.addr, ProxyProtocol: proto.ProxyProtocolV2})
	o.Observer = obs
	start(t, o)
	var port int
	if !waitTrue(func() bool {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		if len(obs.regs) > 0 {
			port = obs.regs[0].RemotePort
		}
		return port != 0
	}, 5*time.Second) {
		t.Fatal("not registered")
	}
	conn := echoOnce(t, fmt.Sprintf("127.0.0.1:%d", port), "hello through the relay")
	defer conn.Close()
	hdrs, errs := target.headers()
	if len(errs) != 0 || len(hdrs) != 1 {
		t.Fatalf("headers %+v, errors %v", hdrs, errs)
	}
	want := proxyHeader{"PROXY", "TCP4", netip.MustParseAddrPort(conn.LocalAddr().String()), netip.MustParseAddrPort(conn.RemoteAddr().String())}
	if hdrs[0] != want {
		t.Fatalf("header %+v, want %+v", hdrs[0], want)
	}
	if got := r.srv.ClientProxyProtocol("svc-web"); got != proto.ProxyProtocolV2 {
		t.Fatalf("relay sees client proxy protocol %q", got)
	}
}

// The service's setting (the dashboard): the relay asks for the header in
// new_connection, and a forward's header names the forwarder's address.
// Without the setting no header is written.
func TestProxyProtocol_TheServicesSettingAndForwards(t *testing.T) {
	r := startForwardRelay(t, true)
	target := newProxyTarget(t)
	r.svcs.setProxyProtocol("rdp", proto.ProxyProtocolV2)
	start(t, r.options("", TunnelSpec{Name: "rdp", Type: "tcp", Access: "private", LocalAddr: target.addr}))
	if !waitTrue(func() bool { _, ok := r.srv.LookupTunnelByServiceID("svc-rdp"); return ok }, 5*time.Second) {
		t.Fatal("not registered")
	}
	fobs := &forwardObserver{}
	fo := r.options("")
	fo.Forwards = []ForwardSpec{{Target: "rdp", Addr: "127.0.0.1:0"}}
	fo.Observer = fobs
	start(t, fo)
	conn := echoOnce(t, fobs.addr(t), "rdp bytes")
	defer conn.Close()
	hdrs, errs := target.headers()
	if len(errs) != 0 || len(hdrs) != 1 {
		t.Fatalf("headers %+v, errors %v", hdrs, errs)
	}
	// The forwarder's address is the one of its session at the relay: here
	// loopback.
	if hdrs[0].command != "PROXY" || hdrs[0].family != "TCP4" || hdrs[0].src.Addr() != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("forward header %+v", hdrs[0])
	}
	if !strings.HasSuffix(r.srv.Addr(), fmt.Sprintf(":%d", hdrs[0].dst.Port())) {
		t.Fatalf("destination %v, relay %s", hdrs[0].dst, r.srv.Addr())
	}
	if got := r.srv.ClientProxyProtocol("svc-rdp"); got != "" {
		t.Fatalf("the client asked for nothing, relay sees %q", got)
	}
}

// Without either setting the target gets the visitor's bytes only.
func TestProxyProtocol_OffWritesNothing(t *testing.T) {
	r := startForwardRelay(t, true)
	got := make(chan []byte, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		_, _ = io.ReadFull(c, buf)
		got <- buf
		_, _ = c.Write(buf)
	}()
	start(t, r.options("", TunnelSpec{Name: "plain", Type: "tcp", Access: "private", LocalAddr: ln.Addr().String()}))
	if !waitTrue(func() bool { _, ok := r.srv.LookupTunnelByServiceID("svc-plain"); return ok }, 5*time.Second) {
		t.Fatal("not registered")
	}
	fobs := &forwardObserver{}
	fo := r.options("")
	fo.Forwards = []ForwardSpec{{Target: "plain", Addr: "127.0.0.1:0"}}
	fo.Observer = fobs
	start(t, fo)
	conn := echoOnce(t, fobs.addr(t), "hello")
	defer conn.Close()
	if b := <-got; string(b) != "hello" {
		t.Fatalf("target got %q", b)
	}
}
