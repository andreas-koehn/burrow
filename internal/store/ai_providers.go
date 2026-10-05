package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/db"
)

// ErrInvalidProviderSlug is returned for a provider slug that is malformed or reserved.
var ErrInvalidProviderSlug = errors.New("store: invalid provider slug")

// ValidProviderSlug reports whether s can name a provider. "v1" is reserved
// for the global gateway endpoint (/ai/v1).
func ValidProviderSlug(s string) bool { return auth.ValidSlug(s) && s != "v1" }

// ProviderSlugFromName derives a slug from a display name: lower-cased, runs
// of other characters collapsed to one hyphen, trimmed. Returns "" when the
// result is not a valid provider slug.
func ProviderSlugFromName(name string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			hyphen = false
		default:
			if !hyphen && b.Len() > 0 {
				b.WriteByte('-')
				hyphen = true
			}
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	if !ValidProviderSlug(slug) {
		return ""
	}
	return slug
}

// ProviderBySlug returns the provider row. Hot path for /ai/ requests; no
// permission gate. Propagates db.ErrNotFound.
func (s *Store) ProviderBySlug(ctx context.Context, slug string) (db.AIProvider, error) {
	return s.q.GetAIProvider(ctx, slug)
}

// aiProvidersBackfilledKey marks, in the settings table, that the one-time
// backfill has completed. It is not in the settings API's key whitelist.
const aiProvidersBackfilledKey = "ai_providers.backfilled"

// BackfillAIProviders creates a tunnel provider for every http service in
// api_key mode that has none. Slug preference: derived from the service name,
// then the service's own slug. Services for which neither is free are skipped
// and can be added by hand.
//
// It does its work once per database: a completed run leaves a marker in the
// settings table and later calls return 0. Without that, a provider an admin
// deleted would come back at the next start, because its service is still in
// api_key mode. Safe to call at every start.
func (s *Store) BackfillAIProviders(ctx context.Context) (int, error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return 0, err
	}
	if settings[aiProvidersBackfilledKey] != "" {
		return 0, nil
	}
	svcs, err := s.q.ListAllServices(ctx)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, svc := range svcs {
		if svc.Type != "http" || svc.AccessMode != "api_key" {
			continue
		}
		if _, err := s.q.GetAIProviderByService(ctx, svc.ID); err == nil {
			continue
		} else if !errors.Is(err, db.ErrNotFound) {
			return created, err
		}
		for _, slug := range []string{ProviderSlugFromName(svc.Name), svc.Subdomain} {
			if !ValidProviderSlug(slug) {
				continue
			}
			err := s.q.CreateAIProvider(ctx, db.AIProvider{
				Slug: slug, Name: svc.Name, Kind: "tunnel", ServiceID: svc.ID, APIFormat: "openai",
			})
			if err == nil {
				created++
				break
			}
			if !errors.Is(err, db.ErrDuplicateProvider) {
				return created, err
			}
		}
	}
	if err := s.SaveSettings(ctx, map[string]string{aiProvidersBackfilledKey: "1"}); err != nil {
		return created, err
	}
	return created, nil
}

var (
	// ErrProviderExists: the slug is taken or the service already backs a provider.
	ErrProviderExists = errors.New("store: provider slug or service already in use")
	// ErrProviderNotFound: no provider has that slug.
	ErrProviderNotFound = errors.New("store: provider not found")
	// ErrProviderBusy: concurrent writers kept changing the provider; the
	// caller may try again.
	ErrProviderBusy = errors.New("store: provider is being changed concurrently")
	// ErrProviderService: a tunnel provider needs an http service in api_key mode.
	ErrProviderService = errors.New("store: provider needs an http service in api_key mode")
)

// ListProviders returns every provider ordered by slug.
func (s *Store) ListProviders(ctx context.Context) ([]db.AIProvider, error) {
	return s.q.ListAIProviders(ctx)
}

// CreateTunnelProvider registers an existing http service in api_key mode as
// a model provider. Callers gate on admin; the store only validates the data.
func (s *Store) CreateTunnelProvider(ctx context.Context, slug, name, serviceID string) (db.AIProvider, error) {
	if !ValidProviderSlug(slug) {
		return db.AIProvider{}, ErrInvalidProviderSlug
	}
	svc, err := s.q.GetServiceByID(ctx, serviceID)
	if errors.Is(err, db.ErrNotFound) {
		return db.AIProvider{}, ErrProviderService
	}
	if err != nil {
		return db.AIProvider{}, err
	}
	if svc.Type != "http" || svc.AccessMode != "api_key" {
		return db.AIProvider{}, ErrProviderService
	}
	p := db.AIProvider{Slug: slug, Name: name, Kind: "tunnel", ServiceID: serviceID, APIFormat: "openai"}
	if err := s.q.CreateAIProvider(ctx, p); err != nil {
		if errors.Is(err, db.ErrDuplicateProvider) {
			return db.AIProvider{}, ErrProviderExists
		}
		return db.AIProvider{}, err
	}
	return s.q.GetAIProvider(ctx, slug)
}

