package server

// Per-service tcp settings (tcp complete, E2–E4): the PROXY protocol header
// the home client is asked to write, the protocol and server name of each
// connection in its log row, and the idle timeout.

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/tcpsniff"
)

// notice waits for the next new_connection the home client was sent.
func (c *handClient) notice(t *testing.T) proto.NewConnection {
	t.Helper()
	select {
	case nc := <-c.notices:
		return nc
	case <-time.After(5 * time.Second):
		t.Fatal("no new_connection")
	}
	return proto.NewConnection{}
}

// The service's PROXY protocol setting travels in every new_connection with
// the address the visitor reached; a change applies to the next visitor
// without a new registration. A forward names the forwarder and the relay's
// control address.
func TestTCPSettings_NewConnectionCarriesProxyProtocol(t *testing.T) {
	r := newForwardRelay(t, 19530, nil)
	r.res.mu.Lock()
	r.res.proxy["svc-web"] = proto.ProxyProtocolV2
	r.res.mu.Unlock()
	rr := r.home.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "127.0.0.1:8443", Access: "open"})
	if !rr.OK || rr.RemotePort != 19530 {
		t.Fatalf("register: %+v", rr)
	}
	conn, err := net.Dial("tcp", "127.0.0.1:19530")
	if err != nil {
		t.Fatal(err)
	}
	nc := r.home.notice(t)
	if nc.ProxyProtocol != proto.ProxyProtocolV2 || nc.DestAddr != "127.0.0.1:19530" || nc.SourceIP != conn.LocalAddr().String() {
		t.Fatalf("new_connection %+v (visitor %s)", nc, conn.LocalAddr())
	}
	_ = conn.Close()

	// Turned off in the dashboard.
	r.access.set("svc-web", TCPAccess{Mode: TCPAccessOpen})
	if err := r.srv.RefreshTCPService(context.Background(), "svc-web"); err != nil {
		t.Fatal(err)
	}
	conn, err = net.Dial("tcp", "127.0.0.1:19530")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if nc := r.home.notice(t); nc.ProxyProtocol != "" || nc.DestAddr != "127.0.0.1:19530" {
		t.Fatalf("after the change: %+v", nc)
	}

	// Back on, and a forward.
	r.access.set("svc-web", TCPAccess{Mode: TCPAccessOpen, ProxyProtocol: proto.ProxyProtocolV2})
	if err := r.srv.RefreshTCPService(context.Background(), "svc-web"); err != nil {
		t.Fatal(err)
	}
	f := r.forwarder("secret")
	if _, resp, err := f.open(t, "web"); err != nil || !resp.OK {
		t.Fatalf("forward: %+v %v", resp, err)
	}
	nc = r.home.notice(t)
	if nc.ProxyProtocol != proto.ProxyProtocolV2 || nc.SourceIP != "forward:"+f.conn.LocalAddr().String() || nc.DestAddr != f.conn.RemoteAddr().String() {
		t.Fatalf("forward new_connection %+v (forwarder %s → %s)", nc, f.conn.LocalAddr(), f.conn.RemoteAddr())
	}
}

// What the client says it writes by itself is kept on the live tunnel, for
// the dashboard; anything but v2 is nothing. It changes no new_connection.
func TestTCPSettings_TheClientsOwnProxyProtocolIsShown(t *testing.T) {
	a := newAccessRelay(t, 19531, 19532, nil)
	if got := a.srv.ClientProxyProtocol("svc-web"); got != "" {
		t.Fatalf("offline: %q", got)
	}
	if rr := a.c.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open", ProxyProtocol: "v2"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if rr := a.c.register(t, proto.TunnelRegister{Name: "odd", Type: "tcp", LocalAddr: "x", Access: "open", ProxyProtocol: "v1"}); !rr.OK {
		t.Fatalf("register odd: %+v", rr)
	}
	if got := a.srv.ClientProxyProtocol("svc-web"); got != proto.ProxyProtocolV2 {
		t.Fatalf("web: %q", got)
	}
	if got := a.srv.ClientProxyProtocol("svc-odd"); got != "" {
		t.Fatalf("odd: %q", got)
	}
	conn, err := net.Dial("tcp", "127.0.0.1:19531")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if nc := a.c.notice(t); nc.ProxyProtocol != "" {
		t.Fatalf("new_connection asks for the header: %+v", nc)
	}
}

