package store

import (
	"context"
	"errors"
	"strings"

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
// stops working at once.
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

// DeleteProvider removes the provider. Its backing service is kept.
func (s *Store) DeleteProvider(ctx context.Context, slug string) error {
	if err := s.q.DeleteAIProvider(ctx, slug); errors.Is(err, db.ErrNotFound) {
		return ErrProviderNotFound
	} else if err != nil {
		return err
	}
	return nil
}
