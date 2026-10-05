package main

// proxy_wiring.go — adapters that bridge internal/server, internal/store, and
// internal/proxy for Task 12 (v0.3.0 HTTP reverse-proxy ingress).
//
// Three adapters are defined here:
//
//  1. serviceResolverAdapter: wraps *db.DB to satisfy server.ServiceResolver.
//     Calls GetOrCreateService to obtain the durable service row, then calls
//     SetServiceSubdomain (retrying on UNIQUE collision) if no subdomain is set.
//
//  2. proxyDialerAdapter: wraps *server.Server + a subdomainStore (narrow
//     interface satisfied by *store.Store) to satisfy proxy.StreamDialer.
//     Lookup returns proxy.ErrNotFound when the service row is missing OR when
//     the live tunnel is not connected. DialTunnelStream opens a per-request
//     yamux stream using server.Server.OpenTunnelStream.
//
//  3. liveTunnelLookupAdapter: wraps the httpTunnelSource (a narrow interface
//     satisfied by *server.Server) to satisfy api.LiveTunnelLookup.
//     Scans HTTPTunnels() for service-ID and SnapshotSessions() for user-ID.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/server"
)

// ---------------------------------------------------------------------------
// serviceResolverAdapter (server.ServiceResolver)
// ---------------------------------------------------------------------------

// serviceDB is the narrow interface serviceResolverAdapter needs from the db
// layer. *db.DB satisfies it implicitly. The interface is also satisfied by
// the fakeServiceDB test double in proxy_wiring_test.go.
type serviceDB interface {
	GetOrCreateService(ctx context.Context, userID, name, typ string) (db.Service, error)
	SetServiceSubdomain(ctx context.Context, id, sub string) error
}

// serviceResolverAdapter adapts the db layer to server.ServiceResolver.
// It owns the collision-retry logic: GenerateSlug is called up to N
// times, retrying whenever SetServiceSubdomain returns a UNIQUE error.
type serviceResolverAdapter struct {
	db serviceDB
}

const subdomainRetries = 8

// Resolve implements server.ServiceResolver.
//
//  1. GetOrCreateService → stable service row.
//  2. If Subdomain is already set → return early (stable identity).
//  3. Otherwise generate up to subdomainRetries random subdomains and try
//     SetServiceSubdomain; on UNIQUE collision retry. On exhaustion → error.
func (a serviceResolverAdapter) Resolve(ctx context.Context, userID, name, typ string) (serviceID, subdomain string, err error) {
	svc, err := a.db.GetOrCreateService(ctx, userID, name, typ)
	if err != nil {
		return "", "", fmt.Errorf("resolve service: get-or-create: %w", err)
	}
	if svc.Subdomain != "" {
		return svc.ID, svc.Subdomain, nil
	}
	for i := 0; i < subdomainRetries; i++ {
		sub, err := auth.GenerateSlug()
		if err != nil {
			return "", "", fmt.Errorf("resolve service: generate slug: %w", err)
		}
		serr := a.db.SetServiceSubdomain(ctx, svc.ID, sub)
		if serr == nil {
			return svc.ID, sub, nil
		}
		if isUNIQUESubdomainError(serr) {
			continue // collision — try a different subdomain
		}
		return "", "", fmt.Errorf("resolve service: set subdomain: %w", serr)
	}
	return "", "", fmt.Errorf("resolve service: exhausted %d subdomain attempts (all collided)", subdomainRetries)
}

// isUNIQUESubdomainError reports whether err is a UNIQUE constraint violation
// on the services.subdomain column. The sqlite driver wraps its errors; we
// inspect the message string (same approach as store.isUniqueViolation).
func isUNIQUESubdomainError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: services.subdomain")
}

// ---------------------------------------------------------------------------
// proxyDialerAdapter (proxy.StreamDialer)
// ---------------------------------------------------------------------------

