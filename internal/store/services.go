package store

// Package store — services.go
// Permission-gated service/key/policy methods and hot-path lookup helpers.
//
// # ServiceView vs ServiceDetail design decision
//
// ServiceView carries the durable fields the store can read from the DB:
// id, name, type, subdomain, access_mode, api_key_header, created_at.
// It intentionally omits live/runtime fields (hostname, connected,
// remote_port, local_addr) that come from the in-process tunnel registry or
// the auth_domain setting — both are owned by the API/wiring layer (Task 10),
// not the store. Task 10 composes the full response by embedding a ServiceView
// and populating the live fields itself.
//
// ServiceDetail extends ServiceView with the two aggregate fields the API needs
// for the single-service GET: api_key_count and access_policy. These require an
// extra db round-trip so they live only on the detail view, not the list view.

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/tcpsniff"
)

var (
	// ErrInvalidSlug is returned when a slug does not match auth.ValidSlug.
	ErrInvalidSlug = errors.New("store: invalid slug")
	// ErrSlugTaken is returned when another service already uses the slug.
	ErrSlugTaken = errors.New("store: slug already in use")
	// ErrDirectService is returned when a setting that only makes sense for a
	// tunnelled service is changed on the backing row of a direct AI provider.
	ErrDirectService = errors.New("store: service backs a direct AI provider")
	// ErrServiceHasProvider is returned by DeleteService, wrapped with the
	// provider's slug, while an AI provider (tunnel or direct) points at the
	// service.
	ErrServiceHasProvider = db.ErrServiceHasProvider
	// ErrServiceHasMCPSource is returned by DeleteService, wrapped with the
	// source's slug, while a tool source points at the service: the source
	// would otherwise be deleted with it without a word.
	ErrServiceHasMCPSource = db.ErrServiceHasMCPSource
	// ErrUserHasMCPSource is returned by DeleteUser, wrapped with the
	// sources' slugs, while the user owns the service behind a tool source.
	ErrUserHasMCPSource = db.ErrUserHasMCPSource
)

// ServiceView is the durable-fields representation of a service returned by
// ListServices. Live/runtime fields (hostname, connected, remote_port,
// local_addr) are absent here; Task 10 (api handlers) composes them from the
// tunnel registry and auth_domain setting.
type ServiceView struct {
	ID           string
	UserID       string
	Name         string
	Type         string // "http" or "tcp"
	Subdomain    string // "" for tcp or unset http
	AccessMode   string // "open" | "api_key" | "burrow_login"
	APIKeyHeader string // effective header name (default "Authorization")
	GatewayOnly  bool   // reachable through the AI gateway only
	// MaxConnections is a tcp service's cap on connected visitors; 0 is the
	// relay's default.
	MaxConnections int
	CreatedAt      time.Time
	// LastSeen is when a client last served the service, nil when none ever
	// did; LastClient is the name of the client token of that client.
	LastSeen   *time.Time
	LastClient string
}

// ServiceDetail extends ServiceView with the aggregate fields returned by the
// single-service GET endpoint. Task 10 may further overlay live runtime fields.
type ServiceDetail struct {
	ServiceView
	APIKeyCount  int
	AccessPolicy []string // roles; empty slice = deny-all
}

// canConfigure loads the service and checks whether callerID/callerRole may
// configure it. It returns the loaded service so callers avoid a second load.
//
//   - PermServicesConfigureAny → allow unconditionally (admin)
//   - PermServicesConfigureOwn → allow only when service.UserID == callerID
//   - otherwise → ErrForbidden
//
// db.ErrNotFound is propagated unchanged (maps to 404, not 403).
func (s *Store) canConfigure(ctx context.Context, callerID, callerRole, serviceID string) (db.Service, error) {
	svc, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return db.Service{}, err // includes db.ErrNotFound
	}
	if authz.Can(callerRole, authz.PermServicesConfigureAny) {
		return svc, nil
	}
	if authz.Can(callerRole, authz.PermServicesConfigureOwn) && svc.UserID == callerID {
		return svc, nil
	}
	return db.Service{}, ErrForbidden
}

