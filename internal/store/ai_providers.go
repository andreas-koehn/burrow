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

// BackfillAIProviders creates a tunnel provider for every http service in
// api_key mode that has none. Slug preference: derived from the service name,
// then the service's own slug. Services for which neither is free are skipped
// and can be added by hand. Safe to run at every start.
func (s *Store) BackfillAIProviders(ctx context.Context) (int, error) {
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
	return created, nil
}
