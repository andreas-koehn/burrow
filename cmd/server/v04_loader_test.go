package main

import (
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
)

// A service whose stored AI config has the settings of the per-service
// Anthropic adapter — "anthropic.enabled" and "routing.translate_to", which
// older dashboards wrote — loads as it always did: that adapter never ran
// and is gone, so nothing translates the service's requests; the "anthropic"
// section still keeps the service on the chain (metered, not pass-through),
// and the routing block, "translate_to" included, is read by nothing.
func TestDecodeServiceAIConfig_SettingsOfTheRetiredAdapter(t *testing.T) {
	cfg, err := decodeServiceAIConfig([]byte(`{
 "anthropic":{"enabled":true},
 "routing":{"strategy":"single","model_alias":"","backends":[],"translate_to":"openai"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Anthropic == nil || !cfg.Anthropic.Enabled {
		t.Fatalf("the anthropic section was not loaded: %+v", cfg.Anthropic)
	}
	if cfg.Routing != nil {
		t.Fatalf("the routing block was decoded: %+v", cfg.Routing)
	}
	if aigw.IsAIPassThrough(cfg) {
		t.Fatal("a service with the anthropic section left the chain: its requests would no longer be metered")
	}
	if cfg.Cache != nil || cfg.Semantic != nil || cfg.Redaction != nil || cfg.Guardrails != nil || cfg.Inspector != nil {
		t.Fatalf("a section nobody stored was switched on: %+v", cfg)
	}
	// "translate_to" alone switches nothing on: such a service is pass-through, as before.
	cfg, err = decodeServiceAIConfig([]byte(`{"routing":{"strategy":"multi_provider","translate_to":"anthropic"}}`))
	if err != nil || !aigw.IsAIPassThrough(cfg) {
		t.Fatalf("err %v, cfg %+v", err, cfg)
	}
}
