package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
)

// fakeSemanticEngine is the SemanticEngine stand-in for handler tests.
type fakeSemanticEngine struct {
	clearAllCalled bool
	clearAllErr    error
	statsErr       error
	// aggregate stats to return
	entries            int
	onDiskBytes        int64
	hitRate24h         float64
	similarReturned24h int
	promotions24h      int
}

func (f *fakeSemanticEngine) ClearAll(_ context.Context) error {
	f.clearAllCalled = true
	return f.clearAllErr
}

func (f *fakeSemanticEngine) AggregateStats(_ context.Context) (SemanticStats, error) {
	return SemanticStats{
		Entries:            f.entries,
		OnDiskBytes:        f.onDiskBytes,
		HitRate24h:         f.hitRate24h,
		SimilarReturned24h: f.similarReturned24h,
		Promotions24h:      f.promotions24h,
	}, f.statsErr
}

// fakeServiceAIConfigStore is the ServiceAIConfigStore stand-in.
type fakeServiceAIConfigStore struct {
	configs  map[string][]byte // serviceID → saved config blob
	saveErr  error
	notFound map[string]bool // serviceIDs that return not found on Get
}

func (f *fakeServiceAIConfigStore) GetServiceAIConfigRaw(_ context.Context, serviceID string) ([]byte, bool, error) {
	if f.notFound[serviceID] {
		return nil, false, nil
	}
	b, ok := f.configs[serviceID]
	return b, ok, nil
}

func (f *fakeServiceAIConfigStore) UpsertServiceAIConfig(_ context.Context, serviceID string, config []byte) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.configs == nil {
		f.configs = map[string][]byte{}
	}
	f.configs[serviceID] = config
	return nil
}

// TestSemanticCacheSettingsHasSemanticBlock asserts GET /api/v1/cache/settings
// returns a top-level "semantic" key with the default values when no
// per-service overrides are configured.
func TestSemanticCacheSettingsHasSemanticBlock(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: &fakeCacheSettingsStore{},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/cache/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", r.StatusCode)
	}
	var got map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()

	semRaw, ok := got["semantic"]
	if !ok {
		t.Fatalf("response missing top-level 'semantic' key; keys=%v", jsonKeys(got))
	}
	var sem SemanticSettings
	if err := json.Unmarshal(semRaw, &sem); err != nil {
		t.Fatalf("decode semantic: %v", err)
	}
	// Verify the spec A.3 defaults.
	if sem.Enabled {
		t.Errorf("semantic.enabled want false, got true")
	}
	if sem.MinSimilarity != 0.85 {
		t.Errorf("semantic.min_similarity want 0.85, got %v", sem.MinSimilarity)
	}
	if sem.EmbeddingMode != "local" {
		t.Errorf("semantic.embedding_mode want local, got %q", sem.EmbeddingMode)
	}
	if sem.EmbeddingURL != "http://localhost:11434/v1/embeddings" {
		t.Errorf("semantic.embedding_url want default, got %q", sem.EmbeddingURL)
	}
	if sem.EmbeddingModel != "nomic-embed-text" {
		t.Errorf("semantic.embedding_model want nomic-embed-text, got %q", sem.EmbeddingModel)
	}
	if sem.FallbackPolicy != "treat_as_miss" {
		t.Errorf("semantic.fallback_policy want treat_as_miss, got %q", sem.FallbackPolicy)
	}
	if !sem.PromoteOnMiss {
		t.Errorf("semantic.promote_on_miss want true, got false")
	}
	if sem.MaxIndexEntries != 10000 {
		t.Errorf("semantic.max_index_entries want 10000, got %d", sem.MaxIndexEntries)
	}
}