// subdomainStore is the narrow interface proxyDialerAdapter.Lookup needs from
// the store layer. *store.Store satisfies it implicitly.
type subdomainStore interface {
	ServiceForSubdomain(ctx context.Context, sub string) (db.Service, error)
	ServiceByID(ctx context.Context, id string) (db.Service, error)
	GetServiceIPGeo(ctx context.Context, serviceID string) (db.ServiceIPGeoConfig, error)
}

// tunnelStreamOpener is the narrow interface proxyDialerAdapter needs from
// *server.Server. Defined as an interface so the adapter can be tested with
// a fake, and to avoid a direct import cycle.
type tunnelStreamOpener interface {
	LookupHTTPTunnelByServiceID(serviceID string) (*server.Tunnel, bool)
	OpenTunnelStream(ctx context.Context, tn *server.Tunnel) (net.Conn, error)
	// LookupSessionByTunnelID resolves UserID + ClientSessionID from the
	// tunnel runtime ID via the Registry's O(1) tunnel index (v0.5.2
	// BACKLOG #1). Replaces the v0.5.1 SnapshotSessions scan.
	LookupSessionByTunnelID(tunnelID string) (sessionID, userID string, ok bool)
}

// proxyDialerAdapter adapts *server.Server + store to proxy.StreamDialer.
type proxyDialerAdapter struct {
	st  subdomainStore
	srv tunnelStreamOpener
}

// lookupSessionFields resolves the UserID and ClientSessionID for the tunnel
// with the given runtime ID via the Registry's O(1) tunnel index (v0.5.2
// BACKLOG #1). Returns empty strings when no matching session is found
// (e.g. the tunnel just disconnected). Safe to call concurrently — the
// underlying SessionByTunnelID probe is RLock-guarded.
func (a proxyDialerAdapter) lookupSessionFields(tunnelID string) (userID, sessionID string) {
	sess, user, ok := a.srv.LookupSessionByTunnelID(tunnelID)
	if !ok {
		return "", ""
	}
	return user, sess
}

// Lookup implements proxy.StreamDialer.Lookup.
// Returns proxy.ErrNotFound when:
//   - the service row does not exist (subdomain not registered), or
//   - no live HTTP tunnel is connected for that service.
//
// The live tunnel is found by service id, never by the slug it registered
// with, so a slug rename applies without a client reconnect.
func (a proxyDialerAdapter) Lookup(ctx context.Context, sub string) (*proxy.Resolved, error) {
	svc, err := a.st.ServiceForSubdomain(ctx, sub)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, proxy.ErrNotFound
		}
		return nil, fmt.Errorf("proxy lookup: service for subdomain: %w", err)
	}
	tn, ok := a.srv.LookupHTTPTunnelByServiceID(svc.ID)
	if !ok {
		// Service exists but no live tunnel — treat as not found for the proxy
		// (the client may have disconnected after registering the subdomain).
		return nil, proxy.ErrNotFound
	}
	userID, sessionID := a.lookupSessionFields(tn.ID)
	ipgeo, err := a.st.GetServiceIPGeo(ctx, svc.ID)
	if err != nil {
		return nil, fmt.Errorf("proxy lookup: ip-geo config: %w", err)
	}
	r := &proxy.Resolved{
		ServiceID:       svc.ID,
		AccessMode:      svc.AccessMode,
		APIKeyHeader:    svc.APIKeyHeader,
		LocalHost:       tn.LocalAddr,
		TunnelID:        tn.ID,
		UserID:          userID,
		ClientSessionID: sessionID,
	}
	if ipgeo.Enabled {
		r.IPAllowCIDRs = ipgeo.AllowCIDRs
		r.IPBlockCIDRs = ipgeo.BlockCIDRs
		r.IPAllowCountries = ipgeo.AllowCountries
		r.IPBlockCountries = ipgeo.BlockCountries
	}
	if svc.MTLSCAPEM != "" {
		r.MTLSCAPEM = []byte(svc.MTLSCAPEM)
	}
	return r, nil
}

