package aigateway

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/db"
)

type mediaCase struct {
	path      string
	multipart bool
}

var mediaCases = []mediaCase{
	{"/v1/images/generations", false},
	{"/v1/audio/speech", false},
	{"/v1/audio/transcriptions", true},
	{"/v1/audio/translations", true},
	{"/v1/rerank", false},
}

// mediaRequest builds the request of one media path for model.
func mediaRequest(c mediaCase, key, model string) *http.Request {
	if c.multipart {
		body := buildForm(
			formPart{name: "file", filename: "a.wav", content: trickyAudio()},
			formPart{name: "model", content: []byte(model)},
		)
		r := httptest.NewRequest("POST", c.path, bytes.NewReader(body))
		r.Header.Set("Content-Type", formType)
		r.Header.Set("Authorization", "Bearer "+key)
		return r
	}
	return post(c.path, key, `{"model":"`+model+`","prompt":"a cat","input":"hi","query":"q","documents":["a"]}`)
}

type upstreamSeen struct {
	path, model, ct string
	body            []byte
	n               int
}

func mediaUpstream(seen *upstreamSeen) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.path, seen.body, seen.ct = r.URL.Path, b, r.Header.Get("Content-Type")
		seen.n++
		w.Header().Set("Content-Type", "application/json")
		answer := `{"ok":true}`
		w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
		_, _ = w.Write([]byte(answer))
	})
}

func TestServeDialect_MediaPaths_SyntheticModel(t *testing.T) {
	for _, c := range mediaCases {
		t.Run(c.path, func(t *testing.T) {
			var seen upstreamSeen
			g := globalGateway(mediaUpstream(&seen), &spyChain{})
			rec := httptest.NewRecorder()
			g.ServeDialect(rec, mediaRequest(c, "bgw_all", "burrow-simple"), DialectOpenAI)
			if rec.Code != 200 {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			if seen.path != c.path {
				t.Fatalf("upstream path = %s", seen.path)
			}
			if c.multipart {
				if !strings.HasPrefix(seen.ct, "multipart/form-data") {
					t.Fatalf("upstream content type = %q", seen.ct)
				}
				want := bytes.Replace(buildForm(formPart{name: "file", filename: "a.wav", content: trickyAudio()}, formPart{name: "model", content: []byte("burrow-simple")}),
					[]byte("burrow-simple"), []byte("mistral"), 1)
				if !bytes.Equal(seen.body, want) {
					t.Fatalf("upstream form differs from the client's except for the model:\n%q", seen.body)
				}
			} else if !strings.Contains(string(seen.body), `"model":"mistral"`) {
				t.Fatalf("upstream body = %s", seen.body)
			}
			if rec.Header().Get("Burrow-Model") != "mistral" {
				t.Fatalf("Burrow-Model = %q", rec.Header().Get("Burrow-Model"))
			}
		})
	}
}

func TestServeDialect_MediaPaths_DirectAddress(t *testing.T) {
	for _, c := range mediaCases {
		var seen upstreamSeen
		g := globalGateway(mediaUpstream(&seen), &spyChain{})
		rec := httptest.NewRecorder()
		g.ServeDialect(rec, mediaRequest(c, "bgw_all", "openrouter/some/model"), DialectOpenAI)
		if rec.Code != 200 || seen.path != c.path || !bytes.Contains(seen.body, []byte("some/model")) || bytes.Contains(seen.body, []byte("openrouter/some")) {
			t.Fatalf("%s: status %d path %s body %q", c.path, rec.Code, seen.path, seen.body)
		}
	}
}

func TestServe_MediaPaths_ProviderPath(t *testing.T) {
	for _, c := range mediaCases {
		var seen upstreamSeen
		g := newGateway(mediaUpstream(&seen), &spyChain{})
		r := mediaRequest(c, "sk-good", "mistral")
		rec := httptest.NewRecorder()
		g.Serve(rec, r, "ollama")
		if rec.Code != 200 || seen.path != c.path {
			t.Fatalf("%s: status %d path %q body %s", c.path, rec.Code, seen.path, rec.Body.String())
		}
	}
}

func TestServeDialect_MediaPaths_NoTargetForPath(t *testing.T) {
	for _, translate := range []bool{false, true} {
		for _, c := range mediaCases {
			var seen upstreamSeen
			g := globalGateway(mediaUpstream(&seen), &spyChain{})
			g.Synthetic.(fakeSynthetic)["claude-only"] = db.AIModel{Name: "claude-only", Enabled: true, Translate: translate, Targets: []db.AIModelTarget{
				{Dialect: "anthropic", ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"},
			}}
			rec := httptest.NewRecorder()
			g.ServeDialect(rec, mediaRequest(c, "bgw_all", "claude-only"), DialectOpenAI)
			want := `model \"claude-only\" has no target for ` + c.path
			if rec.Code != 400 || rec.Header().Get("Burrow-Error-Code") != "model_has_no_target_for_path" || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("%s translate=%v: status %d code %q body %s", c.path, translate, rec.Code, rec.Header().Get("Burrow-Error-Code"), rec.Body.String())
			}
			if seen.n != 0 {
				t.Fatalf("%s: the upstream was called", c.path)
			}
		}
	}
}

func TestServeDialect_MediaPaths_AllowListAndErrors(t *testing.T) {
	for _, c := range mediaCases {
		var seen upstreamSeen
		g := globalGateway(mediaUpstream(&seen), &spyChain{})
		rec := httptest.NewRecorder()
		g.ServeDialect(rec, mediaRequest(c, "bgw_some", "openrouter/x"), DialectOpenAI)
		if rec.Code != 403 || rec.Header().Get("Burrow-Error-Code") != "model_not_allowed" || seen.n != 0 {
			t.Fatalf("%s: allow-list: status %d code %q", c.path, rec.Code, rec.Header().Get("Burrow-Error-Code"))
		}
		rec = httptest.NewRecorder()
		g.ServeDialect(rec, mediaRequest(c, "bgw_some", "ollama/mistral"), DialectOpenAI)
		if rec.Code != 200 {
			t.Fatalf("%s: allowed model: status %d", c.path, rec.Code)
		}
		rec = httptest.NewRecorder()
		g.ServeDialect(rec, mediaRequest(c, "", "burrow-simple"), DialectOpenAI)
		if rec.Code != 401 {
			t.Fatalf("%s: no key: status %d", c.path, rec.Code)
		}
	}
}

func TestServeDialect_Multipart_Refusals(t *testing.T) {
	var seen upstreamSeen
	g := globalGateway(mediaUpstream(&seen), &spyChain{})
	g.MaxBody = 4096
	for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations"} {
		do := func(body []byte, ct string) *httptest.ResponseRecorder {
			r := httptest.NewRequest("POST", path, bytes.NewReader(body))
			r.Header.Set("Content-Type", ct)
			r.Header.Set("Authorization", "Bearer bgw_all")
			rec := httptest.NewRecorder()
			g.ServeDialect(rec, r, DialectOpenAI)
			return rec
		}
		file := formPart{name: "file", filename: "a.wav", content: []byte("x")}
		for name, tc := range map[string]struct {
			rec          *httptest.ResponseRecorder
			status       int
			code, phrase string
		}{
			"no model":       {do(buildForm(file), formType), 400, "model_required", ""},
			"json body":      {do([]byte(`{"model":"burrow-simple"}`), "application/json"), 400, "invalid_request", "this endpoint takes multipart/form-data"},
			"no boundary":    {do(buildForm(file), "multipart/form-data"), 400, "invalid_request", "this endpoint takes multipart/form-data"},
			"over the limit": {do(buildForm(formPart{name: "model", content: []byte("burrow-simple")}, formPart{name: "file", filename: "a", content: bytes.Repeat([]byte("a"), 5000)}), formType), 413, "request_too_large", ""},
		} {
			if tc.rec.Code != tc.status || tc.rec.Header().Get("Burrow-Error-Code") != tc.code || !strings.Contains(tc.rec.Body.String(), tc.phrase) {
				t.Errorf("%s %s: status %d code %q body %s", path, name, tc.rec.Code, tc.rec.Header().Get("Burrow-Error-Code"), tc.rec.Body.String())
			}
		}
	}
	if seen.n != 0 {
		t.Fatalf("a refused request reached the upstream %d times", seen.n)
	}
}