// TestSemanticCacheStatsHasSemanticFields asserts GET /api/v1/cache/stats
// returns five semantic_* fields matching the semantic engine aggregate.
func TestSemanticCacheStatsHasSemanticFields(t *testing.T) {
	fse := &fakeSemanticEngine{
		entries:            42,
		onDiskBytes:        98765,
		hitRate24h:         0.77,
		similarReturned24h: 13,
		promotions24h:      5,
	}
	d := Deps{
		Log:            discardLog(),
		Users:          &fakeUserStore{role: "user"},
		Settings:       &fakeCacheSettingsStore{},
		SemanticEngine: fse,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/cache/stats")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", r.StatusCode)
	}
	var got cacheStatsResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()

	// Exact match on all five semantic fields (spec A.4).
	if got.SemanticEntries != 42 {
		t.Errorf("semantic_entries want 42, got %d", got.SemanticEntries)
	}
	if got.SemanticDiskBytes != 98765 {
		t.Errorf("semantic_disk_bytes want 98765, got %d", got.SemanticDiskBytes)
	}
	if got.SemanticHitRate24h < 0.76 || got.SemanticHitRate24h > 0.78 {
		t.Errorf("semantic_hit_rate_24h want ~0.77, got %v", got.SemanticHitRate24h)
	}
	if got.SemanticSimilarReturned24h != 13 {
		t.Errorf("semantic_similar_returned_24h want 13, got %d", got.SemanticSimilarReturned24h)
	}
	if got.SemanticPromotions24h != 5 {
		t.Errorf("semantic_promotions_24h want 5, got %d", got.SemanticPromotions24h)
	}
}

// TestSemanticCacheStatsNilEngineZero asserts the nil-engine degradation path
// returns all-zero semantic_* fields (no 500).
func TestSemanticCacheStatsNilEngineZero(t *testing.T) {
	d := Deps{
		Log:      discardLog(),
		Users:    &fakeUserStore{role: "user"},
		Settings: &fakeCacheSettingsStore{},
		// SemanticEngine intentionally nil.
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/cache/stats")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", r.StatusCode)
	}
	var got cacheStatsResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if got.SemanticEntries != 0 || got.SemanticDiskBytes != 0 || got.SemanticHitRate24h != 0 {
		t.Errorf("nil engine: expected zero semantic stats, got %+v", got)
	}
}

// TestDeleteSemanticCacheEntriesRequiresAdmin asserts DELETE
// /api/v1/cache/semantic/entries returns 403 for a non-admin and 204 for admin.
func TestDeleteSemanticCacheEntriesRequiresAdmin(t *testing.T) {
	// Non-admin → 403, ClearAll NOT called.
	{
		fse := &fakeSemanticEngine{}
		d := Deps{
			Log:            discardLog(),
			Users:          &fakeUserStore{role: "user"},
			Settings:       &fakeCacheSettingsStore{},
			SemanticEngine: fse,
		}
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.delete(t, "/api/v1/cache/semantic/entries")
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("non-admin status=%d want 403", r.StatusCode)
		}
		r.Body.Close()
		if fse.clearAllCalled {
			t.Fatal("ClearAll unexpectedly called for non-admin")
		}
	}

	// Admin → 204, ClearAll called.
	{
		fse := &fakeSemanticEngine{}
		d := Deps{
			Log:            discardLog(),
			Users:          &fakeUserStore{role: "admin"},
			Settings:       &fakeCacheSettingsStore{},
			SemanticEngine: fse,
		}
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.delete(t, "/api/v1/cache/semantic/entries")
		if r.StatusCode != http.StatusNoContent {
			t.Fatalf("admin status=%d want 204", r.StatusCode)
		}
		r.Body.Close()
		if !fse.clearAllCalled {
			t.Fatal("ClearAll not called for admin")
		}
	}
}

