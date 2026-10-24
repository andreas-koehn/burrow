// Package relaynames is the set of host names the relay serves itself on its
// HTTPS port: the dashboard's names. The mux on that port (H11) terminates
// them whatever tcp service claims one, and the API refuses them as a tcp
// service's TLS passthrough host name, so that nobody can take the
// dashboard's host. cmd/server builds the set once and hands it to both.
package relaynames

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// Set is a set of the relay's own names. The zero value and nil hold none
// but localhost and addresses.
type Set struct {
	exact map[string]struct{}
	// wild holds the parent of each wildcard name "*.<parent>": it covers
	// exactly one label more.
	wild  map[string]struct{}
	under []string
}

// New is the set of names (exact, or "*.<parent>" for a wildcard over one
// label) plus every name under each of under (and that name itself).
// Empty entries are skipped; case and a trailing dot do not count.
func New(under []string, names ...string) *Set {
	s := &Set{exact: map[string]struct{}{}, wild: map[string]struct{}{}}
	for _, n := range names {
		n = norm(n)
		switch {
		case n == "":
		case strings.HasPrefix(n, "*."):
			s.wild[n[2:]] = struct{}{}
		default:
			s.exact[n] = struct{}{}
		}
	}
	for _, d := range under {
		if d = norm(d); d != "" {
			s.under = append(s.under, d)
		}
	}
	return s
}

func norm(n string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".") }

// Owns reports whether the relay serves name itself.
func (s *Set) Owns(name string) bool {
	name = norm(name)
	if name == "" {
		return false
	}
	if name == "localhost" {
		return true
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}
	if s == nil {
		return false
	}
	if _, ok := s.exact[name]; ok {
		return true
	}
	if label, parent, ok := strings.Cut(name, "."); ok && label != "" {
		if _, ok := s.wild[parent]; ok {
			return true
		}
	}
	for _, d := range s.under {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// CertDNSNames reads the DNS names (SANs) of the leaf, the first
// certificate, of the PEM file at path.
func CertDNSNames(path string) ([]string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // an operator-configured certificate path
	if err != nil {
		return nil, err
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, errors.New("no certificate in " + path)
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		return c.DNSNames, nil
	}
}