// DeleteService removes a service and returns its name for the audit entry.
// Same gate as SetServiceAccessMode: the owner, or a role that may configure
// any service. ErrForbidden for anyone else (the API answers 404, as for a
// service that is not there).
//
// A service that an AI provider points at stays: the error wraps
// ErrServiceHasProvider and names the provider. The provider rows reference
// the service with ON DELETE CASCADE, so deleting the service would take a
// tunnel provider with it without a word; a direct provider's backing row is
// deleted with its provider. A race with a new model that targets the
// provider comes back from the database as ErrProviderInUse. A service that a
// tool source points at stays the same way: the error wraps
// ErrServiceHasMCPSource and names the source (the source of a remote kind is
// deleted with DeleteMCPSource, together with its hidden backing row).
func (s *Store) DeleteService(ctx context.Context, callerID, callerRole, serviceID string) (string, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return "", err
	}
	if err := s.q.DeleteService(ctx, serviceID); err != nil {
		return "", err
	}
	return svc.Name, nil
}

// ListServices returns all services visible to the caller.
// Admins (PermTunnelsReadAny re-used as the read-any signal, matching v0.2
// pattern — admin has all :any perms) see every service; regular users see
// only their own. The list is returned as ServiceViews (live fields deferred
// to Task 10).
func (s *Store) ListServices(ctx context.Context, callerID, callerRole string) ([]ServiceView, error) {
	var rows []db.Service
	var err error
	if authz.Can(callerRole, authz.PermServicesConfigureAny) {
		rows, err = s.q.ListAllServices(ctx)
	} else {
		rows, err = s.q.ListServicesByUser(ctx, callerID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]ServiceView, len(rows))
	for i, r := range rows {
		out[i] = serviceToView(r)
	}
	return out, nil
}

// GetService returns the full ServiceDetail for a single service.
// The caller must have configure:own (owner) or configure:any (admin).
// api_key_count and access_policy are populated; live runtime fields are
// left at their zero values for Task 10 to populate.
func (s *Store) GetService(ctx context.Context, callerID, callerRole, serviceID string) (ServiceDetail, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return ServiceDetail{}, err
	}
	keys, err := s.q.ListServiceAPIKeys(ctx, svc.ID)
	if err != nil {
		return ServiceDetail{}, err
	}
	policy, err := s.q.GetAccessPolicy(ctx, svc.ID)
	if err != nil {
		return ServiceDetail{}, err
	}
	return ServiceDetail{
		ServiceView:  serviceToView(svc),
		APIKeyCount:  len(keys),
		AccessPolicy: policy,
	}, nil
}

