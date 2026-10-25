package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/acme"
)

// A TLS-ALPN-01 validation offers acme-tls/1 and nothing else. On the
// ingress TLS configuration (NextProtos from ensureIngressNextProtos, the
// custom-domain certificates in front of the ACME source, as main.go builds
// the dashboard listener) such a hello completes a real handshake with
// acme-tls/1, gets the challenge certificate from the ACME source, and never
// asks the custom-domain lookup, even for a name that is a custom domain.
func TestIngressACMETLSOnlyHelloHandshakes(t *testing.T) {
	challenge := selfSignedCfg(t).Certificates[0]
	listener := selfSignedCfg(t).Certificates[0]
	customAsked := atomic.Int64{}
	custom := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		customAsked.Add(1)
		c := selfSignedCfg(t).Certificates[0]
		return &c, nil
	}
	// The ACME source: the challenge certificate for an acme-tls/1 hello,
	// the listener's own otherwise (certmagic's GetCertificate).
	acmeSource := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if slices.Contains(hello.SupportedProtos, acme.ACMETLSProto) {
			return &challenge, nil
		}
		return &listener, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{acme.ACMETLSProto}} // certmagic's TLSConfig
	ensureIngressNextProtos(cfg)
	cfg.GetCertificate = customDomainGetCertificate(func(string) bool { return false }, custom, acmeSource)
	addr := startIngress(t, protoHandler(), cfg, true)

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		ServerName:         "grpc.custom.test",
		NextProtos:         []string{acme.ACMETLSProto},
		InsecureSkipVerify: true, //nolint:gosec // the validator checks the acmeIdentifier extension, not a chain
	})
	if err != nil {
		t.Fatalf("acme-tls/1-only handshake: %v", err)
	}
	defer conn.Close()
	st := conn.ConnectionState()
	if st.NegotiatedProtocol != acme.ACMETLSProto {
		t.Fatalf("negotiated %q, want %s", st.NegotiatedProtocol, acme.ACMETLSProto)
	}
	if !bytes.Equal(st.PeerCertificates[0].Raw, challenge.Certificate[0]) {
		t.Fatal("the hello did not get the challenge certificate")
	}
	if n := customAsked.Load(); n != 0 {
		t.Fatalf("the custom-domain lookup was asked %d times for an acme-tls/1 hello", n)
	}

	// The same name without acme-tls/1 gets the custom-domain certificate.
	c2, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "grpc.custom.test", NextProtos: []string{"h2"}, InsecureSkipVerify: true}) //nolint:gosec // test cert
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	if customAsked.Load() != 1 {
		t.Fatal("an ordinary hello for a custom domain skipped the lookup")
	}
}

// Extended CONNECT (RFC 8441, SETTINGS_ENABLE_CONNECT_PROTOCOL) is out of
// scope: WebSocket over h2 is not offered, so neither listener flavour may
// announce it in its SETTINGS.
func TestIngressDoesNotAdvertiseExtendedConnect(t *testing.T) {
	const settingsEnableConnectProtocol = 0x8
	read := func(t *testing.T, c net.Conn) map[uint16]uint32 {
		t.Helper()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		// Our own (empty) SETTINGS, so the server keeps talking.
		if _, err := c.Write([]byte{0, 0, 0, 4, 0, 0, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(c)
		var hdr [9]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			t.Fatal(err)
		}
		n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		if hdr[3] != 4 || hdr[4]&1 != 0 {
			t.Fatalf("first server frame type %d flags %d, want SETTINGS", hdr[3], hdr[4])
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			t.Fatal(err)
		}
		got := map[uint16]uint32{}
		for i := 0; i+6 <= len(payload); i += 6 {
			got[binary.BigEndian.Uint16(payload[i:])] = binary.BigEndian.Uint32(payload[i+2:])
		}
		return got
	}

	t.Run("h2c", func(t *testing.T) {
		addr := startIngress(t, protoHandler(), nil, true)
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		s := read(t, c)
		if v, ok := s[settingsEnableConnectProtocol]; ok && v != 0 {
			t.Fatalf("h2c SETTINGS announce extended CONNECT: %v", s)
		}
		if s[0x3] != 250 {
			t.Fatalf("MAX_CONCURRENT_STREAMS %d, want 250 (the read is of the right frame): %v", s[0x3], s)
		}
	})
	t.Run("h2 over TLS", func(t *testing.T) {
		cfg := selfSignedCfg(t)
		ensureIngressNextProtos(cfg)
		addr := startIngress(t, protoHandler(), cfg, true)
		c, err := tls.Dial("tcp", addr, &tls.Config{NextProtos: []string{"h2"}, InsecureSkipVerify: true}) //nolint:gosec // test cert
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if c.ConnectionState().NegotiatedProtocol != "h2" {
			t.Fatalf("ALPN %q", c.ConnectionState().NegotiatedProtocol)
		}
		s := read(t, c)
		if v, ok := s[settingsEnableConnectProtocol]; ok && v != 0 {
			t.Fatalf("h2 SETTINGS announce extended CONNECT: %v", s)
		}
	})
}
