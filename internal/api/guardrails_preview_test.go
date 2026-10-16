package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type gpPreviewResult struct {
	Matches []struct {
		Start int    `json:"start"`
		End   int    `json:"end"`
		Text  string `json:"text"`
	} `json:"matches"`
	Truncated bool `json:"truncated"`
}

func previewCall(t *testing.T, role string, body any) (*http.Response, gpPreviewResult) {
	t.Helper()
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: role}, Settings: &fakeCacheSettingsStore{}}
	srv := httptest.NewServer(NewRouter(d))
	t.Cleanup(srv.Close)
	r := authedClient(t, srv).post(t, "/api/v1/guardrails/preview", body)
	var out gpPreviewResult
	if r.StatusCode == http.StatusOK {
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return r, out
}

func TestGuardrailPreview_Match(t *testing.T) {
	r, out := previewCall(t, "admin", map[string]any{"pattern": `project\s+x`, "text": "a Project x and project  x"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", r.StatusCode)
	}
	if len(out.Matches) != 1 || out.Matches[0].Start != 16 || out.Matches[0].End != 26 || out.Matches[0].Text != "project  x" || out.Truncated {
		t.Fatalf("got %+v", out)
	}
}

func TestGuardrailPreview_NoMatchAndEmptyText(t *testing.T) {
	for _, text := range []string{"nothing here", ""} {
		r, out := previewCall(t, "admin", map[string]any{"pattern": "abc", "text": text})
		if r.StatusCode != http.StatusOK || out.Matches == nil || len(out.Matches) != 0 || out.Truncated {
			t.Fatalf("text %q: status=%d out=%+v", text, r.StatusCode, out)
		}
	}
}

func TestGuardrailPreview_RejectsBadPatterns(t *testing.T) {
	for name, pat := range map[string]string{
		"invalid regex": "(",
		"matches empty": "a*",
		"too long":      strings.Repeat("a", 513),
		"empty":         "",
	} {
		r, _ := previewCall(t, "admin", map[string]any{"pattern": pat, "text": "aaa"})
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status=%d want 400", name, r.StatusCode)
		}
	}
	r, _ := previewCall(t, "admin", map[string]any{"pattern": "(", "text": "x"})
	var e struct{ Error string }
	_ = json.NewDecoder(r.Body).Decode(&e)
	if !strings.Contains(e.Error, "not a valid regular expression") {
		t.Errorf("message = %q", e.Error)
	}
}

func TestGuardrailPreview_RequiresAIConfigureAny(t *testing.T) {
	r, _ := previewCall(t, "user", map[string]any{"pattern": "abc", "text": "abc"})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", r.StatusCode)
	}
}

func TestGuardrailPreview_Truncates(t *testing.T) {
	r, out := previewCall(t, "admin", map[string]any{"pattern": "a", "text": strings.Repeat("a", 150)})
	if r.StatusCode != http.StatusOK || len(out.Matches) != 100 || !out.Truncated {
		t.Fatalf("status=%d n=%d truncated=%v", r.StatusCode, len(out.Matches), out.Truncated)
	}
	// Exactly 100 is not truncated.
	_, out = previewCall(t, "admin", map[string]any{"pattern": "a", "text": strings.Repeat("a", 100)})
	if len(out.Matches) != 100 || out.Truncated {
		t.Fatalf("n=%d truncated=%v", len(out.Matches), out.Truncated)
	}
}

func TestGuardrailPreview_BodyCap(t *testing.T) {
	r, _ := previewCall(t, "admin", map[string]any{"pattern": "a", "text": strings.Repeat("b", 65<<10)})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", r.StatusCode)
	}
}