// SetServiceAccessMode validates and applies a new access mode to the given
// service. Gate: configure:own (owner) or configure:any (admin).
// Validation order: gate → mode enum → per-type constraint → mtls_ca_pem.
//   - ErrForbidden if not authorized
//   - db.ErrNotFound if the service does not exist
//   - ErrInvalidAccessMode if mode ∉ {open,api_key,burrow_login,mtls,ip,private}
//   - ErrTCPAccessMode if the service is tcp and mode ∉ {open,ip,private}
//   - ErrHTTPAccessMode if the service is http and mode ∈ {ip,private}
//   - ErrServiceNotHTTP if mode ≠ "open" and the service is of another type
//   - ErrMTLSCARequired if mode == "mtls" and caPEM is empty
//   - ErrInvalidMTLSCAPEM if mode == "mtls" and caPEM has no valid block
//
// An empty header defaults to "Authorization" for api_key mode (stored in DB).
// For non-api_key modes the header is written as "Authorization" as well,
// keeping the DB column consistent.
//
// caPEM is the operator-supplied trust anchor for mtls mode. When mode is
// "mtls" the store validates it contains at least one CERTIFICATE block and
// persists it via SetServiceMTLSCAPEM; for any other mode caPEM is ignored.
// Switching AWAY from mtls does NOT clear the existing CA blob — that lets
// operators flip back to mtls later without re-pasting the CA. To explicitly
// clear, set access_mode to mtls then immediately switch back, or call the
// dedicated DB method.
func (s *Store) SetServiceAccessMode(ctx context.Context, callerID, callerRole, serviceID, mode, header string, caPEM []byte) error {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return err
	}
	// A direct provider's backing row stays in api_key mode: /ai/ refuses
	// any other mode, and there is no tunnel the other modes could protect.
	if svc.Type == "direct" {
		return ErrDirectService
	}
	switch mode {
	case "open", "api_key", "burrow_login", "mtls", "ip", "private":
	default:
		return ErrInvalidAccessMode
	}
	tcpMode := mode == "open" || mode == "ip" || mode == "private"
	switch {
	case svc.Type == "tcp" && !tcpMode:
		return ErrTCPAccessMode
	case svc.Type == "http" && (mode == "ip" || mode == "private"):
		return ErrHTTPAccessMode
	case svc.Type != "http" && svc.Type != "tcp" && mode != "open":
		return ErrServiceNotHTTP
	}
	if mode == "mtls" {
		if len(caPEM) == 0 {
			return ErrMTLSCARequired
		}
		if !validateCAPEM(caPEM) {
			return ErrInvalidMTLSCAPEM
		}
	}
	if header == "" {
		header = "Authorization"
	}
	oldMode := svc.AccessMode
	if err := s.q.SetServiceAccessMode(ctx, serviceID, mode, header); err != nil {
		return err
	}
	if mode == "mtls" {
		if err := s.q.SetServiceMTLSCAPEM(ctx, serviceID, string(caPEM)); err != nil {
			return err
		}
	}
	ctx = withAuditDeadline(ctx) // one deadline for the rows of this mutation
	action := audit.ActionServiceAccessModeUpdate
	if svc.Type == "tcp" {
		action = audit.ActionServiceTCPAccessUpdate
	}
	s.emitAudit(ctx, action, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]string{"from": oldMode, "to": mode})
	})
	if mode == "mtls" {
		s.emitAudit(ctx, audit.ActionMtlsCAUpdate, func(e *audit.Event) {
			e.SubjectID = serviceID
			e.SubjectLabel = svc.Name
		})
	}
	return nil
}

// SetServiceMaxConnections sets the cap on connected visitors of a tcp
// service: 1 to server.MaxTCPMaxConnections, or 0 for the relay's default.
// Same permission rule as SetServiceAccessMode. ErrServiceNotTCP for any
// other type, ErrInvalidMaxConnections for a number out of range. Returns the
// previous cap.
func (s *Store) SetServiceMaxConnections(ctx context.Context, callerID, callerRole, serviceID string, n int) (int, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return 0, err
	}
	if svc.Type != "tcp" {
		return 0, ErrServiceNotTCP
	}
	if n < 0 || n > MaxTCPMaxConnections {
		return 0, ErrInvalidMaxConnections
	}
	if err := s.q.SetServiceMaxConnections(ctx, serviceID, n); err != nil {
		return 0, err
	}
	s.emitAudit(withAuditDeadline(ctx), audit.ActionServiceMaxConnectionsUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]int{"from": svc.MaxConnections, "to": n})
	})
	return svc.MaxConnections, nil
}

// The idle timeout a tcp service may set, in seconds: 0 (never) or
// MinTCPIdleTimeoutS to MaxTCPIdleTimeoutS. They mirror
// server.MinTCPIdleTimeout and server.MaxTCPIdleTimeout.
const (
	MinTCPIdleTimeoutS = 60
	MaxTCPIdleTimeoutS = 86400
)

// TCPSettings are the per-service settings of a tcp service that shape its
// connections: the PROXY protocol header its client writes ("" or "v2") and
// the idle timeout in seconds (0: never).
type TCPSettings struct {
	ProxyProtocol string `json:"proxy_protocol"`
	IdleTimeoutS  int    `json:"idle_timeout_s"`
}

// TCPSettingsUpdate names the settings to change; a nil field is kept.
type TCPSettingsUpdate struct {
	ProxyProtocol *string
	IdleTimeoutS  *int
}

// ServiceTCPSettings reads the tcp settings of a service. Same permission
// rule as SetServiceMaxConnections; ErrServiceNotTCP for any other type.
func (s *Store) ServiceTCPSettings(ctx context.Context, callerID, callerRole, serviceID string) (TCPSettings, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return TCPSettings{}, err
	}
	if svc.Type != "tcp" {
		return TCPSettings{}, ErrServiceNotTCP
	}
	return TCPSettings{ProxyProtocol: svc.ProxyProtocol, IdleTimeoutS: svc.IdleTimeoutS}, nil
}

