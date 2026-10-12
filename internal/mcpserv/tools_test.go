package mcpserv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// Several model APIs accept only letters, digits and "_" in a tool name, up
// to 64 characters. A dot in a name breaks them.
func TestToolNames_SnakeCase(t *testing.T) {
	re := regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	tools := buildTools(&fakeStore{}, toolEnv{})
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
	tools := buildTools(&fakeStore{}, toolEnv{})
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
	tools := buildTools(&fakeStore{}, toolEnv{})
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
	tools := buildTools(fs, toolEnv{}) // every list of the fake is empty
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

// ---------- page sizes ------------------------------------------------------

// A limit of zero or below is the default and one above the cap is the cap.
// The query under audit_search reads a limit of zero or below as "no limit":
// unclamped, {"limit": -1} returned the whole audit log in one answer.
func TestAuditSearch_LimitIsBounded(t *testing.T) {
	fs := &fakeStore{}
	for i := 0; i < 1500; i++ {
		fs.audit = append(fs.audit, db.AuditEvent{ID: fmt.Sprintf("%04d", i), Action: "user.create"})
	}
	call := buildTools(fs, toolEnv{})["audit_search"].Call
	for _, c := range []struct {
		args string
		want int
	}{
		{`{}`, 100},
		{`{"limit":0}`, 100},
		{`{"limit":-1}`, 100},
		{`{"limit":-9223372036854775808}`, 100},
		{`{"limit":1}`, 1},
		{`{"limit":1000}`, 1000},
		{`{"limit":1001}`, 1000},
		{`{"limit":9223372036854775807}`, 1000},
	} {
		raw, err := call(withCaller(context.Background(), "u1", "admin"), json.RawMessage(c.args))
		if err != nil {
			t.Fatalf("%s: %v", c.args, err)
		}
		var out struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if fs.lastAuditQ.Limit != c.want || len(out.Events) != c.want {
			t.Errorf("%s: the store was asked for %d and %d events came back, want %d",
				c.args, fs.lastAuditQ.Limit, len(out.Events), c.want)
		}
	}
}

// users_list: the same rule for the limit, and a negative offset is the
// start, as GET /api/v1/users reads them.
func TestUsersList_LimitAndOffsetAreBounded(t *testing.T) {
	fs := &fakeStore{}
	call := buildTools(fs, toolEnv{})["users_list"].Call
	for _, c := range []struct {
		args string
		want [2]int // limit, offset
	}{
		{`{}`, [2]int{50, 0}},
		{`{"limit":0}`, [2]int{50, 0}},
		{`{"limit":-1}`, [2]int{50, 0}},
		{`{"limit":-1,"offset":-1}`, [2]int{50, 0}},
		{`{"offset":-9223372036854775808}`, [2]int{50, 0}},
		{`{"limit":200,"offset":40}`, [2]int{200, 40}},
		{`{"limit":201}`, [2]int{200, 0}},
		{`{"limit":1000}`, [2]int{200, 0}},
	} {
		if _, err := call(withCaller(context.Background(), "u1", "admin"), json.RawMessage(c.args)); err != nil {
			t.Fatalf("%s: %v", c.args, err)
		}
		if fs.lastUsersPage != c.want {
			t.Errorf("%s: the store was asked for limit %d offset %d, want %d and %d",
				c.args, fs.lastUsersPage[0], fs.lastUsersPage[1], c.want[0], c.want[1])
		}
	}
}

// ---------- what a description and a schema promise -------------------------

// The limits a tool advertises are the limits it applies.
func TestToolSchemas_AdvertiseTheAppliedLimits(t *testing.T) {
	tools := buildTools(&fakeStore{}, toolEnv{})
	for name, limit := range map[string][2]int{ // default, cap
		"audit_search": {auditSearchDefaultLimit, auditSearchMaxLimit},
		"users_list":   {usersListDefaultLimit, usersListMaxLimit},
	} {
		var schema struct {
			Properties struct {
				Limit struct {
					Minimum int `json:"minimum"`
					Maximum int `json:"maximum"`
				} `json:"limit"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tools[name].ParamsSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Properties.Limit.Minimum != 1 || schema.Properties.Limit.Maximum != limit[1] {
			t.Errorf("%s: schema says limit %d..%d, the tool applies 1..%d",
				name, schema.Properties.Limit.Minimum, schema.Properties.Limit.Maximum, limit[1])
		}
		d := tools[name].Description
		if !strings.Contains(d, fmt.Sprintf("1..%d", limit[1])) || !strings.Contains(d, fmt.Sprintf("default %d", limit[0])) {
			t.Errorf("%s: description does not name the limit 1..%d and the default %d: %q", name, limit[1], limit[0], d)
		}
	}
	if usersListMaxLimit != 200 || auditSearchMaxLimit != 1000 {
		t.Errorf("caps: users_list %d (db.ListUsersPage caps at 200), audit_search %d (REST caps at 1000)",
			usersListMaxLimit, auditSearchMaxLimit)
	}
}

// tunnels_list lists the tunnels of the token's user, as GET /api/v1/tunnels
// does, and asks for the permission that says so.
func TestTunnelsList_AsksForWhatItLists(t *testing.T) {
	tl := buildTools(&fakeStore{}, toolEnv{})["tunnels_list"]
	if tl.Permission != authz.PermTunnelsReadOwn {
		t.Errorf("permission %s, want %s", tl.Permission, authz.PermTunnelsReadOwn)
	}
	if !strings.Contains(tl.Description, "own") || strings.Contains(tl.Description, "all live tunnels") {
		t.Errorf("description promises more than the caller's own tunnels: %q", tl.Description)
	}
}

// ---------- services_set_access_mode: the rules of the REST route -----------

// What PUT /api/v1/services/{id}/access-mode refuses, the tool refuses: with
// a fixed text that does not quote the value, and before the store is asked.
func TestSetAccessMode_RefusesWhatTheRESTRouteRefuses(t *testing.T) {
	const badHeader = "api_key_header must be a valid HTTP header name (RFC 7230 token)"
	for _, c := range []struct {
		name string
		args map[string]any
		env  toolEnv
		want string
	}{
		{"header with a space", map[string]any{"mode": "api_key", "api_key_header": "X Api Key"}, toolEnv{}, badHeader},
		{"header with a colon", map[string]any{"mode": "api_key", "api_key_header": "Authorization:"}, toolEnv{}, badHeader},
		{"header with a value", map[string]any{"mode": "api_key", "api_key_header": "Authorization: Bearer"}, toolEnv{}, badHeader},
		{"header with CR LF", map[string]any{"mode": "api_key", "api_key_header": "X-Key\r\nInjected"}, toolEnv{}, badHeader},
		{"the reviewer's probe", map[string]any{"mode": "api_key", "api_key_header": "X Bad: header\r\nInjected"}, toolEnv{}, badHeader},
		{"header with a tab", map[string]any{"mode": "api_key", "api_key_header": "X-Api\tKey"}, toolEnv{}, badHeader},
		{"header with a NUL", map[string]any{"mode": "api_key", "api_key_header": "X-Key\x00"}, toolEnv{}, badHeader},
		{"header outside ASCII", map[string]any{"mode": "api_key", "api_key_header": "X-Föo"}, toolEnv{}, badHeader},
		{"burrow_login without an auth domain", map[string]any{"mode": "burrow_login"}, toolEnv{}, "burrow_login requires a configured auth_domain"},
		{"mtls", map[string]any{"mode": "mtls"}, toolEnv{authDomain: true}, "mtls access mode requires the REST surface (mtls_ca_pem upload)"},
	} {
		fs := &fakeStore{}
		c.args["id"] = "svc1"
		args, _ := json.Marshal(c.args)
		_, err := buildTools(fs, c.env)["services_set_access_mode"].Call(withCaller(context.Background(), "u1", "admin"), args)
		var ue userError
		if !errors.As(err, &ue) || string(ue) != c.want {
			t.Errorf("%s: err = %v, want the user error %q", c.name, err, c.want)
		}
		if fs.setModeCalls != 0 {
			t.Errorf("%s: the refused input reached the store", c.name)
		}
	}
}

// What the REST route accepts, the tool accepts and hands on unchanged.
func TestSetAccessMode_AcceptsWhatTheRESTRouteAccepts(t *testing.T) {
	for _, c := range []struct {
		name      string
		args      map[string]any
		env       toolEnv
		mode, hdr string
	}{
		{"a header name", map[string]any{"mode": "api_key", "api_key_header": "X-Api-Key"}, toolEnv{}, "api_key", "X-Api-Key"},
		{"every token character", map[string]any{"mode": "api_key", "api_key_header": "!#$%&'*+-.^_`|~Aa0"}, toolEnv{}, "api_key", "!#$%&'*+-.^_`|~Aa0"},
		// No header is never an error: the store then writes "Authorization".
		{"api_key without a header", map[string]any{"mode": "api_key"}, toolEnv{}, "api_key", ""},
		{"api_key with an empty header", map[string]any{"mode": "api_key", "api_key_header": ""}, toolEnv{}, "api_key", ""},
		// The header belongs to api_key mode; with another mode it is dropped
		// unread, as the REST route drops it.
		{"open with a bad header", map[string]any{"mode": "open", "api_key_header": "not a header"}, toolEnv{}, "open", ""},
		{"burrow_login with an auth domain", map[string]any{"mode": "burrow_login"}, toolEnv{authDomain: true}, "burrow_login", ""},
	} {
		fs := &fakeStore{}
		c.args["id"] = "svc1"
		args, _ := json.Marshal(c.args)
		if _, err := buildTools(fs, c.env)["services_set_access_mode"].Call(withCaller(context.Background(), "u1", "admin"), args); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if fs.setModeCalls != 1 || fs.lastSetMode != c.mode || fs.lastSetHeader != c.hdr {
			t.Errorf("%s: the store saw %d calls, mode %q header %q; want one, %q and %q",
				c.name, fs.setModeCalls, fs.lastSetMode, fs.lastSetHeader, c.mode, c.hdr)
		}
	}
}

// The option of New is what decides; without it burrow_login is refused.
func TestSetAccessMode_BurrowLoginFollowsTheAuthDomainOption(t *testing.T) {
	args := json.RawMessage(`{"id":"svc1","mode":"burrow_login"}`)
	ctx := withCaller(context.Background(), "u1", "admin")
	for domain, wantErr := range map[string]bool{"": true, "burrow.example.com": false} {
		fs := &fakeStore{}
		_, err := New(nil, nil, fs, nil, WithAuthDomain(domain)).tools["services_set_access_mode"].Call(ctx, args)
		if (err != nil) != wantErr {
			t.Errorf("auth domain %q: err = %v, want an error: %v", domain, err, wantErr)
		}
	}
	if _, err := New(nil, nil, &fakeStore{}, nil).tools["services_set_access_mode"].Call(ctx, args); err == nil {
		t.Error("without the option burrow_login is accepted")
	}
}
