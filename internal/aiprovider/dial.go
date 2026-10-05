package aiprovider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// dialControl returns a net.Dialer.Control hook. It sees the literal address
// about to be connected to, after DNS resolution, which closes the gap between
// "the name resolved to a public address when the provider was saved" and
// "what it resolves to now".
func dialControl(allowPrivate bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
		}
		if ip := net.ParseIP(host); ip == nil || !PublicIP(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, host)
		}
		return nil
	}
}

// guardedDialer resolves the host itself and connects to IP literals only, so
// the address the control hook vets is the address the socket connects to.
type guardedDialer struct {
	resolver     Resolver
	dialer       *net.Dialer
	allowPrivate bool
}

// DialContext refuses a name with any non-public answer before connecting;
// the dialer's control hook then vets each address actually tried.
func (g *guardedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBlockedAddress, address)
	}
	if net.ParseIP(host) != nil {
		return g.dialer.DialContext(ctx, network, address)
	}
	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	if !g.allowPrivate {
		for _, a := range addrs {
			if !PublicIP(a.IP) {
				return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, a.IP)
			}
		}
	}
	var firstErr error
	for _, a := range addrs {
		conn, err := g.dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

// NewTransport returns the transport shared by all direct providers.
// allowPrivate lifts the address guard for a self-hosted upstream on a private
// network (BURROW_AI_ALLOW_PRIVATE_UPSTREAMS).
func NewTransport(allowPrivate bool) *http.Transport {
	return newTransport(allowPrivate, net.DefaultResolver)
}

func newTransport(allowPrivate bool, r Resolver) *http.Transport {
	g := &guardedDialer{
		resolver:     r,
		allowPrivate: allowPrivate,
		dialer:       &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: dialControl(allowPrivate)},
	}
	return &http.Transport{
		// No proxy: an environment HTTP proxy would make the guard vet the
		// proxy's address instead of the upstream's.
		Proxy:                 nil,
		DialContext:           g.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // slow first token on large models
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
	}
}