// UpdateProvider changes a provider's slug and display name. The old base URL
// stops working at once. The backing service of a direct provider is renamed
// with it.
func (s *Store) UpdateProvider(ctx context.Context, slug, newSlug, name string) (db.AIProvider, error) {
	if !ValidProviderSlug(newSlug) {
		return db.AIProvider{}, ErrInvalidProviderSlug
	}
	switch err := s.q.UpdateAIProvider(ctx, slug, newSlug, name); {
	case errors.Is(err, db.ErrNotFound):
		return db.AIProvider{}, ErrProviderNotFound
	case errors.Is(err, db.ErrDuplicateProvider):
		return db.AIProvider{}, ErrProviderExists
	case err != nil:
		return db.AIProvider{}, err
	}
	return s.q.GetAIProvider(ctx, newSlug)
}

// DeleteProvider removes the provider. A tunnel provider's service is kept; a
// direct provider's backing service goes with it, and with that its API keys,
// AI configuration and model list.
func (s *Store) DeleteProvider(ctx context.Context, slug string) error {
	if err := s.q.DeleteAIProviderAndBacking(ctx, slug); errors.Is(err, db.ErrNotFound) {
		return ErrProviderNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// ErrInvalidProviderConfig is wrapped with the reason a provider's upstream
// settings were refused; the reason is safe to show to an admin.
var ErrInvalidProviderConfig = errors.New("store: invalid provider configuration")

// DirectProviderInput carries the operator-supplied settings of a direct
// provider. CredentialSlot names a vault slot; the credential itself never
// passes through the store.
type DirectProviderInput struct {
	Slug, Name, APIFormat, BaseURL, CredentialSlot, AuthHeader, AuthFormat, Billing string
	ExtraHeaders                                                                    map[string]string
}

var (
	slotRe       = regexp.MustCompile(`^[A-Z0-9_]{1,32}$`) // same rule as credinject slots
	headerNameRe = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$")
)

// relayHeaders are set by the relay or the transport, or change where a
// request goes. They can be neither a static extra header nor the header
// that carries the credential.
var relayHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"keep-alive": true, "te": true, "trailer": true, "upgrade": true,
	"cookie": true, "forwarded": true,
}

// credentialHeaders carry a credential. They are fine as the auth header and
// refused as a static extra header.
var credentialHeaders = map[string]bool{"authorization": true, "x-api-key": true}

func isRelayHeader(lower string) bool {
	return relayHeaders[lower] || strings.HasPrefix(lower, "proxy-") || strings.HasPrefix(lower, "x-forwarded-")
}

const (
	maxExtraHeaders      = 16
	maxExtraHeaderValue  = 512
	maxExtraHeadersBytes = 4096 // all names and values together
	maxAuthFormat        = 128
	maxProviderName      = 120
)

func invalidConfig(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidProviderConfig, reason)
}

// hasControl reports whether s contains a control character (CR, LF, NUL,
// tab, DEL, ...), which is what lets a header value start a new line.
func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

