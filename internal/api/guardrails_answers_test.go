package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/inspector"
)

func guardrailsAdmin(t *testing.T, ss *fakeCacheSettingsStore) *authClient {
	t.Helper()
	srv := httptest.NewServer(NewRouter(Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Settings: ss}))
	t.Cleanup(srv.Close)
	return authedClient(t, srv)
}

// GET serves the new fields with their defaults: answers off with action
// "flag", and an empty list of custom patterns (never null), also for a blob
// stored before the fields existed.
func TestGuardrailGetSettings_AnswerDefaults(t *testing.T) {
	for name, saved := range map[string]map[string]string{
		"no row":   {},
		"old blob": {"guardrails.global": `{"enabled":true,"action":"refuse_403"}`},
	} {
		c := guardrailsAdmin(t, &fakeCacheSettingsStore{saved: saved})
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
		if a := string(raw.Global["answers"]); a != `{"enabled":false,"action":"flag"}` {
			t.Errorf("%s: answers = %s", name, a)
		}
		if cp := string(raw.Global["custom_patterns"]); cp != `[]` {
			t.Errorf("%s: custom_patterns = %s", name, cp)
		}
	}
}

func TestGuardrailPutSettings_AnswersAndCustomPatterns(t *testing.T) {
	ss := &fakeCacheSettingsStore{}
	c := guardrailsAdmin(t, ss)
	r := c.put(t, "/api/v1/guardrails/settings", map[string]any{
		"enabled": true, "action": "refuse_403",
		"answers": map[string]any{"enabled": true, "action": "block"},
		"custom_patterns": []map[string]any{
			{"id": "ticket", "pattern": `TICKET-\d{4}`, "direction": "request", "description": "ticket numbers"},
		},
	})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	var s guardrails.Settings
	if err := json.Unmarshal([]byte(ss.saved["guardrails.global"]), &s); err != nil {
		t.Fatal(err)
	}
	if !s.Answers.Enabled || s.Answers.Action != "block" || len(s.CustomPatterns) != 1 || s.CustomPatterns[0].Pattern != `TICKET-\d{4}` {
		t.Fatalf("persisted %+v", s)
	}
	// GET returns them, custom pattern sources included (they are the
	// operator's own).
	r = c.get(t, "/api/v1/guardrails/settings")
	var got guardrailSettingsResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if got.Global.Answers.Action != "block" || len(got.Global.CustomPatterns) != 1 || got.Global.CustomPatterns[0].ID != "ticket" {
		t.Fatalf("GET %+v", got.Global)
	}

	// An empty answer action is stored as "flag".
	r = c.put(t, "/api/v1/guardrails/settings", map[string]any{"enabled": true, "action": "log_only", "answers": map[string]any{"enabled": true}})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	if err := json.Unmarshal([]byte(ss.saved["guardrails.global"]), &s); err != nil {
		t.Fatal(err)
	}
	if s.Answers.Action != "flag" || s.CustomPatterns == nil {
		t.Fatalf("persisted %+v", s)
	}
}

func TestGuardrailPutSettings_RefusesBadAnswersAndPatterns(t *testing.T) {
	many := make([]map[string]any, 51)
	for i := range many {
		many[i] = map[string]any{"id": fmt.Sprintf("p%d", i), "pattern": "x", "direction": "both"}
	}
	cases := []struct {
		name string
		body map[string]any
		want []string
	}{
		{"answer action", map[string]any{"action": "log_only", "answers": map[string]any{"enabled": true, "action": "refuse_403"}}, []string{"answers.action"}},
		{"51 patterns", map[string]any{"action": "log_only", "custom_patterns": many}, []string{"50"}},
		{"long pattern", map[string]any{"action": "log_only", "custom_patterns": []map[string]any{{"id": "big", "pattern": strings.Repeat("a", 513), "direction": "both"}}}, []string{`"big"`, "512"}},
		{"invalid RE2", map[string]any{"action": "log_only", "custom_patterns": []map[string]any{{"id": "look", "pattern": `(?<=x)`, "direction": "both"}}}, []string{`"look"`, "regular expression"}},
		{"duplicate id", map[string]any{"action": "log_only", "custom_patterns": []map[string]any{{"id": "a", "pattern": "x", "direction": "both"}, {"id": "a", "pattern": "y", "direction": "both"}}}, []string{`"a"`}},
		{"bundled id", map[string]any{"action": "log_only", "custom_patterns": []map[string]any{{"id": "github_token", "pattern": "x", "direction": "both"}}}, []string{`"github_token"`, "bundled"}},
		{"bad direction", map[string]any{"action": "log_only", "custom_patterns": []map[string]any{{"id": "d", "pattern": "x", "direction": "up"}}}, []string{`"d"`, "direction"}},
	}
	for _, tc := range cases {
		ss := &fakeCacheSettingsStore{}
		c := guardrailsAdmin(t, ss)
		r := c.put(t, "/api/v1/guardrails/settings", tc.body)
		body := readBody(t, r)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d %s", tc.name, r.StatusCode, body)
			continue
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &e)
		for _, w := range tc.want {
			if !strings.Contains(e.Error, w) {
				t.Errorf("%s: %q does not name %q", tc.name, e.Error, w)
			}
		}
		if _, ok := ss.saved["guardrails.global"]; ok {
			t.Errorf("%s: saved despite 400", tc.name)
		}
	}
}

