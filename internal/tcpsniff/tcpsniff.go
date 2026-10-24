// Package tcpsniff names the protocol of a raw TCP connection from the first
// bytes its client sends, and reads the server name (SNI) of a TLS
// ClientHello, without terminating anything. The relay uses it for the
// protocol and sni columns of the connection log (native tcp, E3).
//
// Nothing here reads from a connection: the caller hands over the bytes it
// saw. Every length in a ClientHello is checked against what holds it, and
// no more than MaxClientHello bytes are ever looked at.
package tcpsniff

import (
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
)

// The protocols Classify names.
const (
	ProtoTLS     = "tls"
	ProtoSSH     = "ssh"
	ProtoRDP     = "rdp"
	ProtoHTTP    = "http"
	ProtoUnknown = "unknown"
)

// MaxClientHello is the most bytes ServerName and the Sniffer look at: one
// TLS record (a 5-byte header and at most 2^14 bytes).
const MaxClientHello = 5 + maxRecord

const maxRecord = 1 << 14

// ServerName's errors.
var (
	// ErrShort: the bytes end before the ClientHello's record does.
	ErrShort = errors.New("tcpsniff: the TLS record is not complete")
	// ErrNotClientHello: the bytes are no TLS handshake record holding a
	// ClientHello.
	ErrNotClientHello = errors.New("tcpsniff: not a TLS ClientHello")
	// ErrMalformed: a ClientHello whose lengths do not add up, one split
	// over several records, or a server name that is no host name.
	ErrMalformed = errors.New("tcpsniff: malformed ClientHello")
)

// Classify names the protocol of b, the first bytes a client sent: tls (a
// ClientHello record), ssh (an SSH banner), rdp (an X.224 connection request
// in TPKT), http (an HTTP/1 request line or the HTTP/2 preface), or unknown.
// Too few bytes to tell are unknown.
func Classify(b []byte) string {
	if p := classify(b); p != "" {
		return p
	}
	return ProtoUnknown
}

// classify is Classify that answers "" while b is a prefix that more bytes
// could still make one of the protocols.
func classify(b []byte) string {
	undecided := false
	for _, m := range matchers {
		switch m.match(b) {
		case yes:
			return m.proto
		case maybe:
			undecided = true
		}
	}
	if undecided {
		return ""
	}
	return ProtoUnknown
}

type verdict int

const (
	no verdict = iota
	maybe
	yes
)

type matcher struct {
	proto string
	match func([]byte) verdict
}

var matchers = []matcher{
	{ProtoTLS, matchTLS},
	{ProtoSSH, prefix("SSH-")},
	{ProtoRDP, matchRDP},
	{ProtoHTTP, anyPrefix("GET ", "HEAD ", "POST ", "PUT ", "DELETE ", "OPTIONS ", "PATCH ", "CONNECT ", "TRACE ", "PRI * HTTP/2.0")},
}

func prefix(p string) func([]byte) verdict {
	return func(b []byte) verdict {
		switch {
		case len(b) >= len(p) && string(b[:len(p)]) == p:
			return yes
		case len(b) < len(p) && strings.HasPrefix(p, string(b)):
			return maybe
		}
		return no
	}
}

func anyPrefix(ps ...string) func([]byte) verdict {
	ms := make([]func([]byte) verdict, len(ps))
	for i, p := range ps {
		ms[i] = prefix(p)
	}
	return func(b []byte) verdict {
		v := no
		for _, m := range ms {
			switch m(b) {
			case yes:
				return yes
			case maybe:
				v = maybe
			}
		}
		return v
	}
}

// matchTLS: a handshake record (22) of version 3.x whose first message is a
// ClientHello (1).
func matchTLS(b []byte) verdict {
	checks := []func(byte) bool{
		func(c byte) bool { return c == 0x16 },
		func(c byte) bool { return c == 0x03 },
		func(c byte) bool { return c <= 0x04 },
		func(byte) bool { return true },
		func(byte) bool { return true },
		func(c byte) bool { return c == 0x01 },
	}
	return matchBytes(b, checks)
}

// matchRDP: a TPKT header (version 3, reserved 0, a length of at least 11)
// that carries an X.224 connection request (0xE0), what an RDP client sends
// first.
func matchRDP(b []byte) verdict {
	if len(b) >= 4 && binary.BigEndian.Uint16(b[2:4]) < 11 {
		return no
	}
	checks := []func(byte) bool{
		func(c byte) bool { return c == 0x03 },
		func(c byte) bool { return c == 0x00 },
		func(byte) bool { return true },
		func(byte) bool { return true },
		func(byte) bool { return true },
		func(c byte) bool { return c == 0xe0 },
	}
	return matchBytes(b, checks)
}

