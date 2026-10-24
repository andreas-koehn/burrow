package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
)

// v2 builds a header by hand: version/command, family, and the address
// block as given (its length is written from it).
func v2(verCmd, fam byte, addrs []byte) []byte {
	b := append([]byte{}, Signature[:]...)
	b = append(b, verCmd, fam)
	b = binary.BigEndian.AppendUint16(b, uint16(len(addrs)))
	return append(b, addrs...)
}

func tcp4(src, dst string, sp, dp uint16) []byte {
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	b := append(append([]byte{}, s[:]...), d[:]...)
	b = binary.BigEndian.AppendUint16(b, sp)
	return binary.BigEndian.AppendUint16(b, dp)
}

func tcp6(src, dst string, sp, dp uint16) []byte {
	s, d := netip.MustParseAddr(src).As16(), netip.MustParseAddr(dst).As16()
	b := append(append([]byte{}, s[:]...), d[:]...)
	b = binary.BigEndian.AppendUint16(b, sp)
	return binary.BigEndian.AppendUint16(b, dp)
}

func TestParse(t *testing.T) {
	hello := []byte{0x16, 0x03, 0x01}
	withTLV := append(tcp4("198.51.100.7", "10.0.0.2", 50000, 8444), 0x04, 0x00, 0x01, 0xff) // a NOOP TLV
	cases := []struct {
		name    string
		in      []byte
		want    Header
		n       int
		wantErr error
	}{
		{"tcp4", append(v2(0x21, 0x11, tcp4("198.51.100.7", "10.0.0.2", 50000, 8444)), hello...),
			Header{Source: netip.MustParseAddrPort("198.51.100.7:50000"), Dest: netip.MustParseAddrPort("10.0.0.2:8444")}, 28, nil},
		{"tcp6", v2(0x21, 0x21, tcp6("2001:db8::7", "2001:db8::1", 1, 2)),
			Header{Source: netip.MustParseAddrPort("[2001:db8::7]:1"), Dest: netip.MustParseAddrPort("[2001:db8::1]:2")}, 52, nil},
		{"4-in-6 is ipv4", v2(0x21, 0x21, tcp6("::ffff:198.51.100.7", "::ffff:10.0.0.2", 1, 2)),
			Header{Source: netip.MustParseAddrPort("198.51.100.7:1"), Dest: netip.MustParseAddrPort("10.0.0.2:2")}, 52, nil},
		{"tlvs are skipped", v2(0x21, 0x11, withTLV),
			Header{Source: netip.MustParseAddrPort("198.51.100.7:50000"), Dest: netip.MustParseAddrPort("10.0.0.2:8444")}, 16 + len(withTLV), nil},
		{"local", append(v2(0x20, 0x00, nil), hello...), Header{Local: true}, 16, nil},
		{"local with addresses", v2(0x20, 0x11, tcp4("1.2.3.4", "5.6.7.8", 1, 2)), Header{Local: true}, 28, nil},
		{"proxy unspec is local", v2(0x21, 0x00, nil), Header{Local: true}, 16, nil},
		{"tls", []byte{0x16, 0x03, 0x01, 0x00, 0x10}, Header{}, 0, ErrNotProxy},
		{"empty", nil, Header{}, 0, ErrShort},
		{"signature prefix", Signature[:5], Header{}, 0, ErrShort},
		{"no address block yet", v2(0x21, 0x11, tcp4("1.2.3.4", "5.6.7.8", 1, 2))[:20], Header{}, 0, ErrShort},
		{"v1 text", []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1 2\r\n"), Header{}, 0, ErrNotProxy},
		{"version 1 binary", v2(0x11, 0x11, tcp4("1.2.3.4", "5.6.7.8", 1, 2)), Header{}, 0, ErrInvalid},
		{"unknown command", v2(0x22, 0x11, tcp4("1.2.3.4", "5.6.7.8", 1, 2)), Header{}, 0, ErrInvalid},
		{"udp", v2(0x21, 0x12, tcp4("1.2.3.4", "5.6.7.8", 1, 2)), Header{}, 0, ErrInvalid},
		{"unix", v2(0x21, 0x31, make([]byte, 216)), Header{}, 0, ErrInvalid},
		{"tcp4 too short", v2(0x21, 0x11, make([]byte, 11)), Header{}, 0, ErrInvalid},
		{"tcp6 too short", v2(0x21, 0x21, make([]byte, 35)), Header{}, 0, ErrInvalid},
		{"too long", v2(0x21, 0x11, make([]byte, MaxHeader-15)), Header{}, 0, ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n, err := Parse(c.in)
			if !errors.Is(err, c.wantErr) || (err == nil && (got != c.want || n != c.n)) {
				t.Fatalf("Parse = %+v, %d, %v; want %+v, %d, %v", got, n, err, c.want, c.n, c.wantErr)
			}
		})
	}
}

// The longest header that is read is MaxHeader bytes.
func TestParse_Bound(t *testing.T) {
	in := v2(0x21, 0x11, append(tcp4("1.2.3.4", "5.6.7.8", 1, 2), make([]byte, MaxHeader-16-12)...))
	if _, n, err := Parse(in); err != nil || n != MaxHeader {
		t.Fatalf("MaxHeader bytes: %d %v", n, err)
	}
}

// Every prefix of a valid header is ErrShort, never a header and never
// ErrInvalid.
func TestParse_Prefixes(t *testing.T) {
	full := v2(0x21, 0x21, tcp6("2001:db8::7", "2001:db8::1", 1, 2))
	for i := 0; i < len(full); i++ {
		if _, _, err := Parse(full[:i]); !errors.Is(err, ErrShort) {
			t.Fatalf("prefix %d: %v", i, err)
		}
	}
}

func TestHasSignature(t *testing.T) {
	if !MayBeHeader(Signature[:1]) || !MayBeHeader(Signature[:]) || MayBeHeader([]byte{0x16}) || MayBeHeader(nil) {
		t.Fatal("MayBeHeader")
	}
}

func FuzzParse(f *testing.F) {
	f.Add(v2(0x21, 0x11, tcp4("198.51.100.7", "10.0.0.2", 50000, 8444)))
	f.Add(v2(0x21, 0x21, tcp6("2001:db8::7", "2001:db8::1", 1, 2)))
	f.Add(v2(0x20, 0x00, nil))
	f.Fuzz(func(t *testing.T, b []byte) {
		h, n, err := Parse(b)
		if err != nil {
			if n != 0 || h != (Header{}) {
				t.Fatalf("an error with a header: %+v %d", h, n)
			}
			return
		}
		if n < 16 || n > MaxHeader || n > len(b) || !bytes.Equal(b[:12], Signature[:]) {
			t.Fatalf("n=%d of %d", n, len(b))
		}
		if !h.Local && (!h.Source.IsValid() || !h.Dest.IsValid() || h.Source.Addr().Is4In6()) {
			t.Fatalf("header %+v", h)
		}
	})
}
