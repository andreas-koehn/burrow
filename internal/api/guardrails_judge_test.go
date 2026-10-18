package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/inspector"
)

func judgeAdmin(t *testing.T, ss *fakeCacheSettingsStore) *authClient {
	t.Helper()
	models := &fakeModelStore{rows: []db.AIModel{{Name: "judge-model", Enabled: true}}}
	srv := httptest.NewServer(NewRouter(Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Settings: ss, AIModels: models}))
	t.Cleanup(srv.Close)
	return authedClient(t, srv)
}

// GET serves the judge with its defaults, also for a blob stored before the
// judge existed.
func TestGuardrailGetSettings_JudgeDefaults(t *testing.T) {
	for name, saved := range map[string]map[string]string{
		"no row":   {},
		"old blob": {"guardrails.global": `{"enabled":true,"action":"refuse_403","answers":{"enabled":false,"action":"flag"},"custom_patterns":[]}`},
	} {
		c := judgeAdmin(t, &fakeCacheSettingsStore{saved: saved})
		r := c.get(t, "/api/v1/guardrails/settings")
		body := readBody(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", name, r.StatusCode, body)
		}
		var raw struct {
			Global map[string]json.RawMessage `json:"global"`
		}
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatal(err)
		}
		if j := string(raw.Global["judge"]); j != `{"enabled":false,"model":"","policy":"","check":"requests","timeout_ms":3000,"on_failure":"allow"}` {
			t.Errorf("%s: judge = %s", name, j)
		}
	}
}

func TestGuardrailPutSettings_Judge(t *testing.T) {
	ss := &fakeCacheSettingsStore{}
	c := judgeAdmin(t, ss)
	r := c.put(t, "/api/v1/guardrails/settings", map[string]any{
		"enabled": true, "action": "refuse_403",
		"judge": map[string]any{"enabled": true, "model": "judge-model", "policy": "No malware.", "check": "requests_and_answers", "timeout_ms": 1500, "on_failure": "refuse"},
	})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	var s guardrails.Settings
	if err := json.Unmarshal([]byte(ss.saved["guardrails.global"]), &s); err != nil {
		t.Fatal(err)
	}
	want := guardrails.JudgeSettings{Enabled: true, Model: "judge-model", Policy: "No malware.", Check: "requests_and_answers", TimeoutMS: 1500, OnFailure: "refuse"}
	if s.Judge != want {
		t.Fatalf("persisted judge %+v", s.Judge)
	}
	// Empty enums and timeout are stored as their defaults.
	r = c.put(t, "/api/v1/guardrails/settings", map[string]any{
		"action": "log_only", "judge": map[string]any{"enabled": false},
	})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	s = guardrails.Settings{}
	_ = json.Unmarshal([]byte(ss.saved["guardrails.global"]), &s)
	if s.Judge != (guardrails.JudgeSettings{Check: "requests", TimeoutMS: 3000, OnFailure: "allow"}) {
		t.Fatalf("defaults not stored: %+v", s.Judge)
	}
}

func TestGuardrailPutSettings_RefusesBadJudge(t *testing.T) {
	for name, tc := range map[string]struct {
		judge map[string]any
		msg   string
	}{
		"no model":        {map[string]any{"enabled": true}, "judge model is required"},
		"unknown model":   {map[string]any{"enabled": true, "model": "nope"}, `judge model \"nope\" does not exist`},
		"bad model name":  {map[string]any{"enabled": true, "model": "a b/c"}, `does not exist`},
		"long policy":     {map[string]any{"policy": strings.Repeat("p", guardrails.MaxJudgePolicy+1)}, "policy"},
		"bad check":       {map[string]any{"check": "answers"}, "judge.check"},
		"short timeout":   {map[string]any{"timeout_ms": 499}, "timeout_ms"},
		"long timeout":    {map[string]any{"timeout_ms": 30001}, "timeout_ms"},
		"bad on_failure":  {map[string]any{"on_failure": "block"}, "judge.on_failure"},
		"timeout a word":  {map[string]any{"timeout_ms": "fast"}, "invalid setting"},
		"policy a number": {map[string]any{"policy": 3}, "invalid setting"},
	} {
		ss := &fakeCacheSettingsStore{}
		c := judgeAdmin(t, ss)
		r := c.put(t, "/api/v1/guardrails/settings", map[string]any{"enabled": true, "action": "refuse_403", "judge": tc.judge})
		body := readBody(t, r)
		if r.StatusCode != http.StatusBadRequest || !strings.Contains(body, tc.msg) {
			t.Errorf("%s: %d %s", name, r.StatusCode, body)
		}
		if _, saved := ss.saved["guardrails.global"]; saved {
			t.Errorf("%s: saved", name)
		}
	}
}

// An enabled judge cannot name a model that is switched off: it would fail
// every request.
func TestGuardrailPutSettings_RefusesDisabledJudgeModel(t *testing.T) {
	models := &fakeModelStore{rows: []db.AIModel{{Name: "off-model", Enabled: false}}}
	ss := &fakeCacheSettingsStore{}
	srv := httptest.NewServer(NewRouter(Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Settings: ss, AIModels: models}))
	t.Cleanup(srv.Close)
	c := authedClient(t, srv)
	r := c.put(t, "/api/v1/guardrails/settings", map[string]any{"action": "log_only", "judge": map[string]any{"enabled": true, "model": "off-model"}})
	body := readBody(t, r)
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(body, `judge model \"off-model\" is disabled`) {
		t.Fatalf("PUT %d %s", r.StatusCode, body)
	}
	if _, saved := ss.saved["guardrails.global"]; saved {
		t.Fatal("saved")
	}
	// Switched off, the judge may keep naming it.
	r = c.put(t, "/api/v1/guardrails/settings", map[string]any{"action": "log_only", "judge": map[string]any{"enabled": false, "model": "off-model"}})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
}

