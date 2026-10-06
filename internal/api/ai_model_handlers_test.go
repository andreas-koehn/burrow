package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// fakeModelStore implements AIModelStore over a slice. A non-nil err is
// returned by every write instead of touching the slice. It validates
// nothing: what reaches it is what the handler let through.
type fakeModelStore struct {
	rows   []db.AIModel
	err    error
	writes int

	lastName string     // the name UpdateModel / DeleteModel was called with
	last     db.AIModel // the model CreateModel / UpdateModel received
}

func (f *fakeModelStore) ListModels(context.Context) ([]db.AIModel, error) {
	return append([]db.AIModel{}, f.rows...), nil
}

func (f *fakeModelStore) ModelByName(_ context.Context, name string) (db.AIModel, error) {
	for _, m := range f.rows {
		if m.Name == name {
			return m, nil
		}
	}
	return db.AIModel{}, store.ErrModelNotFound
}

// fill does what the store does for a target without a dialect.
func (f *fakeModelStore) fill(m db.AIModel) db.AIModel {
	m.Targets = append([]db.AIModelTarget{}, m.Targets...)
	for i := range m.Targets {
		if m.Targets[i].Dialect == "" {
			m.Targets[i].Dialect = "openai"
		}
	}
	if m.AttemptTimeoutS == 0 {
		m.AttemptTimeoutS = 60
	}
	if m.TotalTimeoutS == 0 {
		m.TotalTimeoutS = 120
	}
	m.UpdatedAt = time.Unix(1_700_000_000, 0).UTC()
	return m
}

func (f *fakeModelStore) CreateModel(_ context.Context, m db.AIModel) (db.AIModel, error) {
	f.writes++
	f.last = m
	if f.err != nil {
		return db.AIModel{}, f.err
	}
	m = f.fill(m)
	m.CreatedAt = m.UpdatedAt
	f.rows = append(f.rows, m)
	return m, nil
}

func (f *fakeModelStore) UpdateModel(_ context.Context, name string, m db.AIModel) (db.AIModel, error) {
	f.writes++
	f.lastName, f.last = name, m
	if f.err != nil {
		return db.AIModel{}, f.err
	}
	for i := range f.rows {
		if f.rows[i].Name == name {
			m = f.fill(m)
			m.CreatedAt = f.rows[i].CreatedAt
			f.rows[i] = m
			return m, nil
		}
	}
	return db.AIModel{}, store.ErrModelNotFound
}

func (f *fakeModelStore) DeleteModel(_ context.Context, name string) error {
	f.writes++
	f.lastName = name
	if f.err != nil {
		return f.err
	}
	for i := range f.rows {
		if f.rows[i].Name == name {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return store.ErrModelNotFound
}

func simpleModel() db.AIModel {
	return db.AIModel{
		Name: "burrow-simple", Enabled: true, AttemptTimeoutS: 60, TotalTimeoutS: 120,
		Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "mistral"}},
	}
}

type modelFixture struct {
	ms  *fakeModelStore
	aud *stubAuditAppender
	d   Deps
}

func newModelFixture(rows ...db.AIModel) *modelFixture {
	f := &modelFixture{ms: &fakeModelStore{rows: rows}, aud: &stubAuditAppender{}}
	f.d = Deps{
		Users:         &fakeUserStore{role: "admin"},
		AIModels:      f.ms,
		AuditAppender: f.aud,
		AuthDomain:    "burrow.example.com",
		Log:           discardLog(),
	}
	return f
}

func (f *modelFixture) serve(t *testing.T) *authClient {
	t.Helper()
	srv, c := newAIProviderServer(t, f.d)
	t.Cleanup(srv.Close)
	return c
}

func decodeModel(t *testing.T, body string) aiModelResp {
	t.Helper()
	var m aiModelResp
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return m
}

