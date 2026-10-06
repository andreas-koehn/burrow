package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
			for len(m.Targets) < 9 { // distinct, so that it is the count that is refused
				next := m.Targets[0]
				next.TargetModel = fmt.Sprintf("m-%d", len(m.Targets))
				m.Targets = append(m.Targets, next)
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
		next := m.Targets[0]
		next.TargetModel = fmt.Sprintf("m-%d", len(m.Targets))
		m.Targets = append(m.Targets, next)
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
	if !errors.Is(err, ErrProviderInUse) || !strings.Contains(err.Error(), "another-model, burrow-simple") || strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("delete in use err = %v", err)
	}
	if _, err := s.ProviderBySlug(ctx, "ollama"); err != nil {
		t.Fatalf("provider gone after a refused delete: %v", err)
	}
	if err := s.DeleteProvider(ctx, "gone"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}

	// A provider cannot take a model's name, on any path that sets a slug.
	if _, err := s.UpdateProvider(ctx, "ollama", "burrow-simple", "x", nil); !errors.Is(err, ErrProviderExists) {
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
	if _, err := s.UpdateProvider(ctx, "ollama", "local", "Local", nil); err != nil {
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

// The same provider and model twice in one format would be tried twice in a
// row; with and without a dialect it is the same target once the dialect is
// filled in.
func TestCreateModel_DuplicateTargets(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	modelFixture(t, s)
	for name, targets := range map[string][]db.AIModelTarget{
		"identical": {
			{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm"},
			{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm"},
		},
		"one without a dialect": {
			{ProviderSlug: "zai", TargetModel: "glm"},
			{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "mistral"},
			{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm"},
		},
		"after trimming": {
			{ProviderSlug: "zai", TargetModel: "glm"},
			{ProviderSlug: "zai", TargetModel: " glm "},
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := simpleModel()
			m.Targets = targets
			_, err := s.CreateModel(ctx, m)
			if !errors.Is(err, ErrInvalidModel) || !strings.Contains(err.Error(), "a target is listed twice") {
				t.Fatalf("create err = %v", err)
			}
		})
	}
	// The same model on two providers, and two models on one, are fine.
	m := simpleModel()
	m.Targets = []db.AIModelTarget{
		{ProviderSlug: "zai", TargetModel: "glm"},
		{ProviderSlug: "ollama", TargetModel: "glm"},
		{ProviderSlug: "zai", TargetModel: "glm-2"},
	}
	created, err := s.CreateModel(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	// An update is checked the same way.
	created.Targets = append(created.Targets, created.Targets[0])
	if _, err := s.UpdateModel(ctx, created.Name, created); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("update err = %v", err)
	}
}

// insertAlias writes a model_aliases row as an earlier version left it: the
// alias API is gone, the import still reads the table.
func insertAlias(t *testing.T, s *Store, a db.ModelAlias) {
	t.Helper()
	if _, err := s.q.DB().Exec(`INSERT INTO model_aliases(alias, concrete_model, service_id, provider, priority) VALUES(?,?,?,?,?)`,
		a.Alias, a.ConcreteModel, a.ServiceID, a.Provider, a.Priority); err != nil {
		t.Fatal(err)
	}
}

// backfilled runs the provider backfill, which the alias import waits for.
func backfilled(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.BackfillAIProviders(context.Background()); err != nil {
		t.Fatal(err)
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
		{Alias: "too-long", ConcreteModel: strings.Repeat("m", 201), ServiceID: svcOllama},
	} {
		insertAlias(t, s, a)
	}

	// Not before the provider backfill: without providers every alias would
	// be skipped, and the marker would make that final.
	if _, _, err := s.ImportModelAliases(ctx); !errors.Is(err, ErrProvidersNotBackfilled) {
		t.Fatalf("import before backfill err = %v", err)
	}
	backfilled(t, s)

	n, skipped, err := s.ImportModelAliases(ctx)
	if err != nil || n != 1 || fmt.Sprint(skipped) != "[Bad Name orphan too-long zai]" {
		t.Fatalf("import: n=%d skipped=%q err=%v", n, skipped, err)
	}
	m, err := s.ModelByName(ctx, "fast")
	if err != nil || !m.Enabled || len(m.Targets) != 1 ||
		m.Targets[0] != (db.AIModelTarget{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "qwen2.5:0.5b"}) {
		t.Fatalf("imported model: %v %+v", err, m)
	}
	if list, _ := s.ListModels(ctx); len(list) != 1 {
		t.Fatalf("models: %+v", list)
	}
	if n, skipped, err := s.ImportModelAliases(ctx); err != nil || n != 0 || len(skipped) != 0 {
		t.Fatalf("second run: n=%d skipped=%q err=%v", n, skipped, err)
	}
	// A model an admin deleted does not come back at the next start.
	if err := s.DeleteModel(ctx, "fast"); err != nil {
		t.Fatal(err)
	}
	if n, _, err := s.ImportModelAliases(ctx); err != nil || n != 0 {
		t.Fatalf("run after delete: n=%d err=%v", n, err)
	}
}

// An alias whose name a model already has is left alone, and reported.
func TestImportModelAliases_KeepsExistingModel(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	svcOllama, _ := modelFixture(t, s)
	backfilled(t, s)
	m := simpleModel()
	m.Name = "fast"
	if _, err := s.CreateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	insertAlias(t, s, db.ModelAlias{Alias: "fast", ConcreteModel: "other", ServiceID: svcOllama})
	if n, skipped, err := s.ImportModelAliases(ctx); err != nil || n != 0 || fmt.Sprint(skipped) != "[fast]" {
		t.Fatalf("import: n=%d skipped=%q err=%v", n, skipped, err)
	}
	if got, _ := s.ModelByName(ctx, "fast"); len(got.Targets) != 1 || got.Targets[0].TargetModel != "mistral" {
		t.Fatalf("existing model changed: %+v", got)
	}
}

// model_aliases has the alias as its primary key, so one name has one row in
// today's schema; the grouping is still defined for rows that share a name
// and is checked here on rows that never touch the table.
func TestAliasModels_GroupsAndOrders(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	providers := map[string]db.AIProvider{
		"svc-a": {Slug: "alpha", APIFormat: "openai"},
		"svc-b": {Slug: "beta", APIFormat: "openai"},
		"svc-c": {Slug: "claude", APIFormat: "anthropic"},
	}
	rows := []db.ModelAlias{
		{Alias: "smart", ConcreteModel: "b-late", ServiceID: "svc-b", Priority: 10, CreatedAt: t0.Add(time.Hour)},
		{Alias: "smart", ConcreteModel: "a-low", ServiceID: "svc-a", Priority: 50, CreatedAt: t0},
		{Alias: "smart", ConcreteModel: "c-only", ServiceID: "svc-c", Priority: 99, CreatedAt: t0},
		{Alias: "smart", ConcreteModel: "a-early", ServiceID: "svc-a", Priority: 10, CreatedAt: t0},
		{Alias: "smart", ConcreteModel: "b-tie", ServiceID: "svc-b", Priority: 10, CreatedAt: t0}, // same priority and time as a-early
		{Alias: "smart", ConcreteModel: "gone", ServiceID: "svc-none", Priority: 1, CreatedAt: t0},
		{Alias: "Bad Name", ConcreteModel: "x", ServiceID: "svc-a"},
		{Alias: "lonely", ConcreteModel: "y", ServiceID: "svc-none"},
		{Alias: "other", ConcreteModel: "z", ServiceID: "svc-b", Priority: 100, CreatedAt: t0},
	}
	want := "[{other [{openai 0 beta z}]} {smart [{openai 0 alpha a-early} {openai 0 beta b-tie} {openai 0 beta b-late} {openai 0 alpha a-low} {anthropic 0 claude c-only}]}]"
	wantSkipped := "[Bad Name lonely]"
	lookup := func(id string) (db.AIProvider, bool) { p, ok := providers[id]; return p, ok }
	// The result does not depend on the order the rows arrive in.
	for shift := 0; shift < len(rows); shift++ {
		in := append(append([]db.ModelAlias{}, rows[shift:]...), rows[:shift]...)
		models, skipped := aliasModels(in, lookup)
		type view struct {
			Name    string
			Targets []db.AIModelTarget
		}
		var got []view
		for _, m := range models {
			if !m.Enabled {
				t.Fatalf("%s is not enabled", m.Name)
			}
			got = append(got, view{m.Name, m.Targets})
		}
		if fmt.Sprint(got) != want || fmt.Sprint(skipped) != wantSkipped {
			t.Fatalf("shift %d:\n got %v, skipped %q\nwant %v, skipped %q", shift, got, skipped, want, wantSkipped)
		}
	}
}

func TestBackfillAIProviders_SkipsModelNames(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	svcA, _, _ := providerFixture(t, s)
	if _, err := s.CreateTunnelProvider(ctx, "ollama", "Ollama", svcA); err != nil {
		t.Fatal(err)
	}
	m := simpleModel()
	m.Name = "second-llm"
	if _, err := s.CreateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	u := mustCreateUser(t, s, "bf-model@x", "user")
	svc := mustGetOrCreateService(t, s, u.ID, "Second LLM", "http")
	if err := s.q.SetServiceSubdomain(ctx, svc.ID, "k3m9qa"); err != nil {
		t.Fatal(err)
	}
	if err := s.q.SetServiceAccessMode(ctx, svc.ID, "api_key", "Authorization"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BackfillAIProviders(ctx); err != nil || n != 1 {
		t.Fatalf("backfill: n=%d err=%v", n, err)
	}
	// The name-derived slug is a model's name, so the service's own slug is used.
	p, err := s.q.GetAIProviderByService(ctx, svc.ID)
	if err != nil || p.Slug != "k3m9qa" {
		t.Fatalf("provider: %v %+v", err, p)
	}
}

// A user who owns the service behind a provider cannot be deleted while a
// model targets that provider.
func TestDeleteUser_RefusedWhileModelTargetsProvider(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, adminID := modelFixture(t, s)
	m := simpleModel()
	m.Targets = []db.AIModelTarget{{ProviderSlug: "zai", TargetModel: "glm"}}
	if _, err := s.CreateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	err := s.DeleteUser(ctx, adminID)
	if !errors.Is(err, ErrProviderInUse) || !strings.Contains(err.Error(), "burrow-simple") || strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("delete user err = %v", err)
	}
	if _, err := s.ProviderBySlug(ctx, "zai"); err != nil {
		t.Fatalf("provider after the refused delete: %v", err)
	}
	if err := s.DeleteModel(ctx, "burrow-simple"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, adminID); err != nil {
		t.Fatalf("delete user without models: %v", err)
	}
	if _, err := s.ProviderBySlug(ctx, "zai"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("provider after its owner went: %v", err)
	}
}
