package client

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Certificate pinning for the control port (spec D13).
//
// What is pinned is the SHA-256 of the leaf certificate :7000 presents. The
// relay names it in the discovery document, which `burrow login` reads over
// the dashboard's HTTPS, a channel verified against the system roots (or
// --cacert). Why the leaf and not a CA: the control port sends only the leaf
// (the relay loads one certificate file), so a CA could never be checked from
// the handshake, and a CA pin would also trust whatever else that CA signs.
//
// A leaf changes when the relay renews (ACME) or regenerates its dev
// certificates. dialControl handles that: when the pin fails and the caller
// gave RefreshPin, discovery is asked again over HTTPS; the pin is replaced
// only if the dashboard names the very certificate :7000 presented. An
// interceptor on :7000 cannot do that, since the dashboard still names the
// real one.

// PinMismatchError says :7000 presented a certificate other than the pinned one.
type PinMismatchError struct {
	Want string // the pinned fingerprint
	Got  string // the fingerprint presented
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("the relay's control certificate does not match the one pinned at login (pinned sha256:%s, presented sha256:%s). "+
		"Someone may be intercepting the connection. If the relay's certificate was replaced on purpose, sign in again to pin the new one: burrow login <relay> --force",
		short(e.Want), short(e.Got))
}

func short(fp string) string {
	if len(fp) > 16 {
		return fp[:16] + "…"
	}
	return fp
}

// Fingerprint is the pin of a DER certificate: SHA-256 in lower-case hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// NormalizeFingerprint accepts a SHA-256 as 64 hex digits, optionally with
// colons or a "sha256:" prefix and in any case, and returns lower-case hex.
func NormalizeFingerprint(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.ReplaceAll(s, ":", "")
	if len(s) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", false
	}
	return s, true
}

// ErrInsecureForward says a forward was asked for without checking the relay.
var ErrInsecureForward = errors.New("refusing to forward with --insecure: the relay's certificate is not checked, so anyone on the path could take the token and reach the private service. Pin the relay with `burrow login <relay> --force`, or pass --insecure-forward to accept the risk")

// ForwardAllowed is the check `burrow forward` makes before it opens a
// listener: a client that does not verify the relay may not carry a private
// service's traffic unless --insecure-forward says the user accepts that.
func ForwardAllowed(insecure, insecureForward bool) error {
	if insecure && !insecureForward {
		return ErrInsecureForward
	}
	return nil
}

// PinnedTLS builds the TLS config of a pinned dial. The chain and the name are
// not checked: the pin replaces them, and it is stricter.
func PinnedTLS(pin string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // the pin below replaces chain and name verification
		// VerifyConnection, not VerifyPeerCertificate: it also runs when a
		// session is resumed.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return &PinMismatchError{Want: pin}
			}
			got := Fingerprint(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare([]byte(got), []byte(pin)) != 1 {
				return &PinMismatchError{Want: pin, Got: got}
			}
			return nil
		},
	}
}

// dialPinned dials with the pin and, on a mismatch, tries the refresh once.
func dialPinned(ctx context.Context, o *Options) (net.Conn, error) {
	d := &tls.Dialer{Config: PinnedTLS(o.PinSHA256)}
	conn, err := d.DialContext(ctx, "tcp", o.Server)
	var pm *PinMismatchError
	if err == nil || !errors.As(err, &pm) || o.RefreshPin == nil || pm.Got == "" {
		return conn, wrapDial(err)
	}
	fresh, rerr := o.RefreshPin(ctx)
	fresh, ok := NormalizeFingerprint(fresh)
	if rerr != nil || !ok || fresh != pm.Got {
		return nil, wrapDial(err)
	}
	d = &tls.Dialer{Config: PinnedTLS(fresh)}
	conn, err = d.DialContext(ctx, "tcp", o.Server)
	if err != nil {
		return nil, wrapDial(err)
	}
	o.PinSHA256 = fresh
	if o.PinRefreshed != nil {
		o.PinRefreshed(fresh)
	}
	return conn, nil
}

func wrapDial(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("dial: %w", err)
}