// SetServiceTCPSettings changes the tcp settings of a service named in u and
// keeps the others. Same permission rule as SetServiceMaxConnections.
// ErrServiceNotTCP for any other type, ErrInvalidProxyProtocol and
// ErrInvalidIdleTimeout for a value out of range (nothing is written then).
// Every write is audited as service.tcp_settings.update with both settings
// before and after. It returns them.
func (s *Store) SetServiceTCPSettings(ctx context.Context, callerID, callerRole, serviceID string, u TCPSettingsUpdate) (before, after TCPSettings, err error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return TCPSettings{}, TCPSettings{}, err
	}
	if svc.Type != "tcp" {
		return TCPSettings{}, TCPSettings{}, ErrServiceNotTCP
	}
	if p := u.ProxyProtocol; p != nil && *p != "" && *p != "v2" {
		return TCPSettings{}, TCPSettings{}, ErrInvalidProxyProtocol
	}
	if n := u.IdleTimeoutS; n != nil && *n != 0 && (*n < MinTCPIdleTimeoutS || *n > MaxTCPIdleTimeoutS) {
		return TCPSettings{}, TCPSettings{}, ErrInvalidIdleTimeout
	}
	// The update writes only the named fields, in one statement; "before"
	// is the row as read above, "after" the row as it is now.
	before = TCPSettings{ProxyProtocol: svc.ProxyProtocol, IdleTimeoutS: svc.IdleTimeoutS}
	if err := s.q.SetServiceTCPSettings(ctx, serviceID, u.ProxyProtocol, u.IdleTimeoutS); err != nil {
		return TCPSettings{}, TCPSettings{}, err
	}
	now, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return TCPSettings{}, TCPSettings{}, err
	}
	after = TCPSettings{ProxyProtocol: now.ProxyProtocol, IdleTimeoutS: now.IdleTimeoutS}
	s.emitAudit(withAuditDeadline(ctx), audit.ActionServiceTCPSettingsUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]TCPSettings{"from": before, "to": after})
	})
	return before, after, nil
}

// TCPHostname is a tcp service's TLS passthrough host name ("" for none)
// with what decides whether it reaches the service: its slug (the other
// name it has under BURROW_TCP_DOMAIN) and its access mode (a private
// service has no names on the passthrough listener).
// HostnameOnly says the relay opens no public port for the service while
// a host name reaches it.
type TCPHostname struct {
	Hostname     string
	Slug         string
	AccessMode   string
	HostnameOnly bool
}

func tcpHostnameOf(svc db.Service) TCPHostname {
	return TCPHostname{Hostname: svc.TCPHostname, Slug: svc.Subdomain, AccessMode: svc.AccessMode, HostnameOnly: svc.TCPHostnameOnly}
}

// NormalizeTCPHostname is the form a TLS passthrough host name is stored in:
// tcpsniff.NormalizeHostname (lower case, no trailing dot), and at least two
// labels of letters, digits and '-' whose last is not all digits, so that
// it is a DNS name a certificate can be for, never an address or a
// wildcard. ok is false for anything else.
func NormalizeTCPHostname(s string) (string, bool) {
	n, ok := tcpsniff.NormalizeHostname(strings.TrimSpace(s))
	if !ok || strings.Contains(n, "_") {
		return "", false
	}
	labels := strings.Split(n, ".")
	if len(labels) < 2 {
		return "", false
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return "", false
	}
	return n, true
}

// ServiceTCPHostname reads the TLS passthrough host name of a tcp service.
// Same permission rule as SetServiceMaxConnections; ErrServiceNotTCP for any
// other type.
func (s *Store) ServiceTCPHostname(ctx context.Context, callerID, callerRole, serviceID string) (TCPHostname, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return TCPHostname{}, err
	}
	if svc.Type != "tcp" {
		return TCPHostname{}, ErrServiceNotTCP
	}
	return tcpHostnameOf(svc), nil
}