func matchBytes(b []byte, checks []func(byte) bool) verdict {
	for i, ok := range checks {
		if i >= len(b) {
			return maybe
		}
		if !ok(b[i]) {
			return no
		}
	}
	return yes
}

// ServerName reads the server name of the TLS ClientHello at the start of b:
// lower case, without a trailing dot, "" when the ClientHello names none. It
// looks at the first record only. It returns ErrShort while b ends before
// that record does, ErrNotClientHello for anything else than a ClientHello,
// and ErrMalformed for one it cannot read.
func ServerName(b []byte) (string, error) {
	exts, err := helloExtensions(b)
	if err != nil {
		return "", err
	}
	for len(exts) > 0 {
		typ, ok1 := exts.u16()
		data, ok2 := exts.vec16()
		if !ok1 || !ok2 {
			return "", ErrMalformed
		}
		if typ != extServerName {
			continue
		}
		return parseSNI(data)
	}
	return "", nil
}

// Hello is what ParseClientHello reads from a ClientHello.
type Hello struct {
	// ServerName is the SNI as ServerName returns it ("" when none).
	ServerName string
	// ALPN lists the application protocols the client offers, in order; nil
	// when it sends no ALPN extension.
	ALPN []string
}

// Offers reports whether the client offers the ALPN protocol proto.
func (h Hello) Offers(proto string) bool {
	for _, p := range h.ALPN {
		if p == proto {
			return true
		}
	}
	return false
}

// ParseClientHello reads the server name and the ALPN protocol list of the
// TLS ClientHello at the start of b. It has ServerName's bounds and errors,
// and additionally returns ErrMalformed for an ALPN extension whose lengths
// do not add up, that is empty, or that holds an empty protocol name. On an
// error the Hello is zero.
func ParseClientHello(b []byte) (Hello, error) {
	exts, err := helloExtensions(b)
	if err != nil {
		return Hello{}, err
	}
	var h Hello
	seenSNI, seenALPN := false, false
	for len(exts) > 0 {
		typ, ok1 := exts.u16()
		data, ok2 := exts.vec16()
		if !ok1 || !ok2 {
			return Hello{}, ErrMalformed
		}
		switch {
		case typ == extServerName && !seenSNI:
			seenSNI = true
			if h.ServerName, err = parseSNI(data); err != nil {
				return Hello{}, err
			}
		case typ == extALPN && !seenALPN:
			seenALPN = true
			if h.ALPN, err = parseALPN(data); err != nil {
				return Hello{}, err
			}
		}
	}
	return h, nil
}

const (
	extServerName = 0
	extALPN       = 16
)

// helloExtensions checks the record and handshake framing of the ClientHello
// at the start of b and returns its extensions block (empty when it has
// none).
func helloExtensions(b []byte) (reader, error) {
	if len(b) < 5 {
		if v := matchTLS(b); v == no {
			return nil, ErrNotClientHello
		}
		return nil, ErrShort
	}
	if b[0] != 0x16 || b[1] != 0x03 {
		return nil, ErrNotClientHello
	}
	n := int(binary.BigEndian.Uint16(b[3:5]))
	if n > maxRecord {
		return nil, ErrMalformed
	}
	if len(b) < 5+n {
		if len(b) > 5 && b[5] != 0x01 {
			return nil, ErrNotClientHello
		}
		return nil, ErrShort
	}
	rec := reader(b[5 : 5+n])
	typ, ok := rec.u8()
	if !ok || typ != 0x01 {
		return nil, ErrNotClientHello
	}
	hl, ok := rec.u24()
	if !ok || hl > len(rec) {
		return nil, ErrMalformed // a ClientHello split over records is not read
	}
	h := reader(rec[:hl])
	if !h.skip(2+32) || !h.skipVec8() || !h.skipVec16() || !h.skipVec8() {
		return nil, ErrMalformed
	}
	if len(h) == 0 {
		return nil, nil // no extensions
	}
	exts, ok := h.vec16()
	if !ok {
		return nil, ErrMalformed
	}
	return exts, nil
}