// TestPutServiceAIConfigValidatesSemanticBlock asserts that PUT
// /api/v1/services/{id}/ai-config validates cache.semantic sub-fields:
// - min_similarity out of [0,1] → 400 with spec error body
// - unknown embedding_mode → 400 with spec error body
// - valid config → 204
func TestPutServiceAIConfigValidatesSemanticBlock(t *testing.T) {
	const svcID = "svc-test"

	makeServiceDeps := func(role string) Deps {
		svcStore := &fakeServiceAIConfigStore{
			configs: map[string][]byte{svcID: []byte(`{}`)},
		}
		return Deps{
			Log:              discardLog(),
			Users:            &fakeUserStore{role: role},
			Settings:         &fakeCacheSettingsStore{},
			ServiceAIConfigs: svcStore,
			CacheServices:    &fakeCacheServiceLookup{owners: map[string]string{svcID: "u-self"}},
		}
	}

	// Out-of-range min_similarity (> 1.0) → 400.
	{
		d := makeServiceDeps("admin")
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.put(t, "/api/v1/services/"+svcID+"/ai-config", map[string]any{
			"cache": map[string]any{
				"semantic": map[string]any{
					"min_similarity": 1.5,
				},
			},
		})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("min_similarity=1.5 status=%d want 400", r.StatusCode)
		}
		var errBody map[string]string
		if err := json.NewDecoder(r.Body).Decode(&errBody); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		r.Body.Close()
		if errBody["error"] != "semantic.min_similarity out of range" {
			t.Errorf("error body=%q want 'semantic.min_similarity out of range'", errBody["error"])
		}
	}

	// Negative min_similarity → 400.
	{
		d := makeServiceDeps("admin")
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.put(t, "/api/v1/services/"+svcID+"/ai-config", map[string]any{
			"cache": map[string]any{
				"semantic": map[string]any{
					"min_similarity": -0.1,
				},
			},
		})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("min_similarity=-0.1 status=%d want 400", r.StatusCode)
		}
		r.Body.Close()
	}

	// Unknown embedding_mode → 400 with quoted value in error.
	{
		d := makeServiceDeps("admin")
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.put(t, "/api/v1/services/"+svcID+"/ai-config", map[string]any{
			"cache": map[string]any{
				"semantic": map[string]any{
					"embedding_mode": "openai",
				},
			},
		})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("embedding_mode=openai status=%d want 400", r.StatusCode)
		}
		var errBody map[string]string
		if err := json.NewDecoder(r.Body).Decode(&errBody); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		r.Body.Close()
		wantErr := `unknown embedding_mode "openai"`
		if errBody["error"] != wantErr {
			t.Errorf("error body=%q want %q", errBody["error"], wantErr)
		}
	}

	// Valid config (min_similarity in range, known embedding_mode) → 204.
	{
		d := makeServiceDeps("admin")
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.put(t, "/api/v1/services/"+svcID+"/ai-config", map[string]any{
			"cache": map[string]any{
				"semantic": map[string]any{
					"enabled":        true,
					"min_similarity": 0.9,
					"embedding_mode": "local",
				},
			},
		})
		if r.StatusCode != http.StatusNoContent {
			t.Fatalf("valid config status=%d want 204", r.StatusCode)
		}
		r.Body.Close()
	}

	// Boundary: min_similarity=0.0 and min_similarity=1.0 are both valid.
	{
		for _, val := range []float64{0.0, 1.0} {
			d := makeServiceDeps("admin")
			srv := httptest.NewServer(NewRouter(d))
			defer srv.Close()
			c := authedClient(t, srv)

			r := c.put(t, "/api/v1/services/"+svcID+"/ai-config", map[string]any{
				"cache": map[string]any{
					"semantic": map[string]any{
						"min_similarity": val,
					},
				},
			})
			if r.StatusCode != http.StatusNoContent {
				t.Fatalf("min_similarity=%v status=%d want 204", val, r.StatusCode)
			}
			r.Body.Close()
		}
	}

	// Non-admin (role=user, not the service owner) → 403.
	{
		d := makeServiceDeps("user")
		// Change owner so the user doesn't own the service.
		d.CacheServices = &fakeCacheServiceLookup{owners: map[string]string{svcID: "u-other"}}
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.put(t, "/api/v1/services/"+svcID+"/ai-config", map[string]any{})
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("non-owner status=%d want 403", r.StatusCode)
		}
		r.Body.Close()
	}
}

// TestPutServiceAIConfigRejectsOwnerWithoutAIConfigureOwnPerm asserts that a
// user who owns the service but holds a custom role that has NO ai:configure:*
// permissions is rejected with 403. Previously the owner branch skipped the
// ai:configure:own check, diverging from the "ai:configure:own|any" mapping in
// the spec (Task 2 / Task 4). The fix mirrors DeleteServiceCacheEntries exactly.
func TestPutServiceAIConfigRejectsOwnerWithoutAIConfigureOwnPerm(t *testing.T) {
	const svcID = "svc-perm-test"
	// "no-ai-perms" is not a builtin role and has no entry in the custom-roles
	// cache (authz.SetRoles was never called in this test), so authz.Can returns
	// false for every permission — including ai:configure:own.
	svcStore := &fakeServiceAIConfigStore{
		configs: map[string][]byte{svcID: []byte(`{}`)},
	}
	d := Deps{
		Log:              discardLog(),
		Users:            &fakeUserStore{role: "no-ai-perms"},
		Settings:         &fakeCacheSettingsStore{},
		ServiceAIConfigs: svcStore,
		// Service is owned by "u-self" — the same ID authedClient logs in as.
		CacheServices: &fakeCacheServiceLookup{owners: map[string]string{svcID: "u-self"}},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/"+svcID+"/ai-config", map[string]any{})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("owner-without-ai:configure:own status=%d want 403", r.StatusCode)
	}
	r.Body.Close()
}