// Fifty patterns of the longest length fit the request body limit.
func TestGuardrailPutSettings_FullListFits(t *testing.T) {
	ps := make([]map[string]any, guardrails.MaxCustomPatterns)
	for i := range ps {
		ps[i] = map[string]any{"id": fmt.Sprintf("p%02d", i), "pattern": strings.Repeat(`\d`, guardrails.MaxCustomPatternLen/2), "direction": "both", "description": strings.Repeat("é", 200)}
	}
	ss := &fakeCacheSettingsStore{}
	c := guardrailsAdmin(t, ss)
	r := c.put(t, "/api/v1/guardrails/settings", map[string]any{"action": "log_only", "custom_patterns": ps})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
}

// An inspector entry names the guardrail pattern that matched it, and an
// entry without one has no "guardrail" field.
func TestInspectorEntryJSON_GuardrailMark(t *testing.T) {
	e := inspector.Entry{ID: "e1", Guardrail: &inspector.GuardrailMark{Pattern: "openai_key", Direction: "answer", Action: "block", MidStream: true}}
	b, err := json.Marshal(inspectorEntryToJSON(e))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"guardrail":{"pattern":"openai_key","direction":"answer","action":"block","mid_stream":true}`) {
		t.Fatalf("entry %s", b)
	}
	b, _ = json.Marshal(inspectorEntryToJSON(inspector.Entry{ID: "e2"}))
	if strings.Contains(string(b), "guardrail") {
		t.Fatalf("entry without a hit: %s", b)
	}
}

// A PUT that leaves out answers or custom_patterns (a client from before
// they existed) keeps what is stored for them; one that sends them replaces
// them.
func TestGuardrailPutSettings_KeepsFieldsLeftOut(t *testing.T) {
	ss := &fakeCacheSettingsStore{saved: map[string]string{
		"guardrails.global": `{"enabled":true,"action":"refuse_403","answers":{"enabled":true,"action":"block"},"custom_patterns":[{"id":"ticket","pattern":"T-\\d+","direction":"both","description":""}]}`,
	}}
	c := guardrailsAdmin(t, ss)
	r := c.put(t, "/api/v1/guardrails/settings", map[string]any{"enabled": false, "action": "log_only"})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	var s guardrails.Settings
	if err := json.Unmarshal([]byte(ss.saved["guardrails.global"]), &s); err != nil {
		t.Fatal(err)
	}
	if s.Enabled || s.Action != "log_only" || !s.Answers.Enabled || s.Answers.Action != "block" || len(s.CustomPatterns) != 1 {
		t.Fatalf("fields left out were not kept: %+v", s)
	}
	r = c.put(t, "/api/v1/guardrails/settings", map[string]any{"action": "log_only", "answers": map[string]any{"enabled": false}, "custom_patterns": []any{}})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	s = guardrails.Settings{}
	if err := json.Unmarshal([]byte(ss.saved["guardrails.global"]), &s); err != nil {
		t.Fatal(err)
	}
	if s.Answers.Enabled || s.Answers.Action != "flag" || len(s.CustomPatterns) != 0 || s.CustomPatterns == nil {
		t.Fatalf("fields sent were not replaced: %+v", s)
	}
}
