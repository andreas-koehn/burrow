package tcpsniff

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// clientHello captures the first flight of a real Go TLS client that asks for
// serverName ("" sends no server name).
func clientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	b, err := helloBytes(serverName)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func helloBytes(serverName string) ([]byte, error) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		c := tls.Client(a, &tls.Config{ServerName: serverName, InsecureSkipVerify: serverName == ""}) //nolint:gosec // the handshake is never finished
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_ = c.Handshake()
	}()
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	hdr := make([]byte, 5)
	if _, err := ioReadFull(b, hdr); err != nil {
		return nil, err
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[3:5]))
	if _, err := ioReadFull(b, body); err != nil {
		return nil, err
	}
	return append(hdr, body...), nil
}

func ioReadFull(c net.Conn, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := c.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestClassify(t *testing.T) {
	hello := clientHello(t, "rdp.example.com")
	for _, c := range []struct {
		name string
		in   []byte
		want string
	}{
		{"tls client hello", hello, ProtoTLS},
		{"tls first bytes only", hello[:6], ProtoTLS},
		{"ssh banner", []byte("SSH-2.0-OpenSSH_9.6\r\n"), ProtoSSH},
		{"rdp x.224 connection request", []byte{0x03, 0x00, 0x00, 0x13, 0x0e, 0xe0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00, 0x00}, ProtoRDP},
		{"http get", []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), ProtoHTTP},
		{"http post", []byte("POST /api HTTP/1.1\r\n"), ProtoHTTP},
		{"http2 preface", []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), ProtoHTTP},
		{"nothing", nil, ProtoUnknown},
		{"postgres startup", []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}, ProtoUnknown},
		{"smtp-ish text", []byte("EHLO example.com\r\n"), ProtoUnknown},
		{"tpkt without x.224 cr", []byte{0x03, 0x00, 0x00, 0x13, 0x0e, 0xd0, 0, 0, 0, 0, 0}, ProtoUnknown},
		{"tls alert record", []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x28}, ProtoUnknown},
		{"lowercase get", []byte("get / http/1.1\r\n"), ProtoUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.in); got != c.want {
				t.Errorf("Classify = %q, want %q", got, c.want)
			}
		})
	}
	// A prefix that could still be any of them is undecided.
	for _, p := range [][]byte{{0x16}, {0x16, 0x03}, []byte("SS"), {0x03, 0x00}, []byte("GE"), []byte("PRI * HT")} {
		if got := classify(p); got != "" {
			t.Errorf("classify(%q) = %q, want undecided", p, got)
		}
	}
}

func TestServerName(t *testing.T) {
	hello := clientHello(t, "RDP.Example.com")
	sni, err := ServerName(hello)
	if err != nil || sni != "rdp.example.com" {
		t.Fatalf("ServerName = %q, %v; want rdp.example.com", sni, err)
	}
	// Every strict prefix is short, never a wrong answer.
	for n := 0; n < len(hello); n++ {
		if _, err := ServerName(hello[:n]); !errors.Is(err, ErrShort) {
			t.Fatalf("prefix of %d bytes: %v, want ErrShort", n, err)
		}
	}
	// More bytes after the record (the client's next flight) change nothing.
	if sni, err := ServerName(append(append([]byte{}, hello...), 0x17, 0x03, 0x03)); err != nil || sni != "rdp.example.com" {
		t.Fatalf("with trailing bytes: %q, %v", sni, err)
	}

	noName := clientHello(t, "")
	if sni, err := ServerName(noName); err != nil || sni != "" {
		t.Fatalf("no server name: %q, %v", sni, err)
	}
	if _, err := ServerName([]byte("SSH-2.0-x\r\n")); !errors.Is(err, ErrNotClientHello) {
		t.Fatalf("ssh: %v", err)
	}
}

// Every length in the ClientHello is checked against what holds it: a
// truncated or lying field is malformed, never read past.
func TestServerName_Malformed(t *testing.T) {
	hello := clientHello(t, "a.example")
	// A record longer than a TLS record may be.
	big := append([]byte{}, hello...)
	binary.BigEndian.PutUint16(big[3:5], 0x4001+256)
	if _, err := ServerName(big); !errors.Is(err, ErrMalformed) {
		t.Fatalf("oversized record: %v", err)
	}
	// A handshake message that claims more than its record holds (one split
	// over records) is not read.
	split := append([]byte{}, hello...)
	split[6], split[7], split[8] = 0x00, 0xff, 0xff
	if _, err := ServerName(split); !errors.Is(err, ErrMalformed) {
		t.Fatalf("split hello: %v", err)
	}
	// Every single byte flipped in the body: no panic, and an answer that is
	// a valid name, "" or an error.
	for i := 5; i < len(hello); i++ {
		for _, v := range []byte{0x00, 0xff} {
			m := append([]byte{}, hello...)
			m[i] = v
			sni, err := ServerName(m)
			if err == nil && sni != "" && !validName(sni) {
				t.Fatalf("byte %d = %#x: invalid name %q accepted", i, v, sni)
			}
		}
	}
	// A server name that is no host name is dropped.
	for _, bad := range []string{"bad name", "x\x00y", strings.Repeat("a", 254), "-", ".."} {
		if validName(bad) {
			t.Errorf("validName(%q) = true", bad)
		}
	}
	for _, good := range []string{"rdp.example.com", "a", "xn--bcher-kva.example", "_srv.example"} {
		if !validName(good) {
			t.Errorf("validName(%q) = false", good)
		}
	}
}

