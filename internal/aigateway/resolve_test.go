package aigateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

type fakeSynthetic map[string]db.AIModel

func (f fakeSynthetic) ModelByName(_ context.Context, name string) (db.AIModel, error) {
	m, ok := f[name]
	if !ok {
		return db.AIModel{}, store.ErrModelNotFound
	}
	return m, nil
}

func resolveGateway() *Gateway {
	g := newGateway(http.NotFoundHandler(), nil)
	g.Providers = fakeProviders{
		"ollama":        {Slug: "ollama", Kind: "tunnel", ServiceID: "svc1", APIFormat: "openai"},
		"openrouter":    {Slug: "openrouter", Kind: "direct", ServiceID: "prov-openrouter", APIFormat: "openai"},
		"zai":           {Slug: "zai", Kind: "direct", ServiceID: "prov-zai", APIFormat: "openai"},
		"zai-anthropic": {Slug: "zai-anthropic", Kind: "direct", ServiceID: "prov-zai-a", APIFormat: "anthropic"},
	}
	g.Synthetic = fakeSynthetic{
		// served in both dialects, two openai targets
		"burrow-intelligence": {Name: "burrow-intelligence", Enabled: true, Targets: []db.AIModelTarget{
			{Dialect: "anthropic", Position: 0, ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"},
			{Dialect: "openai", Position: 0, ProviderSlug: "zai", TargetModel: "glm-5.1"},
			{Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
		}},
		// openai only
		"burrow-simple": {Name: "burrow-simple", Enabled: true, Targets: []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "ollama", TargetModel: "mistral"},
		}},
		"burrow-off":    {Name: "burrow-off", Enabled: false, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm"}}},
		"burrow-broken": {Name: "burrow-broken", Enabled: true, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "gone", TargetModel: "x"}}},
		"burrow-half": {Name: "burrow-half", Enabled: true, Targets: []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "gone", TargetModel: "x"},
			{Dialect: "openai", Position: 1, ProviderSlug: "ollama", TargetModel: "mistral"},
		}},
		// The target rows say "openai", but zai-anthropic speaks anthropic:
		// the store's format lock was bypassed (it is check-then-write).
		"burrow-stale": {Name: "burrow-stale", Enabled: true, Targets: []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"},
			{Dialect: "openai", Position: 1, ProviderSlug: "zai", TargetModel: "glm-5.1"},
		}},
		"burrow-stale-only": {Name: "burrow-stale-only", Enabled: true, Targets: []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"},
		}},
	}
	return g
}

func TestResolve_Synthetic(t *testing.T) {
	g := resolveGateway()
	ctx := context.Background()

	res, err := g.resolve(ctx, "burrow-intelligence", "openai")
	if err != nil || !res.Synthetic || res.Dialect != "openai" || len(res.Targets) != 2 ||
		res.Requested != "burrow-intelligence" || res.Model.Name != "burrow-intelligence" ||
		res.Targets[0].Provider.Slug != "zai" || res.Targets[0].Model != "glm-5.1" ||
		res.Targets[1].Provider.Slug != "openrouter" || res.Targets[1].Model != "google/gemini-x" {
		t.Fatalf("openai: %v %+v", err, res)
	}
	res, err = g.resolve(ctx, "burrow-intelligence", "anthropic")
	if err != nil || len(res.Targets) != 1 || res.Targets[0].Provider.Slug != "zai-anthropic" {
		t.Fatalf("anthropic: %v %+v", err, res)
	}
	// A target whose provider is gone is skipped as long as another remains.
	res, err = g.resolve(ctx, "burrow-half", "openai")
	if err != nil || len(res.Targets) != 1 || res.Targets[0].Provider.Slug != "ollama" {
		t.Fatalf("burrow-half: %v %+v", err, res)
	}
}

