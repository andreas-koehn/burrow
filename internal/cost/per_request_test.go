package cost_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
)

func perRequestPricing() cost.Pricing {
	return cost.Pricing{Version: "test", Entries: map[string]cost.Entry{
		"oai/img":    {PerRequestUSD: 0.04},
		"oai/rerank": {InputPerMillion: 0.1, PerRequestUSD: 0.002},
		"oai/tts":    {InputPerMillion: 15},
	}}
}

// A row with tokens is priced by tokens; the requests without tokens in it by
// the entry's per-request price; neither is 0 and unpriced.
func TestRowUSD_PerRequestPrice(t *testing.T) {
	cases := map[string]struct {
		row      db.UsageRow
		want     float64
		unpriced bool
	}{
		"tokens win": {db.UsageRow{Kind: "openai", ProviderSlug: "oai", TargetModel: "rerank", Requests: 1,
			TokensIn: 1_000_000, PricedTokensIn: 1_000_000}, 0.1, false},
		"no tokens, per request": {db.UsageRow{Kind: "openai", ProviderSlug: "oai", TargetModel: "img", Requests: 3,
			TokenlessRequests: 3}, 0.12, false},
		"mixed row": {db.UsageRow{Kind: "openai", ProviderSlug: "oai", TargetModel: "rerank", Requests: 3,
			TokensIn: 1_000_000, PricedTokensIn: 1_000_000, TokenlessRequests: 2}, 0.1 + 2*0.002, false},
		"no tokens, no per-request price": {db.UsageRow{Kind: "openai", ProviderSlug: "oai", TargetModel: "tts", Requests: 2,
			TokenlessRequests: 2}, 0, true},
		"unknown model": {db.UsageRow{Kind: "openai", ProviderSlug: "oai", TargetModel: "other", Requests: 1,
			TokenlessRequests: 1}, 0, true},
		"reported cost, no tokens": {db.UsageRow{Kind: "openai", ProviderSlug: "oai", TargetModel: "img", Requests: 1,
			ReportedUSD: 0.5}, 0.5, false},
		"only tokens": {db.UsageRow{Kind: "openai", ProviderSlug: "oai", TargetModel: "tts", Requests: 1,
			TokensIn: 10, PricedTokensIn: 10}, 0.00015, false},
	}
	e := cost.NewWithDeps(perRequestPricing(), nil, nil, nil, nil, nil, nil, nil)
	for name, c := range cases {
		if got := e.RowUSD(c.row); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: RowUSD = %v, want %v", name, got, c.want)
		}
		if got := e.Unpriced(c.row); got != c.unpriced {
			t.Errorf("%s: Unpriced = %v, want %v", name, got, c.unpriced)
		}
	}
}

func TestSummary_CountsUnpricedRequests(t *testing.T) {
	usage := &rawUsageReader{rows: []db.UsageRow{
		{Kind: "openai", ProviderSlug: "oai", TargetModel: "img", Requests: 3, TokenlessRequests: 3},
		{Kind: "openai", ProviderSlug: "oai", TargetModel: "tts", Requests: 5, TokenlessRequests: 4, TokensIn: 10, PricedTokensIn: 10},
		{Kind: "openai", ProviderSlug: "oai", TargetModel: "other", Requests: 2, TokenlessRequests: 2},
	}}
	e := cost.NewWithDeps(perRequestPricing(), nil, usage, nil, nil, nil, nil, nil)
	s, err := e.Summary(context.Background(), "today")
	if err != nil {
		t.Fatal(err)
	}
	if s.UnpricedRequests != 6 {
		t.Fatalf("UnpricedRequests = %d, want 6 (4 of tts and 2 of other)", s.UnpricedRequests)
	}
	if math.Abs(s.TotalUSD-0.12-0.00015) > 1e-9 {
		t.Fatalf("TotalUSD = %v", s.TotalUSD)
	}
}

func TestLoadOverride_PerRequestUSD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.yaml")
	yaml := "version: \"v1\"\nentries:\n  - { provider: oai, model: img, per_request_usd: 0.04 }\n  - { provider: oai, model: neg, per_request_usd: -1 }\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cost.LoadOverride(path); err == nil {
		t.Fatal("a negative per_request_usd is refused")
	}
	if err := os.WriteFile(path, []byte("version: \"v1\"\nentries:\n  - { provider: oai, model: img, per_request_usd: 0.04 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := cost.LoadOverride(path)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := p.Lookup("oai/img"); !ok || e.PerRequestUSD != 0.04 {
		t.Fatalf("entry %+v %v", e, ok)
	}
}