// SetServiceTCPHostname sets the TLS passthrough host name of a tcp service
// ("" clears it), stored in NormalizeTCPHostname form. It widens who can
// reach the service (a client that names it on the passthrough listener),
// so the API puts it behind a recent confirmation and off the MCP tools.
// Same permission rule as SetServiceMaxConnections, and a name (not a clear)
// needs services:configure:any (ErrTCPHostnameNeedsAny). ErrServiceNotTCP for
// any other type, ErrInvalidTCPHostname for a name that is none,
// ErrTCPHostnameReserved for one a custom domain covers,
// ErrTCPHostnameTaken when another service holds it. Every write is
// audited as service.tcp_hostname.update {from, to}.
func (s *Store) SetServiceTCPHostname(ctx context.Context, callerID, callerRole, serviceID, hostname string) (before, after TCPHostname, err error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return TCPHostname{}, TCPHostname{}, err
	}
	if svc.Type != "tcp" {
		return TCPHostname{}, TCPHostname{}, ErrServiceNotTCP
	}
	name := ""
	if hostname != "" {
		// A name of its own is a way in that is not under the tcp domain
		// the operator chose: an administrator's decision. Clearing one
		// narrows access, which the owner may.
		if !authz.Can(callerRole, authz.PermServicesConfigureAny) {
			return TCPHostname{}, TCPHostname{}, ErrTCPHostnameNeedsAny
		}
		var ok bool
		if name, ok = NormalizeTCPHostname(hostname); !ok {
			return TCPHostname{}, TCPHostname{}, ErrInvalidTCPHostname
		}
		covered, err := s.q.CustomDomainCovers(ctx, name)
		if err != nil {
			return TCPHostname{}, TCPHostname{}, err
		}
		if covered {
			return TCPHostname{}, TCPHostname{}, ErrTCPHostnameReserved
		}
	}
	before = tcpHostnameOf(svc)
	if err := s.q.SetServiceTCPHostname(ctx, serviceID, name); err != nil {
		if errors.Is(err, db.ErrTCPHostnameTaken) {
			return TCPHostname{}, TCPHostname{}, ErrTCPHostnameTaken
		}
		return TCPHostname{}, TCPHostname{}, err
	}
	now, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return TCPHostname{}, TCPHostname{}, err
	}
	after = tcpHostnameOf(now)
	s.emitAudit(withAuditDeadline(ctx), audit.ActionServiceTCPHostnameUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]string{"from": before.Hostname, "to": after.Hostname})
	})
	return before, after, nil
}

// SetServiceTCPHostnameOnly sets whether a tcp service is reached by its
// host name only: the relay then opens no public port for it while a host
// name reaches it on the TLS passthrough listener. The caller (the API)
// checks that the relay has the listener and the service a name; the relay
// opens the port again whenever none reaches it. Same permission rule as
// SetServiceMaxConnections; ErrServiceNotTCP for any other type. Every write
// is audited as service.tcp_hostname_only.update {from, to}.
func (s *Store) SetServiceTCPHostnameOnly(ctx context.Context, callerID, callerRole, serviceID string, only bool) (before, after TCPHostname, err error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return TCPHostname{}, TCPHostname{}, err
	}
	if svc.Type != "tcp" {
		return TCPHostname{}, TCPHostname{}, ErrServiceNotTCP
	}
	before = tcpHostnameOf(svc)
	if err := s.q.SetServiceTCPHostnameOnly(ctx, serviceID, only); err != nil {
		return TCPHostname{}, TCPHostname{}, err
	}
	now, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return TCPHostname{}, TCPHostname{}, err
	}
	after = tcpHostnameOf(now)
	s.emitAudit(withAuditDeadline(ctx), audit.ActionServiceTCPHostnameOnlyUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]bool{"from": before.HostnameOnly, "to": after.HostnameOnly})
	})
	return before, after, nil
}

// MaxTCPMaxConnections is the most visitors a tcp service may let connect at
// once; it is server.MaxTCPMaxConnections (the store does not import the
// relay).
const MaxTCPMaxConnections = 256

