package api

// contract_v3_test.go is the executable form of the v0.3.0 contract
// (dev/superpowers/specs/2026-05-19-v0.3.0-api-contract.md, Parts C/D/E).
//
// It asserts the *shape* (key set + value types) and the *known error bodies*
// of every v0.3.0 endpoint. Per the spec's own rule ("if shipped code and the
// spec disagree, code wins — fix this doc"), this is the golden the
// integration agent uses to reconcile spec ⟷ shipped.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// keysOf returns the sorted top-level keys of a JSON object.
func keysOf(t *testing.T, m map[string]any) []string {
	t.Helper()
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// decodeObj unmarshals an HTTP response body into a generic JSON object.
func decodeObj(t *testing.T, r *http.Response) map[string]any {
	t.Helper()
	defer r.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	return m
}

// decodeArr unmarshals an HTTP response body into a generic JSON array.
func decodeArr(t *testing.T, r *http.Response) []any {
	t.Helper()
	defer r.Body.Close()
	var a []any
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		t.Fatalf("decode array: %v", err)
	}
	return a
}

// assertKeys asserts that got matches want exactly. Diffs both directions so
// either drift surface (missing or extra) is reported clearly.
func assertKeys(t *testing.T, label string, got, want []string) {
	t.Helper()
	gs := strings.Join(got, ",")
	ws := strings.Join(want, ",")
	if gs != ws {
		t.Errorf("%s: keyset drift\n  want: [%s]\n  got:  [%s]", label, ws, gs)
	}
}

// assertType asserts that v has the JSON type implied by want
// ("string", "number", "bool", "null", "array", "object").
func assertType(t *testing.T, label string, v any, want string) {
	t.Helper()
	var got string
	switch x := v.(type) {
	case nil:
		got = "null"
	case bool:
		got = "bool"
	case float64:
		got = "number"
	case string:
		got = "string"
	case []any:
		got = "array"
	case map[string]any:
		got = "object"
	default:
		got = "?"
		_ = x
	}
	// Accept "null" wherever a nullable field is expected (e.g. last_used).
	if got == "null" && want != "null" && strings.HasPrefix(want, "?") {
		return
	}
	want = strings.TrimPrefix(want, "?")
	if got != want {
		t.Errorf("%s: want type %s, got %s (value=%v)", label, want, got, v)
	}
}

// goldenContractFixtures builds a Deps wired to fakeServiceStore returning the
// canonical happy-path objects the contract documents — exactly the values the
// shipped handlers serialize. The whole point: round-trip through the real
// router + the real json.Marshal path so we test the wire shape.
func goldenContractFixtures() (*fakeServiceStore, fakeLiveTunnels, Deps) {
	created := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	lastUsed := time.Date(2026, 5, 20, 9, 30, 0, 0, time.UTC)

	ss := &fakeServiceStore{
		// /services list
		listSvcs: []store.ServiceView{
			{
				ID: "svc-http", Name: "web", Type: "http", Subdomain: "k7p2qx",
				AccessMode: "open", APIKeyHeader: "Authorization",
			},
		},
		// /services/{id} detail
		getSvc: store.ServiceDetail{
			ServiceView: store.ServiceView{
				ID: "svc-http", Name: "web", Type: "http", Subdomain: "k7p2qx",
				AccessMode: "api_key", APIKeyHeader: "Authorization",
			},
			APIKeyCount:  3,
			AccessPolicy: []string{"user"},
		},
		// /api-keys list
		listKeys: []db.ServiceAPIKey{
			{ID: "sak_one", ServiceID: "svc-http", Name: "prod", CreatedAt: created, LastUsed: &lastUsed},
		},
		createKeyID: "sak_new",
		createKeyPT: "buk_PLAINTEXT_ONCE",
		// /access-policy
		getPolicy: []string{"admin", "user"},
	}
	lt := fakeLiveTunnels{
		svcID:     "svc-http",
		localAddr: "127.0.0.1:3000",
		connected: true,
		exists:    true,
	}
	deps := Deps{
		Users:       &fakeUserStore{role: "admin"},
		Services:    ss,
		LiveTunnels: lt,
		AuthDomain:  "tunnels.example.com",
		Log:         discardLog(),
	}
	return ss, lt, deps
}

// ---------------------------------------------------------------------------
// Part E — Services read surface
// ---------------------------------------------------------------------------

