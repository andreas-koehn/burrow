package aigateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/store"
)

type fakeGatewayKeys map[string]store.GatewayKey // plaintext → key

func (f fakeGatewayKeys) ValidateGatewayKey(_ context.Context, presented string) (store.GatewayKey, bool, error) {
	k, ok := f[presented]
	return k, ok, nil
}

type fakeCatalog struct {
	models    []db.AIModel
	providers []db.AIProvider
	catalog   map[string][]db.AIProviderModel
}

func (f fakeCatalog) ListModels(context.Context) ([]db.AIModel, error)       { return f.models, nil }
func (f fakeCatalog) ListProviders(context.Context) ([]db.AIProvider, error) { return f.providers, nil }
func (f fakeCatalog) ListProviderModels(_ context.Context, slug string) ([]db.AIProviderModel, error) {
	return f.catalog[slug], nil
}

// allowAll is the policy of a direct provider's backing service in tests.
func allowAll(_ context.Context, serviceID string) (*proxy.Resolved, error) {
	return &proxy.Resolved{ServiceID: serviceID, AccessMode: "api_key"}, nil
}

// globalGateway routes every provider of resolveGateway to up: ollama
// (tunnel), openrouter and zai (openai, direct), zai-anthropic (anthropic,
// direct).
func globalGateway(up http.Handler, chain Chain) *Gateway {
	g := resolveGateway()
	g.Chain = chain
	g.Tunnels = fakeTunnels{
		res:      &proxy.Resolved{ServiceID: "svc1", AccessMode: "api_key", LocalHost: "127.0.0.1:11434"},
		upstream: up,
	}
	g.Direct = func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) { return up, nil }
	g.ServicePolicy = allowAll
	g.GatewayKeys = fakeGatewayKeys{
		"bgw_all":  {ID: "gk-all"},
		"bgw_some": {ID: "gk-some", AllowedModels: []string{"burrow-intelligence", "ollama/*"}},
	}
	return g
}

func post(path, key, body string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}

