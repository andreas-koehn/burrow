package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/proxy"
)

type fakeRuleCounter struct{ n int }

func (f fakeRuleCounter) CountCountryRules(context.Context, string) (int, error) { return f.n, nil }

type loadedGeo struct{ proxy.GeoLookup }

func (loadedGeo) Enabled() bool { return true }

// With country rules and no database the start warns, naming the number of
// services and BURROW_GEOIP_DB; with none, or with a database, it does not.
func TestWarnUnenforcedCountryRules(t *testing.T) {
	run := func(geo proxy.GeoLookup, n int) string {
		var buf bytes.Buffer
		warnUnenforcedCountryRules(context.Background(), slog.New(slog.NewTextHandler(&buf, nil)), geo, fakeRuleCounter{n}, "")
		return buf.String()
	}
	out := run(proxy.NoopGeoLookup(), 3)
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "services=3") || !strings.Contains(out, "BURROW_GEOIP_DB") {
		t.Errorf("no warning for 3 services without a database: %q", out)
	}
	if out := run(proxy.NoopGeoLookup(), 0); out != "" {
		t.Errorf("warned without rules: %q", out)
	}
	if out := run(loadedGeo{proxy.NoopGeoLookup()}, 3); out != "" {
		t.Errorf("warned with a database: %q", out)
	}
}
