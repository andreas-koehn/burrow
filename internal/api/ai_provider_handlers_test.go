package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// ---------------------------------------------------------------------------
// Helpers — tiny fakes wired together for these tests only
// ---------------------------------------------------------------------------

// fakeProviderStore implements AIProviderStore over a slice. A non-nil
// createErr/updateErr/deleteErr is returned instead of touching the slice.
// It does not validate slugs — that is the handlers' job — and counts the
// create/update calls it receives.
type fakeProviderStore struct {
	rows      []db.AIProvider
	writes    int
	createErr error
	updateErr error
	deleteErr error

	models     map[string][]db.AIProviderModel
	replaces   int
	modelErr   error
	lastOwner  string
	lastDirect store.DirectProviderInput
}

func (f *fakeProviderStore) CreateDirectProvider(_ context.Context, ownerID string, in store.DirectProviderInput) (db.AIProvider, error) {
	f.writes++
	f.lastOwner, f.lastDirect = ownerID, in
	if f.createErr != nil {
		return db.AIProvider{}, f.createErr
	}
	p := db.AIProvider{
		Slug: in.Slug, Name: in.Name, Kind: "direct", ServiceID: "prov-" + in.Slug, APIFormat: "openai",
		BaseURL: in.BaseURL, CredentialSlot: in.CredentialSlot, AuthHeader: in.AuthHeader, AuthFormat: in.AuthFormat,
		ExtraHeaders: in.ExtraHeaders, Billing: "metered",
	}
	if in.Billing != "" {
		p.Billing = in.Billing
	}
	f.rows = append(f.rows, p)
	return p, nil
}

func (f *fakeProviderStore) UpdateProviderUpstream(_ context.Context, slug string, in store.DirectProviderInput) (db.AIProvider, error) {
	f.writes++
	f.lastDirect = in
	if f.updateErr != nil {
		return db.AIProvider{}, f.updateErr
	}
	for i := range f.rows {
		if f.rows[i].Slug == slug {
			if in.BaseURL != "" {
				f.rows[i].BaseURL = in.BaseURL
			}
			if in.CredentialSlot != "" {
				f.rows[i].CredentialSlot = in.CredentialSlot
			}
			if in.Billing != "" {
				f.rows[i].Billing = in.Billing
			}
			if in.ExtraHeaders != nil {
				f.rows[i].ExtraHeaders = in.ExtraHeaders
			}
			return f.rows[i], nil
		}
	}
	return db.AIProvider{}, store.ErrProviderNotFound
}

func (f *fakeProviderStore) has(slug string) bool {
	for _, p := range f.rows {
		if p.Slug == slug {
			return true
		}
	}
	return false
}

func (f *fakeProviderStore) ListProviderModels(_ context.Context, slug string) ([]db.AIProviderModel, error) {
	if !f.has(slug) {
		return nil, store.ErrProviderNotFound
	}
	return append([]db.AIProviderModel{}, f.models[slug]...), nil
}

func (f *fakeProviderStore) ReplaceProviderModels(_ context.Context, slug string, models []db.AIProviderModel) error {
	f.replaces++
	if f.modelErr != nil {
		return f.modelErr
	}
	if f.models == nil {
		f.models = map[string][]db.AIProviderModel{}
	}
	f.models[slug] = models
	return nil
}

func (f *fakeProviderStore) AddProviderModel(_ context.Context, slug, modelID string) error {
	f.writes++
	if f.modelErr != nil {
		return f.modelErr
	}
	if f.models == nil {
		f.models = map[string][]db.AIProviderModel{}
	}
	f.models[slug] = append(f.models[slug], db.AIProviderModel{ProviderSlug: slug, ModelID: modelID})
	return nil
}

func (f *fakeProviderStore) RemoveProviderModel(_ context.Context, slug, modelID string) error {
	f.writes++
	for i, m := range f.models[slug] {
		if m.ModelID == modelID {
			f.models[slug] = append(f.models[slug][:i], f.models[slug][i+1:]...)
			return nil
		}
	}
	return store.ErrProviderNotFound
}

func (f *fakeProviderStore) ListProviders(context.Context) ([]db.AIProvider, error) {
	return append([]db.AIProvider{}, f.rows...), nil
}

func (f *fakeProviderStore) ProviderBySlug(_ context.Context, slug string) (db.AIProvider, error) {
	for _, p := range f.rows {
		if p.Slug == slug {
			return p, nil
		}
	}
	return db.AIProvider{}, db.ErrNotFound
}

func (f *fakeProviderStore) CreateTunnelProvider(_ context.Context, slug, name, serviceID string) (db.AIProvider, error) {
	f.writes++
	if f.createErr != nil {
		return db.AIProvider{}, f.createErr
	}
	p := db.AIProvider{Slug: slug, Name: name, Kind: "tunnel", ServiceID: serviceID, APIFormat: "openai"}
	f.rows = append(f.rows, p)
	return p, nil
}

func (f *fakeProviderStore) UpdateProvider(_ context.Context, slug, newSlug, name string) (db.AIProvider, error) {
	f.writes++
	if f.updateErr != nil {
		return db.AIProvider{}, f.updateErr
	}
	for i := range f.rows {
		if f.rows[i].Slug == slug {
			f.rows[i].Slug, f.rows[i].Name = newSlug, name
			return f.rows[i], nil
		}
	}
	return db.AIProvider{}, store.ErrProviderNotFound
}

func (f *fakeProviderStore) DeleteProvider(_ context.Context, slug string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for i := range f.rows {
		if f.rows[i].Slug == slug {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return store.ErrProviderNotFound
}

// tunnelProvider is one provider row as the fake store holds it.
func tunnelProvider(slug, name, serviceID string) db.AIProvider {
	return db.AIProvider{Slug: slug, Name: name, Kind: "tunnel", ServiceID: serviceID, APIFormat: "openai"}
}

// newAIProviderDeps assembles Deps for AI provider handler tests.
func newAIProviderDeps(ss *fakeServiceStore, aliases *fakeModelAliasStore, ps *fakeProviderStore) Deps {
	return Deps{
		Users:        &fakeUserStore{role: "admin"},
		Services:     ss,
		AIProviders:  ps,
		ModelAliases: aliases,
		AuthDomain:   "burrow.example.com",
		Log:          discardLog(),
	}
}

// newAIProviderServer builds an httptest.Server with the given Deps and
// returns an authenticated authClient ready to call AI provider routes.
func newAIProviderServer(t *testing.T, d Deps) (*httptest.Server, *authClient) {
	t.Helper()
	srv := httptest.NewServer(NewRouter(d))
	c := authedClient(t, srv)
	return srv, c
}

// seedAPIKey adds one fake API key entry to fakeServiceStore.listKeys so that
// api_key_count reflects at least one key for the given service.
func seedAPIKey(ss *fakeServiceStore, keyID string) {
	ss.listKeys = append(ss.listKeys, db.ServiceAPIKey{ID: keyID, Name: "k1"})
}

// oneProviderFixture is the common setup: service "svc1" in api_key mode,
// visible to the caller, backing provider "ollama".
func oneProviderFixture() (*fakeServiceStore, *fakeProviderStore) {
	ss := &fakeServiceStore{
		listSvcs: []store.ServiceView{
			{ID: "svc1", Name: "my-llm", Type: "http", AccessMode: "api_key"},
		},
		getSvc: store.ServiceDetail{
			ServiceView: store.ServiceView{ID: "svc1", Name: "my-llm", Type: "http", AccessMode: "api_key"},
		},
	}
	return ss, &fakeProviderStore{rows: []db.AIProvider{tunnelProvider("ollama", "Ollama", "svc1")}}
}

func decodeProviders(t *testing.T, resp *http.Response) []aiProviderResp {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var out []aiProviderResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	return out
}

func wantStatus(t *testing.T, resp *http.Response, want int) string {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != want {
		t.Fatalf("want %d, got %d body=%s", want, resp.StatusCode, body)
	}
	return body
}

// ---------------------------------------------------------------------------
// GET /ai/providers
// ---------------------------------------------------------------------------

func TestListProviders(t *testing.T) {
	// One provider on an api_key service. The second service has no provider
	// row and must not appear, whatever its access mode: the table decides.
	ss := &fakeServiceStore{
		listSvcs: []store.ServiceView{
			{ID: "svc-ai", Name: "my-llm", Type: "http", AccessMode: "api_key"},
			{ID: "svc-other", Name: "web", Type: "http", AccessMode: "api_key"},
		},
	}
	seedAPIKey(ss, "key-1")

	aliases := newFakeModelAliasStore()
	if err := aliases.CreateModelAlias(context.Background(), db.ModelAlias{
		Alias:         "gpt4o",
		ConcreteModel: "gpt-4o",
		ServiceID:     "svc-ai",
		Provider:      "openai",
		Priority:      100,
	}); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	ps := &fakeProviderStore{rows: []db.AIProvider{tunnelProvider("ollama", "Ollama", "svc-ai")}}

	srv, c := newAIProviderServer(t, newAIProviderDeps(ss, aliases, ps))
	defer srv.Close()

	out := decodeProviders(t, c.get(t, "/api/v1/ai/providers"))
	if len(out) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(out), out)
	}
	p := out[0]

	if p.Slug != "ollama" || p.Name != "Ollama" || p.Kind != "tunnel" || p.APIFormat != "openai" {
		t.Errorf("identity: got %+v", p)
	}
	if p.ServiceID != "svc-ai" {
		t.Errorf("service_id: got %q want svc-ai", p.ServiceID)
	}
	if p.BaseURL != "https://burrow.example.com/ai/ollama/v1" {
		t.Errorf("base_url: got %q", p.BaseURL)
	}
	if p.ModelAlias != "gpt4o" {
		t.Errorf("model_alias: got %q want gpt4o", p.ModelAlias)
	}
	if p.ConcreteModel != "gpt-4o" {
		t.Errorf("concrete_model: got %q want gpt-4o", p.ConcreteModel)
	}
	if p.BackendType != "openai-compat" {
		t.Errorf("backend_type: got %q want openai-compat", p.BackendType)
	}
	if p.APIKeyCount != 1 {
		t.Errorf("api_key_count: got %d want 1", p.APIKeyCount)
	}
	// Zeroed metering fields.
	if p.Requests24h != 0 || p.CacheHits24h != 0 || p.LatencyP95ms != 0 {
		t.Errorf("metering: got %d/%d/%d want zeros", p.Requests24h, p.CacheHits24h, p.LatencyP95ms)
	}
	// No live tunnel wired → Offline.
	if p.Status != "Offline" {
		t.Errorf("status: got %q want Offline", p.Status)
	}
}