// normalizeDirect validates in and fills defaults. Reasons name the field and
// may quote a header name, never a header value or the URL.
func normalizeDirect(in DirectProviderInput) (DirectProviderInput, error) {
	if _, err := aiprovider.ValidateBaseURL(in.BaseURL); err != nil || len(in.BaseURL) > 2048 {
		return in, invalidConfig("base URL must be an https URL without credentials, query or fragment")
	}
	if !slotRe.MatchString(in.CredentialSlot) {
		return in, invalidConfig("credential slot must be 1-32 characters: A-Z, 0-9, _")
	}
	if in.AuthHeader == "" {
		in.AuthHeader = "Authorization"
	}
	if !headerNameRe.MatchString(in.AuthHeader) || isRelayHeader(strings.ToLower(in.AuthHeader)) {
		return in, invalidConfig("auth header is not a valid header name")
	}
	if in.AuthFormat == "" {
		in.AuthFormat = "Bearer {key}"
	}
	// Exactly one placeholder: the relay fills in the first one only.
	if strings.Count(in.AuthFormat, "{key}") != 1 || hasControl(in.AuthFormat) || len(in.AuthFormat) > maxAuthFormat {
		return in, invalidConfig(`auth format must contain "{key}" once, at most 128 characters, no control characters`)
	}
	switch in.Billing {
	case "":
		in.Billing = "metered"
	case "metered", "flat":
	default:
		return in, invalidConfig("billing must be 'metered' or 'flat'")
	}
	switch in.APIFormat {
	case "":
		in.APIFormat = "openai"
	case "openai", "anthropic":
	default:
		return in, invalidConfig("api_format must be 'openai' or 'anthropic'")
	}
	if len(in.ExtraHeaders) > maxExtraHeaders {
		return in, invalidConfig("at most 16 extra headers")
	}
	total := 0
	for k, v := range in.ExtraHeaders {
		// %q of a name that failed the token rule could carry anything, so
		// only a well-formed name is quoted back.
		if !headerNameRe.MatchString(k) {
			return in, invalidConfig("an extra header has an invalid name")
		}
		lower := strings.ToLower(k)
		if isRelayHeader(lower) || credentialHeaders[lower] || lower == strings.ToLower(in.AuthHeader) {
			return in, invalidConfig("extra header " + strconv.Quote(k) + " is not allowed")
		}
		if hasControl(v) || len(v) > maxExtraHeaderValue {
			return in, invalidConfig("extra header " + strconv.Quote(k) + " has an invalid value")
		}
		total += len(k) + len(v)
	}
	if total > maxExtraHeadersBytes {
		return in, invalidConfig("extra headers are larger than 4096 bytes in total")
	}
	return in, nil
}

// CreateDirectProvider adds a provider the relay calls itself, together with
// its backing service row owned by ownerID. The row's id is generated like
// any other service's, so it survives a change of slug. Callers gate on
// admin; the store validates the data and does no network calls.
func (s *Store) CreateDirectProvider(ctx context.Context, ownerID string, in DirectProviderInput) (db.AIProvider, error) {
	if !ValidProviderSlug(in.Slug) {
		return db.AIProvider{}, ErrInvalidProviderSlug
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return db.AIProvider{}, invalidConfig("name is required")
	}
	if len(in.Name) > maxProviderName {
		return db.AIProvider{}, invalidConfig("name must be at most 120 chars")
	}
	in, err := normalizeDirect(in)
	if err != nil {
		return db.AIProvider{}, err
	}
	svc := db.Service{ID: uuid.NewString(), UserID: ownerID, Name: in.Name, Type: "direct", AccessMode: "api_key", APIKeyHeader: "Authorization"}
	p := db.AIProvider{
		Slug: in.Slug, Name: in.Name, Kind: "direct", ServiceID: svc.ID, APIFormat: in.APIFormat,
		BaseURL: in.BaseURL, CredentialSlot: in.CredentialSlot, AuthHeader: in.AuthHeader, AuthFormat: in.AuthFormat,
		ExtraHeaders: in.ExtraHeaders, Billing: in.Billing,
	}
	if err := s.q.CreateDirectAIProvider(ctx, svc, p); err != nil {
		if errors.Is(err, db.ErrDuplicateProvider) {
			return db.AIProvider{}, ErrProviderExists
		}
		return db.AIProvider{}, err
	}
	return s.q.GetAIProvider(ctx, in.Slug)
}