func TestContractV3_ServicesList_Shape(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	arr := decodeArr(t, r)
	if len(arr) != 1 {
		t.Fatalf("want 1 element, got %d", len(arr))
	}
	obj, ok := arr[0].(map[string]any)
	if !ok {
		t.Fatalf("element 0: want object, got %T", arr[0])
	}
	want := []string{
		"access_mode", "api_key_header", "connected", "id",
		"local_addr", "name", "remote_port", "slug", "type", "url",
	}
	assertKeys(t, "GET /services[0]", keysOf(t, obj), want)

	// Type spot-checks (per spec Part E).
	assertType(t, "id", obj["id"], "string")
	assertType(t, "name", obj["name"], "string")
	assertType(t, "type", obj["type"], "string")
	assertType(t, "slug", obj["slug"], "string")
	assertType(t, "url", obj["url"], "string")
	assertType(t, "access_mode", obj["access_mode"], "string")
	assertType(t, "api_key_header", obj["api_key_header"], "string")
	assertType(t, "connected", obj["connected"], "bool")
	assertType(t, "remote_port", obj["remote_port"], "number")
	assertType(t, "local_addr", obj["local_addr"], "string")

	// URL composition rule.
	if obj["url"] != "https://tunnels.example.com/svc/k7p2qx/" {
		t.Errorf("url composition: got %v", obj["url"])
	}
}

func TestContractV3_ServiceDetail_Shape(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services/svc-http")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := decodeObj(t, r)
	want := []string{
		"access_mode", "access_policy", "api_key_count", "api_key_header",
		"connected", "id", "local_addr", "name", "remote_port",
		"slug", "type", "url",
	}
	assertKeys(t, "GET /services/{id}", keysOf(t, obj), want)

	assertType(t, "api_key_count", obj["api_key_count"], "number")
	assertType(t, "access_policy", obj["access_policy"], "array")
}

func TestContractV3_ServiceDetail_NotFound(t *testing.T) {
	ss, _, deps := goldenContractFixtures()
	ss.getSvcErr = db.ErrNotFound
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services/nope")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", r.StatusCode)
	}
	obj := decodeObj(t, r)
	if obj["error"] != "service not found" {
		t.Errorf(`error body: want {"error":"service not found"}, got %v`, obj)
	}
}

// ---------------------------------------------------------------------------
// Part C — Per-service access mode
// ---------------------------------------------------------------------------

func TestContractV3_SetAccessMode_Success_NoContent(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc-http/access-mode",
		map[string]string{"access_mode": "api_key", "api_key_header": "X-Api-Key"})
	defer r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d body=%s", r.StatusCode, readBody(t, r))
	}
}

func TestContractV3_SetAccessMode_MissingMode_400Body(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc-http/access-mode",
		map[string]string{"access_mode": ""})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", r.StatusCode)
	}
	obj := decodeObj(t, r)
	if obj["error"] != "access_mode is required" {
		t.Errorf(`error body: want {"error":"access_mode is required"}, got %v`, obj)
	}
}

func TestContractV3_SetAccessMode_InvalidMode_400Body(t *testing.T) {
	ss, _, deps := goldenContractFixtures()
	ss.setModeErr = store.ErrInvalidAccessMode
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc-http/access-mode",
		map[string]string{"access_mode": "bogus"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", r.StatusCode)
	}
	obj := decodeObj(t, r)
	// v0.4.0 Task 16: enum now includes "mtls".
	if obj["error"] != "access_mode must be 'open', 'api_key', 'burrow_login', or 'mtls'" {
		t.Errorf(`error body: got %v`, obj)
	}
}

func TestContractV3_SetAccessMode_TCPService_409Body(t *testing.T) {
	ss, _, deps := goldenContractFixtures()
	ss.setModeErr = store.ErrServiceNotHTTP
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc-tcp/access-mode",
		map[string]string{"access_mode": "api_key"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", r.StatusCode)
	}
	obj := decodeObj(t, r)
	// v0.4.0 Task 16: enum now includes "mtls" (also http-only).
	if obj["error"] != "api_key, burrow_login, and mtls require an http service" {
		t.Errorf(`error body: got %v`, obj)
	}
}

// Q4 RESOLVED — burrow_login with no auth_domain configured → 409.
func TestContractV3_SetAccessMode_BurrowLoginNoAuthDomain_409Body(t *testing.T) {
	ss, _, deps := goldenContractFixtures()
	deps.AuthDomain = "" // explicitly unconfigured
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc-http/access-mode",
		map[string]string{"access_mode": "burrow_login"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", r.StatusCode)
	}
	obj := decodeObj(t, r)
	if obj["error"] != "burrow_login requires a configured auth_domain" {
		t.Errorf(`error body: got %v`, obj)
	}
	// And the store must NOT have been called (gate is in the handler).
	if ss.lastMode != "" {
		t.Errorf("store wrongly called, lastMode=%q", ss.lastMode)
	}
}

