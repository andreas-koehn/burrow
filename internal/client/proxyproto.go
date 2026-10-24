package client

// proxyproto.go — the PROXY protocol v2 header the home client writes to its
// local target (tcp complete, E2), so that the service sees the address of
// the visitor or the forwarder instead of the client's own.

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
)

// ProxyProtocolV2 is the PROXY protocol a tcp tunnel may write
// (TunnelSpec.ProxyProtocol).
const ProxyProtocolV2 = proto.ProxyProtocolV2

// proxyHeaderTimeout bounds the write of the header to the local target.
const proxyHeaderTimeout = 5 * time.Second

// proxyV2Sig is the signature every PROXY v2 header starts with.
var proxyV2Sig = [12]byte{'\r', '\n', '\r', '\n', 0x00, '\r', '\n', 'Q', 'U', 'I', 'T', '\n'}

// tcpProxyProtocol is the PROXY protocol the client writes by itself for a
// tunnel: its own setting, for a tcp tunnel only.
func tcpProxyProtocol(tn TunnelSpec) string {
	if tn.Type != "tcp" {
		return ""
	}
	return tn.ProxyProtocol
}

// proxyV2Header is the PROXY v2 header of a connection from src to dst, both
// host:port as new_connection names them (src may carry the "forward:"
// prefix of a forward stream). An IPv4 address written as IPv6 is IPv4; when
// the two families differ both are written as IPv6. Without a destination
// (an older relay) it is the unspecified address of the source's family,
// port 0. A source that cannot be read gives a LOCAL header, which tells the
// target to use the connection's own addresses.
func proxyV2Header(src, dst string) []byte {
	s, err := netip.ParseAddrPort(strings.TrimPrefix(src, "forward:"))
	if err != nil {
		return proxyV2Local()
	}
	s = netip.AddrPortFrom(s.Addr().Unmap().WithZone(""), s.Port())
	d, err := netip.ParseAddrPort(dst)
	if err != nil {
		zero := netip.IPv4Unspecified()
		if s.Addr().Is6() {
			zero = netip.IPv6Unspecified()
		}
		d = netip.AddrPortFrom(zero, 0)
	}
	d = netip.AddrPortFrom(d.Addr().Unmap().WithZone(""), d.Port())
	b := make([]byte, 0, 16+36)
	b = append(b, proxyV2Sig[:]...)
	b = append(b, 0x21) // version 2, PROXY
	if s.Addr().Is4() && d.Addr().Is4() {
		b = append(b, 0x11, 0, 12) // TCP over IPv4, 12 bytes
		sa, da := s.Addr().As4(), d.Addr().As4()
		b = append(b, sa[:]...)
		b = append(b, da[:]...)
	} else {
		b = append(b, 0x21, 0, 36) // TCP over IPv6, 36 bytes
		sa, da := s.Addr().As16(), d.Addr().As16()
		b = append(b, sa[:]...)
		b = append(b, da[:]...)
	}
	b = binary.BigEndian.AppendUint16(b, s.Port())
	b = binary.BigEndian.AppendUint16(b, d.Port())
	return b
}

// proxyV2Local is the header of a connection whose addresses are not known.
func proxyV2Local() []byte {
	b := append([]byte{}, proxyV2Sig[:]...)
	return append(b, 0x20, 0x00, 0, 0) // version 2, LOCAL, UNSPEC, no addresses
}