// The provider's own api_format is checked at request time, whatever the
// target row claims: a target of the other format is never returned.
func TestResolve_Synthetic_ProviderFormatIsRechecked(t *testing.T) {
	g := resolveGateway()
	ctx := context.Background()

	res, err := g.resolve(ctx, "burrow-stale", "openai")
	if err != nil || len(res.Targets) != 1 || res.Targets[0].Provider.Slug != "zai" {
		t.Fatalf("burrow-stale: %v %+v", err, res)
	}
	for _, d := range []string{"openai", "anthropic"} {
		res, err := g.resolve(ctx, "burrow-stale-only", d)
		if err == nil || len(res.Targets) != 0 {
			t.Fatalf("burrow-stale-only in %s resolved: %+v", d, res)
		}
	}
	// Every resolution in this fixture holds the invariant.
	for name := range g.Synthetic.(fakeSynthetic) {
		for _, d := range []string{"openai", "anthropic"} {
			res, err := g.resolve(ctx, name, d)
			if err == nil && len(res.Targets) == 0 {
				t.Errorf("%s/%s: no error and no targets", name, d)
			}
			for _, tg := range res.Targets {
				if tg.Provider.APIFormat != d {
					t.Errorf("%s/%s: target %s speaks %s", name, d, tg.Provider.Slug, tg.Provider.APIFormat)
				}
			}
		}
	}
}

func TestResolve_Direct(t *testing.T) {
	g := resolveGateway()
	ctx := context.Background()
	// The provider is everything before the first slash.
	direct := map[string][2]string{
		"zai/glm-5.1":                {"zai", "glm-5.1"},
		"openrouter/google/gemini-x": {"openrouter", "google/gemini-x"},
		"openrouter/a//b/":           {"openrouter", "a//b/"},
		"ollama/qwen2.5:0.5b":        {"ollama", "qwen2.5:0.5b"},
	}
	for name, want := range direct {
		res, err := g.resolve(ctx, name, "openai")
		if err != nil || res.Synthetic || len(res.Targets) != 1 || res.Dialect != "openai" ||
			res.Targets[0].Provider.Slug != want[0] || res.Targets[0].Model != want[1] || res.Requested != name {
			t.Errorf("%s: %v %+v", name, err, res)
		}
	}
	res, err := g.resolve(ctx, "zai-anthropic/glm-5.1", "anthropic")
	if err != nil || res.Targets[0].Provider.Slug != "zai-anthropic" {
		t.Fatalf("anthropic direct: %v %+v", err, res)
	}
}

func TestResolve_NotFound(t *testing.T) {
	g := resolveGateway()
	for _, name := range []string{"", "unknown", "nope/model", "zai/", "/glm", "/", "burrow-off", "burrow-broken", "ZAI/glm"} {
		for _, d := range []string{"openai", "anthropic"} {
			if _, err := g.resolve(context.Background(), name, d); !errors.Is(err, errModelNotFound) {
				t.Errorf("resolve(%q, %s) err = %v, want errModelNotFound", name, d, err)
			}
		}
	}
	// Over-long names are refused before any lookup.
	g.Synthetic = lookupCounter{t}
	g.Providers = providerCounter{t}
	for _, name := range []string{
		strings.Repeat("a", 64),
		strings.Repeat("a", 100_000),
		"zai/" + strings.Repeat("a", 253),
		"zai/" + strings.Repeat("a", 100_000),
		strings.Repeat("a", 300) + "/m",
	} {
		if _, err := g.resolve(context.Background(), name, "openai"); !errors.Is(err, errModelNotFound) {
			t.Errorf("resolve(%d bytes) err = %v, want errModelNotFound", len(name), err)
		}
	}
	// Without a synthetic-model store a plain name is unknown, not a crash.
	g.Synthetic = nil
	if _, err := g.resolve(context.Background(), "burrow-simple", "openai"); !errors.Is(err, errModelNotFound) {
		t.Fatalf("nil Synthetic: err = %v", err)
	}
}

// A request never crosses dialects. The error says where the model is served.
func TestResolve_FormatMismatch(t *testing.T) {
	g := resolveGateway()
	ctx := context.Background()
	cases := []struct{ name, dialect, servedBy string }{
		{"burrow-simple", "anthropic", "openai"},         // synthetic, only an openai target
		{"zai/glm-5.1", "anthropic", "openai"},           // direct address to an openai provider
		{"zai-anthropic/glm-5.1", "openai", "anthropic"}, // direct address to an anthropic provider
	}
	for _, c := range cases {
		res, err := g.resolve(ctx, c.name, c.dialect)
		var fm *formatMismatchError
		if !errors.As(err, &fm) || fm.Model != c.name || fm.Dialect != c.dialect || fm.ServedBy != c.servedBy {
			t.Errorf("resolve(%q, %s) err = %#v, want format mismatch served by %s", c.name, c.dialect, err, c.servedBy)
		}
		if len(res.Targets) != 0 {
			t.Errorf("resolve(%q, %s) returned targets with an error", c.name, c.dialect)
		}
	}
}

