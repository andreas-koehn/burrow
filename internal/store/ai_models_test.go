package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// modelFixture creates the providers "ollama" (tunnel, openai), "zai"
// (direct, openai) and "zai-anthropic" (direct, anthropic). It returns the id
// of the service behind "ollama" and the id of the admin who owns the rest.
func modelFixture(t *testing.T, s *Store) (svcOllama, adminID string) {
	t.Helper()
	ctx := context.Background()
	svcA, _, _ := providerFixture(t, s)
	if _, err := s.CreateTunnelProvider(ctx, "ollama", "Ollama", svcA); err != nil {
		t.Fatal(err)
	}
	adminID = mustCreateUser(t, s, "models-admin@x", "admin").ID
	for _, in := range []DirectProviderInput{
		{Slug: "zai", Name: "z.ai", BaseURL: "https://api.z.ai/v4", CredentialSlot: "ZAI"},
		{Slug: "zai-anthropic", Name: "z.ai Anthropic", BaseURL: "https://api.z.ai/anthropic", CredentialSlot: "ZAI", APIFormat: "anthropic"},
	} {
		if _, err := s.CreateDirectProvider(ctx, adminID, in); err != nil {
			t.Fatal(err)
		}
	}
	return svcA, adminID
}

func simpleModel() db.AIModel {
	return db.AIModel{Name: "burrow-simple", Enabled: true,
		Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "mistral"}}}
}

func TestValidModelName(t *testing.T) {
	for _, n := range []string{"burrow-simple", "a1", "gpt_4.1-mini", strings.Repeat("a", 63)} {
		if !ValidModelName(n) {
			t.Errorf("ValidModelName(%q) = false", n)
		}
	}
	for _, n := range []string{"", "a", "v1", "A1", "a/b", "-ab", ".ab", "a b", "ab\n", strings.Repeat("a", 64)} {
		if ValidModelName(n) {
			t.Errorf("ValidModelName(%q) = true", n)
		}
	}
}

func TestCreateModel_Validation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	modelFixture(t, s)

	got, err := s.CreateModel(ctx, simpleModel())
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "burrow-simple" || !got.Enabled || got.AttemptTimeoutS != 60 || got.TotalTimeoutS != 120 || len(got.Targets) != 1 {
		t.Fatalf("created: %+v", got)
	}
	if _, err := s.CreateModel(ctx, simpleModel()); !errors.Is(err, ErrModelExists) {
		t.Fatalf("duplicate err = %v", err)
	}

	// A target without a dialect takes its provider's format; positions are
	// numbered per dialect in the order given, whatever the caller sent.
	mixed := db.AIModel{Name: "burrow-mixed", Description: " both formats ", AttemptTimeoutS: 30, TotalTimeoutS: 30, Targets: []db.AIModelTarget{
		{ProviderSlug: "zai", TargetModel: " glm-5.1 ", Position: 7},
		{ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1", Position: 7},
		{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "mistral", Position: 3},
	}}
	got, err = s.CreateModel(ctx, mixed)
	if err != nil {
		t.Fatal(err)
	}
	want := []db.AIModelTarget{
		{Dialect: "anthropic", Position: 0, ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"},
		{Dialect: "openai", Position: 0, ProviderSlug: "zai", TargetModel: "glm-5.1"},
		{Dialect: "openai", Position: 1, ProviderSlug: "ollama", TargetModel: "mistral"},
	}
	if len(got.Targets) != len(want) || got.Enabled || got.Description != "both formats" || got.AttemptTimeoutS != 30 || got.TotalTimeoutS != 30 {
		t.Fatalf("mixed: %+v", got)
	}
	for i := range want {
		if got.Targets[i] != want[i] {
			t.Fatalf("target %d = %+v, want %+v", i, got.Targets[i], want[i])
		}
	}

	bad := map[string]func(m *db.AIModel){
		"name with slash":      func(m *db.AIModel) { m.Name = "a/b" },
		"upper case":           func(m *db.AIModel) { m.Name = "Burrow" },
		"too short":            func(m *db.AIModel) { m.Name = "a" },
		"name equals provider": func(m *db.AIModel) { m.Name = "ollama" },
		"reserved v1":          func(m *db.AIModel) { m.Name = "v1" },
		"no targets":           func(m *db.AIModel) { m.Targets = nil },
		"unknown provider":     func(m *db.AIModel) { m.Targets[0].ProviderSlug = "nope" },
		"dialect mismatch":     func(m *db.AIModel) { m.Targets[0].Dialect = "anthropic" }, // ollama speaks openai
		"unknown dialect":      func(m *db.AIModel) { m.Targets[0].Dialect = "grpc" },
		"empty target model":   func(m *db.AIModel) { m.Targets[0].TargetModel = " " },
		"long target model":    func(m *db.AIModel) { m.Targets[0].TargetModel = strings.Repeat("x", 201) },
		"control in target":    func(m *db.AIModel) { m.Targets[0].TargetModel = "a\nb" },
		"too many targets": func(m *db.AIModel) {
			for len(m.Targets) < 9 {
				m.Targets = append(m.Targets, m.Targets[0])
			}
		},
		"attempt timeout high": func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 601, 601 },
		"attempt timeout neg":  func(m *db.AIModel) { m.AttemptTimeoutS = -1 },
		"total timeout high":   func(m *db.AIModel) { m.TotalTimeoutS = 601 },
		"total < attempt":      func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 60, 30 },
		"long description":     func(m *db.AIModel) { m.Description = strings.Repeat("x", 501) },
		"control in descr":     func(m *db.AIModel) { m.Description = "a\x00b" },
	}
	for name, mutate := range bad {
		m := simpleModel()
		m.Name = "burrow-other"
		mutate(&m)
		if _, err := s.CreateModel(ctx, m); !errors.Is(err, ErrInvalidModel) {
			t.Errorf("%s: err = %v, want ErrInvalidModel", name, err)
		}
	}
	// Eight targets in one format are the limit, not beyond it.
	m := simpleModel()
	m.Name = "burrow-eight"
	for len(m.Targets) < 8 {
		m.Targets = append(m.Targets, m.Targets[0])
	}
	if _, err := s.CreateModel(ctx, m); err != nil {
		t.Fatalf("eight targets: %v", err)
	}
	list, err := s.ListModels(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v %+v", err, list)
	}
}

