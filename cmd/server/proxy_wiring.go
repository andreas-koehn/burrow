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
//  3. liveTunnelLookupAdapter: wraps the registryTunnelSource (a narrow interface
//     satisfied by *server.Server) to satisfy api.LiveTunnelLookup.
//     Scans Tunnels() for service-ID and SnapshotSessions() for user-ID.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
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
	// For ResolveWithOptions.
	ListServicesByUser(ctx context.Context, userID string) ([]db.Service, error)
	CreateService(ctx context.Context, s db.Service) error
	GetServiceBySubdomain(ctx context.Context, sub string) (db.Service, error)
	GetUserByID(ctx context.Context, id string) (db.User, error)
	// For the tcp services (server.TCPServiceResolver).
	SetServiceTCPPort(ctx context.Context, id string, port int, pinned bool, takeFrom string) error
	ListTCPPortReservations(ctx context.Context) (map[int]db.TCPPortHold, error)
}

// serviceResolverAdapter adapts the db layer to server.ServiceResolver and
// server.OptionsResolver. It owns the collision-retry logic: GenerateSlug is
// called up to N times, retrying whenever SetServiceSubdomain returns a UNIQUE
// error.
type serviceResolverAdapter struct {
	db serviceDB
	// authDomain is the domain services are served under; "" means the
	// burrow_login mode cannot be chosen, as in the API.
	authDomain string
	// tcpPortsPerUser is BURROW_TCP_PORTS_PER_USER: how many public ports
	// the tcp services of one user may hold; 0 is no limit. Administrators
	// have none.
	tcpPortsPerUser int
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
	if svc.Type != typ {
		return "", "", nameInUse(name, svc.Type)
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

// refuse builds a refusal the control loop sends to the client as it is.
func refuse(code, msg string) error { return &server.RefusalError{Code: code, Message: msg} }

// accessModesOnCreate are the access modes a client may ask for. mtls is not
// among them: it needs a CA, which is configured in the dashboard.
const accessModesOnCreate = "open, api_key, burrow_login"

// findService returns the user's service of that name, which must be of
// type typ: a name belongs to one service, whatever its type, and a
// registration of another type is refused with proto.CodeNameInUse rather
// than bound to it. A row that backs a direct AI provider is never handed
// out (see db.GetOrCreateService); an http registration gets the text it has
// always got for it.
func (a serviceResolverAdapter) findService(ctx context.Context, userID, name, typ string) (db.Service, bool, error) {
	rows, err := a.db.ListServicesByUser(ctx, userID)
	if err != nil {
		return db.Service{}, false, fmt.Errorf("resolve service: list: %w", err)
	}
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		if r.Type == "direct" && typ == "http" {
			// The text Resolve has always given for such a name.
			return db.Service{}, false, fmt.Errorf("resolve service: get-or-create: %w", db.ErrServiceNameReserved)
		}
		if r.Type != typ {
			return db.Service{}, false, nameInUse(name, r.Type)
		}
		return r, true, nil
	}
	return db.Service{}, false, nil
}

// nameInUse is the refusal of a registration whose name belongs to a service
// of type other.
func nameInUse(name, other string) error {
	what := "a " + other + " service"
	switch other {
	case "http":
		what = "an http service"
	case "direct":
		what = "an AI provider"
	}
	return refuse(proto.CodeNameInUse, fmt.Sprintf("the name %q belongs to %s; choose another name", name, what))
}

// ResolveWithOptions implements server.OptionsResolver. The options are
// checked whatever the client checked, and they are applied only to a service
// this call creates:
//
//   - the service exists: it is returned as it is, with the access mode its
//     row has; Ignored names the wishes that differ from what it has. A row
//     without a slug gets a generated one, as in Resolve.
//   - it does not exist: the row is inserted with slug and mode at once, so
//     that a refused slug leaves nothing behind and exactly one of several
//     registrations at the same moment is the one that created it. Choosing
//     a slug or a mode other than open is configuring a service: the owner
//     must be allowed to, as for the same change in the dashboard.
func (a serviceResolverAdapter) ResolveWithOptions(ctx context.Context, userID, name, typ string, o server.ResolveOptions) (server.Resolved, error) {
	if o.Slug != "" && !auth.ValidSlug(o.Slug) {
		return server.Resolved{}, refuse(proto.CodeSlugInvalid, auth.SlugRule)
	}
	switch o.Access {
	case "", "open", "api_key", "burrow_login":
	default:
		return server.Resolved{}, refuse(proto.CodeAccessInvalid, "access must be one of: "+accessModesOnCreate)
	}

	// Two rounds: when another registration creates the service between the
	// lookup and the insert, the second round finds it.
	for round := 0; round < 2; round++ {
		svc, found, err := a.findService(ctx, userID, name, typ)
		if err != nil {
			return server.Resolved{}, err
		}
		if found {
			slug := svc.Subdomain
			if slug == "" {
				if _, slug, err = a.Resolve(ctx, userID, name, typ); err != nil {
					return server.Resolved{}, err
				}
			}
			res := server.Resolved{ServiceID: svc.ID, Slug: slug, AccessMode: svc.AccessMode, GatewayOnly: svc.GatewayOnly}
			if o.Access != "" && o.Access != svc.AccessMode {
				res.Ignored = append(res.Ignored, "access")
			}
			if o.Slug != "" && o.Slug != slug {
				res.Ignored = append(res.Ignored, "slug")
			}
			return res, nil
		}

		res, raced, err := a.create(ctx, userID, name, typ, o)
		if err != nil {
			return server.Resolved{}, err
		}
		if !raced {
			return res, nil
		}
	}
	return server.Resolved{}, fmt.Errorf("resolve service: %q was created and removed while registering", name)
}

// create inserts the service with the wished slug and mode, or with a
// generated slug and the mode open. raced is true when the name was taken by
// another registration meanwhile.
func (a serviceResolverAdapter) create(ctx context.Context, userID, name, typ string, o server.ResolveOptions) (res server.Resolved, raced bool, err error) {
	mode := o.Access
	if mode == "" {
		mode = "open"
	}
	// A generated slug and the mode open are what every token's owner has
	// always got; anything else needs the permission to configure.
	if o.Slug != "" || mode != "open" {
		u, err := a.db.GetUserByID(ctx, userID)
		if err != nil {
			return res, false, fmt.Errorf("resolve service: owner: %w", err)
		}
		if !authz.Can(u.Role, authz.PermServicesConfigureOwn) && !authz.Can(u.Role, authz.PermServicesConfigureAny) {
			return res, false, refuse(proto.CodeForbidden, "your role may not choose a slug or an access mode; leave them out and ask an administrator to set them")
		}
	}
	if mode == "burrow_login" && a.authDomain == "" {
		return res, false, refuse(proto.CodeAccessInvalid, "burrow_login requires a configured auth_domain")
	}

	insert := func(slug string) (inserted bool, err error) {
		id := uuid.NewString()
		err = a.db.CreateService(ctx, db.Service{ID: id, UserID: userID, Name: name, Type: typ, Subdomain: slug, AccessMode: mode})
		if err == nil {
			res = server.Resolved{ServiceID: id, Slug: slug, AccessMode: mode, Created: true}
			return true, nil
		}
		if !errors.Is(err, db.ErrDuplicateService) {
			return false, fmt.Errorf("resolve service: create: %w", err)
		}
		// Either the name or the slug is in use.
		_, found, ferr := a.findService(ctx, userID, name, typ)
		if ferr != nil {
			return false, ferr
		}
		raced = found
		return false, nil
	}

	if o.Slug != "" {
		inserted, err := insert(o.Slug)
		if err != nil || inserted || raced {
			return res, raced, err
		}
		msg := "slug already in use"
		if free, ferr := a.freeSlug(ctx); ferr == nil {
			msg += "; try: " + free
		}
		return res, false, refuse(proto.CodeSlugTaken, msg)
	}
	for i := 0; i < subdomainRetries; i++ {
		slug, err := auth.GenerateSlug()
		if err != nil {
			return res, false, fmt.Errorf("resolve service: generate slug: %w", err)
		}
		inserted, err := insert(slug)
		if err != nil || inserted || raced {
			return res, raced, err
		}
	}
	return res, false, fmt.Errorf("resolve service: exhausted %d subdomain attempts (all collided)", subdomainRetries)
}

// ResolveTCP implements server.TCPServiceResolver: the user's tcp service of
// that name, created in mode o.Access, else o.AccessDefault, else open, when
// there is none.
// An access that is no tcp mode is refused with proto.CodeAccessInvalid; an
// existing service is returned with the mode it has (the relay serves the
// stricter of the two, or refuses a wish stricter than it). As for http, any
// client token's owner may create one: a tcp tunnel has always been open to
// every token, and the modes ip and private only narrow who may reach it, so
// choosing them needs no permission.
//
// A tcp service has a generated slug, the global name a forward reaches it
// by (never an http address); a row without one (from before) gets one
// here. Created is true until a client served the service once (last_seen is
// set when a registration succeeds), so a registration that created the row
// and then failed for want of a port does not use up the announcement.
func (a serviceResolverAdapter) ResolveTCP(ctx context.Context, userID, name string, o server.ResolveOptions) (server.Resolved, error) {
	// A wish, else the default a new client sends for a service it creates
	// (applied only below, on create), else open, as old clients got.
	mode := o.Access
	if mode == "" {
		mode = o.AccessDefault
	}
	if mode == "" {
		mode = server.TCPAccessOpen
	}
	if !server.ValidTCPAccessMode(mode) {
		return server.Resolved{}, refuse(proto.CodeAccessInvalid, "access for a tcp service must be one of: open, ip, private")
	}
	limit, err := a.tcpPortLimit(ctx, userID)
	if err != nil {
		return server.Resolved{}, err
	}
	// Each round looks the service up and, when there is none, inserts it
	// with a fresh slug. A duplicate is another registration that created it
	// meanwhile (the next round finds it) or a slug that was taken (the next
	// round draws another).
	for round := 0; round <= subdomainRetries; round++ {
		svc, found, err := a.findService(ctx, userID, name, "tcp")
		if err != nil {
			return server.Resolved{}, err
		}
		if found {
			slug := svc.Subdomain
			if slug == "" {
				if slug, err = a.giveSlug(ctx, svc.ID); err != nil {
					return server.Resolved{}, err
				}
			}
			return server.Resolved{
				ServiceID: svc.ID, Slug: slug, AccessMode: svc.AccessMode, Created: svc.LastSeen == nil,
				TCPPort: svc.TCPPort, TCPPortPinned: svc.TCPPortPinned, TCPPortLimit: limit,
				MaxConnections: svc.MaxConnections,
				ProxyProtocol:  svc.ProxyProtocol,
				IdleTimeout:    time.Duration(svc.IdleTimeoutS) * time.Second,
				TCPHostname:    svc.TCPHostname,
			}, nil
		}
		slug, err := auth.GenerateSlug()
		if err != nil {
			return server.Resolved{}, fmt.Errorf("resolve service: generate slug: %w", err)
		}
		id := uuid.NewString()
		err = a.db.CreateService(ctx, db.Service{ID: id, UserID: userID, Name: name, Type: "tcp", Subdomain: slug, AccessMode: mode})
		if err == nil {
			return server.Resolved{ServiceID: id, Slug: slug, AccessMode: mode, Created: true, TCPPortLimit: limit}, nil
		}
		if !errors.Is(err, db.ErrDuplicateService) {
			return server.Resolved{}, fmt.Errorf("resolve service: create: %w", err)
		}
	}
	return server.Resolved{}, fmt.Errorf("resolve service: %q could not be created (raced or all slugs collided)", name)
}

// giveSlug gives the service a generated slug, drawing again while the one
// drawn is taken.
func (a serviceResolverAdapter) giveSlug(ctx context.Context, serviceID string) (string, error) {
	for i := 0; i < subdomainRetries; i++ {
		slug, err := auth.GenerateSlug()
		if err != nil {
			return "", fmt.Errorf("resolve service: generate slug: %w", err)
		}
		serr := a.db.SetServiceSubdomain(ctx, serviceID, slug)
		if serr == nil {
			return slug, nil
		}
		if !isUNIQUESubdomainError(serr) && !strings.Contains(serr.Error(), "unique constraint") {
			return "", fmt.Errorf("resolve service: set slug: %w", serr)
		}
	}
	return "", fmt.Errorf("resolve service: exhausted %d slug attempts (all collided)", subdomainRetries)
}

// tcpPortLimit is how many ports the user's tcp services may hold: the
// configured number, or none for a role that may configure any service.
func (a serviceResolverAdapter) tcpPortLimit(ctx context.Context, userID string) (int, error) {
	if a.tcpPortsPerUser <= 0 {
		return 0, nil
	}
	u, err := a.db.GetUserByID(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("resolve service: owner: %w", err)
	}
	if authz.Can(u.Role, authz.PermServicesConfigureAny) {
		return 0, nil
	}
	return a.tcpPortsPerUser, nil
}

// TCPPortReservations implements server.TCPServiceResolver.
func (a serviceResolverAdapter) TCPPortReservations(ctx context.Context) (map[int]server.TCPPortHold, error) {
	rows, err := a.db.ListTCPPortReservations(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int]server.TCPPortHold, len(rows))
	for p, h := range rows {
		v := server.TCPPortHold{ServiceID: h.ServiceID, UserID: h.UserID, Pinned: h.Pinned}
		if h.LastSeen != nil {
			v.LastSeen = *h.LastSeen
		}
		out[p] = v
	}
	return out, nil
}