func TestListModels(t *testing.T) {
	t.Run("empty list is [] not null", func(t *testing.T) {
		c := newModelFixture().serve(t)
		if body := strings.TrimSpace(wantStatus(t, c.get(t, "/api/v1/ai/models"), http.StatusOK)); body != "[]" {
			t.Fatalf("body = %s", body)
		}
	})
	t.Run("nil store is an empty list", func(t *testing.T) {
		f := newModelFixture()
		f.d.AIModels = nil
		c := f.serve(t)
		if body := strings.TrimSpace(wantStatus(t, c.get(t, "/api/v1/ai/models"), http.StatusOK)); body != "[]" {
			t.Fatalf("body = %s", body)
		}
	})
	t.Run("models with targets", func(t *testing.T) {
		f := newModelFixture(simpleModel())
		f.d.Users = &fakeUserStore{role: "user"} // any session may read
		c := f.serve(t)
		body := wantStatus(t, c.get(t, "/api/v1/ai/models"), http.StatusOK)
		var raw []map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err != nil || len(raw) != 1 {
			t.Fatalf("decode %s: %v", body, err)
		}
		targets, _ := raw[0]["targets"].([]any)
		if len(targets) != 1 {
			t.Fatalf("targets = %v", raw[0]["targets"])
		}
		tg, _ := targets[0].(map[string]any)
		if tg["provider"] != "ollama" || tg["model"] != "mistral" || tg["dialect"] != "openai" || len(tg) != 3 {
			t.Errorf("target = %v", tg)
		}
		for _, k := range []string{"name", "description", "enabled", "fallback_on_rate_limit", "attempt_timeout_s",
			"total_timeout_s", "targets", "dialects", "created_at", "updated_at"} {
			if _, ok := raw[0][k]; !ok {
				t.Errorf("field %q missing in %s", k, body)
			}
		}
	})
}

func TestGetModel(t *testing.T) {
	c := newModelFixture(simpleModel()).serve(t)
	m := decodeModel(t, wantStatus(t, c.get(t, "/api/v1/ai/models/burrow-simple"), http.StatusOK))
	if m.Name != "burrow-simple" || !m.Enabled || len(m.Targets) != 1 || fmt.Sprint(m.Dialects) != "[openai]" {
		t.Errorf("model = %+v", m)
	}
	if body := wantStatus(t, c.get(t, "/api/v1/ai/models/nope"), http.StatusNotFound); !strings.Contains(body, "model not found") {
		t.Errorf("body = %s", body)
	}
	// A name that cannot be a model's is answered like a missing one.
	wantStatus(t, c.get(t, "/api/v1/ai/models/Not%20A%20Name"), http.StatusNotFound)
}