func TestUpdateAndDeleteModel(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	modelFixture(t, s)
	if _, err := s.CreateModel(ctx, simpleModel()); err != nil {
		t.Fatal(err)
	}
	other := simpleModel()
	other.Name = "burrow-other"
	if _, err := s.CreateModel(ctx, other); err != nil {
		t.Fatal(err)
	}

	// Same name: the model is not its own duplicate.
	m := simpleModel()
	m.Description, m.FallbackOnRateLimit = "cheap", true
	m.Targets = append(m.Targets, db.AIModelTarget{ProviderSlug: "zai", TargetModel: "glm"})
	got, err := s.UpdateModel(ctx, "burrow-simple", m)
	if err != nil || got.Description != "cheap" || !got.FallbackOnRateLimit || len(got.Targets) != 2 || got.Targets[1].ProviderSlug != "zai" {
		t.Fatalf("update: %v %+v", err, got)
	}
	m.Name = "burrow-renamed"
	if got, err = s.UpdateModel(ctx, "burrow-simple", m); err != nil || got.Name != "burrow-renamed" {
		t.Fatalf("rename: %v %+v", err, got)
	}
	if _, err := s.ModelByName(ctx, "burrow-simple"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("old name err = %v", err)
	}
	m.Name = "burrow-other"
	if _, err := s.UpdateModel(ctx, "burrow-renamed", m); !errors.Is(err, ErrModelExists) {
		t.Fatalf("rename onto existing err = %v", err)
	}
	m.Name = "zai"
	if _, err := s.UpdateModel(ctx, "burrow-renamed", m); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("rename onto provider slug err = %v", err)
	}
	m.Name = "burrow-renamed"
	if _, err := s.UpdateModel(ctx, "missing", m); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("update missing err = %v", err)
	}
	if err := s.DeleteModel(ctx, "burrow-renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(ctx, "burrow-renamed"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestProviderGuards(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	svcOllama, adminID := modelFixture(t, s)
	if _, err := s.CreateModel(ctx, simpleModel()); err != nil {
		t.Fatal(err)
	}
	second := simpleModel()
	second.Name = "another-model"
	if _, err := s.CreateModel(ctx, second); err != nil {
		t.Fatal(err)
	}

	err := s.DeleteProvider(ctx, "ollama")
	if !errors.Is(err, ErrProviderInUse) || !strings.Contains(err.Error(), "another-model, burrow-simple") {
		t.Fatalf("delete in use err = %v", err)
	}
	if _, err := s.ProviderBySlug(ctx, "ollama"); err != nil {
		t.Fatalf("provider gone after a refused delete: %v", err)
	}
	if err := s.DeleteProvider(ctx, "gone"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}

	// A provider cannot take a model's name, on any path that sets a slug.
	if _, err := s.UpdateProvider(ctx, "ollama", "burrow-simple", "x"); !errors.Is(err, ErrProviderExists) {
		t.Fatalf("rename onto model name err = %v", err)
	}
	if _, err := s.CreateDirectProvider(ctx, adminID, DirectProviderInput{Slug: "burrow-simple", Name: "X", BaseURL: "https://x.example/v1", CredentialSlot: "S"}); !errors.Is(err, ErrProviderExists) {
		t.Fatalf("direct provider on model name err = %v", err)
	}
	if err := s.DeleteProvider(ctx, "zai"); err != nil { // no model uses it
		t.Fatal(err)
	}
	u := mustCreateUser(t, s, "guards@x", "user")
	free := mustGetOrCreateService(t, s, u.ID, "free", "http")
	if err := s.q.SetServiceAccessMode(ctx, free.ID, "api_key", "Authorization"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTunnelProvider(ctx, "burrow-simple", "X", free.ID); !errors.Is(err, ErrProviderExists) {
		t.Fatalf("tunnel provider on model name err = %v", err)
	}

	// A rename is followed by the targets.
	if _, err := s.UpdateProvider(ctx, "ollama", "local", "Local"); err != nil {
		t.Fatal(err)
	}
	m, err := s.ModelByName(ctx, "burrow-simple")
	if err != nil || len(m.Targets) != 1 || m.Targets[0].ProviderSlug != "local" {
		t.Fatalf("after provider rename: %v %+v", err, m)
	}
	if p, err := s.ProviderBySlug(ctx, "local"); err != nil || p.ServiceID != svcOllama {
		t.Fatalf("renamed provider: %v %+v", err, p)
	}

	for _, name := range []string{"burrow-simple", "another-model"} {
		if err := s.DeleteModel(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteProvider(ctx, "local"); err != nil {
		t.Fatalf("delete after the models are gone: %v", err)
	}
}

func TestUpdateProviderUpstream_FormatLockedByModels(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	modelFixture(t, s)
	m := simpleModel()
	m.Targets = []db.AIModelTarget{{ProviderSlug: "zai", TargetModel: "glm"}}
	if _, err := s.CreateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	_, err := s.UpdateProviderUpstream(ctx, "zai", DirectProviderInput{APIFormat: "anthropic"})
	if !errors.Is(err, ErrInvalidProviderConfig) || !strings.Contains(err.Error(), "burrow-simple") {
		t.Fatalf("format change err = %v", err)
	}
	if p, _ := s.ProviderBySlug(ctx, "zai"); p.APIFormat != "openai" {
		t.Fatalf("format changed anyway: %+v", p)
	}
	// Everything else may change, and naming the current format is no change.
	if p, err := s.UpdateProviderUpstream(ctx, "zai", DirectProviderInput{APIFormat: "openai", Billing: "flat"}); err != nil || p.Billing != "flat" {
		t.Fatalf("other fields: %v %+v", err, p)
	}
	if err := s.DeleteModel(ctx, "burrow-simple"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.UpdateProviderUpstream(ctx, "zai", DirectProviderInput{APIFormat: "anthropic"}); err != nil || p.APIFormat != "anthropic" {
		t.Fatalf("format change without models: %v %+v", err, p)
	}
}

func TestImportModelAliases(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	svcOllama, _ := modelFixture(t, s)
	u := mustCreateUser(t, s, "alias@x", "user")
	orphan := mustGetOrCreateService(t, s, u.ID, "orphan", "http")
	for _, a := range []db.ModelAlias{
		{Alias: "fast", ConcreteModel: "qwen2.5:0.5b", ServiceID: svcOllama},
		{Alias: "Bad Name", ConcreteModel: "x", ServiceID: svcOllama},
		{Alias: "orphan", ConcreteModel: "y", ServiceID: orphan.ID},
		{Alias: "zai", ConcreteModel: "z", ServiceID: svcOllama}, // a provider's slug
	} {
		if err := s.q.CreateModelAlias(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ImportModelAliases(ctx)
	if err != nil || n != 1 {
		t.Fatalf("import: n=%d err=%v", n, err)
	}
	m, err := s.ModelByName(ctx, "fast")
	if err != nil || !m.Enabled || len(m.Targets) != 1 ||
		m.Targets[0] != (db.AIModelTarget{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "qwen2.5:0.5b"}) {
		t.Fatalf("imported model: %v %+v", err, m)
	}
	if list, _ := s.ListModels(ctx); len(list) != 1 {
		t.Fatalf("models: %+v", list)
	}
	if n, err := s.ImportModelAliases(ctx); err != nil || n != 0 {
		t.Fatalf("second run: n=%d err=%v", n, err)
	}
	// A model an admin deleted does not come back at the next start.
	if err := s.DeleteModel(ctx, "fast"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ImportModelAliases(ctx); err != nil || n != 0 {
		t.Fatalf("run after delete: n=%d err=%v", n, err)
	}
}

// An alias whose name a model already has is left alone.
func TestImportModelAliases_KeepsExistingModel(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	svcOllama, _ := modelFixture(t, s)
	m := simpleModel()
	m.Name = "fast"
	if _, err := s.CreateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.q.CreateModelAlias(ctx, db.ModelAlias{Alias: "fast", ConcreteModel: "other", ServiceID: svcOllama}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ImportModelAliases(ctx); err != nil || n != 0 {
		t.Fatalf("import: n=%d err=%v", n, err)
	}
	if got, _ := s.ModelByName(ctx, "fast"); len(got.Targets) != 1 || got.Targets[0].TargetModel != "mistral" {
		t.Fatalf("existing model changed: %+v", got)
	}
}