func TestListProviders_NoAuthDomain_BaseURLEmpty(t *testing.T) {
	ss, ps := oneProviderFixture()
	d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	d.AuthDomain = ""
	srv, c := newAIProviderServer(t, d)
	defer srv.Close()

	out := decodeProviders(t, c.get(t, "/api/v1/ai/providers"))
	if len(out) != 1 || out[0].BaseURL != "" {
		t.Fatalf("want one entry with empty base_url, got %+v", out)
	}
}

func TestListProviders_HidesProviderOfInvisibleService(t *testing.T) {
	// The caller's service list holds svc1 only; the provider on svc-hidden
	// belongs to someone else and must be omitted.
	ss, ps := oneProviderFixture()
	ps.rows = append(ps.rows, tunnelProvider("secret", "Secret", "svc-hidden"))
	d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	d.Users = &fakeUserStore{role: "user"}
	srv, c := newAIProviderServer(t, d)
	defer srv.Close()

	out := decodeProviders(t, c.get(t, "/api/v1/ai/providers"))
	if len(out) != 1 || out[0].Slug != "ollama" {
		t.Fatalf("want only ollama, got %+v", out)
	}
	// The single read applies the same visibility.
	wantStatus(t, c.get(t, "/api/v1/ai/providers/secret"), http.StatusNotFound)
}

func TestListProviders_SurfacesMetrics(t *testing.T) {
	ss, ps := oneProviderFixture()
	d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	d.AIMetrics = &fakeAIMetrics{counts: map[string]db.AIEndpointCount{"svc1": {Requests: 42, CacheHits: 7}}}

	srv, c := newAIProviderServer(t, d)
	defer srv.Close()

	out := decodeProviders(t, c.get(t, "/api/v1/ai/providers"))
	if len(out) != 1 {
		t.Fatalf("want 1 entry, got %d", len(out))
	}
	if out[0].Requests24h != 42 {
		t.Errorf("requests_24h: got %d want 42", out[0].Requests24h)
	}
	if out[0].CacheHits24h != 7 {
		t.Errorf("cache_hits_24h: got %d want 7", out[0].CacheHits24h)
	}
}

// ---------------------------------------------------------------------------
// GET /ai/providers/{slug}
// ---------------------------------------------------------------------------

func TestGetProvider(t *testing.T) {
	ss, ps := oneProviderFixture()
	srv, c := newAIProviderServer(t, newAIProviderDeps(ss, newFakeModelAliasStore(), ps))
	defer srv.Close()

	resp := c.get(t, "/api/v1/ai/providers/ollama")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var p aiProviderResp
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if p.Slug != "ollama" || p.ServiceID != "svc1" || p.BaseURL != "https://burrow.example.com/ai/ollama/v1" {
		t.Errorf("got %+v", p)
	}

	wantStatus(t, c.get(t, "/api/v1/ai/providers/nope"), http.StatusNotFound)
}

// ---------------------------------------------------------------------------
// GET /ai/providers/{slug}/metrics
// ---------------------------------------------------------------------------

type fakeAIMetrics struct {
	counts map[string]db.AIEndpointCount
	agg    db.AIEndpointAgg
	gotID  string
	err    error
}

func (f *fakeAIMetrics) AIEndpointCounts24h(context.Context) (map[string]db.AIEndpointCount, error) {
	return f.counts, f.err
}
func (f *fakeAIMetrics) AIEndpointMetrics24h(_ context.Context, serviceID string) (db.AIEndpointAgg, error) {
	f.gotID = serviceID
	return f.agg, f.err
}

func TestProviderMetrics(t *testing.T) {
	ss, ps := oneProviderFixture()
	srv, c := newAIProviderServer(t, newAIProviderDeps(ss, newFakeModelAliasStore(), ps))
	defer srv.Close()

	resp := c.get(t, "/api/v1/ai/providers/ollama/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var out endpointMetricsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	if out.Requests24h != 0 || out.TokensIn24h != 0 || out.TokensOut24h != 0 {
		t.Errorf("counts: got %+v want zeros", out)
	}
	if out.CostUSD24h != 0 || out.CacheHitRatio24h != 0 {
		t.Errorf("cost/ratio: got %v/%v want 0", out.CostUSD24h, out.CacheHitRatio24h)
	}
	if len(out.RequestsPerMinute) != 60 {
		t.Errorf("requests_per_minute: got len=%d want 60", len(out.RequestsPerMinute))
	}
	for i, v := range out.RequestsPerMinute {
		if v != 0 {
			t.Errorf("requests_per_minute[%d]: got %d want 0", i, v)
		}
	}

	// Unknown slug.
	wantStatus(t, c.get(t, "/api/v1/ai/providers/nope/metrics"), http.StatusNotFound)
}

// A provider that exists but whose service belongs to someone else answers
// 404 like an unknown slug, so slugs cannot be enumerated through the status.
func TestProviderMetrics_404WhenServiceNotVisible(t *testing.T) {
	ss, ps := oneProviderFixture()
	ss.getSvcErr = store.ErrForbidden
	srv, c := newAIProviderServer(t, newAIProviderDeps(ss, newFakeModelAliasStore(), ps))
	defer srv.Close()

	hidden := wantStatus(t, c.get(t, "/api/v1/ai/providers/ollama/metrics"), http.StatusNotFound)
	unknown := wantStatus(t, c.get(t, "/api/v1/ai/providers/nope/metrics"), http.StatusNotFound)
	if hidden != unknown {
		t.Errorf("hidden provider body %q differs from unknown provider body %q", hidden, unknown)
	}
}

// A failing metrics query is logged and the handlers still answer with zeros.
func TestProviderMetrics_LogsQueryFailure(t *testing.T) {
	ss, ps := oneProviderFixture()
	var logs bytes.Buffer
	d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	d.AIMetrics = &fakeAIMetrics{err: errors.New("usage query broke")}
	srv, c := newAIProviderServer(t, d)
	defer srv.Close()

	out := decodeProviders(t, c.get(t, "/api/v1/ai/providers"))
	if len(out) != 1 || out[0].Requests24h != 0 {
		t.Fatalf("list: want one zeroed entry, got %+v", out)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "usage query broke") {
		t.Errorf("list: counts failure not logged at warn: %q", logs.String())
	}

	logs.Reset()
	body := wantStatus(t, c.get(t, "/api/v1/ai/providers/ollama/metrics"), http.StatusOK)
	if !strings.Contains(body, `"requests_24h":0`) {
		t.Errorf("metrics: want zeros, got %s", body)
	}
	got := logs.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "provider=ollama") || !strings.Contains(got, "usage query broke") {
		t.Errorf("metrics: failure not logged at warn with the provider slug: %q", got)
	}
}