func TestPostModel(t *testing.T) {
	valid := map[string]any{
		"name": "burrow-intelligence", "description": "the good one",
		"targets": []map[string]string{
			{"dialect": "openai", "provider": "zai", "model": "glm-5.1"},
			{"dialect": "anthropic", "provider": "zai-anthropic", "model": "glm-5.1"},
			{"provider": "ollama", "model": "mistral"},
		},
	}

	t.Run("valid", func(t *testing.T) {
		f := newModelFixture()
		c := f.serve(t)
		m := decodeModel(t, wantStatus(t, c.post(t, "/api/v1/ai/models", valid), http.StatusCreated))

		got := f.ms.last
		if got.Name != "burrow-intelligence" || got.Description != "the good one" || !got.Enabled || len(got.Targets) != 3 {
			t.Fatalf("store received %+v", got)
		}
		if got.Targets[1] != (db.AIModelTarget{Dialect: "anthropic", ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"}) {
			t.Errorf("target 1 = %+v", got.Targets[1])
		}
		if got.Targets[2] != (db.AIModelTarget{ProviderSlug: "ollama", TargetModel: "mistral"}) {
			t.Errorf("a target without a dialect must reach the store without one: %+v", got.Targets[2])
		}
		if fmt.Sprint(m.Dialects) != "[anthropic openai]" {
			t.Errorf("dialects = %v, want sorted [anthropic openai]", m.Dialects)
		}
		for _, tg := range m.Targets {
			if tg.Dialect == "" {
				t.Errorf("target without a dialect in the response: %+v", m.Targets)
			}
		}
		if len(f.aud.events) != 1 || f.aud.events[0].Action != "ai_model.create" || f.aud.events[0].SubjectID != "burrow-intelligence" {
			t.Fatalf("audit = %+v", f.aud.events)
		}
		payload := auditPayload(t, f.aud.events[0])
		if fmt.Sprint(payload["targets"]) != "[openai:zai/glm-5.1 anthropic:zai-anthropic/glm-5.1 openai:ollama/mistral]" {
			t.Errorf("audit targets = %v", payload["targets"])
		}
	})

	t.Run("enabled false is kept", func(t *testing.T) {
		f := newModelFixture()
		c := f.serve(t)
		body := map[string]any{"name": "off", "enabled": false, "targets": valid["targets"]}
		wantStatus(t, c.post(t, "/api/v1/ai/models", body), http.StatusCreated)
		if f.ms.last.Enabled {
			t.Error("enabled:false was dropped")
		}
	})

	t.Run("store refuses", func(t *testing.T) {
		f := newModelFixture()
		f.ms.err = fmt.Errorf("%w: unknown provider nope", store.ErrInvalidModel)
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/models", valid), http.StatusBadRequest)
		var e map[string]string
		_ = json.Unmarshal([]byte(body), &e)
		if e["error"] != "unknown provider nope" {
			t.Errorf("error = %q (%s)", e["error"], body)
		}
		if len(f.aud.events) != 0 {
			t.Errorf("a refused create was audited: %+v", f.aud.events)
		}
	})

	t.Run("name taken", func(t *testing.T) {
		f := newModelFixture()
		f.ms.err = store.ErrModelExists
		wantStatus(t, f.serve(t).post(t, "/api/v1/ai/models", valid), http.StatusConflict)
	})

	t.Run("refused before the store", func(t *testing.T) {
		tooMany := make([]map[string]string, 17)
		for i := range tooMany {
			tooMany[i] = map[string]string{"provider": "ollama", "model": "m"}
		}
		for name, body := range map[string]any{
			"name with a slash": map[string]any{"name": "a/b", "targets": valid["targets"]},
			"upper case name":   map[string]any{"name": "Burrow", "targets": valid["targets"]},
			"reserved name":     map[string]any{"name": "v1", "targets": valid["targets"]},
			"no name":           map[string]any{"targets": valid["targets"]},
			"no targets":        map[string]any{"name": "ok-name"},
			"too many targets":  map[string]any{"name": "ok-name", "targets": tooMany},
			"unknown field":     map[string]any{"name": "ok-name", "targets": valid["targets"], "dialects": []string{"openai"}},
			"not json":          "nope",
		} {
			t.Run(name, func(t *testing.T) {
				f := newModelFixture()
				wantStatus(t, f.serve(t).post(t, "/api/v1/ai/models", body), http.StatusBadRequest)
				if f.ms.writes != 0 {
					t.Errorf("the store was called")
				}
			})
		}
	})

	t.Run("body over 16 KiB", func(t *testing.T) {
		f := newModelFixture()
		body := map[string]any{"name": "big", "description": strings.Repeat("x", 17<<10), "targets": valid["targets"]}
		resp := f.serve(t).post(t, "/api/v1/ai/models", body)
		if readBody(t, resp); resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if f.ms.writes != 0 {
			t.Errorf("the store was called")
		}
	})
}

// modelWriteRoutes is every route that changes a synthetic model.
var modelWriteRoutes = []struct {
	method, path string
	body         any
}{
	{http.MethodPost, "/api/v1/ai/models", map[string]any{"name": "new-one", "targets": []map[string]string{{"provider": "ollama", "model": "m"}}}},
	{http.MethodPut, "/api/v1/ai/models/burrow-simple", map[string]any{"targets": []map[string]string{{"provider": "ollama", "model": "m"}}}},
	{http.MethodDelete, "/api/v1/ai/models/burrow-simple", nil},
}

func TestPostModel_RequiresAdmin(t *testing.T) {
	f := newModelFixture(simpleModel())
	f.d.Users = &fakeUserStore{role: "user"}
	c := f.serve(t)
	for _, rt := range modelWriteRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			wantStatus(t, c.do(t, rt.method, rt.path, rt.body), http.StatusForbidden)
		})
	}
	if f.ms.writes != 0 || len(f.aud.events) != 0 {
		t.Errorf("a forbidden request reached the store or the audit log: writes=%d events=%+v", f.ms.writes, f.aud.events)
	}
}