func TestServeDialect_MediaPaths_NeverCached(t *testing.T) {
	raw, err := db.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	d := db.Wrap(raw)
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var seen upstreamSeen
	sink, refuse := &recSink{}, false
	chain := aigw.NewChain(exact.New(d, log), nil, nil, nil, nil, nil, nil, sink, log)
	chain.Loader = cfgLoader{Cache: &exact.Settings{Enabled: true, AppliesPer: "global", TTLSeconds: 300, MaxEntries: 100, MaxPerEntryKB: 64}}
	chain.RateLimit = limiter(&refuse)
	g := globalGateway(mediaUpstream(&seen), chain)
	for _, c := range mediaCases {
		before := seen.n
		for i := 0; i < 2; i++ {
			rec := httptest.NewRecorder()
			g.ServeDialect(rec, mediaRequest(c, "bgw_all", "burrow-simple"), DialectOpenAI)
			if rec.Code != 200 || rec.Header().Get("Burrow-Cache") != "" {
				t.Fatalf("%s #%d: status %d cache %q", c.path, i, rec.Code, rec.Header().Get("Burrow-Cache"))
			}
		}
		if seen.n-before != 2 {
			t.Fatalf("%s: upstream called %d times for 2 identical requests", c.path, seen.n-before)
		}
	}
	// Premise: chat is cached under the same settings.
	for i, want := range []string{"", "HIT"} {
		rec := httptest.NewRecorder()
		g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", `{"model":"burrow-simple","messages":[]}`), DialectOpenAI)
		if rec.Header().Get("Burrow-Cache") != want {
			t.Fatalf("chat #%d: cache %q, want %q", i, rec.Header().Get("Burrow-Cache"), want)
		}
	}
	if got := len(sink.all()); got != len(mediaCases)*2+2 {
		t.Fatalf("usage rows = %d", got)
	}
}