// SetServiceTCPPort implements server.TCPServiceResolver.
func (a serviceResolverAdapter) SetServiceTCPPort(ctx context.Context, serviceID string, port int, pinned bool, takeFrom string) error {
	err := a.db.SetServiceTCPPort(ctx, serviceID, port, pinned, takeFrom)
	if errors.Is(err, db.ErrTCPPortReserved) {
		return server.ErrTCPPortTaken
	}
	return err
}

// freeSlug returns a generated slug that no service uses right now.
func (a serviceResolverAdapter) freeSlug(ctx context.Context) (string, error) {
	for i := 0; i < subdomainRetries; i++ {
		slug, err := auth.GenerateSlug()
		if err != nil {
			return "", err
		}
		if _, err := a.db.GetServiceBySubdomain(ctx, slug); errors.Is(err, db.ErrNotFound) {
			return slug, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("no free slug")
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

// The control server is the proxy's receiver of request summaries: main hands
// it to proxy.WithSummarySink. It passes each summary to the client whose
// tunnel served the request, when that client asked for summaries, and never
// waits for a client.
var _ proxy.SummarySink = (*server.Server)(nil)

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
	// A gateway-only service does not exist here. Decide before anything else
	// is read, so status and timing match a missing service.
	if svc.GatewayOnly {
		return nil, proxy.ErrNotFound
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
		GatewayOnly:     svc.GatewayOnly,
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
// This is the AI gateway's lookup: a gateway-only service resolves here with
// GatewayOnly set. The proxy's own doors use ingressDialerAdapter, for which
// such a service does not exist.
func (a proxyDialerAdapter) LookupByServiceID(ctx context.Context, serviceID string) (*proxy.Resolved, error) {
	return a.lookupByServiceID(ctx, serviceID, false)
}

// lookupByServiceID resolves a service by id. With hideGatewayOnly a
// gateway-only service answers proxy.ErrNotFound as soon as its row is read,
// before anything else is: status and timing match a missing service even
// when a later query would have failed.
func (a proxyDialerAdapter) lookupByServiceID(ctx context.Context, serviceID string, hideGatewayOnly bool) (*proxy.Resolved, error) {
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
	if hideGatewayOnly && svc.GatewayOnly {
		return nil, proxy.ErrNotFound
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
		GatewayOnly:     svc.GatewayOnly,
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

// ingressDialerAdapter is the dialer of the proxy's own doors (subdomain,
// /svc/<slug>, custom domain, mTLS handshake). It differs from
// proxyDialerAdapter in one thing: a gateway-only service is not found by
// service id either, so a custom domain pointing at one answers like a domain
// pointing at nothing, whatever a later query would have returned.
type ingressDialerAdapter struct{ proxyDialerAdapter }

// LookupByServiceID implements proxy.StreamDialer.LookupByServiceID.
// Used by the custom-domain routing path (v0.5.0 Task 7) where the request
// Host is not a subdomain of authDomain.
func (a ingressDialerAdapter) LookupByServiceID(ctx context.Context, serviceID string) (*proxy.Resolved, error) {
	return a.lookupByServiceID(ctx, serviceID, true)
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

// registryTunnelSource is the narrow interface liveTunnelLookupAdapter needs from
// *server.Server. *server.Server satisfies it implicitly.
type registryTunnelSource interface {
	Tunnels() []*server.Tunnel
	SnapshotSessions() []server.SessionSnapshot
	LookupSessionByTunnelID(tunnelID string) (sessionID, userID string, ok bool)
}

// liveTunnelLookupAdapter exposes the in-memory tunnel registry to the HTTP API
// (api.LiveTunnelLookup). It scans Tunnels() (http and tcp) for service-ID
// lookups and SnapshotSessions() for tunnel-ID → user-ID resolution.
type liveTunnelLookupAdapter struct {
	srv registryTunnelSource
}

// LookupByServiceID implements api.LiveTunnelLookup.
// Returns the first tunnel of any type with matching ServiceID; ok=false when
// absent. A tcp service counts as connected like an http one, so a served
// tcp service cannot be deleted either. SessionID names the session that owns
// the tunnel ("" when it left the registry between the two reads).
func (a liveTunnelLookupAdapter) LookupByServiceID(serviceID string) (api.LiveTunnelSnapshot, bool) {
	if serviceID == "" {
		return api.LiveTunnelSnapshot{}, false
	}
	for _, tn := range a.srv.Tunnels() {
		if tn.ServiceID == serviceID {
			sessionID, _, _ := a.srv.LookupSessionByTunnelID(tn.ID)
			return api.LiveTunnelSnapshot{
				LocalAddr:  tn.LocalAddr,
				Connected:  true,
				RemotePort: tn.RemotePort, // 0 for http tunnels
				SessionID:  sessionID,
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
	// First pass: find the ServiceID from the tunnel registry.
	var serviceID string
	for _, tn := range a.srv.Tunnels() {
		if tn.ID == tunnelID {
			serviceID = tn.ServiceID
			break
		}
	}
	if serviceID == "" {
		// A tunnel without a service: one of a relay without tcp services.
		for _, ss := range a.srv.SnapshotSessions() {
			for _, tv := range ss.Tunnels {
				if tv.ID == tunnelID {
					// Its serviceID is empty — return what we have so the
					// API can still back-compat-route.
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
	// Found via Tunnels but no session snapshot yet — still return serviceID.
	return api.TunnelLocator{ServiceID: serviceID, UserID: ""}, true
}

// ---------------------------------------------------------------------------
// tcpAccessAdapter (server.TCPAccessLoader)
// ---------------------------------------------------------------------------

// tcpAccessDB is what tcpAccessAdapter reads. *db.DB satisfies it.
type tcpAccessDB interface {
	GetServiceByID(ctx context.Context, id string) (db.Service, error)
	GetServiceIPGeo(ctx context.Context, serviceID string) (db.ServiceIPGeoConfig, error)
	ListIPGrants(ctx context.Context, serviceID string, now time.Time) ([]db.IPGrant, error)
}

// tcpAccessAdapter reads the access of a tcp service for the relay: its
// mode and cap from the service row, and for mode ip the CIDR lists of
// service_ip_geo (the countries and the enabled flag are not used: mode ip
// is the switch) and the grants that have not expired.
type tcpAccessAdapter struct {
	db  tcpAccessDB
	now func() time.Time
}

func (a tcpAccessAdapter) TCPAccess(ctx context.Context, serviceID string) (server.TCPAccess, error) {
	svc, err := a.db.GetServiceByID(ctx, serviceID)
	if err != nil {
		return server.TCPAccess{}, fmt.Errorf("tcp access: service: %w", err)
	}
	if svc.Type != "tcp" {
		return server.TCPAccess{}, fmt.Errorf("tcp access: service %s is of type %s", serviceID, svc.Type)
	}
	acc := server.TCPAccess{
		Mode: svc.AccessMode, MaxConnections: svc.MaxConnections,
		ProxyProtocol: svc.ProxyProtocol, IdleTimeout: time.Duration(svc.IdleTimeoutS) * time.Second,
		Hostname: svc.TCPHostname,
	}
	if acc.Mode != server.TCPAccessIP {
		return acc, nil
	}
	ipgeo, err := a.db.GetServiceIPGeo(ctx, serviceID)
	if err != nil {
		return server.TCPAccess{}, fmt.Errorf("tcp access: ip lists: %w", err)
	}
	acc.AllowCIDRs, acc.BlockCIDRs = ipgeo.AllowCIDRs, ipgeo.BlockCIDRs
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	grants, err := a.db.ListIPGrants(ctx, serviceID, now())
	if err != nil {
		return server.TCPAccess{}, fmt.Errorf("tcp access: grants: %w", err)
	}
	for _, g := range grants {
		acc.Grants = append(acc.Grants, server.TCPGrant{CIDR: g.CIDR, ExpiresAt: g.ExpiresAt})
	}
	return acc, nil
}

// ---------------------------------------------------------------------------
// controlAuth (server.TokenAuthenticator)
// ---------------------------------------------------------------------------

// controlAuth is the store as the control port authenticates clients: a
// token the store refused (store.ErrUnauthorized: unknown, expired, of a
// suspended user) is also server.ErrTokenInvalid, which the control port
// counts as a failed sign-in of the source. A failure of the store itself is
// passed on unmarked and not counted.
type controlAuth struct{ *store.Store }

func (a controlAuth) AuthenticateToken(ctx context.Context, token string) (userID, tokenID, tokenName string, err error) {
	userID, tokenID, tokenName, err = a.Store.AuthenticateToken(ctx, token)
	if errors.Is(err, store.ErrUnauthorized) {
		err = fmt.Errorf("%w: %w", server.ErrTokenInvalid, err)
	}
	return userID, tokenID, tokenName, err
}