// ---------------------------------------------------------------------------
// Part C — API keys
// ---------------------------------------------------------------------------

func TestContractV3_APIKeysList_Shape(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services/svc-http/api-keys")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	// Hash/plaintext must never appear (spec Part C).
	if strings.Contains(body, "key_hash") || strings.Contains(body, "service_id") {
		t.Fatalf("forbidden field in api-keys list: %s", body)
	}

	var arr []any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("want 1 key, got %d", len(arr))
	}
	obj, ok := arr[0].(map[string]any)
	if !ok {
		t.Fatalf("element 0: want object, got %T", arr[0])
	}
	want := []string{"created_at", "id", "last_used", "name"}
	assertKeys(t, "GET /api-keys[0]", keysOf(t, obj), want)

	assertType(t, "id", obj["id"], "string")
	assertType(t, "name", obj["name"], "string")
	assertType(t, "created_at", obj["created_at"], "string")
	// last_used is nullable (Part C: `*time.Time`).
	if obj["last_used"] != nil {
		assertType(t, "last_used", obj["last_used"], "string")
	}
}

func TestContractV3_APIKeysCreate_Shape(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/services/svc-http/api-keys",
		map[string]string{"name": "ci"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := decodeObj(t, r)
	want := []string{"id", "key", "name"}
	assertKeys(t, "POST /api-keys", keysOf(t, obj), want)

	if obj["key"] != "buk_PLAINTEXT_ONCE" {
		t.Errorf("key plaintext not echoed once: got %v", obj["key"])
	}
}

func TestContractV3_APIKeysCreate_MissingName_400Body(t *testing.T) {
	ss, _, deps := goldenContractFixtures()
	ss.createKeyErr = store.ErrNameRequired
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/services/svc-http/api-keys",
		map[string]string{"name": ""})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := decodeObj(t, r)
	if obj["error"] != "name is required" {
		t.Errorf(`error body: got %v`, obj)
	}
}

func TestContractV3_APIKeysDelete_NotFound_404Body(t *testing.T) {
	ss, _, deps := goldenContractFixtures()
	ss.deleteKeyErr = db.ErrNotFound
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.delete(t, "/api/v1/services/svc-http/api-keys/missing")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", r.StatusCode)
	}
	obj := decodeObj(t, r)
	if obj["error"] != "api key not found" {
		t.Errorf(`error body: got %v`, obj)
	}
}

// ---------------------------------------------------------------------------
// Part D — Access policy
// ---------------------------------------------------------------------------

func TestContractV3_AccessPolicyGet_Shape(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services/svc-http/access-policy")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	obj := decodeObj(t, r)
	want := []string{"roles"}
	assertKeys(t, "GET /access-policy", keysOf(t, obj), want)
	assertType(t, "roles", obj["roles"], "array")
}

func TestContractV3_AccessPolicyGet_EmptyIsArray(t *testing.T) {
	ss, _, deps := goldenContractFixtures()
	ss.getPolicy = []string{}
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services/svc-http/access-policy")
	body := readBody(t, r)
	// Empty policy MUST serialize as [], never null (spec Part D).
	if !strings.Contains(body, `"roles":[]`) {
		t.Errorf(`want "roles":[] not null, got: %s`, body)
	}
}

func TestContractV3_AccessPolicySet_UnknownRole_400Body(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/services/svc-http/access-policy",
		map[string][]string{"roles": {"superuser"}})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", r.StatusCode)
	}
	obj := decodeObj(t, r)
	// Spec Part D: body MUST include the offending role name.
	if obj["error"] != `unknown role "superuser"` {
		t.Errorf(`error body: want {"error":"unknown role \"superuser\""}, got %v`, obj)
	}
}

// ---------------------------------------------------------------------------
// Cross-cutting — unauthenticated and CSRF (spec Conventions)
// ---------------------------------------------------------------------------

func TestContractV3_Unauthenticated_401Body(t *testing.T) {
	_, _, deps := goldenContractFixtures()
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()

	// Any v0.3.0 route should 401 with the spec error body when no session.
	resp, err := http.Get(srv.URL + "/api/v1/services")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var obj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if obj["error"] != "unauthorized" {
		t.Errorf(`error body: want "unauthorized", got %v`, obj)
	}
}

// Sanity: context.Background usage to keep the import marker honest in a future
// refactor (e.g. when the test uses ctx for cancellation). No-op today.
var _ = context.Background