func TestProviderMetrics_SurfacesAggregates(t *testing.T) {
	ss, ps := oneProviderFixture()
	agg := db.AIEndpointAgg{Requests: 10, TokensIn: 100, TokensOut: 200, CacheHits: 4}
	agg.PerMinute[59] = 3
	d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	m := &fakeAIMetrics{agg: agg}
	d.AIMetrics = m

	srv, c := newAIProviderServer(t, d)
	defer srv.Close()

	resp := c.get(t, "/api/v1/ai/providers/ollama/metrics")
	var out endpointMetricsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if m.gotID != "svc1" {
		t.Errorf("metrics read for service %q, want the provider's service svc1", m.gotID)
	}
	if out.Requests24h != 10 {
		t.Errorf("requests_24h: got %d want 10", out.Requests24h)
	}
	if out.TokensIn24h != 100 || out.TokensOut24h != 200 {
		t.Errorf("tokens: got %d/%d want 100/200", out.TokensIn24h, out.TokensOut24h)
	}
	if out.CacheHitRatio24h != 0.4 {
		t.Errorf("cache_hit_ratio_24h: got %v want 0.4", out.CacheHitRatio24h)
	}
	if len(out.RequestsPerMinute) != 60 || out.RequestsPerMinute[59] != 3 {
		t.Errorf("requests_per_minute: got len=%d [59]=%d want len=60 [59]=3", len(out.RequestsPerMinute), out.RequestsPerMinute[59])
	}
}

// ---------------------------------------------------------------------------
// POST /ai/providers
// ---------------------------------------------------------------------------

func TestPostProvider(t *testing.T) {
	newServer := func(t *testing.T, ps *fakeProviderStore) (*authClient, *stubAuditAppender) {
		ss, _ := oneProviderFixture()
		aud := &stubAuditAppender{}
		d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
		d.AuditAppender = aud
		srv, c := newAIProviderServer(t, d)
		t.Cleanup(srv.Close)
		return c, aud
	}

	t.Run("derives the slug from the name", func(t *testing.T) {
		ps := &fakeProviderStore{}
		c, aud := newServer(t, ps)
		resp := c.post(t, "/api/v1/ai/providers", map[string]string{"name": "Ollama", "kind": "tunnel", "service_id": "svc1"})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("want 201, got %d body=%s", resp.StatusCode, readBody(t, resp))
		}
		var p aiProviderResp
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close()
		if p.Slug != "ollama" || p.Name != "Ollama" || p.Kind != "tunnel" || p.ServiceID != "svc1" {
			t.Errorf("body: got %+v", p)
		}
		if p.BaseURL != "https://burrow.example.com/ai/ollama/v1" {
			t.Errorf("base_url: got %q", p.BaseURL)
		}
		if len(ps.rows) != 1 || ps.rows[0].Slug != "ollama" {
			t.Errorf("store rows: %+v", ps.rows)
		}
		if len(aud.events) != 1 {
			t.Fatalf("want 1 audit event, got %d", len(aud.events))
		}
		ev := aud.events[0]
		if ev.Action != audit.ActionAIProviderCreate || ev.SubjectID != "ollama" || ev.SubjectLabel != "Ollama" {
			t.Errorf("audit event: %+v", ev)
		}
	})

	t.Run("kind defaults to tunnel", func(t *testing.T) {
		c, _ := newServer(t, &fakeProviderStore{})
		wantStatus(t, c.post(t, "/api/v1/ai/providers", map[string]string{"slug": "local", "name": "Local", "service_id": "svc1"}), http.StatusCreated)
	})

	bad := []struct {
		name string
		body map[string]string
		want string
	}{
		{"reserved slug", map[string]string{"slug": "v1", "name": "x", "kind": "tunnel", "service_id": "svc1"}, "slug must be"},
		{"malformed slug", map[string]string{"slug": "Not_OK", "name": "x", "kind": "tunnel", "service_id": "svc1"}, "slug must be"},
		{"no slug derivable from the name", map[string]string{"name": "ä", "kind": "tunnel", "service_id": "svc1"}, "slug must be"},
		{"empty name", map[string]string{"slug": "ok1", "name": "", "kind": "tunnel", "service_id": "svc1"}, "name is required"},
		{"blank name", map[string]string{"slug": "ok1", "name": "   ", "kind": "tunnel", "service_id": "svc1"}, "name is required"},
		{"long name", map[string]string{"slug": "ok1", "name": strings.Repeat("n", 121), "kind": "tunnel", "service_id": "svc1"}, "name must be at most 120 chars"},
		{"unknown kind", map[string]string{"slug": "ok1", "name": "x", "kind": "magic", "service_id": "svc1"}, "kind must be 'tunnel' or 'direct'"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			ps := &fakeProviderStore{}
			c, aud := newServer(t, ps)
			body := wantStatus(t, c.post(t, "/api/v1/ai/providers", tc.body), http.StatusBadRequest)
			if !strings.Contains(body, tc.want) {
				t.Errorf("body %q does not mention %q", body, tc.want)
			}
			if ps.writes != 0 || len(ps.rows) != 0 || len(aud.events) != 0 {
				t.Errorf("rejected request reached the store: writes=%d rows=%+v events=%+v", ps.writes, ps.rows, aud.events)
			}
		})
	}

	t.Run("invalid JSON", func(t *testing.T) {
		c, _ := newServer(t, &fakeProviderStore{})
		wantStatus(t, c.post(t, "/api/v1/ai/providers", "not an object"), http.StatusBadRequest)
	})

	for name, err := range map[string]error{"slug or service taken": store.ErrProviderExists, "service not eligible": store.ErrProviderService} {
		t.Run(name, func(t *testing.T) {
			c, aud := newServer(t, &fakeProviderStore{createErr: err})
			wantStatus(t, c.post(t, "/api/v1/ai/providers", map[string]string{"slug": "ok1", "name": "x", "kind": "tunnel", "service_id": "svc1"}), http.StatusConflict)
			if len(aud.events) != 0 {
				t.Errorf("failed create was audited: %+v", aud.events)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PUT /ai/providers/{slug}
// ---------------------------------------------------------------------------

func TestPutProvider(t *testing.T) {
	newServer := func(t *testing.T) (*authClient, *fakeProviderStore, *stubAuditAppender) {
		ss, ps := oneProviderFixture()
		aud := &stubAuditAppender{}
		d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
		d.AuditAppender = aud
		srv, c := newAIProviderServer(t, d)
		t.Cleanup(srv.Close)
		return c, ps, aud
	}

	t.Run("renames", func(t *testing.T) {
		c, ps, aud := newServer(t)
		resp := c.put(t, "/api/v1/ai/providers/ollama", map[string]string{"slug": "local", "name": "Local"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
		}
		var p aiProviderResp
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close()
		if p.Slug != "local" || p.Name != "Local" || p.BaseURL != "https://burrow.example.com/ai/local/v1" {
			t.Errorf("body: got %+v", p)
		}
		if ps.rows[0].Slug != "local" {
			t.Errorf("store row: %+v", ps.rows[0])
		}
		if len(aud.events) != 1 || aud.events[0].Action != audit.ActionAIProviderUpdate || aud.events[0].SubjectID != "local" {
			t.Fatalf("audit events: %+v", aud.events)
		}
		var payload map[string]string
		if err := json.Unmarshal(aud.events[0].Payload, &payload); err != nil {
			t.Fatalf("audit payload: %v", err)
		}
		if payload["old_slug"] != "ollama" || payload["new_slug"] != "local" {
			t.Errorf("audit payload: %+v", payload)
		}
	})

	for name, body := range map[string]map[string]string{
		"reserved slug":  {"slug": "v1", "name": "x"},
		"malformed slug": {"slug": "-bad-", "name": "x"},
		"missing slug":   {"name": "x"},
		"missing name":   {"slug": "local"},
		"long name":      {"slug": "local", "name": strings.Repeat("n", 121)},
	} {
		t.Run(name, func(t *testing.T) {
			c, ps, aud := newServer(t)
			wantStatus(t, c.put(t, "/api/v1/ai/providers/ollama", body), http.StatusBadRequest)
			if ps.writes != 0 || ps.rows[0].Slug != "ollama" || len(aud.events) != 0 {
				t.Errorf("rejected request reached the store: writes=%d rows=%+v events=%+v", ps.writes, ps.rows, aud.events)
			}
		})
	}

	t.Run("unknown provider", func(t *testing.T) {
		c, _, _ := newServer(t)
		wantStatus(t, c.put(t, "/api/v1/ai/providers/nope", map[string]string{"slug": "local", "name": "Local"}), http.StatusNotFound)
	})

	t.Run("slug taken", func(t *testing.T) {
		c, ps, _ := newServer(t)
		ps.updateErr = store.ErrProviderExists
		wantStatus(t, c.put(t, "/api/v1/ai/providers/ollama", map[string]string{"slug": "local", "name": "Local"}), http.StatusConflict)
	})
}

// ---------------------------------------------------------------------------
// DELETE /ai/providers/{slug}
// ---------------------------------------------------------------------------

func TestDeleteProvider(t *testing.T) {
	ss, ps := oneProviderFixture()
	aud := &stubAuditAppender{}
	d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	d.AuditAppender = aud
	srv, c := newAIProviderServer(t, d)
	defer srv.Close()

	wantStatus(t, c.delete(t, "/api/v1/ai/providers/ollama"), http.StatusNoContent)
	if len(ps.rows) != 0 {
		t.Errorf("provider not deleted: %+v", ps.rows)
	}
	if len(aud.events) != 1 || aud.events[0].Action != audit.ActionAIProviderDelete || aud.events[0].SubjectID != "ollama" {
		t.Fatalf("audit events: %+v", aud.events)
	}

	wantStatus(t, c.delete(t, "/api/v1/ai/providers/ollama"), http.StatusNotFound)
	if len(aud.events) != 1 {
		t.Errorf("failed delete was audited: %+v", aud.events)
	}
}

// ---------------------------------------------------------------------------
// Authorization, through the router
// ---------------------------------------------------------------------------

// providerRoutes is every /ai/providers route with a body that would succeed
// for an admin.
var providerRoutes = []struct {
	method, path string
	body         any
	adminOnly    bool
}{
	{http.MethodGet, "/api/v1/ai/providers", nil, false},
	{http.MethodGet, "/api/v1/ai/providers/ollama", nil, false},
	{http.MethodGet, "/api/v1/ai/providers/ollama/metrics", nil, false},
	{http.MethodPost, "/api/v1/ai/providers", map[string]string{"slug": "local", "name": "Local", "kind": "tunnel", "service_id": "svc1"}, true},
	{http.MethodPut, "/api/v1/ai/providers/ollama", map[string]string{"slug": "local", "name": "Local"}, true},
	{http.MethodPut, "/api/v1/ai/providers/ollama/upstream", map[string]string{"base_url": "https://x.example/v1"}, true},
	{http.MethodGet, "/api/v1/ai/providers/ollama/models", nil, false},
	{http.MethodPost, "/api/v1/ai/providers/ollama/models/sync", nil, true},
	{http.MethodPost, "/api/v1/ai/providers/ollama/models", map[string]string{"id": "mistral"}, true},
	{http.MethodDelete, "/api/v1/ai/providers/ollama/models?id=mistral", nil, true},
	{http.MethodPost, "/api/v1/ai/providers", map[string]string{"slug": "zai", "name": "z.ai", "kind": "direct", "base_url": "https://api.z.ai/v4", "credential_slot": "ZAI"}, true},
	// Last: it removes the provider the other routes address.
	{http.MethodDelete, "/api/v1/ai/providers/ollama", nil, true},
}

func TestProviderRoutes_Unauthenticated(t *testing.T) {
	ss, ps := oneProviderFixture()
	srv := httptest.NewServer(NewRouter(newAIProviderDeps(ss, newFakeModelAliasStore(), ps)))
	defer srv.Close()

	// No session cookie at all.
	anon := &authClient{base: srv.URL, hc: &http.Client{}}
	for _, rt := range providerRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			wantStatus(t, anon.do(t, rt.method, rt.path, rt.body), http.StatusUnauthorized)
		})
	}
	if len(ps.rows) != 1 || ps.rows[0].Slug != "ollama" {
		t.Errorf("anonymous request changed the store: %+v", ps.rows)
	}
}

func TestPostProvider_RequiresAdmin(t *testing.T) {
	ss, ps := oneProviderFixture()
	aud := &stubAuditAppender{}
	d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	d.Users = &fakeUserStore{role: "user"}
	d.AuditAppender = aud
	srv, c := newAIProviderServer(t, d)
	defer srv.Close()

	for _, rt := range providerRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			want := http.StatusOK
			if rt.adminOnly {
				want = http.StatusForbidden
			}
			wantStatus(t, c.do(t, rt.method, rt.path, rt.body), want)
		})
	}
	if len(ps.rows) != 1 || ps.rows[0].Slug != "ollama" || ps.rows[0].Name != "Ollama" {
		t.Errorf("non-admin request changed the store: %+v", ps.rows)
	}
	if len(aud.events) != 0 {
		t.Errorf("forbidden requests were audited: %+v", aud.events)
	}
}