func TestServeDialect_SyntheticModel(t *testing.T) {
	var gotBody string
	var gotHeader http.Header
	var route aigw.RouteInfo
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotHeader = string(b), r.Header.Clone()
		route, _ = aigw.RouteFrom(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"glm-5.1","choices":[]}`))
	})
	chain := &spyChain{}
	g := globalGateway(up, chain)

	rec := httptest.NewRecorder()
	rec.Header().Set("Burrow-Request-Id", "req-1") // set by the router in production
	req := post("/v1/chat/completions", "bgw_all", `{ "model" : "burrow-intelligence" ,"messages":[],"x_future_field":{"a":1,"model":"inner"}}`)
	req.Header.Set("X-Api-Key", "bgw_all")
	req.Header.Set("Proxy-Authorization", "Bearer bgw_all")
	req.Header.Set("Cookie", "burrow_session=s")
	g.ServeDialect(rec, req, DialectOpenAI)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	// The upstream sees the native model; every other byte, known field or
	// not, is what the client sent.
	if want := `{ "model" : "glm-5.1" ,"messages":[],"x_future_field":{"a":1,"model":"inner"}}`; gotBody != want {
		t.Fatalf("upstream body = %s\nwant          %s", gotBody, want)
	}
	for _, h := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization"} {
		if gotHeader.Get(h) != "" {
			t.Fatalf("%s reached the upstream: %q", h, gotHeader.Get(h))
		}
	}
	if rec.Body.String() != `{"model":"glm-5.1","choices":[]}` {
		t.Fatalf("response body rewritten: %s", rec.Body.String())
	}
	if rec.Header().Get("Burrow-Provider") != "zai" || rec.Header().Get("Burrow-Model") != "glm-5.1" {
		t.Fatalf("headers: %v", rec.Header())
	}
	want := aigw.RouteInfo{GatewayKeyID: "gk-all", Dialect: "openai", ProviderSlug: "zai", RequestedModel: "burrow-intelligence", TargetModel: "glm-5.1", RequestID: "req-1"}
	if route != want {
		t.Fatalf("route = %+v", route)
	}
	// Reported cost is trusted: the relay calls this upstream itself.
	if chain.serviceID != "prov-zai" || chain.keyID != "" || !chain.metered || !chain.trustCost {
		t.Fatalf("chain = %+v", chain)
	}
	for k, v := range rec.Header() {
		if strings.Contains(strings.Join(v, " "), "bgw_") {
			t.Fatalf("the gateway key is in response header %s", k)
		}
	}
}

func TestServeDialect_DirectAddressAndTunnelTarget(t *testing.T) {
	var gotBody string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	})

	chain := &spyChain{}
	g := globalGateway(up, chain)
	rec := httptest.NewRecorder()
	g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", `{"model":"openrouter/google/gemini-x"}`), DialectOpenAI)
	if rec.Code != 200 || gotBody != `{"model":"google/gemini-x"}` {
		t.Fatalf("status %d upstream body %s", rec.Code, gotBody)
	}
	if rec.Header().Get("Burrow-Provider") != "openrouter" || rec.Header().Get("Burrow-Model") != "google/gemini-x" {
		t.Fatalf("headers: %v", rec.Header())
	}
	if chain.serviceID != "prov-openrouter" || !chain.trustCost {
		t.Fatalf("chain = %+v", chain)
	}

	// A synthetic model whose target is a tunnel.
	chain = &spyChain{}
	g = globalGateway(up, chain)
	g.Direct = func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) {
		t.Error("the direct factory was called for a tunnelled target")
		return nil, errors.New("unused")
	}
	rec = httptest.NewRecorder()
	g.ServeDialect(rec, post("/v1/embeddings", "bgw_some", `{"model":"ollama/mistral","input":"x"}`), DialectOpenAI)
	if rec.Code != 200 || gotBody != `{"model":"mistral","input":"x"}` {
		t.Fatalf("status %d upstream body %s", rec.Code, gotBody)
	}
	rec = httptest.NewRecorder()
	g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", `{"model":"burrow-simple"}`), DialectOpenAI)
	if rec.Code != 200 || gotBody != `{"model":"mistral"}` || rec.Header().Get("Burrow-Provider") != "ollama" {
		t.Fatalf("status %d upstream body %s headers %v", rec.Code, gotBody, rec.Header())
	}
	// Whoever holds the tunnel runs the model: its cost figure is not believed.
	if chain.serviceID != "svc1" || !chain.metered || chain.trustCost {
		t.Fatalf("chain = %+v", chain)
	}
}

func TestServeDialect_Errors(t *testing.T) {
	hit := false
	up := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true })
	big := `{"model":"burrow-intelligence","pad":"` + strings.Repeat("x", 100) + `"}`
	type tc struct {
		name   string
		req    *http.Request
		setup  func(g *Gateway)
		status int
		code   string
		msg    string // substring of the message, "" = not checked
	}
	noDirect := func(g *Gateway) {
		g.Direct = func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) {
			t.Error("the upstream credential was read for a refused request")
			return nil, errors.New("unused")
		}
	}
	cases := []tc{
		{name: "no key", req: post("/v1/chat/completions", "", `{"model":"burrow-intelligence"}`), status: 401, code: "invalid_api_key"},
		{name: "unknown key", req: post("/v1/chat/completions", "bgw_nope", `{"model":"burrow-intelligence"}`), status: 401, code: "invalid_api_key"},
		{name: "service key is not a gateway key", req: post("/v1/chat/completions", "sk-good", `{"model":"burrow-intelligence"}`), status: 401, code: "invalid_api_key"},
		{name: "no model", req: post("/v1/chat/completions", "bgw_all", `{"messages":[]}`), status: 400, code: "model_required"},
		{name: "not json", req: post("/v1/chat/completions", "bgw_all", `nope`), status: 400, code: "model_required"},
		{name: "model in another letter case only", req: post("/v1/chat/completions", "bgw_all", `{"Model":"burrow-intelligence"}`), status: 400, code: "model_required"},
		{name: "two model fields", req: post("/v1/chat/completions", "bgw_all", `{"model":"burrow-intelligence","Model":"zai/other"}`), status: 400, code: "invalid_request"},
		{name: "two model fields, restricted key", req: post("/v1/chat/completions", "bgw_some", `{"model":"burrow-intelligence","model":"zai/other"}`), status: 400, code: "invalid_request"},
		{name: "unknown model", req: post("/v1/chat/completions", "bgw_all", `{"model":"nope"}`), status: 404, code: "model_not_found"},
		{name: "disabled model", req: post("/v1/chat/completions", "bgw_all", `{"model":"burrow-off"}`), status: 404, code: "model_not_found"},
		{name: "model outside allow-list", req: post("/v1/chat/completions", "bgw_some", `{"model":"zai/glm-5.1"}`), status: 403, code: "model_not_allowed"},
		{name: "unknown model outside allow-list", req: post("/v1/chat/completions", "bgw_some", `{"model":"secret-model"}`), status: 403, code: "model_not_allowed"},
		{name: "other dialect's provider", req: post("/v1/chat/completions", "bgw_all", `{"model":"zai-anthropic/glm-5.1"}`), status: 400, code: "format_mismatch",
			msg: "https://burrow.example.com/anthropic"},
		{name: "not an endpoint of this dialect", req: post("/v1/messages", "bgw_all", `{"model":"burrow-intelligence"}`), status: 404, code: "endpoint_not_found"},
		{name: "outside /v1", req: post("/", "bgw_all", `{"model":"burrow-intelligence"}`), status: 404, code: "endpoint_not_found"},
		{name: "GET on an inference path without a key", req: httptest.NewRequest("GET", "/v1/chat/completions", nil), status: 401, code: "invalid_api_key"},
		{name: "DELETE on an inference path", req: func() *http.Request {
			r := post("/v1/chat/completions", "bgw_all", `{"model":"burrow-intelligence"}`)
			r.Method = "DELETE"
			return r
		}(), status: 405, code: "method_not_allowed"},
		{name: "body over the limit", req: post("/v1/chat/completions", "bgw_all", big), setup: func(g *Gateway) { g.MaxBody = 64; noDirect(g) },
			status: 413, code: "request_too_large"},
		{name: "chunked body over the limit", req: func() *http.Request {
			r := post("/v1/chat/completions", "bgw_all", "")
			r.Body = io.NopCloser(io.MultiReader(strings.NewReader(big[:40]), strings.NewReader(big[40:])))
			r.ContentLength = -1 // no Content-Length: the size is only known by reading
			r.Header.Del("Content-Length")
			r.TransferEncoding = []string{"chunked"}
			return r
		}(), setup: func(g *Gateway) { g.MaxBody = 64; noDirect(g) }, status: 413, code: "request_too_large"},
		{name: "path with an encoded slash", req: post("/v1/chat%2fcompletions", "bgw_all", `{"model":"burrow-intelligence"}`), status: 404, code: "endpoint_not_found"},
		{name: "path with an encoded letter", req: post("/v1/chat/c%6fmpletions", "bgw_all", `{"model":"burrow-intelligence"}`), status: 404, code: "endpoint_not_found"},
		{name: "path with an empty segment", req: post("/v1//chat/completions", "bgw_all", `{"model":"burrow-intelligence"}`), status: 404, code: "endpoint_not_found"},
		{name: "path with a dot segment", req: post("/v1/./chat/completions", "bgw_all", `{"model":"burrow-intelligence"}`), status: 404, code: "endpoint_not_found"},
		{name: "address denied by the target's service", req: post("/v1/chat/completions", "bgw_all", `{"model":"burrow-intelligence"}`),
			setup: func(g *Gateway) {
				g.IPGeoDeny = func(*proxy.Resolved, *http.Request) bool { return true }
				noDirect(g)
			}, status: 403, code: "forbidden"},
		{name: "target's service is not in api_key mode", req: post("/v1/chat/completions", "bgw_all", `{"model":"zai/glm-5.1"}`),
			setup: func(g *Gateway) {
				g.ServicePolicy = func(_ context.Context, id string) (*proxy.Resolved, error) {
					return &proxy.Resolved{ServiceID: id, AccessMode: "burrow_login"}, nil
				}
				noDirect(g)
			}, status: 403, code: "provider_unavailable"},
		{name: "credential slot unset", req: post("/v1/chat/completions", "bgw_all", `{"model":"burrow-intelligence"}`),
			setup: func(g *Gateway) {
				g.Direct = func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) {
					return nil, aiprovider.ErrNotConfigured
				}
			}, status: 503, code: "provider_not_configured"},
		{name: "no gateway key store", req: post("/v1/chat/completions", "bgw_all", `{"model":"burrow-intelligence"}`),
			setup: func(g *Gateway) { g.GatewayKeys = nil }, status: 401, code: "invalid_api_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hit = false
			g := globalGateway(up, &spyChain{})
			if c.setup != nil {
				c.setup(g)
			}
			rec := httptest.NewRecorder()
			g.ServeDialect(rec, c.req, DialectOpenAI)
			if rec.Code != c.status || rec.Header().Get("Burrow-Error-Code") != c.code {
				t.Fatalf("status %d code %s, want %d %s", rec.Code, rec.Header().Get("Burrow-Error-Code"), c.status, c.code)
			}
			if got := errCode(t, rec); got != c.code {
				t.Fatalf("body code %s, want %s", got, c.code)
			}
			if c.msg != "" && !strings.Contains(rec.Body.String(), c.msg) {
				t.Fatalf("body = %s, want it to name %s", rec.Body.String(), c.msg)
			}
			if hit {
				t.Fatal("the upstream was called")
			}
			if g.Chain.(*spyChain).serviceID != "" {
				t.Fatal("the chain ran")
			}
			// Which provider would have served is not told to a refused caller.
			if rec.Header().Get("Burrow-Provider") != "" || rec.Header().Get("Burrow-Model") != "" {
				t.Fatalf("routing headers on a refused request: %v", rec.Header())
			}
		})
	}
}

// unreadBody fails the test when it is read.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("the body was read before the key was accepted")
	return 0, io.EOF
}
func (unreadBody) Close() error { return nil }

// A caller without a valid gateway key learns nothing: one answer for every
// way a key can be wrong, and the body is never looked at.
func TestServeDialect_NoKeyLearnsNothing(t *testing.T) {
	g := globalGateway(http.NotFoundHandler(), nil)
	g.Providers = providerCounter{t} // any lookup fails the test
	g.Synthetic = lookupCounter{t}
	var first string
	for _, key := range []string{"", "bgw_", "bgw_nope", "sk-good", "bgw_" + strings.Repeat("A", 43), "Bearer"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/models", "/v1/nope", "/"} {
			r := post(path, key, "")
			r.Body = unreadBody{t}
			rec := httptest.NewRecorder()
			g.ServeDialect(rec, r, DialectOpenAI)
			if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("key %q path %s: status %d", key, path, rec.Code)
			}
			if first == "" {
				first = rec.Body.String()
			}
			if rec.Body.String() != first {
				t.Fatalf("key %q path %s answers differently:\n%s\n%s", key, path, rec.Body.String(), first)
			}
		}
	}
}

// A restricted key learns nothing about models outside its list, not even
// whether they exist: the denial is the same answer and no lookup is made.
func TestServeDialect_AllowListCheckedBeforeExistence(t *testing.T) {
	g := globalGateway(http.NotFoundHandler(), nil)
	g.Providers = providerCounter{t}
	g.Synthetic = lookupCounter{t}
	g.ServicePolicy = func(context.Context, string) (*proxy.Resolved, error) {
		t.Error("the service policy was read")
		return nil, errors.New("unused")
	}
	var bodies []string
	for _, model := range []string{
		"openrouter/does-not-exist", // provider exists, model unknown
		"zai/glm-5.1",               // exists
		"nope/x",                    // provider does not exist
		"burrow-simple",             // synthetic, exists
		"burrow-off",                // synthetic, disabled
		"secret-model",              // synthetic, does not exist
		"zai-anthropic/glm-5.1",     // other dialect
		strings.Repeat("x", 300),    // cannot exist
	} {
		rec := httptest.NewRecorder()
		g.ServeDialect(rec, post("/v1/chat/completions", "bgw_some", `{"model":"`+model+`"}`), DialectOpenAI)
		if rec.Code != 403 || rec.Header().Get("Burrow-Error-Code") != "model_not_allowed" {
			t.Fatalf("%s: status %d code %s, want 403 before any lookup", model, rec.Code, rec.Header().Get("Burrow-Error-Code"))
		}
		bodies = append(bodies, rec.Body.String())
	}
	for i, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("denial %d differs: %s vs %s", i, b, bodies[0])
		}
	}
}

// Nothing about a key is remembered between requests.
func TestServeDialect_RevokedKeyFailsOnNextRequest(t *testing.T) {
	g := globalGateway(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }), nil)
	keys := g.GatewayKeys.(fakeGatewayKeys)
	do := func() int {
		rec := httptest.NewRecorder()
		g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", `{"model":"burrow-intelligence"}`), DialectOpenAI)
		return rec.Code
	}
	if got := do(); got != 200 {
		t.Fatalf("status %d before revocation", got)
	}
	delete(keys, "bgw_all") // what ValidateGatewayKey answers for a revoked key
	if got := do(); got != 401 {
		t.Fatalf("status %d after revocation, want 401", got)
	}
}

type errGatewayKeys struct{}

func (errGatewayKeys) ValidateGatewayKey(context.Context, string) (store.GatewayKey, bool, error) {
	return store.GatewayKey{}, false, errors.New("db down")
}

// The presented key appears in no log line and no response.
func TestServeDialect_KeyIsNeverLoggedOrEchoed(t *testing.T) {
	var logs bytes.Buffer
	const key = "bgw_SECRETSECRETSECRETSECRETSECRETSECRETSECRETS"
	for name, setup := range map[string]func(g *Gateway){
		"store error":  func(g *Gateway) { g.GatewayKeys = errGatewayKeys{} },
		"unknown key":  func(*Gateway) {},
		"not allowed":  func(g *Gateway) { g.GatewayKeys = fakeGatewayKeys{key: {ID: "gk", AllowedModels: []string{"x/y"}}} },
		"no model":     func(g *Gateway) { g.GatewayKeys = fakeGatewayKeys{key: {ID: "gk"}} },
		"lookup fails": func(g *Gateway) { g.GatewayKeys = fakeGatewayKeys{key: {ID: "gk"}}; g.Synthetic = errSynthetic{} },
		"geo deny": func(g *Gateway) {
			g.GatewayKeys = fakeGatewayKeys{key: {ID: "gk"}}
			g.IPGeoDeny = func(*proxy.Resolved, *http.Request) bool { return true }
		},
	} {
		g := globalGateway(http.NotFoundHandler(), nil)
		g.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		setup(g)
		body := `{"model":"burrow-intelligence"}`
		if name == "no model" {
			body = `{}`
		}
		rec := httptest.NewRecorder()
		g.ServeDialect(rec, post("/v1/chat/completions", key, body), DialectOpenAI)
		if rec.Code < 400 {
			t.Fatalf("%s: status %d", name, rec.Code)
		}
		dump := rec.Body.String()
		for k, v := range rec.Header() {
			dump += k + strings.Join(v, " ")
		}
		if strings.Contains(dump, "SECRET") {
			t.Fatalf("%s: the key is in the response: %s", name, dump)
		}
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("the key was logged: %s", logs.String())
	}
}

func modelIDs(t *testing.T, g *Gateway, key string) (int, []string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	g.ServeDialect(rec, r, DialectOpenAI)
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("model list: %s", rec.Body.String())
		}
	}
	ids := []string{}
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return rec.Code, ids
}

func TestServeDialect_Models(t *testing.T) {
	g := globalGateway(http.NotFoundHandler(), nil)
	g.Providers = providerCounter{t} // the list is answered from the catalog alone
	g.Catalog = fakeCatalog{
		models: []db.AIModel{
			{Name: "burrow-intelligence", Enabled: true, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "zai"}, {Dialect: "anthropic", ProviderSlug: "zai-anthropic"}}},
			{Name: "burrow-off", Enabled: false, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "zai"}}},
			{Name: "burrow-simple", Enabled: true, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "ollama"}}},
			{Name: "claude-only", Enabled: true, Targets: []db.AIModelTarget{{Dialect: "anthropic", ProviderSlug: "zai-anthropic"}}},
			// rows say openai, the provider speaks anthropic: cannot be served here
			{Name: "burrow-stale-only", Enabled: true, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "zai-anthropic"}}},
			{Name: "burrow-broken", Enabled: true, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "gone"}}},
		},
		providers: []db.AIProvider{{Slug: "zai", APIFormat: "openai"}, {Slug: "ollama", APIFormat: "openai"}, {Slug: "zai-anthropic", APIFormat: "anthropic"}},
		catalog: map[string][]db.AIProviderModel{
			"ollama":        {{ModelID: "mistral"}},
			"zai":           {{ModelID: "glm-5.1"}},
			"zai-anthropic": {{ModelID: "glm-5.1"}},
		},
	}
	// Only models and providers of the openai dialect.
	if code, ids := modelIDs(t, g, "bgw_all"); code != 200 || !reflect.DeepEqual(ids, []string{"burrow-intelligence", "burrow-simple", "ollama/mistral", "zai/glm-5.1"}) {
		t.Fatalf("bgw_all: %d %v", code, ids)
	}
	// A restricted key sees what it may use and nothing else.
	if code, ids := modelIDs(t, g, "bgw_some"); code != 200 || !reflect.DeepEqual(ids, []string{"burrow-intelligence", "ollama/mistral"}) {
		t.Fatalf("bgw_some: %d %v", code, ids)
	}
	if code, _ := modelIDs(t, g, ""); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	g.Catalog = nil
	if code, ids := modelIDs(t, g, "bgw_all"); code != 200 || len(ids) != 0 {
		t.Fatalf("no catalog: %d %v", code, ids)
	}
}

// Through the real chain and the real factory the first chunk reaches the
// caller while the upstream still holds the response open.
func TestServeDialect_StreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	var gotAuth, gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotBody = r.Header.Get("Authorization"), string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
		_ = http.NewResponseController(w).Flush()
		<-release
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := globalGateway(http.NotFoundHandler(), aigw.NewChain(nil, nil, nil, nil, nil, nil, nil, nil, log))
	g.Direct = DirectUpstreams(vaultMap{"ZAI": "sk-zai"}, srv.Client().Transport)
	g.Providers.(fakeProviders)["zai"] = db.AIProvider{
		Slug: "zai", Kind: "direct", ServiceID: "prov-zai", APIFormat: "openai", BaseURL: srv.URL + "/api/v4", CredentialSlot: "ZAI",
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.ServeDialect(w, r, DialectOpenAI) }))
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"burrow-intelligence","stream":true}`))
	req.Header.Set("Authorization", "Bearer bgw_all")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	first := make(chan string, 1)
	br := bufio.NewReader(resp.Body)
	go func() {
		line, _ := br.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: one\n" {
			t.Fatalf("first chunk = %q (status %d)", line, resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first chunk was held back until the upstream finished")
	}
	unblock()
	rest, err := io.ReadAll(br)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest = %q err = %v", rest, err)
	}
	// The upstream got its own credential, never the gateway key.
	if gotAuth != "Bearer sk-zai" || gotBody != `{"model":"glm-5.1","stream":true}` {
		t.Fatalf("upstream auth %q body %s", gotAuth, gotBody)
	}
}
