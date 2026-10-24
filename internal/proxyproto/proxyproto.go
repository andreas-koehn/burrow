// Package proxyproto reads the PROXY protocol v2 header a proxy in front of
// the relay (Traefik's TCP router, HAProxy) writes before the bytes of the
// connection it passes on, so that the relay sees the visitor's address and
// not the proxy's (tcp complete, E1).
//
// Only the binary version 2 is read. Nothing here reads from a connection:
// the caller hands over the first bytes it peeked, and no more than
// MaxHeader bytes are looked at. Whether a header is believed at all is the
// caller's decision: only a peer it trusts may send one.
package proxyproto

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// Signature starts every PROXY protocol v2 header.
var Signature = [12]byte{'\r', '\n', '\r', '\n', 0x00, '\r', '\n', 'Q', 'U', 'I', 'T', '\n'}

// MaxHeader is the longest header Parse reads: the 16 fixed bytes and an
// address block of at most 512 bytes (an IPv6 pair is 36, the rest are
// TLVs, which are skipped). A longer one is ErrInvalid.
const MaxHeader = 16 + 512

// Parse's errors.
var (
	// ErrShort: the bytes end before the header does.
	ErrShort = errors.New("proxyproto: the header is not complete")
	// ErrNotProxy: the bytes do not start with the v2 signature.
	ErrNotProxy = errors.New("proxyproto: no PROXY protocol v2 header")
	// ErrInvalid: a v2 header that is not version 2, has an unknown
	// command, is longer than MaxHeader, or carries no TCP addresses.
	ErrInvalid = errors.New("proxyproto: invalid PROXY protocol v2 header")
)

// Header is what a v2 header says. Local is a connection of the proxy
// itself (a health check, or a source it does not know): the connection's
// own addresses apply. Otherwise Source is the visitor and Dest the address
// it connected to, IPv4 unmapped.
type Header struct {
	Local        bool
	Source, Dest netip.AddrPort
}

// MayBeHeader reports whether b, the first bytes of a connection, are a
// prefix of the v2 signature (so a header may follow). An empty b is not.
func MayBeHeader(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	n := min(len(b), len(Signature))
	return string(b[:n]) == string(Signature[:n])
}

// Parse reads the v2 header at the start of b and returns it with its
// length in bytes; the connection's own bytes follow it.
func Parse(b []byte) (Header, int, error) {
	if len(b) < 16 {
		if len(b) == 0 || MayBeHeader(b) {
			return Header{}, 0, ErrShort
		}
		return Header{}, 0, ErrNotProxy
	}
	if !MayBeHeader(b[:12]) {
		return Header{}, 0, ErrNotProxy
	}
	verCmd, fam := b[12], b[13]
	l := int(binary.BigEndian.Uint16(b[14:16]))
	if verCmd>>4 != 2 || 16+l > MaxHeader {
		return Header{}, 0, ErrInvalid
	}
	cmd := verCmd & 0x0f
	if cmd > 1 {
		return Header{}, 0, ErrInvalid
	}
	if len(b) < 16+l {
		return Header{}, 0, ErrShort
	}
	n := 16 + l
	if cmd == 0 || fam == 0x00 { // LOCAL, or PROXY with UNSPEC
		return Header{Local: true}, n, nil
	}
	a := b[16:n]
	switch fam {
	case 0x11: // TCP over IPv4
		if l < 12 {
			return Header{}, 0, ErrInvalid
		}
		src := netip.AddrFrom4([4]byte(a[0:4]))
		dst := netip.AddrFrom4([4]byte(a[4:8]))
		return Header{
			Source: netip.AddrPortFrom(src, binary.BigEndian.Uint16(a[8:10])),
			Dest:   netip.AddrPortFrom(dst, binary.BigEndian.Uint16(a[10:12])),
		}, n, nil
	case 0x21: // TCP over IPv6
		if l < 36 {
			return Header{}, 0, ErrInvalid
		}
		src := netip.AddrFrom16([16]byte(a[0:16])).Unmap()
		dst := netip.AddrFrom16([16]byte(a[16:32])).Unmap()
		return Header{
			Source: netip.AddrPortFrom(src, binary.BigEndian.Uint16(a[32:34])),
			Dest:   netip.AddrPortFrom(dst, binary.BigEndian.Uint16(a[34:36])),
		}, n, nil
	}
	// UDP or unix sockets: nothing the relay could carry.
	return Header{}, 0, ErrInvalid
}