// DialTunnelStream implements proxy.StreamDialer.DialTunnelStream.
// Resolves the slug to its service row, then dials the live tunnel by service
// id (the registry's slug may be stale after a rename). Returns
// proxy.ErrNotFound if the service or the tunnel is gone.
func (a proxyDialerAdapter) DialTunnelStream(ctx context.Context, sub string) (net.Conn, error) {
	svc, err := a.st.ServiceForSubdomain(ctx, sub)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, proxy.ErrNotFound
		}
		return nil, fmt.Errorf("proxy dial stream: service for slug: %w", err)
	}
	return a.DialTunnelStreamByServiceID(ctx, svc.ID)
}

// LookupByServiceID implements proxy.StreamDialer.LookupByServiceID.
// Used by the custom-domain routing path (v0.5.0 Task 7) where the request
// Host is not a subdomain of authDomain.
func (a proxyDialerAdapter) LookupByServiceID(ctx context.Context, serviceID string) (*proxy.Resolved, error) {
	tn, ok := a.srv.LookupHTTPTunnelByServiceID(serviceID)
	if !ok {
		return nil, proxy.ErrNotFound
	}
	// We need the service row for access mode / api-key header. Use a fresh DB
	// lookup by service ID (tolerate a miss — the tunnel may be gone).
	svc, err := a.st.ServiceByID(ctx, serviceID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, proxy.ErrNotFound
		}
		return nil, fmt.Errorf("proxy lookup by service id: service by id: %w", err)
	}
	userID, sessionID := a.lookupSessionFields(tn.ID)
	ipgeo, err := a.st.GetServiceIPGeo(ctx, svc.ID)
	if err != nil {
		return nil, fmt.Errorf("proxy lookup by service id: ip-geo config: %w", err)
	}
	r := &proxy.Resolved{
		ServiceID:       svc.ID,
		AccessMode:      svc.AccessMode,
		APIKeyHeader:    svc.APIKeyHeader,
		LocalHost:       tn.LocalAddr,
		TunnelID:        tn.ID,
		UserID:          userID,
		ClientSessionID: sessionID,
	}
	if ipgeo.Enabled {
		r.IPAllowCIDRs = ipgeo.AllowCIDRs
		r.IPBlockCIDRs = ipgeo.BlockCIDRs
		r.IPAllowCountries = ipgeo.AllowCountries
		r.IPBlockCountries = ipgeo.BlockCountries
	}
	if svc.MTLSCAPEM != "" {
		r.MTLSCAPEM = []byte(svc.MTLSCAPEM)
	}
	return r, nil
}

// directServicePolicy returns the lookup the /ai/ gateway uses for a direct
// provider: the backing service's access mode and IP/geo policy, read from
// the service's rows. Such a service has no tunnel, so the tunnel registry is
// not consulted.
func directServicePolicy(st subdomainStore) func(ctx context.Context, serviceID string) (*proxy.Resolved, error) {
	return func(ctx context.Context, serviceID string) (*proxy.Resolved, error) {
		svc, err := st.ServiceByID(ctx, serviceID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, proxy.ErrNotFound
			}
			return nil, fmt.Errorf("direct service policy: service by id: %w", err)
		}
		// Only a direct provider's own backing row: a provider pointing at
		// any other service must not inherit that service's keys and policy.
		if svc.Type != "direct" {
			return nil, proxy.ErrNotFound
		}
		ipgeo, err := st.GetServiceIPGeo(ctx, svc.ID)
		if err != nil {
			return nil, fmt.Errorf("direct service policy: ip-geo config: %w", err)
		}
		r := &proxy.Resolved{ServiceID: svc.ID, AccessMode: svc.AccessMode, APIKeyHeader: svc.APIKeyHeader}
		if ipgeo.Enabled {
			r.IPAllowCIDRs = ipgeo.AllowCIDRs
			r.IPBlockCIDRs = ipgeo.BlockCIDRs
			r.IPAllowCountries = ipgeo.AllowCountries
			r.IPBlockCountries = ipgeo.BlockCountries
		}
		return r, nil
	}
}