// ---------------------------------------------------------------------------
// The derived "AI endpoints" routes are gone
// ---------------------------------------------------------------------------

func TestAIEndpointsRouteRemoved(t *testing.T) {
	ss, ps := oneProviderFixture()
	srv, c := newAIProviderServer(t, newAIProviderDeps(ss, newFakeModelAliasStore(), ps))
	defer srv.Close()

	for _, path := range []string{"/api/v1/ai/endpoints", "/api/v1/ai/endpoints/svc1/metrics"} {
		wantStatus(t, c.get(t, path), http.StatusNotFound)
	}
}

// No secret reaches a provider response: the view carries a key count only.
func TestProviderResponse_CarriesNoKeyMaterial(t *testing.T) {
	ss, ps := oneProviderFixture()
	ss.listKeys = []db.ServiceAPIKey{{ID: "key-1", Name: "k1", KeyHash: "hash-secretvalue"}}
	srv, c := newAIProviderServer(t, newAIProviderDeps(ss, newFakeModelAliasStore(), ps))
	defer srv.Close()

	for _, path := range []string{"/api/v1/ai/providers", "/api/v1/ai/providers/ollama"} {
		body := wantStatus(t, c.get(t, path), http.StatusOK)
		if strings.Contains(body, "hash-secretvalue") || strings.Contains(body, "key-1") {
			t.Errorf("%s leaks key data: %s", path, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Direct providers
// ---------------------------------------------------------------------------

// upstreamSecret is the value of the test vault's slot. No response, audit
// payload or log line may contain it.
const upstreamSecret = "sk-upstream-do-not-leak-7f3a"

type secretVault map[string]string

func (v secretVault) Get(slot string) (string, bool) { s, ok := v[slot]; return s, ok }
func (v secretVault) Slots() []string {
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	return out
}

// directFixture is a relay with one direct provider "openrouter" (slot
// OPENROUTER set in the vault) next to the tunnel provider "ollama".
type directFixture struct {
	ps   *fakeProviderStore
	aud  *stubAuditAppender
	logs *bytes.Buffer
	d    Deps
}

func newDirectFixture() *directFixture {
	ss, ps := oneProviderFixture()
	ss.listSvcs = append(ss.listSvcs,
		store.ServiceView{ID: "prov-openrouter", Name: "OpenRouter", Type: "direct", AccessMode: "api_key"},
		store.ServiceView{ID: "prov-zai", Name: "z.ai", Type: "direct", AccessMode: "api_key"},
		store.ServiceView{ID: "prov-nokey", Name: "No key", Type: "direct", AccessMode: "api_key"})
	ps.rows = append(ps.rows, db.AIProvider{
		Slug: "openrouter", Name: "OpenRouter", Kind: "direct", ServiceID: "prov-openrouter", APIFormat: "openai",
		BaseURL: "https://openrouter.ai/api/v1", CredentialSlot: "OPENROUTER", AuthHeader: "Authorization", AuthFormat: "Bearer {key}",
		ExtraHeaders: map[string]string{"X-Title": "header-value-do-not-leak"}, Billing: "metered",
	})
	f := &directFixture{ps: ps, aud: &stubAuditAppender{}, logs: &bytes.Buffer{}}
	f.d = newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	f.d.AuditAppender = f.aud
	f.d.Log = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.d.CredentialVault = secretVault{"OPENROUTER": upstreamSecret, "EMPTY": ""}
	f.d.HostCheck = func(context.Context, string) error { return nil }
	return f
}

func (f *directFixture) serve(t *testing.T) *authClient {
	t.Helper()
	srv, c := newAIProviderServer(t, f.d)
	t.Cleanup(srv.Close)
	return c
}

// noSecrets fails when the upstream credential or a header value shows up in
// body, in an audit payload or in the log.
func (f *directFixture) noSecrets(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{upstreamSecret, "header-value-do-not-leak"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q: %s", secret, body)
		}
		for _, ev := range f.aud.events {
			if strings.Contains(string(ev.Payload), secret) || strings.Contains(ev.SubjectLabel, secret) {
				t.Errorf("audit event %s leaks %q: %s", ev.Action, secret, ev.Payload)
			}
		}
		if strings.Contains(f.logs.String(), secret) {
			t.Errorf("log leaks %q: %s", secret, f.logs.String())
		}
	}
}

func decodeProvider(t *testing.T, body string) aiProviderResp {
	t.Helper()
	var p aiProviderResp
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return p
}

func TestPostDirectProvider(t *testing.T) {
	valid := map[string]any{
		"name": "z.ai", "slug": "zai", "kind": "direct", "base_url": "https://api.z.ai/v4", "credential_slot": "OPENROUTER",
		"billing": "flat", "extra_headers": map[string]string{"X-Title": "header-value-do-not-leak"},
	}

	t.Run("creates the provider", func(t *testing.T) {
		f := newDirectFixture()
		var checked string
		f.d.HostCheck = func(_ context.Context, host string) error { checked = host; return nil }
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers", valid), http.StatusCreated)
		p := decodeProvider(t, body)
		if p.Kind != "direct" || p.Slug != "zai" || p.ServiceID != "prov-zai" || p.UpstreamBaseURL != "https://api.z.ai/v4" ||
			p.CredentialSlot != "OPENROUTER" || p.Billing != "flat" || !p.CredentialPresent || p.Status != "Connected" ||
			p.BaseURL != "https://burrow.example.com/ai/zai/v1" {
			t.Errorf("view: %+v", p)
		}
		if checked != "api.z.ai" {
			t.Errorf("host check saw %q", checked)
		}
		if f.ps.lastOwner != "u-self" || f.ps.lastDirect.Slug != "zai" || f.ps.lastDirect.ExtraHeaders["X-Title"] == "" {
			t.Errorf("store input: owner %q %+v", f.ps.lastOwner, f.ps.lastDirect)
		}
		if len(f.aud.events) != 1 || f.aud.events[0].Action != audit.ActionAIProviderCreate || f.aud.events[0].SubjectID != "zai" {
			t.Fatalf("audit: %+v", f.aud.events)
		}
		var payload map[string]any
		_ = json.Unmarshal(f.aud.events[0].Payload, &payload)
		if payload["kind"] != "direct" || payload["base_url"] != "https://api.z.ai/v4" || payload["credential_slot"] != "OPENROUTER" || payload["billing"] != "flat" || len(payload) != 4 {
			t.Errorf("audit payload: %s", f.aud.events[0].Payload)
		}
		f.noSecrets(t, body)
	})

	t.Run("slot not in the vault", func(t *testing.T) {
		for _, slot := range []string{"MISSING", "EMPTY"} {
			f := newDirectFixture()
			c := f.serve(t)
			in := map[string]any{"name": "No key", "slug": "nokey", "kind": "direct", "base_url": "https://x.example/v1", "credential_slot": slot}
			p := decodeProvider(t, wantStatus(t, c.post(t, "/api/v1/ai/providers", in), http.StatusCreated))
			if p.CredentialPresent || p.Status != "Offline" {
				t.Errorf("slot %s: %+v", slot, p)
			}
		}
	})

	t.Run("a credential value in the body is not stored or echoed", func(t *testing.T) {
		f := newDirectFixture()
		c := f.serve(t)
		in := map[string]any{"name": "z.ai", "slug": "zai", "kind": "direct", "base_url": "https://api.z.ai/v4", "credential_slot": "OPENROUTER",
			"credential": upstreamSecret, "api_key": upstreamSecret}
		// Refused, so that the admin learns the key went nowhere (see
		// TestDirectProvider_UnknownFieldRefused); it is not stored or echoed.
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers", in), http.StatusBadRequest)
		if got := fmt.Sprintf("%+v", f.ps.lastDirect); strings.Contains(got, upstreamSecret) || f.ps.writes != 0 {
			t.Errorf("the store was called (%d writes) or handed a credential value: %s", f.ps.writes, got)
		}
		f.noSecrets(t, body)
	})

	t.Run("store refuses the configuration", func(t *testing.T) {
		f := newDirectFixture()
		f.ps.createErr = fmt.Errorf("%w: base URL must be an https URL without credentials, query or fragment", store.ErrInvalidProviderConfig)
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers", valid), http.StatusBadRequest)
		if !strings.Contains(body, `"base URL must be an https URL without credentials, query or fragment"`) || strings.Contains(body, "store:") {
			t.Errorf("body = %s", body)
		}
		if len(f.aud.events) != 0 {
			t.Errorf("a refused create was audited: %+v", f.aud.events)
		}
	})

	t.Run("host resolves to a private address", func(t *testing.T) {
		f := newDirectFixture()
		f.d.HostCheck = func(_ context.Context, host string) error {
			return fmt.Errorf("%w: %s resolves to 10.0.0.7", aiprovider.ErrBlockedAddress, host)
		}
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers", valid), http.StatusBadRequest)
		if !strings.Contains(body, "base URL resolves to a private or loopback address") || strings.Contains(body, "10.0.0.7") {
			t.Errorf("body = %s", body)
		}
		if f.ps.writes != 0 {
			t.Error("the store was called for a blocked host")
		}
	})

	t.Run("host does not resolve", func(t *testing.T) {
		f := newDirectFixture()
		f.d.HostCheck = func(context.Context, string) error {
			return errors.New("lookup api.z.ai on 10.0.0.53:53: no such host")
		}
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers", valid), http.StatusBadRequest)
		if !strings.Contains(body, "base URL host could not be resolved") || strings.Contains(body, "10.0.0.53") || f.ps.writes != 0 {
			t.Errorf("body = %s writes = %d", body, f.ps.writes)
		}
	})

	t.Run("a hanging resolver does not hang the request", func(t *testing.T) {
		old := upstreamHostCheckTimeout
		upstreamHostCheckTimeout = 50 * time.Millisecond
		t.Cleanup(func() { upstreamHostCheckTimeout = old })
		f := newDirectFixture()
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		// Ignores its context, like a resolver stuck in a system call.
		f.d.HostCheck = func(context.Context, string) error { <-release; return nil }
		c := f.serve(t)
		start := time.Now()
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers", valid), http.StatusBadRequest)
		if time.Since(start) > 3*time.Second || !strings.Contains(body, "could not be resolved") || f.ps.writes != 0 {
			t.Errorf("took %v body = %s writes = %d", time.Since(start), body, f.ps.writes)
		}
	})

	t.Run("malformed base URL never reaches the resolver", func(t *testing.T) {
		for _, u := range []string{"http://api.z.ai/v4", "https://u:pw@api.z.ai/v4", "https://api.z.ai/v4?x=1", "", "https://"} {
			f := newDirectFixture()
			f.d.HostCheck = func(context.Context, string) error { t.Errorf("%q reached the host check", u); return nil }
			c := f.serve(t)
			in := map[string]any{"name": "z.ai", "slug": "zai", "kind": "direct", "base_url": u, "credential_slot": "OPENROUTER"}
			body := wantStatus(t, c.post(t, "/api/v1/ai/providers", in), http.StatusBadRequest)
			if strings.Contains(body, "pw@") || f.ps.writes != 0 {
				t.Errorf("%q: body = %s writes = %d", u, body, f.ps.writes)
			}
		}
	})

	t.Run("private upstreams allowed: the host check is not consulted", func(t *testing.T) {
		f := newDirectFixture()
		f.d.AllowPrivateUpstreams = true
		f.d.HostCheck = func(context.Context, string) error {
			t.Error("host check consulted")
			return aiprovider.ErrBlockedAddress
		}
		c := f.serve(t)
		in := map[string]any{"name": "LAN", "slug": "lan", "kind": "direct", "base_url": "https://10.0.0.7:8443/v1", "credential_slot": "OPENROUTER"}
		wantStatus(t, c.post(t, "/api/v1/ai/providers", in), http.StatusCreated)
	})

	t.Run("bad kind, slug and name", func(t *testing.T) {
		f := newDirectFixture()
		c := f.serve(t)
		for _, in := range []map[string]any{
			{"name": "x", "kind": "magic", "base_url": "https://x.example/v1", "credential_slot": "S"},
			{"name": "z.ai", "slug": "v1", "kind": "direct", "base_url": "https://x.example/v1", "credential_slot": "S"},
			{"name": "", "slug": "zai", "kind": "direct", "base_url": "https://x.example/v1", "credential_slot": "S"},
		} {
			wantStatus(t, c.post(t, "/api/v1/ai/providers", in), http.StatusBadRequest)
		}
		if f.ps.writes != 0 {
			t.Errorf("store writes = %d", f.ps.writes)
		}
	})
}

// With the real resolver check a literal private address is refused at save
// time, without any DNS.
func TestPostDirectProvider_RealHostCheckRefusesLoopback(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1:8443/v1", "https://10.1.2.3/v1", "https://[::1]/v1", "https://169.254.169.254/latest"} {
		f := newDirectFixture()
		f.d.HostCheck = func(ctx context.Context, host string) error {
			return aiprovider.CheckHostPublic(ctx, nil, host) // nil resolver: a literal never needs one
		}
		c := f.serve(t)
		in := map[string]any{"name": "LAN", "slug": "lan", "kind": "direct", "base_url": u, "credential_slot": "OPENROUTER"}
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers", in), http.StatusBadRequest)
		if !strings.Contains(body, "private or loopback") || f.ps.writes != 0 {
			t.Errorf("%s: body = %s writes = %d", u, body, f.ps.writes)
		}
	}
}

func TestPutProviderUpstream(t *testing.T) {
	f := newDirectFixture()
	c := f.serve(t)

	// The credential slot is left out: it stays.
	body := wantStatus(t, c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"base_url": "https://openrouter.ai/api/v2", "billing": "flat"}), http.StatusOK)
	p := decodeProvider(t, body)
	if p.UpstreamBaseURL != "https://openrouter.ai/api/v2" || p.Billing != "flat" || p.CredentialSlot != "OPENROUTER" || !p.CredentialPresent {
		t.Errorf("view: %+v", p)
	}
	if f.ps.lastDirect.CredentialSlot != "" || f.ps.lastDirect.ExtraHeaders != nil {
		t.Errorf("fields the body left out were sent to the store: %+v", f.ps.lastDirect)
	}
	if len(f.aud.events) != 1 || f.aud.events[0].Action != audit.ActionAIProviderUpdate || f.aud.events[0].SubjectID != "openrouter" {
		t.Fatalf("audit: %+v", f.aud.events)
	}
	f.noSecrets(t, body)

	// Naming another slot changes it; the new one is not set in the vault.
	p = decodeProvider(t, wantStatus(t, c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"credential_slot": "OTHER"}), http.StatusOK))
	if p.CredentialSlot != "OTHER" || p.CredentialPresent || p.Status != "Offline" {
		t.Errorf("after slot change: %+v", p)
	}

	wantStatus(t, c.put(t, "/api/v1/ai/providers/ollama/upstream", map[string]any{"base_url": "https://x.example/v1"}), http.StatusConflict)
	wantStatus(t, c.put(t, "/api/v1/ai/providers/nope/upstream", map[string]any{"base_url": "https://x.example/v1"}), http.StatusNotFound)

	writes := f.ps.writes
	f.d.HostCheck = func(context.Context, string) error { return aiprovider.ErrBlockedAddress }
	c = f.serve(t)
	body = wantStatus(t, c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"base_url": "https://intranet.example/v1"}), http.StatusBadRequest)
	if !strings.Contains(body, "private or loopback") || f.ps.writes != writes {
		t.Errorf("blocked host: body = %s, store writes %d -> %d", body, writes, f.ps.writes)
	}

	f.ps.updateErr = fmt.Errorf("%w: extra header \"Host\" is not allowed", store.ErrInvalidProviderConfig)
	body = wantStatus(t, c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"extra_headers": map[string]string{"Host": "evil.example"}}), http.StatusBadRequest)
	if !strings.Contains(body, "is not allowed") || strings.Contains(body, "evil.example") {
		t.Errorf("body = %s", body)
	}
}