// SetServiceSlug changes the URL segment of an http service. Same permission
// rule as SetServiceAccessMode (owner or a role that may configure any
// service). Returns the previous slug so the caller can audit the change, and
// whether the service is gateway-only (then it has no /svc/ address to report).
func (s *Store) SetServiceSlug(ctx context.Context, callerID, callerRole, serviceID, slug string) (string, bool, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return "", false, err
	}
	if svc.Type == "direct" {
		return "", false, ErrDirectService
	}
	if svc.Type != "http" {
		return "", false, ErrServiceNotHTTP
	}
	if !auth.ValidSlug(slug) {
		return "", false, ErrInvalidSlug
	}
	if svc.Subdomain == slug {
		return slug, svc.GatewayOnly, nil
	}
	if err := s.q.SetServiceSubdomain(ctx, serviceID, slug); err != nil {
		// isUniqueViolation matches SQLite; Postgres reports
		// "violates unique constraint".
		if isUniqueViolation(err) || containsStr(err.Error(), "unique constraint") {
			return "", false, ErrSlugTaken
		}
		return "", false, err
	}
	return svc.Subdomain, svc.GatewayOnly, nil
}

// SetServiceGatewayOnly closes (or reopens) every direct door of an http
// service: with it set the service is reachable through the AI gateway only.
// Same permission rule as SetServiceAccessMode.
func (s *Store) SetServiceGatewayOnly(ctx context.Context, callerID, callerRole, serviceID string, on bool) error {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return err
	}
	if svc.Type == "direct" {
		return ErrDirectService
	}
	if svc.Type != "http" {
		return ErrServiceNotHTTP
	}
	return s.q.SetServiceGatewayOnly(ctx, serviceID, on)
}

// SuggestSlug returns a generated slug that no service uses right now. The
// result is a suggestion only: uniqueness is enforced when it is saved.
func (s *Store) SuggestSlug(ctx context.Context) (string, error) {
	for i := 0; i < 8; i++ {
		slug, err := auth.GenerateSlug()
		if err != nil {
			return "", err
		}
		if _, err := s.q.GetServiceBySubdomain(ctx, slug); errors.Is(err, db.ErrNotFound) {
			return slug, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("store: no free slug after 8 attempts")
}

// validateCAPEM reports whether pem contains at least one parseable
// CERTIFICATE block.
func validateCAPEM(in []byte) bool {
	rest := in
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" {
			if _, err := x509.ParseCertificate(block.Bytes); err == nil {
				return true
			}
		}
		if len(next) == 0 {
			return false
		}
		rest = next
	}
}

// ValidHTTPHeaderName reports whether s is a non-empty RFC 7230 token, the
// grammar of an HTTP header field name:
//
//	tchar = "!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." / "^"
//	      / "_" / "`" / "|" / "~" / DIGIT / ALPHA
//
// In particular: NO colon, NO whitespace, NO control characters. It is the
// rule for a service's api_key_header. Every surface that takes one from a
// caller (the JSON API, the MCP tool) checks it before SetServiceAccessMode,
// which stores the name as it is given.
func ValidHTTPHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'':
		case c == '*' || c == '+' || c == '-' || c == '.' || c == '^' || c == '_':
		case c == '`' || c == '|' || c == '~':
		default:
			return false
		}
	}
	return true
}

// ListAPIKeys returns all API keys for the given service.
// Gate: configure:own (owner) or configure:any (admin).
// Key hashes are included in the returned rows (the caller is trusted).
func (s *Store) ListAPIKeys(ctx context.Context, callerID, callerRole, serviceID string) ([]db.ServiceAPIKey, error) {
	if _, err := s.canConfigure(ctx, callerID, callerRole, serviceID); err != nil {
		return nil, err
	}
	return s.q.ListServiceAPIKeys(ctx, serviceID)
}

