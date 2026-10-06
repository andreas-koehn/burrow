package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
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
		if tg["provider"] != "ollama" || tg["model"] != "mistral" || tg["dialect"] != "openai" || tg["available"] != false || len(tg) != 4 {
			t.Errorf("target = %v", tg)
		}
		for _, k := range []string{"name", "description", "enabled", "fallback_on_rate_limit", "attempt_timeout_s",
			"total_timeout_s", "targets", "dialects", "serving", "created_at", "updated_at"} {
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

// aiRoute is one of the routes this file and ai_key_handlers_test.go cover.
type aiRoute struct {
	method, path string
	body         any
}

func (r aiRoute) key() string { return r.method + " " + r.path }

var (
	rtGateway   = aiRoute{http.MethodGet, "/api/v1/ai/gateway", nil}
	rtModelList = aiRoute{http.MethodGet, "/api/v1/ai/models", nil}
	rtModelGet  = aiRoute{http.MethodGet, "/api/v1/ai/models/burrow-simple", nil}
	rtModelPost = aiRoute{http.MethodPost, "/api/v1/ai/models", map[string]any{"name": "new-one", "targets": []map[string]string{{"provider": "ollama", "model": "m"}}}}
	rtModelPut  = aiRoute{http.MethodPut, "/api/v1/ai/models/burrow-simple", map[string]any{"targets": []map[string]string{{"provider": "ollama", "model": "m"}}}}
	rtModelDel  = aiRoute{http.MethodDelete, "/api/v1/ai/models/burrow-simple", nil}
	rtKeyList   = aiRoute{http.MethodGet, "/api/v1/ai/keys", nil}
	rtKeyPost   = aiRoute{http.MethodPost, "/api/v1/ai/keys", map[string]any{"name": "k"}}
	rtKeyDel    = aiRoute{http.MethodDelete, "/api/v1/ai/keys/k-mine", nil} // the caller's own key
)

// aiRoutes is every route of the synthetic-model and gateway-key API.
var aiRoutes = []aiRoute{rtGateway, rtModelList, rtModelGet, rtModelPost, rtModelPut, rtModelDel, rtKeyList, rtKeyPost, rtKeyDel}

// TestAIModelAndKeyRoutes_Authorization is the whole matrix: every route for
// every kind of caller. Each cell gets a fresh server and stores.
//
//   - Model writes need a session of an admin or of a role with
//     ai:configure:any, or an automation token that DECLARES ai:configure:any
//     (and whose user still holds it). The role of a token's user alone is
//     not enough: an admin's narrow token cannot write models.
//   - Creating a gateway key needs a dashboard session: no token may mint a
//     credential that outlives it.
//   - Listing and revoking keys: a token acts for its user's own keys only.
func TestAIModelAndKeyRoutes_Authorization(t *testing.T) {
	authz.SetRoles(map[string][]authz.Permission{"ai-operator": {authz.PermAIConfigureAny}})
	defer authz.SetRoles(nil)

	const configure = string(authz.PermAIConfigureAny)
	ok := map[string]int{
		rtGateway.key(): 200, rtModelList.key(): 200, rtModelGet.key(): 200,
		rtModelPost.key(): 201, rtModelPut.key(): 200, rtModelDel.key(): 204,
		rtKeyList.key(): 200, rtKeyPost.key(): 201, rtKeyDel.key(): 204,
	}
	// with returns the all-success row with some cells replaced.
	with := func(status int, routes ...aiRoute) map[string]int {
		m := map[string]int{}
		for k, v := range ok {
			m[k] = v
		}
		for _, r := range routes {
			m[r.key()] = status
		}
		return m
	}
	all := func(status int) map[string]int { return with(status, aiRoutes...) }

	callers := []struct {
		name   string
		role   string // role of the user
		anon   bool   // no credentials at all
		bearer bool   // automation token instead of a session
		// The role the user had when the token was minted; "" = role.
		mintedAs string
		perms    []string // what the token declares
		want     map[string]int
	}{
		{name: "anonymous", anon: true, want: all(401)},
		{name: "session of a plain user", role: "user", want: with(403, rtModelPost, rtModelPut, rtModelDel)},
		{name: "session of a role with ai:configure:any", role: "ai-operator", want: ok},
		{name: "session of an admin", role: "admin", want: ok},
		{name: "admin's token without permissions", role: "admin", bearer: true,
			want: with(403, rtModelPost, rtModelPut, rtModelDel, rtKeyPost)},
		{name: "admin's token with another permission", role: "admin", bearer: true, perms: []string{"tunnels:read:any"},
			want: with(403, rtModelPost, rtModelPut, rtModelDel, rtKeyPost)},
		{name: "admin's token with ai:configure:any", role: "admin", bearer: true, perms: []string{configure},
			want: with(403, rtKeyPost)},
		{name: "operator's token with ai:configure:any", role: "ai-operator", bearer: true, perms: []string{configure},
			want: with(403, rtKeyPost)},
		{name: "plain user's token", role: "user", bearer: true,
			want: with(403, rtModelPost, rtModelPut, rtModelDel, rtKeyPost)},
		// What a token declares opens nothing its user's role does not hold:
		// these tokens were minted while the user was an admin.
		{name: "plain user's token declaring nothing", role: "user", bearer: true, mintedAs: "admin", perms: []string{},
			want: with(403, rtModelPost, rtModelPut, rtModelDel, rtKeyPost)},
		{name: "plain user's token declaring ai:configure:any", role: "user", bearer: true, mintedAs: "admin", perms: []string{configure},
			want: with(403, rtModelPost, rtModelPut, rtModelDel, rtKeyPost)},
		{name: "plain user's token declaring ai:configure:own", role: "user", bearer: true, mintedAs: "admin", perms: []string{string(authz.PermAIConfigureOwn)},
			want: with(403, rtModelPost, rtModelPut, rtModelDel, rtKeyPost)},
	}

	for _, c := range callers {
		for _, rt := range aiRoutes {
			t.Run(c.name+"/"+rt.key(), func(t *testing.T) {
				f := newModelFixture(simpleModel())
				ks := &fakeKeyStore{rows: []store.GatewayKey{{ID: "k-mine", Name: "mine", UserID: "u-self"}}}
				auto := newFakeAutomationStore()
				f.d.AIGatewayKeys, f.d.Automation, f.d.Bearer = ks, auto, auto
				f.d.Users = &fakeUserStore{role: c.role}
				srv := httptest.NewServer(NewRouter(f.d))
				defer srv.Close()

				var resp *http.Response
				switch {
				case c.anon:
					resp = (&authClient{base: srv.URL, hc: &http.Client{}}).do(t, rt.method, rt.path, rt.body)
				case c.bearer:
					mintRole := c.role
					if c.mintedAs != "" {
						mintRole = c.mintedAs
					}
					_, token, err := auto.MintAutomationToken(context.Background(), "u-self", mintRole, "ci", c.perms, nil)
					if err != nil {
						t.Fatal(err)
					}
					resp = bearerDo(t, srv, token, rt.method, rt.path, rt.body)
				default:
					resp = authedClient(t, srv).do(t, rt.method, rt.path, rt.body)
				}
				want := c.want[rt.key()]
				wantStatus(t, resp, want)
				if want >= 400 {
					if f.ms.writes != 0 || ks.creates != 0 || ks.revokes != 0 || len(f.aud.events) != 0 {
						t.Errorf("a refused request reached a store or the audit log: model writes=%d key creates=%d revokes=%d events=%+v",
							f.ms.writes, ks.creates, ks.revokes, f.aud.events)
					}
				}
			})
		}
	}
}

// bearerDo sends one request authenticated by an automation token: no cookie
// and no CSRF header.
func bearerDo(t *testing.T, srv *httptest.Server, token, method, path string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = mustJSON(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
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

// fakeBreaker is the gateway's breaker as the API sees it.
type fakeBreaker struct {
	open   map[string]bool
	forgot []string
}

func (f *fakeBreaker) Open(slug string) bool { return f.open[slug] }
func (f *fakeBreaker) Forget(slug string)    { f.forgot = append(f.forgot, slug) }

// failingProviderList is a provider store whose list cannot be read.
type failingProviderList struct{ *fakeProviderStore }

func (failingProviderList) ListProviders(context.Context) ([]db.AIProvider, error) {
	return nil, errors.New("provider list is down")
}

func directProvider(slug, format, slot string) db.AIProvider {
	return db.AIProvider{Slug: slug, Name: slug, Kind: "direct", ServiceID: "prov-" + slug, APIFormat: format,
		BaseURL: "https://" + slug + ".example/v1", CredentialSlot: slot}
}

// servingFixture is a model with three OpenAI targets (zai, openrouter and
// the tunnelled ollama) and one Anthropic target.
func servingFixture() (*modelFixture, *fakeBreaker) {
	f := newModelFixture(db.AIModel{
		Name: "burrow-smart", Enabled: true, AttemptTimeoutS: 60, TotalTimeoutS: 120,
		Targets: []db.AIModelTarget{
			{Dialect: "anthropic", ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"},
			{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm-5.1"},
			{Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
			{Dialect: "openai", Position: 2, ProviderSlug: "ollama", TargetModel: "mistral"},
		},
	})
	br := &fakeBreaker{open: map[string]bool{"zai": true}}
	f.d.AIProviders = &fakeProviderStore{rows: []db.AIProvider{
		tunnelProvider("ollama", "Ollama", "svc1"),
		directProvider("openrouter", "openai", "OPENROUTER"),
		directProvider("zai", "openai", "ZAI"),
		directProvider("zai-anthropic", "anthropic", "ZAI, ZAI2"),
	}}
	f.d.CredentialVault = secretVault{"OPENROUTER": upstreamSecret, "ZAI": upstreamSecret, "ZAI2": upstreamSecret}
	f.d.LiveTunnels = fakeLiveTunnels{svcID: "svc1", exists: true, connected: true}
	f.d.AIBreaker = br
	return f, br
}

// servingOf reads one model view as raw JSON: the availability of the OpenAI
// targets in order, and the serving object.
func servingOf(t *testing.T, body string) (openai []bool, serving map[string]any) {
	t.Helper()
	var raw struct {
		Targets []struct {
			Dialect   string `json:"dialect"`
			Available *bool  `json:"available"`
		} `json:"targets"`
		Serving map[string]any `json:"serving"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	for _, tg := range raw.Targets {
		if tg.Available == nil {
			t.Fatalf("a target has no available field: %s", body)
		}
		if tg.Dialect == "openai" {
			openai = append(openai, *tg.Available)
		}
	}
	return openai, raw.Serving
}

func TestModelView_ServingAndAvailability(t *testing.T) {
	const path = "/api/v1/ai/models/burrow-smart"
	target := func(provider, model string) string {
		return fmt.Sprint(map[string]any{"provider": provider, "model": model})
	}

	t.Run("first available target serves, per dialect", func(t *testing.T) {
		f, _ := servingFixture()
		f.d.Users = &fakeUserStore{role: "user"} // any session reads models
		c := f.serve(t)
		for _, body := range []string{
			wantStatus(t, c.get(t, path), http.StatusOK),
			strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(wantStatus(t, c.get(t, "/api/v1/ai/models"), http.StatusOK)), "["), "]"),
		} {
			openai, serving := servingOf(t, body)
			if fmt.Sprint(openai) != "[false true true]" {
				t.Errorf("openai availability = %v in %s", openai, body)
			}
			if fmt.Sprint(serving["openai"]) != target("openrouter", "google/gemini-x") {
				t.Errorf("serving.openai = %v", serving["openai"])
			}
			if fmt.Sprint(serving["anthropic"]) != target("zai-anthropic", "glm-5.1") {
				t.Errorf("serving.anthropic = %v", serving["anthropic"])
			}
			if strings.Contains(body, upstreamSecret) {
				t.Errorf("model view leaks a credential: %s", body)
			}
		}
	})

	t.Run("a dialect without targets has no serving key", func(t *testing.T) {
		f := newModelFixture(simpleModel())
		f.d.AIProviders = &fakeProviderStore{rows: []db.AIProvider{tunnelProvider("ollama", "Ollama", "svc1")}}
		f.d.LiveTunnels = fakeLiveTunnels{svcID: "svc1", exists: true, connected: true}
		_, serving := servingOf(t, wantStatus(t, f.serve(t).get(t, "/api/v1/ai/models/burrow-simple"), http.StatusOK))
		if _, has := serving["anthropic"]; has || serving["openai"] == nil {
			t.Errorf("serving = %v", serving)
		}
	})

	t.Run("offline tunnel client", func(t *testing.T) {
		f, _ := servingFixture()
		f.d.LiveTunnels = fakeLiveTunnels{svcID: "svc1", exists: true, connected: false}
		openai, _ := servingOf(t, wantStatus(t, f.serve(t).get(t, path), http.StatusOK))
		if fmt.Sprint(openai) != "[false true false]" {
			t.Errorf("openai availability = %v", openai)
		}
	})

	t.Run("one slot of several missing", func(t *testing.T) {
		f, _ := servingFixture()
		f.d.CredentialVault = secretVault{"OPENROUTER": upstreamSecret, "ZAI": upstreamSecret, "ZAI2": ""}
		_, serving := servingOf(t, wantStatus(t, f.serve(t).get(t, path), http.StatusOK))
		if v, has := serving["anthropic"]; !has || v != nil {
			t.Errorf("serving.anthropic = %v (present %v), want null", v, has)
		}
	})

	t.Run("nothing available", func(t *testing.T) {
		f, br := servingFixture()
		br.open["ollama"] = true
		f.d.CredentialVault = secretVault{"ZAI": upstreamSecret, "ZAI2": upstreamSecret}
		body := wantStatus(t, f.serve(t).get(t, path), http.StatusOK)
		openai, serving := servingOf(t, body)
		if fmt.Sprint(openai) != "[false false false]" {
			t.Errorf("openai availability = %v", openai)
		}
		if v, has := serving["openai"]; !has || v != nil {
			t.Errorf("serving.openai = %v (present %v), want null in %s", v, has, body)
		}
	})

	t.Run("no breaker wired", func(t *testing.T) {
		f, _ := servingFixture()
		f.d.AIBreaker = nil
		openai, serving := servingOf(t, wantStatus(t, f.serve(t).get(t, path), http.StatusOK))
		if fmt.Sprint(openai) != "[true true true]" || fmt.Sprint(serving["openai"]) != target("zai", "glm-5.1") {
			t.Errorf("availability = %v serving = %v", openai, serving)
		}
	})

	t.Run("provider gone or of another format", func(t *testing.T) {
		f, br := servingFixture()
		br.open = nil
		ps := f.d.AIProviders.(*fakeProviderStore)
		ps.rows = ps.rows[:3]              // zai-anthropic is gone
		ps.rows[1].APIFormat = "anthropic" // openrouter no longer speaks openai
		openai, serving := servingOf(t, wantStatus(t, f.serve(t).get(t, path), http.StatusOK))
		if fmt.Sprint(openai) != "[true false true]" || serving["anthropic"] != nil {
			t.Errorf("availability = %v serving = %v", openai, serving)
		}
	})

	t.Run("a disabled model serves nothing, its targets keep their availability", func(t *testing.T) {
		f, _ := servingFixture()
		f.ms.rows[0].Enabled = false
		body := wantStatus(t, f.serve(t).get(t, path), http.StatusOK)
		openai, serving := servingOf(t, body)
		if fmt.Sprint(openai) != "[false true true]" {
			t.Errorf("openai availability = %v", openai)
		}
		for _, dialect := range []string{"openai", "anthropic"} {
			if v, has := serving[dialect]; !has || v != nil {
				t.Errorf("serving.%s = %v (present %v), want null in %s", dialect, v, has, body)
			}
		}
	})

	t.Run("a write that went through is answered even when the providers cannot be read", func(t *testing.T) {
		f, _ := servingFixture()
		logs := &bytes.Buffer{}
		f.d.Log = slog.New(slog.NewTextHandler(logs, nil))
		f.d.AIProviders = failingProviderList{f.d.AIProviders.(*fakeProviderStore)}
		c := f.serve(t)
		body := wantStatus(t, c.post(t, "/api/v1/ai/models", map[string]any{"name": "new-one",
			"targets": []map[string]any{{"provider": "zai", "model": "glm-5.1"}}}), http.StatusCreated)
		openai, serving := servingOf(t, body)
		if v, has := serving["openai"]; fmt.Sprint(openai) != "[false]" || !has || v != nil {
			t.Errorf("availability = %v serving = %v", openai, serving)
		}
		body = wantStatus(t, c.put(t, path, map[string]any{"targets": []map[string]any{{"provider": "zai", "model": "glm-5.1"}}}), http.StatusOK)
		if openai, _ := servingOf(t, body); fmt.Sprint(openai) != "[false]" {
			t.Errorf("availability = %v", openai)
		}
		if f.ms.writes != 2 || len(f.aud.events) != 2 {
			t.Errorf("writes = %d, audit events = %d", f.ms.writes, len(f.aud.events))
		}
		if !strings.Contains(logs.String(), "provider list is down") {
			t.Errorf("the lookup error was not logged: %s", logs.String())
		}
		if strings.Contains(body, "provider list is down") {
			t.Errorf("the lookup error reached the client: %s", body)
		}
	})

	t.Run("writes answer with the same view and refuse the read-only fields", func(t *testing.T) {
		f, _ := servingFixture()
		c := f.serve(t)
		body := wantStatus(t, c.put(t, path, map[string]any{"targets": []map[string]any{
			{"provider": "zai", "model": "glm-5.1"}, {"provider": "openrouter", "model": "google/gemini-x"}}}), http.StatusOK)
		openai, serving := servingOf(t, body)
		if fmt.Sprint(openai) != "[false true]" || fmt.Sprint(serving["openai"]) != target("openrouter", "google/gemini-x") {
			t.Errorf("availability = %v serving = %v", openai, serving)
		}
		wantStatus(t, c.put(t, path, map[string]any{"targets": []map[string]any{
			{"provider": "zai", "model": "glm-5.1", "available": true}}}), http.StatusBadRequest)
		wantStatus(t, c.put(t, path, map[string]any{"serving": map[string]any{},
			"targets": []map[string]any{{"provider": "zai", "model": "glm-5.1"}}}), http.StatusBadRequest)
	})
}
