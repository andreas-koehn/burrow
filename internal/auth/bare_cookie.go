package auth

import (
	"context"
	"fmt"
	"time"
)

// BareCookieCutoffKey is the settings key holding the time (RFC 3339, UTC)
// of the first start of a version that writes the dashboard's cookies under
// HostCookiePrefix when Secure cookies are on (v0.12.1). With Secure cookies,
// a session read from the bare burrow_session cookie counts only when it was
// created before then: such a cookie is a browser's from before the upgrade,
// while a later one can only have been planted by another host of the site
// (with Domain=). The bare cookies had a Max-Age of 7 days, so the fallback
// has nothing left to accept a week after the cutoff.
const BareCookieCutoffKey = "auth.bare_cookie_cutoff"

// SettingsStore reads and writes the relay's settings.
type SettingsStore interface {
	GetSettings(ctx context.Context) (map[string]string, error)
	SaveSettings(ctx context.Context, kv map[string]string) error
}

// EnsureBareCookieCutoff returns the bare-cookie cutoff, minting it (now,
// to the second) and storing it on the first call. It must run at start-up,
// before any session is created.
func EnsureBareCookieCutoff(ctx context.Context, ss SettingsStore) (time.Time, error) {
	m, err := ss.GetSettings(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("auth: load settings: %w", err)
	}
	if v := m[BareCookieCutoffKey]; v != "" {
		c, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, fmt.Errorf("auth: setting %s: %w", BareCookieCutoffKey, err)
		}
		return c.UTC(), nil
	}
	c := time.Now().UTC().Truncate(time.Second)
	if err := ss.SaveSettings(ctx, map[string]string{BareCookieCutoffKey: c.Format(time.RFC3339)}); err != nil {
		return time.Time{}, fmt.Errorf("auth: persist bare-cookie cutoff: %w", err)
	}
	return c, nil
}