// CreateAPIKey generates and stores a new service API key.
// Gate: configure:own (owner) or configure:any (admin).
// Returns: (id, plaintext, error).
//   - ErrNameRequired if name is empty
//   - ErrForbidden if not authorized
//   - db.ErrNotFound if the service does not exist
//
// Only the sha256-hex hash is persisted; plaintext is returned once and never
// stored. Plaintext has the "buk_" prefix.
func (s *Store) CreateAPIKey(ctx context.Context, callerID, callerRole, serviceID, name string) (id, plaintext string, err error) {
	if name == "" {
		return "", "", ErrNameRequired
	}
	if _, err := s.canConfigure(ctx, callerID, callerRole, serviceID); err != nil {
		return "", "", err
	}
	pt, hash, err := auth.GenerateAPIKey()
	if err != nil {
		return "", "", err
	}
	id = uuid.NewString()
	if err := s.q.CreateServiceAPIKey(ctx, db.ServiceAPIKey{
		ID:        id,
		ServiceID: serviceID,
		Name:      name,
		KeyHash:   hash,
	}); err != nil {
		return "", "", err
	}
	keyID := id
	s.emitAudit(ctx, audit.ActionServiceAPIKeyCreate, func(e *audit.Event) {
		e.SubjectID = keyID
		e.SubjectLabel = name
		e.Payload = audit.MustJSON(map[string]string{"service_id": serviceID})
	})
	return id, pt, nil
}

// DeleteAPIKey removes a service API key.
// Gate: configure:own (owner) or configure:any (admin).
// Propagates db.ErrNotFound if the key does not exist.
func (s *Store) DeleteAPIKey(ctx context.Context, callerID, callerRole, serviceID, keyID string) error {
	if _, err := s.canConfigure(ctx, callerID, callerRole, serviceID); err != nil {
		return err
	}
	// Capture key name for the audit row before delete.
	var keyName string
	if keys, err := s.q.ListServiceAPIKeys(ctx, serviceID); err == nil {
		for _, k := range keys {
			if k.ID == keyID {
				keyName = k.Name
				break
			}
		}
	}
	if err := s.q.DeleteServiceAPIKey(ctx, keyID, serviceID); err != nil {
		return err
	}
	s.emitAudit(ctx, audit.ActionServiceAPIKeyRevoke, func(e *audit.Event) {
		e.SubjectID = keyID
		e.SubjectLabel = keyName
		e.Payload = audit.MustJSON(map[string]string{"service_id": serviceID})
	})
	return nil
}

// GetAccessPolicy returns the access policy roles for the given service.
// Gate: configure:own (owner) or configure:any (admin).
// Returns a non-nil empty slice when no roles are set (deny-all).
func (s *Store) GetAccessPolicy(ctx context.Context, callerID, callerRole, serviceID string) ([]string, error) {
	if _, err := s.canConfigure(ctx, callerID, callerRole, serviceID); err != nil {
		return nil, err
	}
	return s.q.GetAccessPolicy(ctx, serviceID)
}

// SetAccessPolicy replaces the full access policy for the given service.
// Gate: configure:own (owner) or configure:any (admin).
// Each role must be a built-in role or a stored custom role; unknown roles
// return ErrUnknownRole (maps to HTTP 400). An empty roles slice means
// deny-all.
func (s *Store) SetAccessPolicy(ctx context.Context, callerID, callerRole, serviceID string, roles []string) error {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return err
	}
	for _, r := range roles {
		if ok, err := s.roleExists(ctx, r); err != nil {
			return err
		} else if !ok {
			return ErrUnknownRole
		}
	}
	if err := s.q.SetAccessPolicy(ctx, serviceID, roles); err != nil {
		return err
	}
	s.emitAudit(ctx, audit.ActionServiceAccessPolicyUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]any{"roles": roles})
	})
	return nil
}

