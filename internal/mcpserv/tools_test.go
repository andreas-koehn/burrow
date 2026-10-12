package mcpserv

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/store"
)

// Several model APIs accept only letters, digits and "_" in a tool name, up
// to 64 characters. A dot in a name breaks them.
func TestToolNames_SnakeCase(t *testing.T) {
	re := regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	tools := buildTools(&fakeStore{})
	if len(tools) != len(orderedTools) || len(orderedTools) != 12 {
		t.Fatalf("registry has %d tools, orderedTools %d, want 12", len(tools), len(orderedTools))
	}
	for _, name := range orderedTools {
		if !re.MatchString(name) {
			t.Errorf("tool name %q does not match %s", name, re)
		}
		if tools[name].Name != name {
			t.Errorf("orderedTools names %q, the registry has no such tool", name)
		}
	}
}

func TestToolAnnotations(t *testing.T) {
	type hints struct{ readOnly, destructive, idempotent bool }
	want := map[string]hints{
		"tunnels_list":             {readOnly: true},
		"services_list":            {readOnly: true},
		"services_get":             {readOnly: true},
		"services_set_access_mode": {idempotent: true},
		"api_keys_list":            {readOnly: true},
		"api_keys_create":          {},
		"api_keys_delete":          {destructive: true},
		"tokens_list":              {readOnly: true},
		"tokens_mint":              {},
		"users_list":               {readOnly: true},
		"audit_search":             {readOnly: true},
		"metrics_snapshot":         {readOnly: true},
	}
	tools := buildTools(&fakeStore{})
	if len(want) != len(tools) {
		t.Fatalf("table covers %d tools, registry has %d", len(want), len(tools))
	}
	for name, w := range want {
		tl, ok := tools[name]
		if !ok {
			t.Errorf("no tool %q", name)
			continue
		}
		if got := (hints{tl.ReadOnly, tl.Destructive, tl.Idempotent}); got != w {
			t.Errorf("%s: hints %+v, want %+v", name, got, w)
		}
		if tl.ReadOnly && (tl.Destructive || tl.Idempotent) {
			t.Errorf("%s: a read-only tool carries no other hint", name)
		}
	}
}

// A tool that hands out a secret says so, so that a client can warn before
// the value lands in a transcript.
func TestToolDescriptions_SecretsAreAnnounced(t *testing.T) {
	tools := buildTools(&fakeStore{})
	for _, name := range []string{"api_keys_create", "tokens_mint"} {
		d := tools[name].Description
		if !strings.Contains(d, "Returns a secret") || !strings.Contains(d, "exactly once") {
			t.Errorf("%s: description does not announce the secret: %q", name, d)
		}
	}
	for name, tl := range tools {
		if name != "api_keys_create" && name != "tokens_mint" && strings.Contains(tl.Description, "Returns a secret") {
			t.Errorf("%s announces a secret it does not return", name)
		}
	}
}

// MCP's structuredContent is a JSON object in the revisions before
// 2026-07-28, so no tool may answer with a bare array, null or a scalar —
// with rows or without.
func TestToolResults_AreObjects(t *testing.T) {
	args := map[string]string{
		"tunnels_list":             `{}`,
		"services_list":            `{}`,
		"services_get":             `{"id":"svc1"}`,
		"services_set_access_mode": `{"id":"svc1","mode":"open"}`,
		"api_keys_list":            `{"service_id":"svc1"}`,
		"api_keys_create":          `{"service_id":"svc1","name":"ci"}`,
		"api_keys_delete":          `{"service_id":"svc1","id":"k1"}`,
		"tokens_list":              `{}`,
		"tokens_mint":              `{"name":"ci"}`,
		"users_list":               `{}`,
		"audit_search":             `{}`,
		"metrics_snapshot":         `{}`,
	}
	listKey := map[string]string{
		"tunnels_list": "tunnels", "services_list": "services", "api_keys_list": "api_keys",
		"tokens_list": "tokens", "audit_search": "events", "users_list": "users",
	}
	fs := &fakeStore{serviceDetail: store.ServiceDetail{ServiceView: store.ServiceView{ID: "svc1"}}}
	tools := buildTools(fs) // every list of the fake is empty
	for _, name := range orderedTools {
		raw, err := tools[name].Call(withCaller(context.Background(), "u1", "admin"), json.RawMessage(args[name]))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			t.Errorf("%s: result is not a JSON object: %s", name, raw)
			continue
		}
		if key, isList := listKey[name]; isList && string(obj[key]) != "[]" {
			t.Errorf("%s: empty list under %q = %s, want []", name, key, obj[key])
		}
	}
}

// Tools() feeds GET /api/v1/mcp/tools; its order is the declaration order,
// the same on every start.
func TestServerTools_DeclarationOrder(t *testing.T) {
	for i := 0; i < 20; i++ { // a map iteration would differ within a few runs
		descs := New(nil, nil, &fakeStore{}, nil).Tools()
		if len(descs) != len(orderedTools) {
			t.Fatalf("Tools() returned %d, want %d", len(descs), len(orderedTools))
		}
		for j, d := range descs {
			if d.Name != orderedTools[j] {
				t.Fatalf("Tools()[%d] = %q, want %q", j, d.Name, orderedTools[j])
			}
			if d.Description == "" || d.Permission == "" || len(d.ParametersSchema) == 0 {
				t.Fatalf("Tools()[%d] is incomplete: %+v", j, d)
			}
		}
	}
}