// rowFor waits for the connection-log row of kind that matches.
func (a *accessRelay) rowFor(t *testing.T, kind string, match func(ConnLogEntry) bool) ConnLogEntry {
	t.Helper()
	var got ConnLogEntry
	a.waitFor("the connection-log row", func() bool {
		for _, e := range a.rows(kind) {
			if match(e) {
				got = e
				return true
			}
		}
		return false
	})
	return got
}

// The first bytes of a visitor name its protocol, and a TLS ClientHello its
// server name, in the connection-log row and on the live connection. Nothing
// waits for them: a visitor that sends nothing still reaches the home client
// at once, and is unknown.
func TestTCPSniff_ProtocolAndServerNameAreLogged(t *testing.T) {
	r := newForwardRelay(t, 19533, nil)
	if rr := r.home.register(t, proto.TunnelRegister{Name: "any", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	addr := "127.0.0.1:19533"

	// TLS: the home client echoes, so the handshake fails; the row has the
	// name the client asked for.
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(raw, &tls.Config{ServerName: "RDP.example.com", InsecureSkipVerify: true}) //nolint:gosec // never completes
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
	_ = tc.Handshake()
	_ = raw.Close()
	tlsRow := r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.Protocol == tcpsniff.ProtoTLS })
	if tlsRow.SNI != "rdp.example.com" {
		t.Fatalf("tls row %+v", tlsRow)
	}

	// SSH, and the live connection while it is open.
	ssh, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssh.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, ssh, 3); got != "hi " {
		t.Fatalf("home said %q", got)
	}
	readN(t, ssh, len("SSH-2.0-OpenSSH_9.6\r\n"))
	live := r.srv.ServiceConnections("svc-any")
	if len(live) != 1 || live[0].Protocol != tcpsniff.ProtoSSH || live[0].SNI != "" {
		t.Fatalf("live: %+v", live)
	}
	_ = ssh.Close()
	r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.Protocol == tcpsniff.ProtoSSH })

	// Nothing sent: the home client is told at once, and the row says
	// unknown.
	quiet, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if got := readN(t, quiet, 3); got != "hi " || time.Since(start) > 2*time.Second {
		t.Fatalf("home said %q after %v", got, time.Since(start))
	}
	_ = quiet.Close()
	r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.Protocol == tcpsniff.ProtoUnknown && e.SNI == "" })

	// A forward is looked at the same way.
	f := r.forwarder("secret")
	st, resp, err := f.open(t, "any")
	if err != nil || !resp.OK {
		t.Fatalf("forward: %+v %v", resp, err)
	}
	if _, err := st.Write([]byte("GET / HTTP/1.1\r\nHost: any\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	readN(t, st, 3)
	_ = st.Close()
	r.rowFor(t, TrafficTCPForward, func(e ConnLogEntry) bool { return e.Protocol == tcpsniff.ProtoHTTP })
}

// closedWithin reports whether the peer closed conn within d (a read gives
// EOF or a reset, not a timeout).
func closedWithin(conn net.Conn, d time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	return true
}

// A connection with no byte in either direction for the service's idle
// timeout is closed with reason idle_timeout; one that keeps talking stays,
// and a timeout set while connections are open applies to them.
func TestTCPIdle_QuietConnectionsAreClosed(t *testing.T) {
	r := newForwardRelay(t, 19534, nil)
	r.srv.idleTick = 20 * time.Millisecond
	if rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	quiet, err := net.Dial("tcp", "127.0.0.1:19534")
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	readN(t, quiet, 3)
	// No timeout yet.
	if closedWithin(quiet, 400*time.Millisecond) {
		t.Fatal("closed without an idle timeout")
	}

	r.access.set("svc-rdp", TCPAccess{Mode: TCPAccessOpen, IdleTimeout: 300 * time.Millisecond})
	if err := r.srv.RefreshTCPService(context.Background(), "svc-rdp"); err != nil {
		t.Fatal(err)
	}
	talker, err := net.Dial("tcp", "127.0.0.1:19534")
	if err != nil {
		t.Fatal(err)
	}
	defer talker.Close()
	readN(t, talker, 3)
	// The talker sends a byte every 100 ms for 900 ms and is served all
	// along.
	talked := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		for i := 0; i < 9; i++ {
			if _, err := talker.Write([]byte("x")); err != nil {
				talked <- fmt.Errorf("write %d: %w", i, err)
				return
			}
			_ = talker.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := io.ReadFull(talker, buf); err != nil {
				talked <- fmt.Errorf("read %d: %w", i, err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		talked <- nil
	}()
	if !closedWithin(quiet, 2*time.Second) {
		t.Fatal("the quiet connection is still open")
	}
	r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.Reason == ConnLogReasonIdleTimeout })
	if err := <-talked; err != nil {
		t.Fatalf("the talker: %v", err)
	}
	if n := len(r.srv.ServiceConnections("svc-rdp")); n != 1 {
		t.Fatalf("live connections: %d", n)
	}

	// A forward that goes quiet is closed too.
	f := r.forwarder("secret")
	st, resp, err := f.open(t, "rdp")
	if err != nil || !resp.OK {
		t.Fatalf("forward: %+v %v", resp, err)
	}
	readN(t, st, 3)
	_ = st.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.Copy(io.Discard, st); err != nil && strings.Contains(err.Error(), "timeout") {
		t.Fatalf("the quiet forward is still open: %v", err)
	}
	r.rowFor(t, TrafficTCPForward, func(e ConnLogEntry) bool { return e.Reason == ConnLogReasonIdleTimeout })
}

// A service that asks for a PROXY v2 header is served only by a client that
// says it writes one (proto.CapProxyProtocolV2): otherwise a visitor is
// closed before a byte and before the home client hears of it, a forward is
// refused, and both leave a proxy_unsupported row. The relay tells whether
// the serving client can.
func TestTCPSettings_ProxyProtocolNeedsTheClientsCapability(t *testing.T) {
	r := newForwardRelayCaps(t, 19535, nil, nil)
	r.res.mu.Lock()
	r.res.proxy["svc-web"] = proto.ProxyProtocolV2
	r.res.mu.Unlock()
	if supported, online := r.srv.ClientProxySupported("svc-web"); supported || online {
		t.Fatalf("offline: %v %v", supported, online)
	}
	if rr := r.home.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if supported, online := r.srv.ClientProxySupported("svc-web"); supported || !online {
		t.Fatalf("an old client: %v %v", supported, online)
	}
	if got, err := visit(t, 19535, 2*time.Second); err == nil || len(got) != 0 {
		t.Fatalf("visitor got %q %v", got, err)
	}
	f := r.forwarder("secret")
	if _, resp, err := f.open(t, "web"); err != nil || resp.OK || resp.Code != proto.CodeOffline || !strings.Contains(resp.Error, "PROXY") {
		t.Fatalf("forward: %+v %v", resp, err)
	}
	r.waitFor("two proxy_unsupported rows", func() bool {
		return len(r.refusedRows(TCPRefusedProxyUnsupported)) == 2
	})
	if n := r.told.Load(); n != 0 {
		t.Fatalf("the home client was told of %d connections", n)
	}
	select {
	case nc := <-r.home.notices:
		t.Fatalf("new_connection sent: %+v", nc)
	default:
	}

	// Without the setting the same client serves.
	r.access.set("svc-web", TCPAccess{Mode: TCPAccessOpen})
	if err := r.srv.RefreshTCPService(context.Background(), "svc-web"); err != nil {
		t.Fatal(err)
	}
	if got, err := visit(t, 19535, 2*time.Second); err != nil || string(got) != "hi he" {
		t.Fatalf("without the setting: %q %v", got, err)
	}
}

// A client that announces the capability is told it can.
func TestTCPSettings_TheCapabilityIsKept(t *testing.T) {
	r := newForwardRelay(t, 19536, nil)
	if rr := r.home.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if supported, online := r.srv.ClientProxySupported("svc-web"); !supported || !online {
		t.Fatalf("%v %v", supported, online)
	}
}

// Bytes in one direction only keep a connection alive. The timeout is ten
// times the write interval, so a loaded machine (the whole suite under -race)
// that stretches a sleep or the pairing does not look like silence; the read
// still spans three timeouts, so a sweeper counting one direction fails it.
func TestTCPIdle_OneWayTrafficKeepsAConnection(t *testing.T) {
	res, access, log := newTCPResolver(), newFakeTCPAccess(), &recConnLog{}
	res.idle["svc-feed"] = time.Second
	r := tcpRelay(t, 19537, 19537, res, func(o *Options) { o.TCPAccess, o.ConnLog = access, log })
	r.srv.idleTick = 20 * time.Millisecond
	home := r.client(func(st *yamux.Stream) {
		for i := 0; i < 40; i++ {
			if _, err := st.Write([]byte("t")); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	if rr := home.register(t, proto.TunnelRegister{Name: "feed", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	conn, err := net.Dial("tcp", "127.0.0.1:19537")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	buf := make([]byte, 30) // 3 s of writes, three times the timeout
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("the feed was cut: %v", err)
	}
}