// DialTunnelStreamByServiceID implements proxy.StreamDialer.DialTunnelStreamByServiceID.
func (a proxyDialerAdapter) DialTunnelStreamByServiceID(ctx context.Context, serviceID string) (net.Conn, error) {
	tn, ok := a.srv.LookupHTTPTunnelByServiceID(serviceID)
	if !ok {
		return nil, proxy.ErrNotFound
	}
	conn, err := a.srv.OpenTunnelStream(ctx, tn)
	if err != nil {
		return nil, fmt.Errorf("proxy dial stream by service id: %w", err)
	}
	return conn, nil
}

// ---------------------------------------------------------------------------
// liveTunnelLookupAdapter (api.LiveTunnelLookup)
// ---------------------------------------------------------------------------

// httpTunnelSource is the narrow interface liveTunnelLookupAdapter needs from
// *server.Server. *server.Server satisfies it implicitly.
type httpTunnelSource interface {
	HTTPTunnels() []*server.Tunnel
	SnapshotSessions() []server.SessionSnapshot
}

// liveTunnelLookupAdapter exposes the in-memory tunnel registry to the HTTP API
// (api.LiveTunnelLookup). It scans HTTPTunnels() for service-ID lookups and
// SnapshotSessions() for tunnel-ID → user-ID resolution.
type liveTunnelLookupAdapter struct {
	srv httpTunnelSource
}

// LookupByServiceID implements api.LiveTunnelLookup.
// Returns the first HTTP tunnel with matching ServiceID; ok=false when absent.
func (a liveTunnelLookupAdapter) LookupByServiceID(serviceID string) (api.LiveTunnelSnapshot, bool) {
	for _, tn := range a.srv.HTTPTunnels() {
		if tn.ServiceID == serviceID {
			return api.LiveTunnelSnapshot{
				LocalAddr:  tn.LocalAddr,
				Connected:  true,
				RemotePort: tn.RemotePort, // 0 for http tunnels
			}, true
		}
	}
	return api.LiveTunnelSnapshot{}, false
}

// LookupByTunnelID implements api.LiveTunnelLookup.
// Returns a TunnelLocator for the tunnel with the given runtime tunnel ID.
// ServiceID comes from the HTTP-tunnel registry; UserID is resolved via
// SnapshotSessions (the session that owns the tunnel).
func (a liveTunnelLookupAdapter) LookupByTunnelID(tunnelID string) (api.TunnelLocator, bool) {
	// First pass: find the ServiceID from the HTTP-tunnel registry.
	var serviceID string
	for _, tn := range a.srv.HTTPTunnels() {
		if tn.ID == tunnelID {
			serviceID = tn.ServiceID
			break
		}
	}
	if serviceID == "" {
		// Also scan non-HTTP tunnels by iterating sessions.
		for _, ss := range a.srv.SnapshotSessions() {
			for _, tv := range ss.Tunnels {
				if tv.ID == tunnelID {
					// For non-HTTP tunnels serviceID is empty — return
					// what we have so the API can still back-compat-route.
					return api.TunnelLocator{ServiceID: "", UserID: ss.UserID}, true
				}
			}
		}
		return api.TunnelLocator{}, false
	}
	// Second pass: resolve owning UserID from session snapshots.
	for _, ss := range a.srv.SnapshotSessions() {
		for _, tv := range ss.Tunnels {
			if tv.ID == tunnelID {
				return api.TunnelLocator{ServiceID: serviceID, UserID: ss.UserID}, true
			}
		}
	}
	// Found via HTTPTunnels but no session snapshot yet — still return serviceID.
	return api.TunnelLocator{ServiceID: serviceID, UserID: ""}, true
}