// jsonKeys returns the sorted key names of a map[string]json.RawMessage
// (used only in test error messages).
func jsonKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// A successful PUT /services/{id}/ai-config writes one audit event that names
// the sections enabled after the change and nothing of their content. A
// refused or failed write leaves none.
func TestPutServiceAIConfigAudits(t *testing.T) {
	const svcID = "svc-audit"
	deps := func(role string, saveErr error) (Deps, *stubAuditAppender) {
		aud := &stubAuditAppender{}
		return Deps{
			Log:              discardLog(),
			Users:            &fakeUserStore{role: role},
			Settings:         &fakeCacheSettingsStore{},
			ServiceAIConfigs: &fakeServiceAIConfigStore{configs: map[string][]byte{}, saveErr: saveErr},
			CacheServices:    &fakeCacheServiceLookup{owners: map[string]string{svcID: "u-self"}},
			AuditAppender:    aud,
		}, aud
	}
	put := func(d Deps, body any) int {
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		r := authedClient(t, srv).put(t, "/api/v1/services/"+svcID+"/ai-config", body)
		r.Body.Close()
		return r.StatusCode
	}

	d, aud := deps("admin", nil)
	code := put(d, map[string]any{
		"cache": map[string]any{
			"enabled":  true,
			"semantic": map[string]any{"enabled": false, "embedding_url": "http://embed.internal/v1"},
		},
		"redaction":  map[string]any{"enabled": false},
		"guardrails": map[string]any{"enabled": true, "action": "refuse_403", "patterns": []string{"SECRET-PATTERN-BODY"}},
		"inspector":  nil,
		"anthropic":  map[string]any{"enabled": true},
		"prompt":     "SECRET-PROMPT-TEXT",
	})
	if code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", code)
	}
	if len(aud.events) != 1 {
		t.Fatalf("%d audit events, want 1", len(aud.events))
	}
	e := aud.events[0]
	if e.Action != audit.ActionServiceAIConfigUpdate || e.SubjectID != svcID || e.Result != "ok" || e.ActorID != "u-self" {
		t.Fatalf("event = %+v", e)
	}
	var got map[string]any
	if err := json.Unmarshal(e.Payload, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"cache": true, "semantic": false, "redaction": false, "guardrails": true, "inspector": false, "anthropic": true}
	if len(got) != len(want) {
		t.Fatalf("payload = %s", e.Payload)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("payload[%q] = %v, want %v (%s)", k, got[k], v, e.Payload)
		}
	}
	for _, leak := range []string{"SECRET", "embed.internal", "refuse_403"} {
		if strings.Contains(string(e.Payload), leak) {
			t.Fatalf("payload carries configuration content %q: %s", leak, e.Payload)
		}
	}

	// An empty body is a valid write: everything off.
	d, aud = deps("admin", nil)
	if code := put(d, map[string]any{}); code != http.StatusNoContent || len(aud.events) != 1 {
		t.Fatalf("empty body: status %d, %d events", code, len(aud.events))
	}

	// Refused (400), forbidden (403) and failed (500) writes are not audited.
	d, aud = deps("admin", nil)
	if code := put(d, map[string]any{"cache": map[string]any{"semantic": map[string]any{"min_similarity": 1.5}}}); code != http.StatusBadRequest || len(aud.events) != 0 {
		t.Fatalf("invalid config: status %d, %d events", code, len(aud.events))
	}
	d, aud = deps("no-ai-perms", nil)
	if code := put(d, map[string]any{}); code != http.StatusForbidden || len(aud.events) != 0 {
		t.Fatalf("forbidden: status %d, %d events", code, len(aud.events))
	}
	d, aud = deps("admin", errors.New("disk full"))
	if code := put(d, map[string]any{}); code != http.StatusInternalServerError || len(aud.events) != 0 {
		t.Fatalf("failed save: status %d, %d events", code, len(aud.events))
	}
}