// UpdateProviderUpstream changes the upstream settings of a direct provider.
// A field left empty keeps its stored value, so an update that does not name
// the credential slot keeps the slot; ExtraHeaders nil keeps the stored
// headers and an empty map removes them. Slug and Name of in are ignored.
// The /ai/ data plane reads the row per request, so the change applies at once.
func (s *Store) UpdateProviderUpstream(ctx context.Context, slug string, in DirectProviderInput) (db.AIProvider, error) {
	// The merge with the stored values runs inside the database's
	// read-and-write step, so a concurrent update of other fields is kept.
	err := s.q.ModifyAIProviderUpstream(ctx, slug, func(p db.AIProvider) (db.AIProvider, error) {
		if p.Kind != "direct" {
			return p, invalidConfig("only direct providers have upstream settings")
		}
		in := in
		keep := func(v *string, stored string) {
			if *v == "" {
				*v = stored
			}
		}
		keep(&in.BaseURL, p.BaseURL)
		keep(&in.CredentialSlot, p.CredentialSlot)
		keep(&in.AuthHeader, p.AuthHeader)
		keep(&in.AuthFormat, p.AuthFormat)
		keep(&in.Billing, p.Billing)
		keep(&in.APIFormat, p.APIFormat)
		if in.ExtraHeaders == nil {
			in.ExtraHeaders = p.ExtraHeaders
		}
		in, err := normalizeDirect(in)
		if err != nil {
			return p, err
		}
		p.BaseURL, p.CredentialSlot, p.AuthHeader, p.AuthFormat = in.BaseURL, in.CredentialSlot, in.AuthHeader, in.AuthFormat
		p.Billing, p.APIFormat, p.ExtraHeaders = in.Billing, in.APIFormat, in.ExtraHeaders
		return p, nil
	})
	if errors.Is(err, db.ErrNotFound) {
		return db.AIProvider{}, ErrProviderNotFound
	}
	if errors.Is(err, db.ErrProviderBusy) {
		return db.AIProvider{}, ErrProviderBusy
	}
	if err != nil {
		return db.AIProvider{}, err
	}
	return s.q.GetAIProvider(ctx, slug)
}

func (s *Store) providerOrNotFound(ctx context.Context, slug string) (db.AIProvider, error) {
	p, err := s.q.GetAIProvider(ctx, slug)
	if errors.Is(err, db.ErrNotFound) {
		return db.AIProvider{}, ErrProviderNotFound
	}
	return p, err
}

// ListProviderModels returns the provider's stored model list ordered by id.
func (s *Store) ListProviderModels(ctx context.Context, slug string) ([]db.AIProviderModel, error) {
	if _, err := s.providerOrNotFound(ctx, slug); err != nil {
		return nil, err
	}
	return s.q.ListAIProviderModels(ctx, slug)
}

// validModel checks one catalog row. Model lists come from an upstream or
// from a form; either way they are stored as plain data.
func validModel(m db.AIProviderModel) error {
	if !aiprovider.ValidModelID(m.ModelID) {
		return invalidConfig("model id must be 1-200 characters without control characters")
	}
	if len(m.DisplayName) > 200 || hasControl(m.DisplayName) {
		return invalidConfig("model name must be at most 200 characters without control characters")
	}
	if m.ContextLength < 0 {
		return invalidConfig("context length must not be negative")
	}
	return nil
}

// ReplaceProviderModels swaps the provider's whole model list. Calling it
// again with the same list leaves the same rows.
func (s *Store) ReplaceProviderModels(ctx context.Context, slug string, models []db.AIProviderModel) error {
	if len(models) > aiprovider.MaxModels {
		return invalidConfig("at most " + strconv.Itoa(aiprovider.MaxModels) + " models per provider")
	}
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if err := validModel(m); err != nil {
			return err
		}
		if seen[m.ModelID] {
			return invalidConfig("a model id is listed twice")
		}
		seen[m.ModelID] = true
	}
	if _, err := s.providerOrNotFound(ctx, slug); err != nil {
		return err
	}
	return s.q.ReplaceAIProviderModels(ctx, slug, models)
}

// AddProviderModel adds one model id by hand. Adding an id that is already
// listed is not an error.
func (s *Store) AddProviderModel(ctx context.Context, slug, modelID string) error {
	m := db.AIProviderModel{ProviderSlug: slug, ModelID: modelID}
	if err := validModel(m); err != nil {
		return err
	}
	if _, err := s.providerOrNotFound(ctx, slug); err != nil {
		return err
	}
	existing, err := s.q.ListAIProviderModels(ctx, slug)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.ModelID == modelID {
			return nil // keep the synced name and context length
		}
	}
	if len(existing) >= aiprovider.MaxModels {
		return invalidConfig("at most " + strconv.Itoa(aiprovider.MaxModels) + " models per provider")
	}
	return s.q.UpsertAIProviderModel(ctx, m)
}

// RemoveProviderModel removes one model id. ErrProviderNotFound covers both
// an unknown provider and an id that is not listed.
func (s *Store) RemoveProviderModel(ctx context.Context, slug, modelID string) error {
	if _, err := s.providerOrNotFound(ctx, slug); err != nil {
		return err
	}
	if err := s.q.DeleteAIProviderModel(ctx, slug, modelID); errors.Is(err, db.ErrNotFound) {
		return ErrProviderNotFound
	} else if err != nil {
		return err
	}
	return nil
}