// No GET or list response carries the credential or a header value.
func TestDirectProviderResponses_CarryNoCredential(t *testing.T) {
	f := newDirectFixture()
	f.ps.models = map[string][]db.AIProviderModel{"openrouter": {{ProviderSlug: "openrouter", ModelID: "glm-5.1"}}}
	c := f.serve(t)
	for _, path := range []string{"/api/v1/ai/providers", "/api/v1/ai/providers/openrouter", "/api/v1/ai/providers/openrouter/models", "/api/v1/ai/providers/openrouter/metrics"} {
		f.noSecrets(t, wantStatus(t, c.get(t, path), http.StatusOK))
	}
	p := decodeProvider(t, wantStatus(t, c.get(t, "/api/v1/ai/providers/openrouter"), http.StatusOK))
	if !p.CredentialPresent || p.CredentialSlot != "OPENROUTER" || p.Status != "Connected" || p.ModelCount != 1 || p.UpstreamBaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("view: %+v", p)
	}
	// The raw JSON has no field that could hold the value.
	var raw map[string]any
	_ = json.Unmarshal([]byte(wantStatus(t, c.get(t, "/api/v1/ai/providers/openrouter"), http.StatusOK)), &raw)
	for _, k := range []string{"credential", "api_key", "extra_headers", "key"} {
		if _, ok := raw[k]; ok {
			t.Errorf("response has field %q", k)
		}
	}
	// A tunnel provider has the fields, empty.
	o := decodeProvider(t, wantStatus(t, c.get(t, "/api/v1/ai/providers/ollama"), http.StatusOK))
	if o.CredentialPresent || o.CredentialSlot != "" || o.UpstreamBaseURL != "" {
		t.Errorf("tunnel view: %+v", o)
	}
}