// parseSNI reads the body of a server_name extension.
func parseSNI(data reader) (string, error) {
	list, ok := data.vec16()
	if !ok {
		return "", ErrMalformed
	}
	for len(list) > 0 {
		kind, ok1 := list.u8()
		name, ok2 := list.vec16()
		if !ok1 || !ok2 {
			return "", ErrMalformed
		}
		if kind != 0 { // host_name
			continue
		}
		s, ok := NormalizeHostname(string(name))
		if !ok {
			return "", ErrMalformed
		}
		return s, nil
	}
	return "", nil
}

// parseALPN reads the body of an application_layer_protocol_negotiation
// extension: a non-empty list of non-empty protocol names, nothing after.
func parseALPN(data reader) ([]string, error) {
	list, ok := data.vec16()
	if !ok || len(data) != 0 || len(list) == 0 {
		return nil, ErrMalformed
	}
	var out []string
	for len(list) > 0 {
		n, ok := list.u8()
		if !ok || n == 0 || len(list) < n {
			return nil, ErrMalformed
		}
		out = append(out, string(list[:n]))
		list = list[n:]
	}
	return out, nil
}

// NormalizeHostname is the form a host name is compared and stored in: lower
// case, without a trailing dot. ok is false for anything that is no host
// name (see validName). The SNI of a ClientHello is read through it, and a
// tcp service's TLS passthrough host name (services.tcp_hostname, Q03) must
// be stored through it too, so that the two compare equal.
func NormalizeHostname(s string) (string, bool) {
	n := strings.TrimSuffix(strings.ToLower(s), ".")
	if !validName(n) {
		return "", false
	}
	return n, true
}

// validName reports whether s is a host name: at most 253 bytes of labels of
// 1 to 63 letters, digits, '-' and '_' (lower case), no label starting or
// ending with '-'.
func validName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// reader is a bounded cursor over a ClientHello.
type reader []byte

func (r *reader) u8() (int, bool) {
	if len(*r) < 1 {
		return 0, false
	}
	v := (*r)[0]
	*r = (*r)[1:]
	return int(v), true
}

func (r *reader) u16() (int, bool) {
	if len(*r) < 2 {
		return 0, false
	}
	v := binary.BigEndian.Uint16(*r)
	*r = (*r)[2:]
	return int(v), true
}

func (r *reader) u24() (int, bool) {
	if len(*r) < 3 {
		return 0, false
	}
	v := int((*r)[0])<<16 | int((*r)[1])<<8 | int((*r)[2])
	*r = (*r)[3:]
	return v, true
}

func (r *reader) skip(n int) bool {
	if len(*r) < n {
		return false
	}
	*r = (*r)[n:]
	return true
}

func (r *reader) vec16() (reader, bool) {
	n, ok := r.u16()
	if !ok || len(*r) < n {
		return nil, false
	}
	v := (*r)[:n]
	*r = (*r)[n:]
	return v, true
}

func (r *reader) skipVec8() bool {
	n, ok := r.u8()
	return ok && r.skip(n)
}

func (r *reader) skipVec16() bool {
	_, ok := r.vec16()
	return ok
}

// Sniffer watches the first bytes a client sends, chunk by chunk as they
// pass, and names their protocol and, for TLS, the server name. It keeps at
// most MaxClientHello bytes and stops looking once it can tell (for TLS: once
// the ClientHello's record is complete). Observe and Result may be called
// from different goroutines.
type Sniffer struct {
	done     atomic.Bool
	mu       sync.Mutex
	buf      []byte
	protocol string
	sni      string
}

// Observe hands the sniffer the next bytes the client sent. It is cheap once
// the sniffer is done.
func (s *Sniffer) Observe(p []byte) {
	if len(p) == 0 || s.done.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done.Load() {
		return
	}
	room := MaxClientHello - len(s.buf)
	s.buf = append(s.buf, p[:min(len(p), room)]...)
	full := len(s.buf) >= MaxClientHello
	label := classify(s.buf)
	switch {
	case label == "" && !full:
		return
	case label == "":
		label = ProtoUnknown
	case label == ProtoTLS:
		sni, err := ServerName(s.buf)
		if errors.Is(err, ErrShort) && !full {
			s.protocol = label
			return
		}
		if err == nil {
			s.sni = sni
		}
	}
	s.protocol = label
	s.buf = nil
	s.done.Store(true)
}

// Done reports whether the sniffer has stopped looking.
func (s *Sniffer) Done() bool { return s.done.Load() }

// Result is what the sniffer can tell so far: the protocol (unknown until it
// is known) and the server name ("" for none).
func (s *Sniffer) Result() (protocol, sni string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.protocol == "" {
		return ProtoUnknown, ""
	}
	return s.protocol, s.sni
}