func TestModelAndKeyRoutes_Unauthenticated(t *testing.T) {
	f := newModelFixture(simpleModel())
	ks := &fakeKeyStore{}
	f.d.AIGatewayKeys = ks
	srv := httptest.NewServer(NewRouter(f.d))
	defer srv.Close()
	anon := &authClient{base: srv.URL, hc: &http.Client{}}
	for _, rt := range modelWriteRoutes {
		wantStatus(t, anon.do(t, rt.method, rt.path, rt.body), http.StatusUnauthorized)
	}
	for _, path := range []string{"/api/v1/ai/models", "/api/v1/ai/models/burrow-simple", "/api/v1/ai/keys", "/api/v1/ai/gateway"} {
		wantStatus(t, anon.get(t, path), http.StatusUnauthorized)
	}
	wantStatus(t, anon.post(t, "/api/v1/ai/keys", map[string]string{"name": "k"}), http.StatusUnauthorized)
	wantStatus(t, anon.delete(t, "/api/v1/ai/keys/k1"), http.StatusUnauthorized)
	if f.ms.writes != 0 || ks.creates != 0 || ks.revokes != 0 {
		t.Error("an anonymous request reached a store")
	}
}

func TestPutModel(t *testing.T) {
	targets := []map[string]string{{"dialect": "openai", "provider": "zai", "model": "glm-5.1"}}

	t.Run("update", func(t *testing.T) {
		f := newModelFixture(simpleModel())
		c := f.serve(t)
		m := decodeModel(t, wantStatus(t, c.put(t, "/api/v1/ai/models/burrow-simple",
			map[string]any{"description": "d", "fallback_on_rate_limit": true, "attempt_timeout_s": 10, "total_timeout_s": 20, "targets": targets}),
			http.StatusOK))
		got := f.ms.last
		if f.ms.lastName != "burrow-simple" || got.Name != "burrow-simple" {
			t.Errorf("a body without a name must keep it: path %q, model %q", f.ms.lastName, got.Name)
		}
		if !got.Enabled {
			t.Error("a body without enabled must keep the stored value")
		}
		if !got.FallbackOnRateLimit || got.AttemptTimeoutS != 10 || got.TotalTimeoutS != 20 || got.Description != "d" {
			t.Errorf("store received %+v", got)
		}
		if m.Targets[0].Provider != "zai" {
			t.Errorf("response = %+v", m)
		}
		if len(f.aud.events) != 1 || f.aud.events[0].Action != "ai_model.update" || f.aud.events[0].SubjectID != "burrow-simple" {
			t.Fatalf("audit = %+v", f.aud.events)
		}
	})

	t.Run("a disabled model stays disabled", func(t *testing.T) {
		off := simpleModel()
		off.Enabled = false
		f := newModelFixture(off)
		wantStatus(t, f.serve(t).put(t, "/api/v1/ai/models/burrow-simple", map[string]any{"targets": targets}), http.StatusOK)
		if f.ms.last.Enabled {
			t.Error("a body without enabled switched the model on")
		}
	})

	t.Run("rename", func(t *testing.T) {
		f := newModelFixture(simpleModel())
		m := decodeModel(t, wantStatus(t, f.serve(t).put(t, "/api/v1/ai/models/burrow-simple",
			map[string]any{"name": "burrow-fast", "targets": targets}), http.StatusOK))
		if f.ms.lastName != "burrow-simple" || f.ms.last.Name != "burrow-fast" || m.Name != "burrow-fast" {
			t.Errorf("rename: path %q, model %q, response %q", f.ms.lastName, f.ms.last.Name, m.Name)
		}
		ev := f.aud.events[0]
		if ev.SubjectID != "burrow-fast" || auditPayload(t, ev)["old_name"] != "burrow-simple" {
			t.Errorf("audit = %+v payload %s", ev, ev.Payload)
		}
	})

	t.Run("errors", func(t *testing.T) {
		f := newModelFixture(simpleModel())
		c := f.serve(t)
		wantStatus(t, c.put(t, "/api/v1/ai/models/nope", map[string]any{"targets": targets}), http.StatusNotFound)
		wantStatus(t, c.put(t, "/api/v1/ai/models/burrow-simple", map[string]any{"name": "a/b", "targets": targets}), http.StatusBadRequest)
		if f.ms.writes != 0 {
			t.Errorf("the store was written %d times", f.ms.writes)
		}
		f.ms.err = store.ErrModelExists
		wantStatus(t, c.put(t, "/api/v1/ai/models/burrow-simple", map[string]any{"name": "taken", "targets": targets}), http.StatusConflict)
		f.ms.err = fmt.Errorf("%w: provider zai speaks openai, not anthropic", store.ErrInvalidModel)
		if body := wantStatus(t, c.put(t, "/api/v1/ai/models/burrow-simple", map[string]any{"targets": targets}), http.StatusBadRequest); !strings.Contains(body, "provider zai speaks openai, not anthropic") {
			t.Errorf("body = %s", body)
		}
		if len(f.aud.events) != 0 {
			t.Errorf("a failed update was audited: %+v", f.aud.events)
		}
	})
}