func TestGetProviderModels(t *testing.T) {
	f := newDirectFixture()
	synced := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	f.ps.models = map[string][]db.AIProviderModel{"openrouter": {
		{ProviderSlug: "openrouter", ModelID: "google/gemini-x", DisplayName: "Gemini <b>X</b>", ContextLength: 1000000, SyncedAt: synced},
	}}
	c := f.serve(t)
	body := wantStatus(t, c.get(t, "/api/v1/ai/providers/openrouter/models"), http.StatusOK)
	var got []aiProviderModelResp
	if err := json.Unmarshal([]byte(body), &got); err != nil || len(got) != 1 || got[0].ID != "google/gemini-x" ||
		got[0].DisplayName != "Gemini <b>X</b>" || got[0].ContextLength != 1000000 || !got[0].SyncedAt.Equal(synced) {
		t.Fatalf("models: %v %s", err, body)
	}
	if strings.Contains(body, "<b>") {
		t.Errorf("markup in a model name is not escaped: %s", body)
	}
	if body := wantStatus(t, c.get(t, "/api/v1/ai/providers/ollama/models"), http.StatusOK); strings.TrimSpace(body) != "[]" {
		t.Errorf("empty catalog = %s, want []", body)
	}
	wantStatus(t, c.get(t, "/api/v1/ai/providers/nope/models"), http.StatusNotFound)
}

// A provider whose service the caller may not see has no readable catalog.
func TestGetProviderModels_HiddenProvider(t *testing.T) {
	f := newDirectFixture()
	f.d.Services.(*fakeServiceStore).listSvcs = nil
	f.ps.models = map[string][]db.AIProviderModel{"openrouter": {{ProviderSlug: "openrouter", ModelID: "glm-5.1"}}}
	c := f.serve(t)
	hidden := wantStatus(t, c.get(t, "/api/v1/ai/providers/openrouter/models"), http.StatusNotFound)
	unknown := wantStatus(t, c.get(t, "/api/v1/ai/providers/nope/models"), http.StatusNotFound)
	if hidden != unknown {
		t.Errorf("hidden %q differs from unknown %q", hidden, unknown)
	}
}

func TestSyncProviderModels(t *testing.T) {
	t.Run("stores what the upstream lists", func(t *testing.T) {
		f := newDirectFixture()
		var asked db.AIProvider
		f.d.FetchProviderModels = func(_ context.Context, p db.AIProvider) ([]aiprovider.Model, error) {
			asked = p
			return []aiprovider.Model{{ID: "a"}, {ID: "b", DisplayName: "B", ContextLength: 8192}, {ID: "c"}}, nil
		}
		c := f.serve(t)
		for i := 0; i < 2; i++ { // twice: same result
			body := wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil), http.StatusOK)
			if strings.TrimSpace(body) != `{"count":3}` {
				t.Fatalf("body = %s", body)
			}
			got := f.ps.models["openrouter"]
			if len(got) != 3 || got[1].ModelID != "b" || got[1].DisplayName != "B" || got[1].ContextLength != 8192 || got[1].ProviderSlug != "openrouter" {
				t.Fatalf("stored: %+v", got)
			}
			f.noSecrets(t, body)
		}
		if asked.Slug != "openrouter" || f.ps.replaces != 2 {
			t.Errorf("asked %+v, replaces %d", asked, f.ps.replaces)
		}
		last := f.aud.events[len(f.aud.events)-1]
		if last.Action != audit.ActionAIProviderModelsSync || string(last.Payload) != `{"count":3}` {
			t.Errorf("audit: %s %s", last.Action, last.Payload)
		}
	})

	t.Run("upstream failure", func(t *testing.T) {
		f := newDirectFixture()
		f.ps.models = map[string][]db.AIProviderModel{"openrouter": {{ProviderSlug: "openrouter", ModelID: "kept"}}}
		f.d.FetchProviderModels = func(context.Context, db.AIProvider) ([]aiprovider.Model, error) {
			return nil, errors.New("fetch models: the provider answered 401")
		}
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil), http.StatusBadGateway)
		if strings.TrimSpace(body) != `{"error":"fetch models: the provider answered 401"}` {
			t.Errorf("body = %s", body)
		}
		if f.ps.replaces != 0 || len(f.ps.models["openrouter"]) != 1 || len(f.aud.events) != 0 {
			t.Errorf("a failed sync touched the store or the audit log: %d %+v", f.ps.replaces, f.aud.events)
		}
		f.noSecrets(t, body)
	})

	t.Run("credential slot not set", func(t *testing.T) {
		f := newDirectFixture()
		f.d.FetchProviderModels = func(context.Context, db.AIProvider) ([]aiprovider.Model, error) {
			return nil, aiprovider.ErrNotConfigured
		}
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil), http.StatusConflict)
		if !strings.Contains(body, "the credential slot OPENROUTER is not set") {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("a list the store refuses", func(t *testing.T) {
		f := newDirectFixture()
		f.ps.modelErr = fmt.Errorf("%w: a model id is listed twice", store.ErrInvalidProviderConfig)
		f.d.FetchProviderModels = func(context.Context, db.AIProvider) ([]aiprovider.Model, error) {
			return []aiprovider.Model{{ID: "a"}, {ID: "a"}}, nil
		}
		c := f.serve(t)
		wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil), http.StatusBadGateway)
	})

	t.Run("tunnel provider, unknown provider, no fetcher", func(t *testing.T) {
		f := newDirectFixture()
		called := false
		f.d.FetchProviderModels = func(context.Context, db.AIProvider) ([]aiprovider.Model, error) { called = true; return nil, nil }
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/providers/ollama/models/sync", nil), http.StatusConflict)
		if !strings.Contains(body, "sync is available for direct providers") {
			t.Errorf("body = %s", body)
		}
		wantStatus(t, c.post(t, "/api/v1/ai/providers/nope/models/sync", nil), http.StatusNotFound)
		if called {
			t.Error("an outbound call was made for a provider that cannot be synced")
		}
		f.d.FetchProviderModels = nil
		c = f.serve(t)
		wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil), http.StatusServiceUnavailable)
	})

	t.Run("one sync per provider at a time, bounded in time", func(t *testing.T) {
		old := modelSyncTimeout
		modelSyncTimeout = 300 * time.Millisecond
		t.Cleanup(func() { modelSyncTimeout = old })
		f := newDirectFixture()
		started := make(chan struct{})
		f.d.FetchProviderModels = func(ctx context.Context, _ db.AIProvider) ([]aiprovider.Model, error) {
			close(started)
			<-ctx.Done() // an upstream that never answers
			return nil, errors.New("fetch models: the provider did not answer")
		}
		c := f.serve(t)
		first := make(chan int, 1)
		go func() {
			resp := c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil)
			resp.Body.Close()
			first <- resp.StatusCode
		}()
		<-started
		wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil), http.StatusConflict)
		select {
		case code := <-first:
			if code != http.StatusBadGateway {
				t.Errorf("first sync status %d, want 502", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the sync was not bounded in time")
		}
	})
}

