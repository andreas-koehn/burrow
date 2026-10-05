package aigateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ankoehn/burrow/internal/db"
)

type fakeAliases map[string][]db.ModelAlias

func (f fakeAliases) GetAliasesByPriority(_ context.Context, alias string) ([]db.ModelAlias, error) {
	return f[alias], nil
}

func bodyModel(t *testing.T, body io.Reader) (string, map[string]json.RawMessage) {
	t.Helper()
	raw, _ := io.ReadAll(body)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("body is not JSON: %s", raw)
	}
	var model string
	_ = json.Unmarshal(m["model"], &model)
	return model, m
}

func TestRewriteModelAlias(t *testing.T) {
	aliases := fakeAliases{
		"fast": {
			{Alias: "fast", ConcreteModel: "other:1b", ServiceID: "svc-other", Priority: 0},
			{Alias: "fast", ConcreteModel: "qwen2.5:0.5b", ServiceID: "svc1", Priority: 1},
		},
	}

	t.Run("rewrites an alias of this service and keeps other fields", func(t *testing.T) {
		in := `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi <b>"}]}`
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(in))
		r.Header.Set("Content-Type", "application/json")
		if err := rewriteModelAlias(r, "svc1", aliases); err != nil {
			t.Fatal(err)
		}
		model, m := bodyModel(t, r.Body)
		if model != "qwen2.5:0.5b" {
			t.Fatalf("model = %q", model)
		}
		if string(m["stream"]) != "true" || !strings.Contains(string(m["messages"]), `hi <b>`) {
			t.Fatalf("other fields changed: %v", m)
		}
	})

	t.Run("content length matches the new body", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fast"}`))
		r.Header.Set("Content-Type", "application/json")
		_ = rewriteModelAlias(r, "svc1", aliases)
		raw, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(raw)) {
			t.Fatalf("ContentLength %d, body %d bytes", r.ContentLength, len(raw))
		}
	})

	unchanged := map[string]struct{ method, ct, body string }{
		"unknown model":            {"POST", "application/json", `{"model":"llama3"}`},
		"alias of another service": {"POST", "application/json", `{"model":"slow"}`},
		"not JSON":                 {"POST", "application/json", `not json`},
		"JSON array":               {"POST", "application/json", `["fast"]`},
		"model is not a string":    {"POST", "application/json", `{"model":7}`},
		"multipart upload":         {"POST", "multipart/form-data; boundary=x", `--x\r\nmodel=fast`},
		"GET request":              {"GET", "", ``},
	}
	for name, c := range unchanged {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "/v1/chat/completions", strings.NewReader(c.body))
			if c.ct != "" {
				r.Header.Set("Content-Type", c.ct)
			}
			if err := rewriteModelAlias(r, "svc1", aliases); err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(r.Body)
			if string(raw) != c.body {
				t.Fatalf("body changed: %q -> %q", c.body, raw)
			}
		})
	}

	t.Run("content type is matched without regard to case", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fast"}`))
		r.Header.Set("Content-Type", "Application/JSON; charset=utf-8")
		if err := rewriteModelAlias(r, "svc1", aliases); err != nil {
			t.Fatal(err)
		}
		if model, _ := bodyModel(t, r.Body); model != "qwen2.5:0.5b" {
			t.Fatalf("model = %q", model)
		}
	})

	// The upstream must see what the client sent up to the failure, and the
	// failure itself: not a body that silently starts in the middle.
	t.Run("read error keeps the bytes already read", func(t *testing.T) {
		broken := errors.New("client went away")
		r := httptest.NewRequest("POST", "/v1/chat/completions",
			io.MultiReader(strings.NewReader(`{"model":"fa`), iotest.ErrReader(broken)))
		r.Header.Set("Content-Type", "application/json")
		if err := rewriteModelAlias(r, "svc1", aliases); !errors.Is(err, broken) {
			t.Fatalf("err = %v, want the read error", err)
		}
		raw, err := io.ReadAll(r.Body)
		if string(raw) != `{"model":"fa` || !errors.Is(err, broken) {
			t.Fatalf("forwarded body = %q, err = %v", raw, err)
		}
	})

	t.Run("oversized body passes through untouched", func(t *testing.T) {
		big := `{"model":"fast","pad":"` + strings.Repeat("x", maxAliasBody) + `"}`
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big))
		r.Header.Set("Content-Type", "application/json")
		if err := rewriteModelAlias(r, "svc1", aliases); err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != big {
			t.Fatal("oversized body was altered or truncated")
		}
	})
}