func TestDeleteModel(t *testing.T) {
	f := newModelFixture(simpleModel())
	c := f.serve(t)
	wantStatus(t, c.delete(t, "/api/v1/ai/models/burrow-simple"), http.StatusNoContent)
	if len(f.ms.rows) != 0 {
		t.Errorf("not deleted: %+v", f.ms.rows)
	}
	if len(f.aud.events) != 1 || f.aud.events[0].Action != "ai_model.delete" || f.aud.events[0].SubjectID != "burrow-simple" {
		t.Fatalf("audit = %+v", f.aud.events)
	}
	wantStatus(t, c.delete(t, "/api/v1/ai/models/burrow-simple"), http.StatusNotFound)
	if len(f.aud.events) != 1 {
		t.Errorf("a failed delete was audited: %+v", f.aud.events)
	}
}

func TestModelAliasRoutesRemoved(t *testing.T) {
	c := newModelFixture().serve(t)
	wantStatus(t, c.get(t, "/api/v1/models/aliases"), http.StatusNotFound)
	wantStatus(t, c.post(t, "/api/v1/models/aliases", map[string]string{"alias": "a"}), http.StatusNotFound)
	wantStatus(t, c.put(t, "/api/v1/models/aliases/a", map[string]string{"concrete_model": "m"}), http.StatusNotFound)
	wantStatus(t, c.delete(t, "/api/v1/models/aliases/a"), http.StatusNotFound)
}

func TestDeleteProvider_InUse(t *testing.T) {
	ss, ps := oneProviderFixture()
	ps.deleteErr = fmt.Errorf("%w: burrow-simple, burrow-fast", store.ErrProviderInUse)
	aud := &stubAuditAppender{}
	d := newAIProviderDeps(ss, &fakeModelStore{}, ps)
	d.AuditAppender = aud
	srv, c := newAIProviderServer(t, d)
	defer srv.Close()

	body := wantStatus(t, c.delete(t, "/api/v1/ai/providers/ollama"), http.StatusConflict)
	if !strings.Contains(body, "provider is used by model(s): burrow-simple, burrow-fast") {
		t.Errorf("body = %s", body)
	}
	if len(aud.events) != 0 {
		t.Errorf("a refused delete was audited: %+v", aud.events)
	}
}

func TestDeleteUser_ProviderInUse(t *testing.T) {
	u := &userMgmtStore{deleteUserErr: fmt.Errorf("delete user: %w: burrow-simple", store.ErrProviderInUse)}
	ts, cl, csrf := newUserMgmtServer(t, u)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/users/other-user-id", nil)
	resp := doWithCSRF(t, cl, req, csrf)
	if body := wantStatus(t, resp, http.StatusConflict); !strings.Contains(body, "burrow-simple") || strings.Contains(body, "delete user:") {
		t.Errorf("body = %s", body)
	}
}

// The provider view names the first synthetic model that targets the provider.
func TestListProviders_NamesFirstModel(t *testing.T) {
	ss, ps := oneProviderFixture()
	ms := &fakeModelStore{rows: []db.AIModel{
		{Name: "aaa", Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "other", TargetModel: "x"}}},
		{Name: "burrow-simple", Targets: []db.AIModelTarget{
			{Dialect: "openai", ProviderSlug: "other", TargetModel: "x"},
			{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "mistral"},
		}},
		{Name: "zzz", Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "late"}}},
	}}
	srv, c := newAIProviderServer(t, newAIProviderDeps(ss, ms, ps))
	defer srv.Close()
	out := decodeProviders(t, c.get(t, "/api/v1/ai/providers"))
	if len(out) != 1 || out[0].ModelAlias != "burrow-simple" || out[0].ConcreteModel != "mistral" {
		t.Fatalf("providers = %+v", out)
	}
}

func TestAIModelAuditActionsRegistered(t *testing.T) {
	for _, a := range []string{audit.ActionAIModelCreate, audit.ActionAIModelUpdate, audit.ActionAIModelDelete,
		audit.ActionAIGatewayKeyCreate, audit.ActionAIGatewayKeyRevoke} {
		if !slices.Contains(audit.AllActions, a) {
			t.Errorf("audit action %q is not registered", a)
		}
	}
}