// ValidateAPIKey checks the presented plaintext key against the service's keys
// and returns the matched key's id. ok is false (with a nil error) when no key
// matches. This is a hot-path helper used by the proxy middleware; it has NO
// permission gate (the caller has already proven service identity via
// routing).
//
// On a hit: best-effort touch last_used (touch error ignored).
// On other errors than db.ErrNotFound: propagates.
func (s *Store) ValidateAPIKey(ctx context.Context, serviceID, presented string) (string, bool, error) {
	hash := auth.HashToken(presented)
	key, err := s.q.GetServiceAPIKeyByHash(ctx, serviceID, hash)
	if err != nil {
		if err == db.ErrNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	// Best-effort: update last_used without failing validation on error.
	_ = s.q.TouchServiceAPIKey(ctx, key.ID)
	return key.ID, true, nil
}

// ServiceForSubdomain returns the service registered for the given subdomain.
// This is a hot-path helper used by the proxy router and the login gate; it
// has NO permission gate. A tcp service's slug names it for forwards only:
// it is never an address over HTTP, so it is not found here (db.ErrNotFound,
// as for a slug nobody has). Propagates db.ErrNotFound.
func (s *Store) ServiceForSubdomain(ctx context.Context, sub string) (db.Service, error) {
	svc, err := s.q.GetServiceBySubdomain(ctx, sub)
	if err == nil && svc.Type == "tcp" {
		return db.Service{}, db.ErrNotFound
	}
	return svc, err
}

// ServiceByID returns the service row for id. Hot-path helper used by the
// proxy router; it has NO permission gate. Propagates db.ErrNotFound.
func (s *Store) ServiceByID(ctx context.Context, id string) (db.Service, error) {
	return s.q.GetServiceByID(ctx, id)
}

// GetServiceIPGeo returns the ip-geo policy config for the given service.
// This is a hot-path helper used by the proxy router; it has NO permission gate.
// Returns a zero-value config (Enabled=false, empty slices) when no row exists.
func (s *Store) GetServiceIPGeo(ctx context.Context, serviceID string) (db.ServiceIPGeoConfig, error) {
	return s.q.GetServiceIPGeo(ctx, serviceID)
}

// RoleAllowed reports whether the given role is in the service's access policy.
// This is a hot-path helper used by the proxy auth middleware; it has NO
// permission gate.
// An empty policy (deny-all) returns false.
func (s *Store) RoleAllowed(ctx context.Context, serviceID, role string) (bool, error) {
	policy, err := s.q.GetAccessPolicy(ctx, serviceID)
	if err != nil {
		return false, err
	}
	for _, r := range policy {
		if r == role {
			return true, nil
		}
	}
	return false, nil
}

// serviceToView maps a db.Service row to a ServiceView (durable fields only).
func serviceToView(s db.Service) ServiceView {
	return ServiceView{
		ID:           s.ID,
		UserID:       s.UserID,
		Name:         s.Name,
		Type:         s.Type,
		Subdomain:    s.Subdomain,
		AccessMode:   s.AccessMode,
		APIKeyHeader: s.APIKeyHeader,
		GatewayOnly:  s.GatewayOnly,
		CreatedAt:    s.CreatedAt,
		LastSeen:     s.LastSeen,
		LastClient:   s.LastClient,

		MaxConnections: s.MaxConnections,
	}
}

// TouchServiceLastSeen notes that the client that logged in with the token
// named tokenName is serving the service, or just stopped. db.ErrNotFound
// when the service was deleted meanwhile. It is not scoped to a caller: the
// control server calls it for a tunnel it bound to the service itself.
func (s *Store) TouchServiceLastSeen(ctx context.Context, serviceID, tokenName string) error {
	return s.q.TouchServiceLastSeen(ctx, serviceID, tokenName)
}

// CreateService is the admin-only pre-provisioning surface exposed via
// POST /api/v1/services (v0.5.2 P3.6). It delegates straight to the DB layer
// — permission checking happens in the API (services:configure:any) so this
// method intentionally has no per-caller authz; tests that want to bypass
// the API entirely call it on *db.DB directly. Returns db.ErrDuplicateService
// on UNIQUE-constraint violations, mapped to HTTP 409 by the handler.
func (s *Store) CreateService(ctx context.Context, svc db.Service) error {
	if err := s.q.CreateService(ctx, svc); err != nil {
		return err
	}
	if s.events != nil {
		s.events.EmitServiceCreated(svc.ID, svc.Name, svc.Type, svc.AccessMode)
	}
	return nil
}

// ServiceAccessMode returns the durable access mode of a service. It is the
// source of truth the dashboard's Tunnels and Client views must show; the
// per-session tunnels row only carries the legacy v0.2 value.
func (s *Store) ServiceAccessMode(ctx context.Context, serviceID string) (string, error) {
	svc, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return "", err
	}
	return svc.AccessMode, nil
}
