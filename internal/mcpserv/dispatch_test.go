package mcpserv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
)

// fakeAPI records what reaches the router and answers with a canned reply.
type fakeAPI struct {
	mu     sync.Mutex
	seen   []*http.Request
	bodies []string
	status int
	reply  string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, r)
	f.bodies = append(f.bodies, string(b))
	status, reply := f.status, f.reply
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply)
}

func (f *fakeAPI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func presetPerms(name string) []string {
	for _, p := range authz.Presets() {
		if p.Name == name {
			out := make([]string, len(p.Permissions))
			for i, perm := range p.Permissions {
				out[i] = string(perm)
			}
			return out
		}
	}
	panic(name)
}

// newDispatchServer serves the real spec for a token with perms of an admin
// user, against api.
func newDispatchServer(t *testing.T, api http.Handler, perms []string) (*httptest.Server, *auditSink) {
	t.Helper()
	spec, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(nil, nil, spec, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	s.SetAPIHandler(api)
	sink := &auditSink{}
	s.SetAuditAppender(sink)
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: perms,
		bearer: "Bearer bua_test", remoteAddr: "198.51.100.7:4242", userAgent: "claude-code/2.0", host: "relay.example.com"}
	h := s.mcpHandler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	}))
	t.Cleanup(hs.Close)
	return hs, sink
}

func TestCuratedToolsFollowTheToken(t *testing.T) {
	for _, rev := range testRevisions {
		t.Run(rev, func(t *testing.T) {
			hs, _ := newDispatchServer(t, &fakeAPI{}, presetPerms(authz.PresetReadOnly))
			tools := listedTools(t, connectMCP(t, hs.URL, "", rev))
			// services_list needs services:configure:*, which read-only does not hold
			// (Burrow has no read-only permission for services).
			for _, name := range []string{"known_clients_list", "providers_list", "me_get", "ai_requests_list"} {
				if tools[name] == nil {
					t.Errorf("read-only token does not see %s", name)
				}
			}
			for _, name := range []string{"services_list", "budget_create", "provider_delete", "ai_model_create"} {
				if tools[name] != nil {
					t.Errorf("read-only token sees %s", name)
				}
			}
			if a := tools["providers_list"].Annotations; a == nil || !a.ReadOnlyHint {
				t.Error("providers_list is not marked read-only")
			}
		})
	}
	hs, _ := newDispatchServer(t, &fakeAPI{}, presetPerms(authz.PresetOperator))
	tools := listedTools(t, connectMCP(t, hs.URL, "", testRevisions[0]))
	del := tools["provider_delete"]
	if del == nil || del.Annotations == nil || del.Annotations.DestructiveHint == nil || !*del.Annotations.DestructiveHint {
		t.Fatalf("operator: provider_delete = %+v", del)
	}
}

