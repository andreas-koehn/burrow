package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/db"
)

// Tool sources (MCP gateway). The store validates and writes; it does no
// network calls (the API does the save-time DNS check) and holds no
// credential: CredentialSlot names a vault slot.

var (
	// ErrInvalidMCPSourceSlug: the slug is malformed or reserved.
	ErrInvalidMCPSourceSlug = errors.New("store: invalid tool source slug")
	// ErrMCPSourceExists: the slug is taken or the service already backs a source.
	ErrMCPSourceExists = errors.New("store: tool source slug or service already in use")
	// ErrMCPSourceNotFound: no source has that slug.
	ErrMCPSourceNotFound = errors.New("store: tool source not found")
	// ErrMCPSourceBusy: concurrent writers kept changing the source; try again.
	ErrMCPSourceBusy = errors.New("store: tool source is being changed concurrently")
	// ErrMCPSourceService: a tunnel source needs an existing http service.
	ErrMCPSourceService = errors.New("store: a tunnel tool source needs an http service")
	// ErrInvalidMCPSource is wrapped with the reason a source's settings were
	// refused; the reason is safe to show to an admin.
	ErrInvalidMCPSource = errors.New("store: invalid tool source")
)

// mcpSlugRe is the slug rule of D11 (1-24 characters of [a-z0-9-]) with one
// addition: no hyphen at either end.
var mcpSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$`)

// MCPSourceSlugRule is the 400 message for a malformed or reserved slug.
const MCPSourceSlugRule = `slug must be 1-24 characters: a-z, 0-9 and hyphens, starting and ending with a letter or digit; "burrow" and "v1" are reserved`

// ValidMCPSourceSlug reports whether s can name a tool source. "burrow" is
// the management server (/mcp/burrow) and "v1" is reserved as for providers.
func ValidMCPSourceSlug(s string) bool {
	return mcpSlugRe.MatchString(s) && s != "burrow" && s != "v1"
}

// Limits of a source.
const (
	DefaultMCPTimeoutS   = 60
	MaxMCPTimeoutS       = 300
	MaxMCPRPM            = 100000
	MaxMCPConcurrency    = 1000
	maxMCPSourceName     = 120
	maxMCPURL            = 2048
	maxMCPProtocolLength = 32
)

var protocolVersionRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func invalidMCP(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidMCPSource, reason) }

// MCPSourceInput is what the API passes to create a source. Kind is "remote"
// (URL and the credential fields apply) or "tunnel" (ServiceID applies). A
// nil limit or flag takes its default: no rpm and concurrency limit, a 60 s
// timeout, enabled.
type MCPSourceInput struct {
	Slug, Name, Kind, ServiceID                                  string
	URL, CredentialSlot, AuthHeader, AuthFormat, ProtocolVersion string
	RPM, MaxConcurrent, TimeoutS                                 *int
	Enabled                                                      *bool
}

// MCPSourceUpdate holds the fields PUT changes; nil keeps the stored value.
// A pointer to "" clears a credential slot or the protocol revision.
type MCPSourceUpdate struct {
	Name, URL, CredentialSlot, AuthHeader, AuthFormat, ProtocolVersion *string
	RPM, MaxConcurrent, TimeoutS                                       *int
	Enabled                                                            *bool
}

func validMCPLimits(rpm, conc, timeout *int) error {
	if rpm != nil && (*rpm < 0 || *rpm > MaxMCPRPM) {
		return invalidMCP("rpm must be between 0 and 100000")
	}
	if conc != nil && (*conc < 0 || *conc > MaxMCPConcurrency) {
		return invalidMCP("max concurrent calls must be between 0 and 1000")
	}
	if timeout != nil && (*timeout < 1 || *timeout > MaxMCPTimeoutS) {
		return invalidMCP("timeout must be between 1 and 300 seconds")
	}
	return nil
}

func validMCPName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", invalidMCP("name is required")
	}
	if len(name) > maxMCPSourceName || hasControl(name) {
		return "", invalidMCP("name must be at most 120 characters without control characters")
	}
	return name, nil
}

// validMCPUpstream checks the settings of a remote source's upstream: the
// URL, one credential slot (or none: a public server), the header that
// carries it and its format. Reasons never repeat the URL or a header value.
func validMCPUpstream(s db.MCPSource) error {
	if _, err := aiprovider.ValidateBaseURL(s.URL); err != nil || len(s.URL) > maxMCPURL {
		return invalidMCP("url must be an https URL without credentials, query or fragment")
	}
	if s.CredentialSlot != "" && !slotRe.MatchString(s.CredentialSlot) {
		return invalidMCP("credential slot must be one name of 1-32 characters: A-Z, 0-9, _")
	}
	if !headerNameRe.MatchString(s.AuthHeader) || isRelayHeader(strings.ToLower(s.AuthHeader)) {
		return invalidMCP("auth header is not a valid header name")
	}
	// Exactly one placeholder: the relay fills in the first one only.
	if strings.Count(s.AuthFormat, "{key}") != 1 || hasControl(s.AuthFormat) || len(s.AuthFormat) > maxAuthFormat {
		return invalidMCP(`auth format must contain "{key}" once, at most 128 characters, no control characters`)
	}
	return nil
}

func validProtocolVersion(v string) error {
	if v != "" && (len(v) > maxMCPProtocolLength || !protocolVersionRe.MatchString(v)) {
		return invalidMCP("protocol version must be empty or a revision date such as 2025-06-18")
	}
	return nil
}

func firstInt(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

// CreateMCPSource adds a tool source. A remote source gets a hidden backing
// service row owned by ownerID (type "direct", never listed as a service); a
// tunnel source is bound to the existing http service in.ServiceID, whose
// direct doors it closes (gateway-only). Callers gate on the permission to
// manage sources, and, for a tunnel source, on the right to configure the
// service. Does no network calls.
func (s *Store) CreateMCPSource(ctx context.Context, ownerID string, in MCPSourceInput) (db.MCPSource, error) {
	if !ValidMCPSourceSlug(in.Slug) {
		return db.MCPSource{}, ErrInvalidMCPSourceSlug
	}
	name, err := validMCPName(in.Name)
	if err != nil {
		return db.MCPSource{}, err
	}
	if err := validMCPLimits(in.RPM, in.MaxConcurrent, in.TimeoutS); err != nil {
		return db.MCPSource{}, err
	}
	if err := validProtocolVersion(in.ProtocolVersion); err != nil {
		return db.MCPSource{}, err
	}
	src := db.MCPSource{
		Slug: in.Slug, Name: name, Kind: in.Kind, ProtocolVersion: in.ProtocolVersion,
		AuthHeader: "Authorization", AuthFormat: "Bearer {key}",
		RPM: firstInt(in.RPM, 0), MaxConcurrent: firstInt(in.MaxConcurrent, 0), TimeoutS: firstInt(in.TimeoutS, DefaultMCPTimeoutS),
		Enabled: in.Enabled == nil || *in.Enabled,
	}
	switch in.Kind {
	case "remote":
		if in.ServiceID != "" {
			return db.MCPSource{}, invalidMCP("service_id is for kind 'tunnel'")
		}
		src.URL, src.CredentialSlot = in.URL, in.CredentialSlot
		if in.AuthHeader != "" {
			src.AuthHeader = in.AuthHeader
		}
		if in.AuthFormat != "" {
			src.AuthFormat = in.AuthFormat
		}
		if err := validMCPUpstream(src); err != nil {
			return db.MCPSource{}, err
		}
		// Named after the slug, which never changes: the row is the source's
		// own and no tunnel can take the name (GetOrCreateService refuses it).
		svc := db.Service{ID: uuid.NewString(), UserID: ownerID, Name: "mcp:" + in.Slug, Type: "direct", AccessMode: "api_key", APIKeyHeader: "Authorization"}
		if err := s.q.CreateRemoteMCPSource(ctx, svc, src); err != nil {
			return db.MCPSource{}, mapMCPDBErr(err)
		}
	case "tunnel":
		if in.URL != "" || in.CredentialSlot != "" || in.AuthHeader != "" || in.AuthFormat != "" {
			return db.MCPSource{}, invalidMCP("url, credential_slot, auth_header and auth_format are for kind 'remote'")
		}
		svc, err := s.q.GetServiceByID(ctx, in.ServiceID)
		if errors.Is(err, db.ErrNotFound) {
			return db.MCPSource{}, ErrMCPSourceService
		}
		if err != nil {
			return db.MCPSource{}, err
		}
		if svc.Type != "http" {
			return db.MCPSource{}, ErrMCPSourceService
		}
		src.ServiceID = svc.ID
		if err := s.q.CreateTunnelMCPSource(ctx, src); err != nil {
			return db.MCPSource{}, mapMCPDBErr(err)
		}
	default:
		return db.MCPSource{}, invalidMCP("kind must be 'remote' or 'tunnel'")
	}
	return s.q.GetMCPSource(ctx, in.Slug)
}

func mapMCPDBErr(err error) error {
	switch {
	case errors.Is(err, db.ErrDuplicateMCPSource):
		return ErrMCPSourceExists
	case errors.Is(err, db.ErrNotFound):
		return ErrMCPSourceService // the service vanished under the create
	}
	return err
}

// UpdateMCPSource changes the editable fields of a source: a nil field keeps
// its stored value. The URL and the credential fields belong to a remote
// source; naming one on a tunnel source is refused. Kind, slug and service
// never change. The merge with the stored values runs inside the database's
// read-and-write step, so a concurrent update of other fields is kept.
//
// A change of URL or credential slot voids the approvals of the source's
// tools: repending is how many approved tools went back to pending (see
// db.ModifyMCPSource).
func (s *Store) UpdateMCPSource(ctx context.Context, slug string, u MCPSourceUpdate) (src db.MCPSource, repending int, err error) {
	if err := validMCPLimits(u.RPM, u.MaxConcurrent, u.TimeoutS); err != nil {
		return db.MCPSource{}, 0, err
	}
	if u.ProtocolVersion != nil {
		if err := validProtocolVersion(*u.ProtocolVersion); err != nil {
			return db.MCPSource{}, 0, err
		}
	}
	repending, err = s.q.ModifyMCPSource(ctx, slug, func(p db.MCPSource) (db.MCPSource, error) {
		if u.Name != nil {
			name, err := validMCPName(*u.Name)
			if err != nil {
				return p, err
			}
			p.Name = name
		}
		if p.Kind == "tunnel" && (u.URL != nil || u.CredentialSlot != nil || u.AuthHeader != nil || u.AuthFormat != nil) {
			return p, invalidMCP("url, credential_slot, auth_header and auth_format are for kind 'remote'")
		}
		set := func(dst *string, v *string) {
			if v != nil {
				*dst = *v
			}
		}
		set(&p.URL, u.URL)
		set(&p.CredentialSlot, u.CredentialSlot)
		set(&p.AuthHeader, u.AuthHeader)
		set(&p.AuthFormat, u.AuthFormat)
		set(&p.ProtocolVersion, u.ProtocolVersion)
		p.AuthHeader, p.AuthFormat = orDefaultStr(p.AuthHeader, "Authorization"), orDefaultStr(p.AuthFormat, "Bearer {key}")
		p.RPM, p.MaxConcurrent, p.TimeoutS = firstInt(u.RPM, p.RPM), firstInt(u.MaxConcurrent, p.MaxConcurrent), firstInt(u.TimeoutS, p.TimeoutS)
		if u.Enabled != nil {
			p.Enabled = *u.Enabled
		}
		if p.Kind == "remote" {
			if err := validMCPUpstream(p); err != nil {
				return p, err
			}
		}
		return p, nil
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.MCPSource{}, 0, ErrMCPSourceNotFound
	case errors.Is(err, db.ErrMCPSourceBusy):
		return db.MCPSource{}, 0, ErrMCPSourceBusy
	case err != nil:
		return db.MCPSource{}, 0, err
	}
	src, err = s.q.GetMCPSource(ctx, slug)
	return src, repending, err
}

func orDefaultStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// DeleteMCPSource removes the source and its tools. A remote source's hidden
// backing service goes with it; a tunnel source's service is unbound and
// stays (with its gateway-only flag as it is).
func (s *Store) DeleteMCPSource(ctx context.Context, slug string) error {
	if err := s.q.DeleteMCPSourceAndBacking(ctx, slug); errors.Is(err, db.ErrNotFound) {
		return ErrMCPSourceNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// MCPSourceBySlug returns the source, or ErrMCPSourceNotFound.
func (s *Store) MCPSourceBySlug(ctx context.Context, slug string) (db.MCPSource, error) {
	src, err := s.q.GetMCPSource(ctx, slug)
	if errors.Is(err, db.ErrNotFound) {
		return db.MCPSource{}, ErrMCPSourceNotFound
	}
	return src, err
}

// ListMCPSources returns every source ordered by slug.
func (s *Store) ListMCPSources(ctx context.Context) ([]db.MCPSource, error) {
	return s.q.ListMCPSources(ctx)
}