// lookupCounter and providerCounter fail the test when they are asked.
type lookupCounter struct{ t *testing.T }

func (l lookupCounter) ModelByName(_ context.Context, name string) (db.AIModel, error) {
	l.t.Errorf("model lookup for a name of %d bytes", len(name))
	return db.AIModel{}, store.ErrModelNotFound
}

type providerCounter struct{ t *testing.T }

func (p providerCounter) ProviderBySlug(_ context.Context, slug string) (db.AIProvider, error) {
	p.t.Errorf("provider lookup for a slug of %d bytes", len(slug))
	return db.AIProvider{}, db.ErrNotFound
}

// Names at the limit still resolve.
func TestResolve_LongestNames(t *testing.T) {
	g := resolveGateway()
	long := strings.Repeat("a", 63)
	g.Synthetic.(fakeSynthetic)[long] = db.AIModel{Name: long, Enabled: true, Targets: []db.AIModelTarget{
		{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm"},
	}}
	if res, err := g.resolve(context.Background(), long, "openai"); err != nil || len(res.Targets) != 1 {
		t.Fatalf("63-byte synthetic name: %v %+v", err, res)
	}
	direct := "zai/" + strings.Repeat("a", 252) // 256 bytes
	if res, err := g.resolve(context.Background(), direct, "openai"); err != nil || res.Targets[0].Model != direct[4:] {
		t.Fatalf("256-byte direct address: %v %+v", err, res)
	}
}

type errSynthetic struct{}

func (errSynthetic) ModelByName(context.Context, string) (db.AIModel, error) {
	return db.AIModel{}, errors.New("db down")
}

// downProviders fails the lookup of the listed slugs.
type downProviders struct {
	fakeProviders
	down map[string]bool
}

func (f downProviders) ProviderBySlug(ctx context.Context, slug string) (db.AIProvider, error) {
	if f.down[slug] {
		return db.AIProvider{}, errors.New("db down")
	}
	return f.fakeProviders.ProviderBySlug(ctx, slug)
}

// A provider this request can never use is not looked up: its failure must
// not fail the request.
func TestResolve_OtherDialectProviderIsNotLookedUp(t *testing.T) {
	g := resolveGateway()
	g.Providers = downProviders{g.Providers.(fakeProviders), map[string]bool{"zai-anthropic": true}}
	res, err := g.resolve(context.Background(), "burrow-intelligence", "openai")
	if err != nil || len(res.Targets) != 2 {
		t.Fatalf("openai: %v %+v", err, res)
	}
	// Asked in the dialect whose provider is down, it is a real error ...
	_, err = g.resolve(context.Background(), "burrow-intelligence", "anthropic")
	var fm *formatMismatchError
	if err == nil || errors.Is(err, errModelNotFound) || errors.As(err, &fm) {
		t.Fatalf("anthropic: err = %v, want a real error", err)
	}
	// ... and so it is when telling a mismatch from an unknown model needs it.
	g.Providers = downProviders{resolveGateway().Providers.(fakeProviders), map[string]bool{"ollama": true}}
	_, err = g.resolve(context.Background(), "burrow-simple", "anthropic")
	if err == nil || errors.Is(err, errModelNotFound) || errors.As(err, &fm) {
		t.Fatalf("burrow-simple: err = %v, want a real error", err)
	}
}

type errProviders struct{}

func (errProviders) ProviderBySlug(context.Context, string) (db.AIProvider, error) {
	return db.AIProvider{}, errors.New("db down")
}

func TestResolve_StoreErrorIsNotNotFound(t *testing.T) {
	g := resolveGateway()
	g.Synthetic = errSynthetic{}
	_, err := g.resolve(context.Background(), "burrow-intelligence", "openai")
	var fm *formatMismatchError
	if err == nil || errors.Is(err, errModelNotFound) || errors.As(err, &fm) {
		t.Fatalf("err = %v, want a real error", err)
	}

	g = resolveGateway()
	g.Providers = errProviders{}
	for _, name := range []string{"zai/glm-5.1", "burrow-intelligence"} {
		_, err := g.resolve(context.Background(), name, "openai")
		if err == nil || errors.Is(err, errModelNotFound) || errors.As(err, &fm) {
			t.Fatalf("%s: err = %v, want a real error", name, err)
		}
	}
}