// A disabled judge may name a model that is gone: saving the other settings
// must not fail on it.
func TestGuardrailPutSettings_DisabledJudgeModelNotLookedUp(t *testing.T) {
	c := judgeAdmin(t, &fakeCacheSettingsStore{})
	r := c.put(t, "/api/v1/guardrails/settings", map[string]any{"action": "log_only", "judge": map[string]any{"enabled": false, "model": "deleted-model"}})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
}

// The judge keeps its stored value when the body leaves it out.
func TestGuardrailPutSettings_KeepsJudgeLeftOut(t *testing.T) {
	ss := &fakeCacheSettingsStore{saved: map[string]string{
		"guardrails.global": `{"enabled":true,"action":"refuse_403","answers":{"enabled":false,"action":"flag"},"custom_patterns":[],"judge":{"enabled":true,"model":"judge-model","policy":"p","check":"requests","timeout_ms":900,"on_failure":"refuse"}}`,
	}}
	c := judgeAdmin(t, ss)
	r := c.put(t, "/api/v1/guardrails/settings", map[string]any{"enabled": false, "action": "log_only"})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	var s guardrails.Settings
	if err := json.Unmarshal([]byte(ss.saved["guardrails.global"]), &s); err != nil {
		t.Fatal(err)
	}
	if !s.Judge.Enabled || s.Judge.Model != "judge-model" || s.Judge.TimeoutMS != 900 || s.Judge.OnFailure != "refuse" {
		t.Fatalf("judge left out was not kept: %+v", s.Judge)
	}
}

// An inspector entry names the judge's verdict (and, for a block, its
// reason), and a judge call says what it is for.
func TestInspectorEntryJSON_Judge(t *testing.T) {
	e := inspector.Entry{ID: "e1", Judge: &inspector.JudgeMark{Direction: "request", Verdict: "block", Reason: "asks for malware"}}
	b, err := json.Marshal(inspectorEntryToJSON(e))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"judge":{"direction":"request","verdict":"block","reason":"asks for malware"}`) {
		t.Fatalf("entry %s", b)
	}
	b, _ = json.Marshal(inspectorEntryToJSON(inspector.Entry{ID: "e2", Purpose: "guardrail-judge"}))
	if !strings.Contains(string(b), `"purpose":"guardrail-judge"`) || strings.Contains(string(b), `"judge"`) {
		t.Fatalf("judge call entry %s", b)
	}
	b, _ = json.Marshal(inspectorEntryToJSON(inspector.Entry{ID: "e3"}))
	if strings.Contains(string(b), "judge") || strings.Contains(string(b), "purpose") {
		t.Fatalf("plain entry %s", b)
	}
}

// The model an enabled judge uses cannot be switched off or deleted.
func TestAIModel_GuardrailJudgeModelIsProtected(t *testing.T) {
	saved := `{"enabled":true,"action":"refuse_403","answers":{"enabled":false,"action":"flag"},"custom_patterns":[],"judge":{"enabled":%s,"model":"judge-model","check":"requests","timeout_ms":3000,"on_failure":"allow"}}`
	for name, tc := range map[string]struct {
		judgeEnabled string
		wantBlocked  bool
	}{"judge on": {"true", true}, "judge off": {"false", false}} {
		t.Run(name, func(t *testing.T) {
			ss := &fakeCacheSettingsStore{saved: map[string]string{"guardrails.global": strings.Replace(saved, "%s", tc.judgeEnabled, 1)}}
			models := &fakeModelStore{rows: []db.AIModel{{Name: "judge-model", Enabled: true}, {Name: "other", Enabled: true}}}
			srv := httptest.NewServer(NewRouter(Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Settings: ss, AIModels: models}))
			t.Cleanup(srv.Close)
			c := authedClient(t, srv)
			target := map[string]any{"provider": "p", "model": "m", "dialect": "openai"}
			body := func(en bool) map[string]any {
				return map[string]any{"name": "judge-model", "enabled": en, "targets": []any{target}}
			}
			r := c.put(t, "/api/v1/ai/models/judge-model", body(false))
			msg := readBody(t, r)
			if tc.wantBlocked {
				if r.StatusCode != http.StatusBadRequest || !strings.Contains(msg, `model \"judge-model\" is the guardrail judge; change the judge first`) {
					t.Fatalf("disable: %d %s", r.StatusCode, msg)
				}
				if !models.rows[0].Enabled {
					t.Fatal("the model was disabled")
				}
			} else if r.StatusCode != http.StatusOK {
				t.Fatalf("disable: %d %s", r.StatusCode, msg)
			}
			// Enabling it, or changing another model, is fine.
			if r = c.put(t, "/api/v1/ai/models/other", map[string]any{"name": "other", "enabled": false, "targets": []any{target}}); r.StatusCode != http.StatusOK {
				t.Fatalf("other model: %d %s", r.StatusCode, readBody(t, r))
			}
			r = c.delete(t, "/api/v1/ai/models/judge-model")
			msg = readBody(t, r)
			if tc.wantBlocked != (r.StatusCode == http.StatusBadRequest) || (tc.wantBlocked && !strings.Contains(msg, "is the guardrail judge")) {
				t.Fatalf("delete: %d %s", r.StatusCode, msg)
			}
		})
	}
}