// The sync endpoint with the real fetcher: it reaches the upstream only
// through the transport it is given, sends the credential there and returns
// nothing of the upstream's answer but a count or a fixed error text.
func TestSyncProviderModels_RealFetcher(t *testing.T) {
	status := http.StatusOK
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+upstreamSecret {
			t.Errorf("upstream saw Authorization %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("X-Upstream-Internal", "upstream-header-do-not-leak")
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"error":"upstream-body-do-not-leak ` + upstreamSecret + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.1"},{"id":"google/gemini-x","name":"Gemini X"},{"id":""}]}`))
	}))
	defer up.Close()

	f := newDirectFixture()
	f.ps.rows[1].BaseURL = up.URL + "/api/v1"
	fetch := func(rt http.RoundTripper) func(context.Context, db.AIProvider) ([]aiprovider.Model, error) {
		return func(ctx context.Context, p db.AIProvider) ([]aiprovider.Model, error) {
			return aiprovider.FetchModels(ctx, aiprovider.Config{
				Slug: p.Slug, BaseURL: p.BaseURL, CredentialSlot: p.CredentialSlot,
				AuthHeader: p.AuthHeader, AuthFormat: p.AuthFormat,
			}, f.d.CredentialVault, rt)
		}
	}
	f.d.FetchProviderModels = fetch(up.Client().Transport)
	c := f.serve(t)

	resp := c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil)
	if resp.Header.Get("X-Upstream-Internal") != "" {
		t.Error("an upstream header reached the caller")
	}
	if body := wantStatus(t, resp, http.StatusOK); strings.TrimSpace(body) != `{"count":2}` {
		t.Fatalf("body = %s", body)
	}

	status = http.StatusUnauthorized
	body := wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil), http.StatusBadGateway)
	if strings.Contains(body, "do-not-leak") || strings.Contains(body, "127.0.0.1") || !strings.Contains(body, "answered 401") {
		t.Errorf("body = %s", body)
	}
	f.noSecrets(t, body)
	if strings.Contains(f.logs.String(), "do-not-leak") {
		t.Errorf("log carries upstream data: %s", f.logs.String())
	}

	// Through the guarded transport the loopback upstream is out of reach:
	// the endpoint cannot be used to probe the relay's own network.
	status = http.StatusOK
	guarded := aiprovider.NewTransport(false)
	defer guarded.CloseIdleConnections()
	f.d.FetchProviderModels = fetch(guarded)
	c = f.serve(t)
	body = wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models/sync", nil), http.StatusBadGateway)
	if strings.Contains(body, "127.0.0.1") || !strings.Contains(body, "did not answer") {
		t.Errorf("guarded: body = %s", body)
	}
}

func TestAddAndRemoveProviderModel(t *testing.T) {
	f := newDirectFixture()
	c := f.serve(t)

	wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models", map[string]string{"id": "glm-5.1"}), http.StatusNoContent)
	wantStatus(t, c.post(t, "/api/v1/ai/providers/ollama/models", map[string]string{"id": "google/gemini-x"}), http.StatusNoContent)
	if len(f.ps.models["openrouter"]) != 1 || f.ps.models["ollama"][0].ModelID != "google/gemini-x" {
		t.Fatalf("stored: %+v", f.ps.models)
	}
	if f.aud.events[0].Action != audit.ActionAIProviderModelAdd || string(f.aud.events[0].Payload) != `{"model_id":"glm-5.1"}` {
		t.Errorf("audit: %+v", f.aud.events[0])
	}
	writes := f.ps.writes
	for _, id := range []string{"", strings.Repeat("x", 201), "a\nb", " a"} {
		wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models", map[string]string{"id": id}), http.StatusBadRequest)
	}
	wantStatus(t, c.post(t, "/api/v1/ai/providers/nope/models", map[string]string{"id": "m"}), http.StatusNotFound)
	// A body over the cap is refused, not buffered.
	wantStatus(t, c.post(t, "/api/v1/ai/providers/openrouter/models", map[string]string{"id": "m", "pad": strings.Repeat("p", 9<<10)}), http.StatusBadRequest)
	if f.ps.writes != writes {
		t.Errorf("refused requests reached the store")
	}

	wantStatus(t, c.delete(t, "/api/v1/ai/providers/ollama/models?id=google/gemini-x"), http.StatusNoContent)
	if len(f.ps.models["ollama"]) != 0 {
		t.Errorf("not removed: %+v", f.ps.models["ollama"])
	}
	last := f.aud.events[len(f.aud.events)-1]
	if last.Action != audit.ActionAIProviderModelRemove || last.SubjectID != "ollama" {
		t.Errorf("audit: %+v", last)
	}
	wantStatus(t, c.delete(t, "/api/v1/ai/providers/ollama/models"), http.StatusBadRequest)
	wantStatus(t, c.delete(t, "/api/v1/ai/providers/ollama/models?id="), http.StatusBadRequest)
	wantStatus(t, c.delete(t, "/api/v1/ai/providers/ollama/models?id=gone"), http.StatusNotFound)
	wantStatus(t, c.delete(t, "/api/v1/ai/providers/nope/models?id=m"), http.StatusNotFound)
}

func TestDeleteDirectProvider_Audit(t *testing.T) {
	f := newDirectFixture()
	c := f.serve(t)
	wantStatus(t, c.delete(t, "/api/v1/ai/providers/openrouter"), http.StatusNoContent)
	if len(f.aud.events) != 1 || f.aud.events[0].Action != audit.ActionAIProviderDelete || f.aud.events[0].SubjectLabel != "OpenRouter" ||
		string(f.aud.events[0].Payload) != `{"kind":"direct"}` {
		t.Fatalf("audit: %+v", f.aud.events)
	}
	f.noSecrets(t, "")
}

func TestAIProviderAuditActionsRegistered(t *testing.T) {
	for _, a := range []string{audit.ActionAIProviderModelsSync, audit.ActionAIProviderModelAdd, audit.ActionAIProviderModelRemove} {
		if !slices.Contains(audit.AllActions, a) {
			t.Errorf("audit action %q is not registered", a)
		}
	}
}

// PUT …/upstream keeps the fields a body leaves out, so a caller who may
// configure the provider is shown what it keeps: the header name, the format
// with its {key} placeholder and the NAMES of the extra headers. A header
// value is free text an admin typed and may be a secret: no response has it.
func TestProviderView_UpstreamFieldsForAdminsOnly(t *testing.T) {
	f := newDirectFixture()
	f.ps.rows[len(f.ps.rows)-1].ExtraHeaders = map[string]string{"X-Title": "header-value-do-not-leak", "HTTP-Referer": "header-value-do-not-leak"}
	c := f.serve(t)
	for _, path := range []string{"/api/v1/ai/providers/openrouter", "/api/v1/ai/providers"} {
		body := wantStatus(t, c.get(t, path), http.StatusOK)
		for _, want := range []string{`"auth_header":"Authorization"`, `"auth_format":"Bearer {key}"`, `"extra_header_names":["HTTP-Referer","X-Title"]`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: admin view lacks %s: %s", path, want, body)
			}
		}
		f.noSecrets(t, body)
	}
	// The write answers with the same view.
	body := wantStatus(t, c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"billing": "flat"}), http.StatusOK)
	if p := decodeProvider(t, body); p.AuthHeader != "Authorization" || p.AuthFormat != "Bearer {key}" || !slices.Equal(p.ExtraHeaderNames, []string{"HTTP-Referer", "X-Title"}) {
		t.Errorf("view after PUT: %s", body)
	}
	f.noSecrets(t, body)
	// A direct provider without extra headers says so; a tunnel provider has
	// no such fields.
	f.ps.rows[len(f.ps.rows)-1].ExtraHeaders = nil
	if body := wantStatus(t, c.get(t, "/api/v1/ai/providers/openrouter"), http.StatusOK); !strings.Contains(body, `"extra_header_names":[]`) {
		t.Errorf("no extra headers: %s", body)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(wantStatus(t, c.get(t, "/api/v1/ai/providers/ollama"), http.StatusOK)), &raw)
	for _, k := range []string{"auth_header", "auth_format", "extra_header_names", "extra_headers"} {
		if _, ok := raw[k]; ok {
			t.Errorf("tunnel provider has field %q", k)
		}
	}

	// A caller who can see the provider but not configure it.
	f = newDirectFixture()
	f.d.Users = &fakeUserStore{role: "user"}
	c = f.serve(t)
	for _, path := range []string{"/api/v1/ai/providers/openrouter", "/api/v1/ai/providers"} {
		body := wantStatus(t, c.get(t, path), http.StatusOK)
		for _, k := range []string{"auth_header", "auth_format", "extra_header", "X-Title", "header-value-do-not-leak"} {
			if strings.Contains(body, k) {
				t.Errorf("%s: non-admin view contains %q: %s", path, k, body)
			}
		}
	}
}

// A key sent by mistake in a field the API does not have is refused with the
// field's name. Its value is in no response, audit event or log line, and
// nothing is stored.
func TestDirectProvider_UnknownFieldRefused(t *testing.T) {
	const stray = "sk-or-stray-key-do-not-echo"
	cases := map[string]func(c *authClient) *http.Response{
		"create api_key": func(c *authClient) *http.Response {
			return c.post(t, "/api/v1/ai/providers", map[string]any{
				"name": "z.ai", "slug": "zai", "kind": "direct", "base_url": "https://api.z.ai/v4", "credential_slot": "OPENROUTER", "api_key": stray})
		},
		"create credential": func(c *authClient) *http.Response {
			return c.post(t, "/api/v1/ai/providers", map[string]any{
				"name": "z.ai", "slug": "zai", "kind": "direct", "base_url": "https://api.z.ai/v4", "credential_slot": "OPENROUTER", "credential": stray})
		},
		"upstream api_key": func(c *authClient) *http.Response {
			return c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"billing": "flat", "api_key": stray})
		},
		"upstream credential": func(c *authClient) *http.Response {
			return c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"credential": map[string]string{"value": stray}})
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDirectFixture()
			c := f.serve(t)
			body := wantStatus(t, call(c), http.StatusBadRequest)
			field := strings.Fields(name)[1]
			if !strings.Contains(body, "unknown field") || !strings.Contains(body, field) {
				t.Errorf("body does not name the field %q: %s", field, body)
			}
			if strings.Contains(body, stray) || strings.Contains(f.logs.String(), stray) {
				t.Errorf("the value was echoed: body %s log %s", body, f.logs.String())
			}
			if len(f.aud.events) != 0 || f.ps.writes != 0 {
				t.Errorf("audit events %+v, store writes %d, want none", f.aud.events, f.ps.writes)
			}
		})
	}

	// A field name that is not a plain identifier is not repeated either.
	f := newDirectFixture()
	c := f.serve(t)
	body := wantStatus(t, c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"Bearer " + stray: "x"}), http.StatusBadRequest)
	if !strings.Contains(body, "unknown field") || strings.Contains(body, stray) {
		t.Errorf("body = %s", body)
	}
	// The same for a tunnel provider and for a body without a kind, which is
	// a tunnel create: a key next to a base_url must not vanish in a 201.
	for name, in := range map[string]map[string]any{
		"kind tunnel": {"name": "Ollama", "slug": "ollama", "kind": "tunnel", "service_id": "svc1", "api_key": stray},
		"no kind":     {"name": "Ollama", "slug": "ollama", "base_url": "https://api.z.ai/v4", "api_key": stray},
	} {
		ss, ps := oneProviderFixture()
		ps.rows = nil
		logs := &bytes.Buffer{}
		d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
		d.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		srv, tc := newAIProviderServer(t, d)
		body := wantStatus(t, tc.post(t, "/api/v1/ai/providers", in), http.StatusBadRequest)
		if !strings.Contains(body, `unknown field \"api_key\"`) || strings.Contains(body, stray) || strings.Contains(logs.String(), stray) || ps.writes != 0 {
			t.Errorf("%s: body %s, log %s, writes %d", name, body, logs.String(), ps.writes)
		}
		srv.Close()
	}
	// What the dashboard sends for a tunnel create is accepted: with and
	// without slug (NewProviderDialog.tsx, Services.tsx).
	for _, in := range []map[string]any{
		{"slug": "ollama", "name": "Ollama", "kind": "tunnel", "service_id": "svc1"},
		{"name": "Ollama", "kind": "tunnel", "service_id": "svc1"},
	} {
		ss, ps := oneProviderFixture()
		ps.rows = nil
		srv, tc := newAIProviderServer(t, newAIProviderDeps(ss, newFakeModelAliasStore(), ps))
		wantStatus(t, tc.post(t, "/api/v1/ai/providers", in), http.StatusCreated)
		srv.Close()
	}
}