func TestCallReplaysThroughTheAPI(t *testing.T) {
	api := &fakeAPI{reply: `{"id":"svc_1","name":"grafana"}`}
	hs, sink := newDispatchServer(t, api, presetPerms(authz.PresetOperator))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	res, err := callTool(t, cs, "service_get", map[string]any{"serviceID": "svc_1"})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if api.calls() != 1 {
		t.Fatalf("%d requests reached the API", api.calls())
	}
	r := api.seen[0]
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/services/svc_1" {
		t.Errorf("request = %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("Authorization") != "Bearer bua_test" || r.RemoteAddr != "198.51.100.7:4242" {
		t.Errorf("auth %q addr %q", r.Header.Get("Authorization"), r.RemoteAddr)
	}
	if !strings.Contains(resultText(t, res), `"grafana"`) {
		t.Errorf("result = %s", resultText(t, res))
	}
	rows := sink.all()
	if len(rows) != 1 || rows[0].SubjectLabel != "service_get" || rows[0].Result != "ok" {
		t.Errorf("audit rows = %+v", rows)
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	api := &fakeAPI{}
	hs, sink := newDispatchServer(t, api, presetPerms(authz.PresetOperator))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	res, err := callTool(t, cs, "budget_create", map[string]any{
		"dry_run": true, "body": map[string]any{"scope": "global", "action_on_exceed": "throttle_zero", "daily_usd": 7.5},
	})
	if err != nil || res.IsError {
		t.Fatalf("dry run: %v %+v", err, res)
	}
	if api.calls() != 0 {
		t.Fatalf("a dry run reached the API %d times", api.calls())
	}
	text := resultText(t, res)
	for _, want := range []string{`"dry_run":true`, `"method":"POST"`, `"path":"/api/v1/budgets"`, `7.5`} {
		if !strings.Contains(text, want) {
			t.Errorf("dry run result lacks %s: %s", want, text)
		}
	}
	if rows := sink.all(); len(rows) != 1 || rows[0].Result != "dry_run" {
		t.Errorf("audit = %+v", rows)
	}

	// A body that breaks the schema is refused before anything is sent, dry run or not.
	res, _ = callTool(t, cs, "budget_create", map[string]any{"body": map[string]any{"scope": "global"}})
	if !res.IsError || api.calls() != 0 {
		t.Errorf("a body without action_on_exceed: isError=%v calls=%d", res.IsError, api.calls())
	}
	res, _ = callTool(t, cs, "budget_create", map[string]any{"body": map[string]any{"scope": "global", "action_on_exceed": "throttle_zero"}, "colour": "red"})
	if !res.IsError || !strings.Contains(resultText(t, res), "colour") {
		t.Errorf("unknown argument: %+v", res)
	}
}

func TestConfirmGuardsDeletes(t *testing.T) {
	api := &fakeAPI{status: http.StatusNoContent}
	hs, _ := newDispatchServer(t, api, presetPerms(authz.PresetOperator))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	for _, args := range []map[string]any{
		{"slug": "zai"},
		{"slug": "zai", "confirm": "openrouter"},
	} {
		res, _ := callTool(t, cs, "provider_delete", args)
		if !res.IsError || !strings.Contains(resultText(t, res), `"zai"`) {
			t.Errorf("%v: %+v", args, res)
		}
	}
	if api.calls() != 0 {
		t.Fatalf("an unconfirmed delete reached the API")
	}
	res, _ := callTool(t, cs, "provider_delete", map[string]any{"slug": "zai", "dry_run": true})
	if res.IsError || api.calls() != 0 {
		t.Errorf("a dry-run delete needs no confirm and sends nothing: %+v", res)
	}
	res, _ = callTool(t, cs, "provider_delete", map[string]any{"slug": "zai", "confirm": "zai"})
	if res.IsError || api.calls() != 1 || api.seen[0].Method != http.MethodDelete {
		t.Errorf("confirmed delete: %+v calls=%d", res, api.calls())
	}
	if !strings.Contains(resultText(t, res), `"status":204`) {
		t.Errorf("204 result = %s", resultText(t, res))
	}
}

func TestPathValuesStayOneSegment(t *testing.T) {
	api := &fakeAPI{reply: `{}`}
	hs, _ := newDispatchServer(t, api, presetPerms(authz.PresetOperator))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	if res, _ := callTool(t, cs, "service_get", map[string]any{"serviceID": "../users"}); res.IsError {
		t.Fatalf("an odd id is the handler's to refuse, not ours: %+v", res)
	}
	r := api.seen[0]
	if r.URL.EscapedPath() != "/api/v1/services/..%2Fusers" {
		t.Errorf("escaped path = %s", r.URL.EscapedPath())
	}
	for _, v := range []string{"", ".", ".."} {
		res, _ := callTool(t, cs, "service_get", map[string]any{"serviceID": v})
		if !res.IsError {
			t.Errorf("path value %q was sent", v)
		}
	}
	if api.calls() != 1 {
		t.Errorf("%d calls, want 1", api.calls())
	}
}

func TestAPIErrorsAreResults(t *testing.T) {
	api := &fakeAPI{status: http.StatusForbidden, reply: `{"error":"ai:providers:manage required"}`}
	hs, sink := newDispatchServer(t, api, presetPerms(authz.PresetReadOnly))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	res, err := callTool(t, cs, "providers_list", nil)
	if err != nil || !res.IsError {
		t.Fatalf("got %v %+v, want an isError result", err, res)
	}
	if text := resultText(t, res); !strings.Contains(text, "403") || !strings.Contains(text, "ai:providers:manage required") {
		t.Errorf("error text = %s", text)
	}
	if rows := sink.all(); rows[len(rows)-1].Result != "denied" {
		t.Errorf("audit = %+v", rows)
	}
}

func TestLargeListsAreCut(t *testing.T) {
	items := make([]string, 5000)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id":"req_%05d","path":"/v1/chat/completions","status":200}`, i)
	}
	api := &fakeAPI{reply: "[" + strings.Join(items, ",") + "]"}
	hs, _ := newDispatchServer(t, api, presetPerms(authz.PresetReadOnly))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	res, _ := callTool(t, cs, "ai_requests_list", nil)
	text := resultText(t, res)
	if len(text) > MaxResultBytes {
		t.Fatalf("result is %d bytes", len(text))
	}
	var out struct {
		Items     []json.RawMessage `json:"items"`
		Truncated bool              `json:"truncated"`
		Total     int               `json:"total"`
		Note      string            `json:"note"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || out.Total != 5000 || len(out.Items) == 0 || len(out.Items) >= 5000 || !strings.Contains(out.Note, "provider") {
		t.Errorf("cut list: truncated=%v total=%d items=%d note=%q", out.Truncated, out.Total, len(out.Items), out.Note)
	}
}

func TestNotReadyWithoutAPIHandler(t *testing.T) {
	spec, _ := os.ReadFile("../../docs/openapi.yaml")
	s, err := New(nil, nil, spec, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	op, _ := s.reg.Lookup("servicesList")
	res, _ := s.invoke(context.Background(), &authedToken{role: "admin", perms: presetPerms(authz.PresetFull)}, op, map[string]any{})
	if !res.IsError {
		t.Errorf("a call before SetAPIHandler succeeded: %+v", res)
	}
}

func TestNewRefusesAnUnknownCuratedTool(t *testing.T) {
	spec, _ := os.ReadFile("../../docs/openapi.yaml")
	saved := curatedTools
	t.Cleanup(func() { curatedTools = saved })
	curatedTools = append(slices.Clone(saved), curatedTool{ID: "noSuchOperation"})
	if _, err := New(nil, nil, spec, quietLog()); err == nil || !strings.Contains(err.Error(), "noSuchOperation") {
		t.Errorf("New with an unknown curated id: %v", err)
	}
}

// The MCP endpoint is itself served by the relay's chi router. The request a
// call becomes is routed afresh by the API router, never as a sub-request of
// /mcp/burrow, and a path value stays one segment there: "../users" does not
// reach the users route.
func TestCallIsRoutedAfresh(t *testing.T) {
	api := chi.NewRouter()
	api.Get("/api/v1/services/{serviceID}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": chi.URLParam(r, "serviceID"), "pattern": chi.RouteContext(r.Context()).RoutePattern(),
		})
	})
	api.Get("/api/v1/users", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"users":"everyone"}`)
	})
	spec, _ := os.ReadFile("../../docs/openapi.yaml")
	s, err := New(nil, nil, spec, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	s.SetAPIHandler(api)
	s.SetAuditAppender(&auditSink{})
	tok := &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: presetPerms(authz.PresetOperator), bearer: "Bearer bua_test"}
	outer := chi.NewRouter()
	h := s.mcpHandler()
	outer.Post("/mcp/burrow", func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), tok)))
	})
	hs := httptest.NewServer(outer)
	t.Cleanup(hs.Close)
	cs := connectMCP(t, hs.URL+"/mcp/burrow", "", testRevisions[0])

	for value, wantID := range map[string]string{"svc_1": "svc_1", "../users": "..%2Fusers", "a/b": "a%2Fb"} {
		res, err := callTool(t, cs, "service_get", map[string]any{"serviceID": value})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", value, err, res)
		}
		var got struct{ ID, Pattern string }
		if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
			t.Fatalf("%s: %s", value, resultText(t, res))
		}
		if got.ID != wantID || got.Pattern != "/api/v1/services/{serviceID}" {
			t.Errorf("%s: reached %+v, want id %q on /api/v1/services/{serviceID}", value, got, wantID)
		}
	}
}

func TestCallKeepsTheHostOfTheMCPRequest(t *testing.T) {
	api := &fakeAPI{reply: `{"id":"svc_1"}`}
	hs, _ := newDispatchServer(t, api, presetPerms(authz.PresetOperator))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	if res, err := callTool(t, cs, "service_get", map[string]any{"serviceID": "svc_1"}); err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if api.calls() != 1 || api.seen[0].Host != "relay.example.com" {
		t.Errorf("the API saw host %q", api.seen[0].Host)
	}
}
