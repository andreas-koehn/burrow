// Package cost ships the bundled-pricing table, the live cost engine, and
// the budget-trigger machinery defined by v0.4.0 spec Part F.
//
// # Locked invariants (spec Part F)
//
//   - Pricing is shipped as an embedded YAML file (pricing.yaml). The
//     operator may override the table at runtime via PUT /api/v1/cost/pricing
//     OR by pointing BURROW_PRICING_PATH at an on-disk YAML (Task 24).
//     Burrow MUST NEVER fetch prices online.
//
//   - Budget action_on_exceed ∈ {alert_webhook, throttle_zero, disable_key}.
//
//   - A budget triggers its action exactly once per exceed transition (not
//     on every charge after exceed). This package implements the "once"
//     gate via an in-memory map[budgetID]bool keyed by the UTC date — the
//     map is reset whenever the engine observes a new UTC day.
//
//   - current_usd is computed live from usage_events × pricing entries,
//     never persisted on the budgets row (which only stores configuration).
package cost

import (
	"embed"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

//go:embed pricing.yaml
var bundledFS embed.FS

// Entry is the per-model USD price (per 1,000,000 tokens, split by side).
// A zero-cost model (e.g. ollama) is represented as both fields == 0.
// PerRequestUSD is the price of one request that reports no tokens (images,
// speech, transcription); 0 = not priced per request.
type Entry struct {
	InputPerMillion  float64
	OutputPerMillion float64
	PerRequestUSD    float64
}

// Pricing is the full table: a version string + an entries map keyed by
// "provider/model". Lookups by raw model name are also accepted (UsdFor and
// the engine handle the fallback) so callers that haven't plumbed provider
// alongside the model name still get a price for popular names.
type Pricing struct {
	Version string
	Entries map[string]Entry
}

// yamlEntry mirrors the on-disk shape; we map it into the by-key Entries
// dictionary at load time so callers never deal with slice search.
type yamlEntry struct {
	Provider         string  `yaml:"provider"`
	Model            string  `yaml:"model"`
	InputPerMillion  float64 `yaml:"input_per_million"`
	OutputPerMillion float64 `yaml:"output_per_million"`
	PerRequestUSD    float64 `yaml:"per_request_usd"`
}

type yamlPricing struct {
	Version string      `yaml:"version"`
	Entries []yamlEntry `yaml:"entries"`
}

// LoadEmbedded reads the bundled pricing.yaml from the package's embed.FS.
// Returns an error if the file is missing or malformed (should not happen
// in production builds — the file is compiled in).
func LoadEmbedded() (Pricing, error) {
	raw, err := bundledFS.ReadFile("pricing.yaml")
	if err != nil {
		return Pricing{}, fmt.Errorf("cost: read embedded pricing: %w", err)
	}
	return parsePricing(raw)
}

// LoadOverride reads a pricing.yaml from the given path (operator override).
// The file MUST be locally readable; this package never reaches the network.
func LoadOverride(path string) (Pricing, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Pricing{}, fmt.Errorf("cost: read override pricing %s: %w", path, err)
	}
	return parsePricing(raw)
}

// parsePricing unmarshals the YAML bytes into the by-key Pricing shape and
// validates that every entry has both fields >= 0 and a non-empty model.
// Two keys are written per entry: "provider/model" (canonical) and "model"
// (fallback for callers that don't carry the provider separately).
func parsePricing(raw []byte) (Pricing, error) {
	var y yamlPricing
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return Pricing{}, fmt.Errorf("cost: parse pricing yaml: %w", err)
	}
	if y.Version == "" {
		return Pricing{}, fmt.Errorf("cost: pricing yaml missing version")
	}
	p := Pricing{
		Version: y.Version,
		Entries: make(map[string]Entry, len(y.Entries)*2),
	}
	for i, e := range y.Entries {
		if e.Model == "" {
			return Pricing{}, fmt.Errorf("cost: pricing entry %d missing model", i)
		}
		if e.InputPerMillion < 0 || e.OutputPerMillion < 0 || e.PerRequestUSD < 0 {
			return Pricing{}, fmt.Errorf("cost: pricing entry %s/%s has negative price",
				e.Provider, e.Model)
		}
		entry := Entry{
			InputPerMillion:  e.InputPerMillion,
			OutputPerMillion: e.OutputPerMillion,
			PerRequestUSD:    e.PerRequestUSD,
		}
		if e.Provider != "" {
			p.Entries[e.Provider+"/"+e.Model] = entry
		}
		// Fallback key — last-write-wins is acceptable; popular model
		// names are unique across providers in the bundled table.
		if _, exists := p.Entries[e.Model]; !exists {
			p.Entries[e.Model] = entry
		}
	}
	return p, nil
}

// Lookup returns the Entry for key (canonical "provider/model" or bare
// model name) and ok=true when present. Unknown models get a zero Entry +
// ok=false; callers should treat that as "no price available" rather than
// "zero-cost" — the latter is signalled by both fields being literally 0
// in the YAML (e.g. ollama entries).
func (p Pricing) Lookup(key string) (Entry, bool) {
	e, ok := p.Entries[key]
	return e, ok
}