func TestSniffer(t *testing.T) {
	hello := clientHello(t, "rdp.example.com")
	// The ClientHello may come in pieces: the sniffer keeps what it needs.
	var s Sniffer
	for i := 0; i < len(hello); i += 100 {
		end := min(i+100, len(hello))
		s.Observe(hello[i:end])
	}
	if !s.Done() {
		t.Fatal("not done after the whole ClientHello")
	}
	if p, sni := s.Result(); p != ProtoTLS || sni != "rdp.example.com" {
		t.Fatalf("Result = %q %q", p, sni)
	}
	// What comes after is not looked at.
	s.Observe([]byte("SSH-2.0-x"))
	if p, _ := s.Result(); p != ProtoTLS {
		t.Fatalf("changed to %q", p)
	}

	var ssh Sniffer
	ssh.Observe([]byte("SS"))
	if ssh.Done() {
		t.Fatal("decided on two bytes")
	}
	ssh.Observe([]byte("H-2.0-OpenSSH\r\n"))
	if p, sni := ssh.Result(); !ssh.Done() || p != ProtoSSH || sni != "" {
		t.Fatalf("ssh: %q %q", p, sni)
	}

	var none Sniffer
	if p, _ := none.Result(); p != ProtoUnknown {
		t.Fatalf("nothing seen: %q", p)
	}

	// A TLS record that never completes is held to MaxClientHello bytes.
	var slow Sniffer
	slow.Observe(hello[:5])
	junk := bytes.Repeat([]byte{0x01}, 1024)
	for i := 0; i < 40 && !slow.Done(); i++ {
		slow.Observe(junk)
	}
	if !slow.Done() || len(slow.buf) > MaxClientHello {
		t.Fatalf("done %v, kept %d bytes", slow.Done(), len(slow.buf))
	}
	if p, _ := slow.Result(); p != ProtoTLS {
		t.Fatalf("slow: %q", p)
	}
}

// ServerName never panics and never reads past what it is given; a name it
// returns is a valid, normalised host name.
func FuzzServerName(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("SSH-2.0-x\r\n"))
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00})
	for _, h := range seedHellos {
		f.Add(h)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		sni, err := ServerName(b)
		if err == nil && sni != "" {
			if n, ok := NormalizeHostname(sni); !ok || n != sni {
				t.Fatalf("returned %q, which is not a normalised host name", sni)
			}
		}
		if err != nil && sni != "" {
			t.Fatalf("a name %q with the error %v", sni, err)
		}
	})
}

// The Sniffer, fed the same bytes in any chunks, keeps to its bound and
// gives the answer of the bytes as a whole.
func FuzzSniffer(f *testing.F) {
	for _, h := range seedHellos {
		f.Add(h, uint8(7))
	}
	f.Add([]byte("SSH-2.0-OpenSSH\r\n"), uint8(1))
	f.Add([]byte("GET / HTTP/1.1\r\n"), uint8(3))
	f.Fuzz(func(t *testing.T, b []byte, step uint8) {
		n := int(step%64) + 1
		var s Sniffer
		for i := 0; i < len(b); i += n {
			s.Observe(b[i:min(i+n, len(b))])
			if len(s.buf) > MaxClientHello {
				t.Fatalf("kept %d bytes", len(s.buf))
			}
		}
		var whole Sniffer
		whole.Observe(b)
		p1, n1 := s.Result()
		p2, n2 := whole.Result()
		if s.Done() && whole.Done() && (p1 != p2 || n1 != n2) {
			t.Fatalf("in chunks of %d: %q %q, whole: %q %q", n, p1, n1, p2, n2)
		}
	})
}

// seedHellos are ClientHellos of a real Go client, with and without a name.
var seedHellos = func() [][]byte {
	var out [][]byte
	for _, name := range []string{"rdp.example.com", ""} {
		if b, err := helloBytes(name); err == nil {
			out = append(out, b)
		}
	}
	return out
}()

func TestNormalizeHostname(t *testing.T) {
	for in, want := range map[string]string{"RDP.Example.COM.": "rdp.example.com", "a": "a", " x": "", "bad name": "", "": ""} {
		got, ok := NormalizeHostname(in)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizeHostname(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}
