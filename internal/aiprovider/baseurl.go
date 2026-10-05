// Package aiprovider reaches model providers the relay calls directly
// (OpenRouter, z.ai, any OpenAI-compatible HTTPS endpoint).
package aiprovider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

var (
	// ErrInvalidBaseURL is returned for a base URL that is not a plain https URL.
	ErrInvalidBaseURL = errors.New("aiprovider: invalid base URL")
	// ErrBlockedAddress is returned when an upstream address is not public.
	ErrBlockedAddress = errors.New("aiprovider: address is not public")
)

// ValidateBaseURL accepts "https://host[:port][/path]" without credentials,
// query or fragment. Its errors never repeat the input, which may carry a
// password or a key.
func ValidateBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse quotes the whole input in its error.
		return nil, fmt.Errorf("%w: not a URL", ErrInvalidBaseURL)
	}
	switch {
	case u.Scheme != "https":
		return nil, fmt.Errorf("%w: scheme must be https", ErrInvalidBaseURL)
	case u.Hostname() == "":
		return nil, fmt.Errorf("%w: host is missing", ErrInvalidBaseURL)
	case u.User != nil:
		return nil, fmt.Errorf("%w: must not contain credentials", ErrInvalidBaseURL)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, fmt.Errorf("%w: must not contain a query or fragment", ErrInvalidBaseURL)
	}
	return u, nil
}

// nonPublic lists ranges that netip.Addr's own predicates do not cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, includes broadcast
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: embeds an IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2001::/32"),      // Teredo: embeds an IPv4 address
	netip.MustParsePrefix("2002::/16"),      // 6to4: embeds an IPv4 address
}

// PublicIP reports whether ip is a globally routable unicast address.
func PublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap() // judge ::ffff:a.b.c.d as the IPv4 address it is
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() ||
		addr.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Resolver is the subset of *net.Resolver used to vet a host name.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// CheckHostPublic resolves host and fails unless every address is public. It
// gives the operator an early, readable error when saving a provider; the
// dial guard in NewTransport is what enforces the rule on every connection.
func CheckHostPublic(ctx context.Context, r Resolver, host string) error {
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		if !PublicIP(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, host)
		}
		return nil
	}
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if !PublicIP(a.IP) {
			return fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, a.IP)
		}
	}
	return nil
}
