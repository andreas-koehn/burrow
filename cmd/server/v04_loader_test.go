package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/db"
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

// Answer inspection and the custom patterns are global: the loader takes
// them from the global guardrails blob for every service with a guardrails
// section of its own, whatever that section says about them. A service
// without the section stays without guardrails (and on the pass-through
// path).
func TestChainConfigLoader_GlobalGuardrails(t *testing.T) {
	ctx := context.Background()
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "loader.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	x := db.Wrap(sqldb)
	if err := x.CreateUser(ctx, db.User{ID: "u-ld", Email: "u-ld@test.invalid", PasswordHash: "h", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	withSection, err := x.GetOrCreateService(ctx, "u-ld", "with", "http")
	if err != nil {
		t.Fatal(err)
	}
	without, err := x.GetOrCreateService(ctx, "u-ld", "without", "http")
	if err != nil {
		t.Fatal(err)
	}
	must(t, x.UpsertServiceAIConfig(ctx, withSection.ID, []byte(`{"guardrails":{"enabled":true,"action":"refuse_403","answers":{"enabled":false},"custom_patterns":[{"id":"own","pattern":"x","direction":"both"}]}}`)), "upsert with")
	must(t, x.UpsertServiceAIConfig(ctx, without.ID, []byte(`{"cache":{"enabled":false,"applies_per":"global","ttl_seconds":60,"max_entries":10,"max_per_entry_kb":8}}`)), "upsert without")
	must(t, x.SetSettings(ctx, map[string]string{
		"guardrails.global": `{"enabled":false,"action":"log_only","answers":{"enabled":true,"action":"block"},"custom_patterns":[{"id":"ticket","pattern":"TICKET-\\d+","direction":"request","description":""}]}`,
	}), "settings")

	l := chainConfigLoader{db: x, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	svc, ok, err := l.LoadAIConfig(ctx, withSection.ID)
	if err != nil || !ok {
		t.Fatalf("load: %v %v", ok, err)
	}
	g := svc.AIConfig.Guardrails
	if g == nil || !g.Enabled || g.Action != "refuse_403" {
		t.Fatalf("the service's own enabled/action were not kept: %+v", g)
	}
	if !g.Answers.Enabled || g.Answers.Action != "block" {
		t.Fatalf("answers not taken from the global blob: %+v", g.Answers)
	}
	if len(g.CustomPatterns) != 1 || g.CustomPatterns[0].ID != "ticket" {
		t.Fatalf("custom patterns not taken from the global blob: %+v", g.CustomPatterns)
	}
	svc, ok, err = l.LoadAIConfig(ctx, without.ID)
	if err != nil || !ok || svc.AIConfig.Guardrails != nil {
		t.Fatalf("a service without a guardrails section got one: %+v %v %v", svc.AIConfig.Guardrails, ok, err)
	}

	// The cached reader serves the same and sees a change after its interval.
	cached := chainConfigLoader{db: x, log: l.log, global: newGlobalGuardrails(x, time.Hour, nil)}
	svc, _, _ = cached.LoadAIConfig(ctx, withSection.ID)
	if !svc.AIConfig.Guardrails.Answers.Enabled {
		t.Fatal("cached loader: answers off")
	}
	must(t, x.SetSettings(ctx, map[string]string{"guardrails.global": `{"enabled":false,"action":"log_only"}`}), "settings 2")
	if svc, _, _ = l.LoadAIConfig(ctx, withSection.ID); svc.AIConfig.Guardrails.Answers.Enabled || len(svc.AIConfig.Guardrails.CustomPatterns) != 0 {
		t.Fatalf("uncached loader did not see the change: %+v", svc.AIConfig.Guardrails)
	}
}
