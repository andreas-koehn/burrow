package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// ---------------------------------------------------------------------------
// Helpers — tiny fakes wired together for these tests only
// ---------------------------------------------------------------------------

// fakeProviderStore implements AIProviderStore over a slice. A non-nil
// createErr/updateErr/deleteErr is returned instead of touching the slice.
type fakeProviderStore struct {
	rows      []db.AIProvider
	createErr error
	updateErr error
	deleteErr error
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
	if f.createErr != nil {
		return db.AIProvider{}, f.createErr
	}
	if !store.ValidProviderSlug(slug) {
		return db.AIProvider{}, store.ErrInvalidProviderSlug
	}
	p := db.AIProvider{Slug: slug, Name: name, Kind: "tunnel", ServiceID: serviceID, APIFormat: "openai"}
	f.rows = append(f.rows, p)
	return p, nil
}

func (f *fakeProviderStore) UpdateProvider(_ context.Context, slug, newSlug, name string) (db.AIProvider, error) {
	if f.updateErr != nil {
		return db.AIProvider{}, f.updateErr
	}
	if !store.ValidProviderSlug(newSlug) {
		return db.AIProvider{}, store.ErrInvalidProviderSlug
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
}

func (f *fakeAIMetrics) AIEndpointCounts24h(context.Context) (map[string]db.AIEndpointCount, error) {
	return f.counts, nil
}
func (f *fakeAIMetrics) AIEndpointMetrics24h(_ context.Context, serviceID string) (db.AIEndpointAgg, error) {
	f.gotID = serviceID
	return f.agg, nil
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

func TestProviderMetrics_404WhenServiceNotVisible(t *testing.T) {
	ss, ps := oneProviderFixture()
	ss.getSvcErr = db.ErrNotFound
	srv, c := newAIProviderServer(t, newAIProviderDeps(ss, newFakeModelAliasStore(), ps))
	defer srv.Close()

	wantStatus(t, c.get(t, "/api/v1/ai/providers/ollama/metrics"), http.StatusNotFound)
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
		{"direct kind", map[string]string{"slug": "ok1", "name": "x", "kind": "direct", "service_id": "svc1"}, "kind must be 'tunnel'"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			ps := &fakeProviderStore{}
			c, aud := newServer(t, ps)
			body := wantStatus(t, c.post(t, "/api/v1/ai/providers", tc.body), http.StatusBadRequest)
			if !strings.Contains(body, tc.want) {
				t.Errorf("body %q does not mention %q", body, tc.want)
			}
			if len(ps.rows) != 0 || len(aud.events) != 0 {
				t.Errorf("rejected request left traces: rows=%+v events=%+v", ps.rows, aud.events)
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
			if ps.rows[0].Slug != "ollama" || len(aud.events) != 0 {
				t.Errorf("rejected request left traces: rows=%+v events=%+v", ps.rows, aud.events)
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