// Two admins saving the upstream settings at once, five times over: the
// loser is told to try again, not handed a 500.
func TestPutProviderUpstream_BusyIs409(t *testing.T) {
	f := newDirectFixture()
	f.ps.updateErr = fmt.Errorf("wrapped: %w", store.ErrProviderBusy)
	c := f.serve(t)
	body := wantStatus(t, c.put(t, "/api/v1/ai/providers/openrouter/upstream", map[string]any{"billing": "flat"}), http.StatusConflict)
	if !strings.Contains(body, "try again") {
		t.Errorf("body = %s", body)
	}
	if len(f.aud.events) != 0 {
		t.Errorf("a refused update was audited: %+v", f.aud.events)
	}
}

// cost_usd_24h is the cost engine's figure: what upstreams reported plus the
// price table for the tokens of rows without a reported cost.
func TestProviderMetrics_CostUsesReportedCost(t *testing.T) {
	ss, ps := oneProviderFixture()
	d := newAIProviderDeps(ss, newFakeModelAliasStore(), ps)
	d.AIMetrics = &fakeAIMetrics{agg: db.AIEndpointAgg{Requests: 3, TokensIn: 3_000_000, ByKind: []db.AIEndpointKindTokens{
		{Kind: "k", TokensIn: 3_000_000, ReportedUSD: 0.25, PricedTokensIn: 1_000_000},
		{Kind: "no-price", TokensIn: 5, TokensOut: 5, ReportedUSD: 0.5},
	}}}
	d.CostEngine = &fakeCostEngine{pricing: cost.Pricing{Entries: map[string]cost.Entry{"k": {InputPerMillion: 1, OutputPerMillion: 2}}}}
	srv, c := newAIProviderServer(t, d)
	defer srv.Close()
	var out endpointMetricsResp
	if err := json.Unmarshal([]byte(wantStatus(t, c.get(t, "/api/v1/ai/providers/ollama/metrics"), http.StatusOK)), &out); err != nil {
		t.Fatal(err)
	}
	if out.CostUSD24h != 1.75 || out.TokensIn24h != 3_000_000 {
		t.Fatalf("cost_usd_24h = %v tokens_in = %d, want 1.75 over all 3000000 tokens", out.CostUSD24h, out.TokensIn24h)
	}
}

// The sync's own deadline has to be the first one to fire: the router gives
// a request 30 s and FetchModels stops at 30 s.
func TestModelSyncTimeout_BelowRequestTimeout(t *testing.T) {
	if modelSyncTimeout >= 30*time.Second || modelSyncTimeout <= 0 {
		t.Fatalf("modelSyncTimeout = %v, want below the 30 s request timeout", modelSyncTimeout)
	}
}
