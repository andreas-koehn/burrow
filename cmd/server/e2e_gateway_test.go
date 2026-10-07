// cmd/server/e2e_gateway_test.go
//
// The global gateway (/openai/v1, /ai/v1, /anthropic) through the real
// wiring: the router, the store on a SQLite file, the AI chain with the SQL
// usage sink, the cost engine with its budget guard, and the gateway as
// newAIGateway assembles it for main. The one thing a test must replace is the
// upstream transport: hosted providers cannot be called from a test, and the
// production transport refuses a test server's address and certificate.
//
// One TLS server stands in for three providers, routed by path prefix:
// "alpha" and "beta" speak the OpenAI format, "gamma" the Anthropic one.
// Everything else goes through HTTP against the router, as an admin session.
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/metrics"
	"github.com/ankoehn/burrow/internal/store"
)

const (
	gwAlphaSecret = "sk-upstream-alpha-0001"
	gwBetaSecret  = "sk-upstream-beta-0002"
	gwGammaSecret = "sk-upstream-gamma-0003"
)

// gwSeen is one request as a stand-in provider received it.
type gwSeen struct {
	Path   string
	Header http.Header
	Body   []byte
	Model  string
}

// gwUpstream is the stand-in for the three providers. What a provider does is
// switchable per test; by default it answers like the real thing, with a
// usage object.
type gwUpstream struct {
	srv *httptest.Server

	mu       sync.Mutex
	seen     map[string][]gwSeen
	override map[string]func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

func newGWUpstream(t *testing.T) *gwUpstream {
	t.Helper()
	u := &gwUpstream{seen: map[string][]gwSeen{}, override: map[string]func(http.ResponseWriter, *http.Request, []byte) bool{}}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

// set replaces a provider's behaviour; fn returns false to fall through to the
// default answer. reset puts the default back.
func (u *gwUpstream) set(provider string, fn func(w http.ResponseWriter, r *http.Request, body []byte) bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.override[provider] = fn
}

func (u *gwUpstream) reset(provider string) { u.set(provider, nil) }

func (u *gwUpstream) calls(provider string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.seen[provider])
}

func (u *gwUpstream) last(t *testing.T, provider string) gwSeen {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.seen[provider]
	if len(s) == 0 {
		t.Fatalf("provider %s was never called", provider)
	}
	return s[len(s)-1]
}

func (u *gwUpstream) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	provider, path := parts[0], "/"+parts[1]
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	u.mu.Lock()
	u.seen[provider] = append(u.seen[provider], gwSeen{Path: path, Header: r.Header.Clone(), Body: body, Model: req.Model})
	fn := u.override[provider]
	u.mu.Unlock()

	w.Header().Set("X-Seen-Model", req.Model)
	if fn != nil && fn(w, r, body) {
		return
	}
	switch path {
	case "/v1/chat/completions":
		if req.Stream {
			gwWriteChatStream(w, nil)
			return
		}
		gwJSON(w, 200, map[string]any{
			"id": "chatcmpl-e2e", "object": "chat.completion", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "hi from " + provider}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
		})
	case "/v1/responses":
		gwJSON(w, 200, map[string]any{
			"id": "resp_e2e", "object": "response", "model": req.Model,
			"output": []map[string]any{{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": "ok"}}}},
			"usage":  map[string]int{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
		})
	case "/v1/messages":
		gwJSON(w, 200, map[string]any{
			"id": "msg_e2e", "type": "message", "role": "assistant", "model": req.Model,
			"content":     []map[string]string{{"type": "text", "text": "hi from " + provider}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 13, "output_tokens": 9},
		})
	case "/v1/messages/count_tokens":
		gwJSON(w, 200, map[string]int{"input_tokens": 13})
	default:
		http.NotFound(w, r)
	}
}

func gwJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// gwWriteChatStream writes a Chat Completions stream in three flushed chunks
// 50 ms apart, the last one carrying the usage object. beforeLast, when set,
// runs after the second chunk is out and before the last one is written.
func gwWriteChatStream(w http.ResponseWriter, beforeLast func()) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	chunk := func(s string) {
		_, _ = io.WriteString(w, "data: "+s+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	chunk(`{"id":"chatcmpl-e2e","choices":[{"index":0,"delta":{"content":"one"},"finish_reason":null}]}`)
	time.Sleep(50 * time.Millisecond)
	chunk(`{"id":"chatcmpl-e2e","choices":[{"index":0,"delta":{"content":" two"},"finish_reason":"stop"}]}`)
	time.Sleep(50 * time.Millisecond)
	if beforeLast != nil {
		beforeLast()
	}
	chunk(`{"id":"chatcmpl-e2e","choices":[],"usage":{"prompt_tokens":21,"completion_tokens":4,"total_tokens":25}}`)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// gwEnv is the relay under test and an admin session on it.
type gwEnv struct {
	srv   *httptest.Server
	hc    *http.Client // admin session (cookie jar)
	plain *http.Client // data plane: no cookies
	csrf  string
	sqldb *sql.DB
	up    *gwUpstream
}

func bootGatewayE2E(t *testing.T) *gwEnv {
	t.Helper()
	// The vault reads the environment once, when it is built.
	t.Setenv("BURROW_UPSTREAM_KEY_ALPHA", gwAlphaSecret)
	t.Setenv("BURROW_UPSTREAM_KEY_BETA", gwBetaSecret)
	t.Setenv("BURROW_UPSTREAM_KEY_GAMMA", gwGammaSecret)

	sqldb, err := db.Open(filepath.Join(t.TempDir(), "gateway-e2e.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if err := db.Migrate(sqldb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	wrapped := db.Wrap(sqldb)
	st := store.New(sqldb)
	const adminEmail, adminPass = "admin-gw@test", "password1-very-strong"
	if err := st.SeedAdmin(context.Background(), adminEmail, adminPass); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The same construction path as main: buildV04Stack makes the chain with
	// the SQL usage sink and the cost engine, buildV05Stack the vault.
	v05, err := buildV05Stack(context.Background(), wrapped, metrics.New(), log)
	if err != nil {
		t.Fatalf("buildV05Stack: %v", err)
	}
	v04, err := buildV04Stack(context.Background(), &config.ServerConfig{}, sqldb, st, log)
	if err != nil {
		t.Fatalf("buildV04Stack: %v", err)
	}
	v04.WebhookDispatcher.Start()
	t.Cleanup(v04.GuardrailAudit.stop)
	t.Cleanup(func() { v04.WebhookDispatcher.Close() })
	v04.AIChain.Semantic = v05.SemanticCache
	v04.AIChain.CredInjector = v05.CredInjector

	up := newGWUpstream(t)
	breaker, limiter := aigateway.NewBreaker(), aigateway.NewLimiter()
	gateway := newAIGateway(aiGatewayParts{
		Store: st,
		// No tunnel is connected in this test: every provider is direct.
		Tunnels:    proxyDialerAdapter{st: st, srv: &fakeHTTPTunnelLookup{}},
		Chain:      v04.AIChain,
		Vault:      v05.CredVault,
		Upstream:   up.srv.Client().Transport, // trusts the stand-in's certificate
		CostEngine: v04.CostEngine,
		BudgetTTL:  0, // a budget is seen by the very next request
		Breaker:    breaker,
		Limiter:    limiter,
		PublicHost: "gateway.test",
		Log:        log,
	})

	srv := httptest.NewServer(api.NewRouter(api.Deps{
		Users: st, Roles: st, Sessions: st, Settings: st,
		Clients: noopClientLister{}, AccessModes: st, DB: sqldb, Services: st, Log: log,
		AuditEvents: wrapped, AuditChain: api.NewAuditChainAdapter(v04.AuditLogger), AuditAppender: v04.AuditAppender,
		AIProviders: st, AIModels: st, AIGatewayKeys: st,
		AILimiter: limiter, AIBreaker: breaker, AIAttempts: st,
		AllowPrivateUpstreams: true, // the stand-in listens on loopback
		AIGateway:             gateway,
		AuthDomain:            "gateway.test",
		RateLimitDB:           wrapped, RateLimits: v04.QuotaEngine,
		Budgets: wrapped, CostEngine: v04.CostEngine, AIMetrics: wrapped,
		Bearer: api.NewStoreBearerStore(st), Automation: st,
		ServiceAIConfigs: wrapped, CredentialVault: v05.CredVault, CredentialDB: wrapped, CredentialServices: wrapped,
	}))
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 60 * time.Second}
	body, _ := json.Marshal(map[string]string{"email": adminEmail, "password": adminPass})
	resp, err := hc.Post(srv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	var csrf string
	base, _ := url.Parse(srv.URL)
	for _, ck := range jar.Cookies(base) {
		if ck.Name == "burrow_csrf" {
			csrf = ck.Value
		}
	}
	if csrf == "" {
		t.Fatal("no CSRF cookie after login")
	}
	return &gwEnv{srv: srv, hc: hc, plain: &http.Client{Timeout: 60 * time.Second}, csrf: csrf, sqldb: sqldb, up: up}
}

// admin sends a management request as the admin session.
func (e *gwEnv) admin(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", e.csrf)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// gwResp is a data-plane answer, read to the end.
type gwResp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r gwResp) code() string { return r.Header.Get("Burrow-Error-Code") }

// call sends a data-plane request. hdr holds header pairs.
func (e *gwEnv) call(t *testing.T, method, path, body string, hdr ...string) gwResp {
	t.Helper()
	resp := e.open(t, method, path, body, hdr...)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return gwResp{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// open is call without reading the body.
func (e *gwEnv) open(t *testing.T, method, path, body string, hdr ...string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := e.plain.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func bearer(key string) []string { return []string{"Authorization", "Bearer " + key} }

// gwUsage is one usage_events row.
type gwUsage struct {
	GatewayKeyID, Dialect, Provider, Requested, Target, RequestID string
	TokensIn, TokensOut, Streamed, LatencyMs, Status              int
}

func (e *gwEnv) usageCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.sqldb.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil {
		t.Fatalf("count usage rows: %v", err)
	}
	return n
}

// usageFor waits for the usage row of a request: it is written behind the
// response.
func (e *gwEnv) usageFor(t *testing.T, requestID string) gwUsage {
	t.Helper()
	if requestID == "" {
		t.Fatal("the response carries no Burrow-Request-Id")
	}
	var u gwUsage
	deadline := time.Now().Add(20 * time.Second)
	for {
		rows, err := e.sqldb.Query(`
			SELECT gateway_key_id, dialect, provider_slug, requested_model, target_model, request_id,
			       tokens_in, tokens_out, streamed, latency_ms, upstream_status
			  FROM usage_events WHERE request_id = ?`, requestID)
		if err != nil {
			t.Fatalf("read usage rows: %v", err)
		}
		n := 0
		for rows.Next() {
			n++
			if err := rows.Scan(&u.GatewayKeyID, &u.Dialect, &u.Provider, &u.Requested, &u.Target, &u.RequestID,
				&u.TokensIn, &u.TokensOut, &u.Streamed, &u.LatencyMs, &u.Status); err != nil {
				t.Fatalf("scan usage row: %v", err)
			}
		}
		_ = rows.Close()
		if n == 1 {
			return u
		}
		if n > 1 {
			t.Fatalf("request %s has %d usage rows, want exactly one", requestID, n)
		}
		if time.Now().After(deadline) {
			t.Fatalf("request %s left no usage row", requestID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settled waits until the usage rows of everything sent so far are written,
// and returns their number.
func (e *gwEnv) settled(t *testing.T) int {
	t.Helper()
	last, same := -1, 0
	for i := 0; i < 400; i++ {
		n := e.usageCount(t)
		if n == last {
			if same++; same >= 15 {
				return n
			}
		} else {
			last, same = n, 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the number of usage rows never settled")
	return 0
}

func openAIErrCode(t *testing.T, r gwResp) string {
	t.Helper()
	var b struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &b); err != nil || b.Error.Type != "burrow_error" || b.Error.Message == "" {
		t.Fatalf("not an error in the OpenAI shape: status %d body %s", r.Status, r.Body)
	}
	if r.code() != b.Error.Code {
		t.Fatalf("Burrow-Error-Code %q differs from the body's code %q", r.code(), b.Error.Code)
	}
	return b.Error.Code
}

// anthropicErr returns Burrow's code and the Anthropic error type.
func anthropicErr(t *testing.T, r gwResp) (code, typ, message string) {
	t.Helper()
	var b struct {
		Type  string `json:"type"`
		Error struct {
			Type, Message string
		} `json:"error"`
		BurrowCode string `json:"burrow_code"`
	}
	if err := json.Unmarshal(r.Body, &b); err != nil || b.Type != "error" || b.Error.Type == "" || b.Error.Message == "" {
		t.Fatalf("not an error in the Anthropic shape: status %d body %s", r.Status, r.Body)
	}
	if r.code() != b.BurrowCode {
		t.Fatalf("Burrow-Error-Code %q differs from burrow_code %q", r.code(), b.BurrowCode)
	}
	return b.BurrowCode, b.Error.Type, b.Error.Message
}

func openAIModelIDs(t *testing.T, r gwResp) []string {
	t.Helper()
	var b struct {
		Object string `json:"object"`
		Data   []struct {
			ID, Object string
		} `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &b); err != nil || r.Status != 200 || b.Object != "list" {
		t.Fatalf("not an OpenAI model list: status %d body %s", r.Status, r.Body)
	}
	ids := []string{}
	for _, m := range b.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func anthropicModelIDs(t *testing.T, r gwResp) []string {
	t.Helper()
	var b struct {
		Data []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"data"`
		HasMore *bool `json:"has_more"`
	}
	if err := json.Unmarshal(r.Body, &b); err != nil || r.Status != 200 || b.HasMore == nil {
		t.Fatalf("not an Anthropic model list: status %d body %s", r.Status, r.Body)
	}
	ids := []string{}
	for _, m := range b.Data {
		if m.Type != "model" {
			t.Fatalf("list entry of type %q: %s", m.Type, r.Body)
		}
		ids = append(ids, m.ID)
	}
	return ids
}

const (
	gwChatBody     = `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	gwMessagesBody = `{"model":"smart","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"burrow_unknown_field":{"nested":[1,2,3],"keep":"me"}}`
)

func TestE2E_Gateway(t *testing.T) {
	e := bootGatewayE2E(t)
	base := e.up.srv.URL

	// ---- 1. Setup through the API -------------------------------------
	var (
		smart              map[string]any
		k1ID, k1, k2ID, k2 string
	)
	t.Run("01 Setup through the API", func(t *testing.T) {
		for _, p := range []map[string]any{
			{"slug": "alpha", "name": "Alpha", "kind": "direct", "api_format": "openai", "base_url": base + "/alpha/v1", "credential_slot": "ALPHA", "supports_responses": true},
			{"slug": "beta", "name": "Beta", "kind": "direct", "api_format": "openai", "base_url": base + "/beta/v1", "credential_slot": "BETA"},
			{"slug": "gamma", "name": "Gamma", "kind": "direct", "api_format": "anthropic", "base_url": base + "/gamma/v1", "credential_slot": "GAMMA", "auth_header": "x-api-key", "auth_format": "{key}"},
		} {
			if code, body := e.admin(t, "POST", "/api/v1/ai/providers", p); code != http.StatusCreated {
				t.Fatalf("create provider %v: %d %s", p["slug"], code, body)
			}
		}
		// One catalog entry each, so the model lists have direct addresses to show.
		for slug, id := range map[string]string{"alpha": "m-a", "beta": "m-b", "gamma": "m-g"} {
			if code, body := e.admin(t, "POST", "/api/v1/ai/providers/"+slug+"/models", map[string]string{"id": id}); code/100 != 2 {
				t.Fatalf("add catalog entry %s/%s: %d %s", slug, id, code, body)
			}
		}
		smart = map[string]any{"name": "smart", "targets": []map[string]string{
			{"dialect": "openai", "provider": "alpha", "model": "m-a"},
			{"dialect": "openai", "provider": "beta", "model": "m-b"},
			{"dialect": "anthropic", "provider": "gamma", "model": "m-g"},
		}}
		if code, body := e.admin(t, "POST", "/api/v1/ai/models", smart); code != http.StatusCreated {
			t.Fatalf("create model smart: %d %s", code, body)
		}
		if code, body := e.admin(t, "POST", "/api/v1/ai/models", map[string]any{"name": "only-openai",
			"targets": []map[string]string{{"dialect": "openai", "provider": "alpha", "model": "m-a"}}}); code != http.StatusCreated {
			t.Fatalf("create model only-openai: %d %s", code, body)
		}
		newKey := func(body map[string]any) (id, key string) {
			t.Helper()
			code, b := e.admin(t, "POST", "/api/v1/ai/keys", body)
			var out struct{ ID, Key string }
			if err := json.Unmarshal(b, &out); code != http.StatusCreated || err != nil || !strings.HasPrefix(out.Key, "bgw_") || out.ID == "" {
				t.Fatalf("create key %v: %d %s", body["name"], code, b)
			}
			return out.ID, out.Key
		}
		k1ID, k1 = newKey(map[string]any{"name": "all"})
		k2ID, k2 = newKey(map[string]any{"name": "only-smart", "allowed_models": []string{"smart"}})
	})
	if t.Failed() {
		t.Fatal("setup failed; nothing else can be checked")
	}

	// ---- 2. OpenAI dialect, happy path, on both of its addresses ------
	t.Run("02 OpenAI dialect happy path", func(t *testing.T) {
		// The upstream takes a moment, so the latency the usage row records
		// is not rounded down to 0 ms on a fast machine.
		const upstreamTakes = 5 * time.Millisecond
		e.up.set("alpha", func(http.ResponseWriter, *http.Request, []byte) bool { time.Sleep(upstreamTakes); return false })
		defer e.up.reset("alpha")
		for _, prefix := range []string{"/openai/v1", "/ai/v1"} {
			before := e.settled(t)
			r := e.call(t, "POST", prefix+"/chat/completions", gwChatBody, bearer(k1)...)
			if r.Status != 200 {
				t.Fatalf("%s: status %d body %s", prefix, r.Status, r.Body)
			}
			id := r.Header.Get("Burrow-Request-Id")
			if r.Header.Get("Burrow-Provider") != "alpha" || r.Header.Get("Burrow-Model") != "m-a" || r.Header.Get("Burrow-Attempts") != "1" || id == "" {
				t.Fatalf("%s: headers %v", prefix, r.Header)
			}
			if r.Header.Get("X-Seen-Model") != "m-a" {
				t.Errorf("%s: X-Seen-Model = %q", prefix, r.Header.Get("X-Seen-Model"))
			}
			seen := e.up.last(t, "alpha")
			if seen.Model != "m-a" || seen.Path != "/v1/chat/completions" {
				t.Fatalf("%s: the upstream saw model %q on %s", prefix, seen.Model, seen.Path)
			}
			if got := seen.Header.Get("Authorization"); got != "Bearer "+gwAlphaSecret {
				t.Fatalf("%s: the upstream saw Authorization %q", prefix, got)
			}
			// The gateway key reached the upstream nowhere: no header, not the body.
			for name, vals := range seen.Header {
				for _, v := range vals {
					if strings.Contains(v, k1) {
						t.Fatalf("%s: the gateway key reached the upstream in header %s", prefix, name)
					}
				}
			}
			if bytes.Contains(seen.Body, []byte(k1)) {
				t.Fatalf("%s: the gateway key reached the upstream in the body", prefix)
			}
			u := e.usageFor(t, id)
			want := gwUsage{GatewayKeyID: k1ID, Dialect: "openai", Provider: "alpha", Requested: "smart", Target: "m-a", RequestID: id, TokensIn: 11, TokensOut: 7, Status: 200}
			got := u
			got.LatencyMs = 0
			if got != want {
				t.Fatalf("%s: usage row\n got %+v\nwant %+v", prefix, u, want)
			}
			if u.LatencyMs < int(upstreamTakes/time.Millisecond) {
				t.Errorf("%s: latency_ms = %d, the upstream alone took %v", prefix, u.LatencyMs, upstreamTakes)
			}
			if after := e.settled(t); after != before+1 {
				t.Fatalf("%s: %d new usage rows, want exactly one", prefix, after-before)
			}
		}
	})

	// ---- 3. Anthropic dialect -----------------------------------------
	t.Run("03 Anthropic dialect", func(t *testing.T) {
		const beta = "prompt-caching-2099-01-01,burrow-unknown-beta"
		r := e.call(t, "POST", "/anthropic/v1/messages", gwMessagesBody,
			"x-api-key", k1, "anthropic-version", "2023-06-01", "anthropic-beta", beta)
		if r.Status != 200 {
			t.Fatalf("status %d body %s", r.Status, r.Body)
		}
		if r.Header.Get("Burrow-Provider") != "gamma" || r.Header.Get("Burrow-Model") != "m-g" {
			t.Fatalf("headers %v", r.Header)
		}
		seen := e.up.last(t, "gamma")
		// Review Focus 5: byte-identical apart from the model value.
		if want := strings.Replace(gwMessagesBody, `"model":"smart"`, `"model":"m-g"`, 1); string(seen.Body) != want {
			t.Fatalf("the upstream body differs in more than the model:\n got %s\nwant %s", seen.Body, want)
		}
		if seen.Header.Get("Anthropic-Beta") != beta || seen.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Fatalf("anthropic headers at the upstream: beta %q version %q", seen.Header.Get("Anthropic-Beta"), seen.Header.Get("Anthropic-Version"))
		}
		if got := seen.Header.Get("X-Api-Key"); got != gwGammaSecret {
			t.Fatalf("the upstream saw x-api-key %q", got)
		}
		if seen.Header.Get("Authorization") != "" {
			t.Fatalf("the upstream saw an Authorization header: %q", seen.Header.Get("Authorization"))
		}
		u := e.usageFor(t, r.Header.Get("Burrow-Request-Id"))
		if u.Dialect != "anthropic" || u.Provider != "gamma" || u.Target != "m-g" || u.Requested != "smart" || u.TokensIn != 13 || u.TokensOut != 9 || u.GatewayKeyID != k1ID {
			t.Fatalf("usage row %+v", u)
		}

		// Counting tokens is served and leaves no usage row.
		before, gammaCalls := e.settled(t), e.up.calls("gamma")
		r = e.call(t, "POST", "/anthropic/v1/messages/count_tokens", gwMessagesBody, "x-api-key", k1)
		if r.Status != 200 || !bytes.Contains(r.Body, []byte(`"input_tokens"`)) {
			t.Fatalf("count_tokens: status %d body %s", r.Status, r.Body)
		}
		if e.up.calls("gamma") != gammaCalls+1 || e.up.last(t, "gamma").Path != "/v1/messages/count_tokens" {
			t.Fatalf("count_tokens did not reach gamma")
		}
		if after := e.settled(t); after != before {
			t.Fatalf("count_tokens wrote %d usage rows", after-before)
		}

		// The model list, in the Anthropic shape: the synthetic model and
		// gamma's own; nothing of the OpenAI-format providers.
		ids := anthropicModelIDs(t, e.call(t, "GET", "/anthropic/v1/models", "", "x-api-key", k1))
		if strings.Join(ids, ",") != "smart,gamma/m-g" {
			t.Fatalf("anthropic model list = %v", ids)
		}

		// An error on this endpoint is in the Anthropic shape.
		r = e.call(t, "GET", "/anthropic/v1/models", "")
		if code, typ, _ := anthropicErr(t, r); r.Status != 401 || typ != "authentication_error" || code == "" {
			t.Fatalf("no key: status %d code %q type %q", r.Status, code, typ)
		}
	})

	// ---- 4. No crossing of formats ------------------------------------
	t.Run("04 No crossing of formats", func(t *testing.T) {
		alpha, gamma := e.up.calls("alpha"), e.up.calls("gamma")
		r := e.call(t, "POST", "/anthropic/v1/messages", strings.Replace(gwMessagesBody, "smart", "only-openai", 1), "x-api-key", k1)
		code, _, msg := anthropicErr(t, r)
		if r.Status != 400 || code != "format_mismatch" || !strings.Contains(msg, "/openai/v1") {
			t.Fatalf("openai-only model on /anthropic: status %d code %q message %q", r.Status, code, msg)
		}
		r = e.call(t, "POST", "/openai/v1/chat/completions", strings.Replace(gwChatBody, "smart", "gamma/m-g", 1), bearer(k1)...)
		if c := openAIErrCode(t, r); r.Status != 400 || c != "format_mismatch" {
			t.Fatalf("anthropic address on /openai: status %d code %q", r.Status, c)
		}
		// And the direct address of an OpenAI provider on /anthropic.
		r = e.call(t, "POST", "/anthropic/v1/messages", strings.Replace(gwMessagesBody, "smart", "alpha/m-a", 1), "x-api-key", k1)
		if code, _, _ := anthropicErr(t, r); r.Status != 400 || code != "format_mismatch" {
			t.Fatalf("openai address on /anthropic: status %d code %q", r.Status, code)
		}
		if e.up.calls("alpha") != alpha || e.up.calls("gamma") != gamma {
			t.Fatal("a refused request reached an upstream")
		}
	})

	// ---- 5. Responses API ---------------------------------------------
	t.Run("05 Responses API", func(t *testing.T) {
		const body = `{"model":"smart","input":"hi","burrow_unknown":{"a":[1,2]},"store":false}`
		beta := e.up.calls("beta")
		r := e.call(t, "POST", "/openai/v1/responses", body, bearer(k1)...)
		if r.Status != 200 || r.Header.Get("Burrow-Provider") != "alpha" || r.Header.Get("Burrow-Model") != "m-a" {
			t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
		}
		seen := e.up.last(t, "alpha")
		if want := strings.Replace(body, `"model":"smart"`, `"model":"m-a"`, 1); seen.Path != "/v1/responses" || string(seen.Body) != want {
			t.Fatalf("upstream got %s on %s, want %s", seen.Body, seen.Path, want)
		}
		if e.up.calls("beta") != beta {
			t.Fatal("beta, which does not offer the Responses API, was called")
		}
		u := e.usageFor(t, r.Header.Get("Burrow-Request-Id"))
		if u.TokensIn != 5 || u.TokensOut != 3 || u.Provider != "alpha" || u.Dialect != "openai" {
			t.Fatalf("usage row %+v", u)
		}

		// With alpha's flag off no target offers it.
		off, on := false, true
		if code, b := e.admin(t, "PUT", "/api/v1/ai/providers/alpha", map[string]any{"slug": "alpha", "name": "Alpha", "supports_responses": off}); code != 200 {
			t.Fatalf("turn the flag off: %d %s", code, b)
		}
		alpha := e.up.calls("alpha")
		r = e.call(t, "POST", "/openai/v1/responses", body, bearer(k1)...)
		if c := openAIErrCode(t, r); r.Status != 400 || c != "endpoint_unsupported" {
			t.Fatalf("flag off: status %d code %q", r.Status, c)
		}
		if e.up.calls("alpha") != alpha || e.up.calls("beta") != beta {
			t.Fatal("an upstream was called for an endpoint nobody offers")
		}
		if code, b := e.admin(t, "PUT", "/api/v1/ai/providers/alpha", map[string]any{"slug": "alpha", "name": "Alpha", "supports_responses": on}); code != 200 {
			t.Fatalf("turn the flag back on: %d %s", code, b)
		}
	})

	// ---- 6. Fallback within the openai dialect -------------------------
	t.Run("06 Fallback", func(t *testing.T) {
		e.up.set("alpha", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
			gwJSON(w, 500, map[string]any{"error": map[string]string{"message": "boom"}})
			return true
		})
		defer e.up.reset("alpha")
		alpha, gamma := e.up.calls("alpha"), e.up.calls("gamma")
		r := e.call(t, "POST", "/openai/v1/chat/completions", gwChatBody, bearer(k1)...)
		if r.Status != 200 || r.Header.Get("Burrow-Provider") != "beta" || r.Header.Get("Burrow-Model") != "m-b" || r.Header.Get("Burrow-Attempts") != "2" {
			t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
		}
		if bytes.Contains(r.Body, []byte("boom")) {
			t.Fatalf("the failed attempt's body reached the client: %s", r.Body)
		}
		// Review Focus 6: both attempts got the client's body, model apart.
		a, b := e.up.last(t, "alpha"), e.up.last(t, "beta")
		if string(a.Body) != strings.Replace(gwChatBody, "smart", "m-a", 1) || string(b.Body) != strings.Replace(gwChatBody, "smart", "m-b", 1) {
			t.Fatalf("attempt bodies:\nalpha %s\nbeta  %s", a.Body, b.Body)
		}
		if b.Header.Get("Authorization") != "Bearer "+gwBetaSecret {
			t.Fatalf("beta saw Authorization %q", b.Header.Get("Authorization"))
		}
		if e.up.calls("alpha") != alpha+1 || e.up.calls("gamma") != gamma {
			t.Fatalf("alpha calls +%d, gamma calls +%d", e.up.calls("alpha")-alpha, e.up.calls("gamma")-gamma)
		}
		id := r.Header.Get("Burrow-Request-Id")
		if u := e.usageFor(t, id); u.Provider != "beta" || u.Target != "m-b" || u.Requested != "smart" || u.TokensIn != 11 {
			t.Fatalf("usage row %+v", u)
		}
		// The attempt log is written behind the response.
		type attempt struct {
			Position        int
			Provider, Model string
			Status          int
			ErrorCode       string `json:"error_code"`
		}
		var attempts []attempt
		for end := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			code, body := e.admin(t, "GET", "/api/v1/ai/requests/"+url.PathEscape(id)+"/attempts", nil)
			if code != 200 {
				t.Fatalf("attempts: %d %s", code, body)
			}
			attempts = nil
			if err := json.Unmarshal(body, &attempts); err != nil {
				t.Fatalf("attempts body %s: %v", body, err)
			}
			if len(attempts) == 2 || time.Now().After(end) {
				break
			}
		}
		if len(attempts) != 2 ||
			attempts[0].Provider != "alpha" || attempts[0].Model != "m-a" || attempts[0].Status != 500 || attempts[0].ErrorCode != "http_500" ||
			attempts[1].Provider != "beta" || attempts[1].Model != "m-b" || attempts[1].Status != 200 || attempts[1].ErrorCode != "" {
			t.Fatalf("attempts %+v", attempts)
		}
	})

	// ---- 7. Allow-list --------------------------------------------------
	t.Run("07 Allow-list", func(t *testing.T) {
		if r := e.call(t, "POST", "/openai/v1/chat/completions", gwChatBody, bearer(k2)...); r.Status != 200 {
			t.Fatalf("allowed model: status %d body %s", r.Status, r.Body)
		}
		if r := e.call(t, "POST", "/anthropic/v1/messages", gwMessagesBody, "x-api-key", k2); r.Status != 200 {
			t.Fatalf("allowed model on /anthropic: status %d body %s", r.Status, r.Body)
		}
		alpha, gamma := e.up.calls("alpha"), e.up.calls("gamma")
		chat := func(model string) string { return strings.Replace(gwChatBody, "smart", model, 1) }
		// A target of the allowed model, an existing other model, and a name
		// that does not exist: one identical denial.
		var denial string
		for _, model := range []string{"alpha/m-a", "only-openai", "no-such-model", "gamma/m-g"} {
			r := e.call(t, "POST", "/openai/v1/chat/completions", chat(model), bearer(k2)...)
			if c := openAIErrCode(t, r); r.Status != 403 || c != "model_not_allowed" {
				t.Fatalf("%s on /openai: status %d code %q", model, r.Status, c)
			}
			if denial == "" {
				denial = string(r.Body)
			} else if string(r.Body) != denial {
				t.Fatalf("the denial for %s differs: %s vs %s", model, r.Body, denial)
			}
		}
		r := e.call(t, "POST", "/anthropic/v1/messages", strings.Replace(gwMessagesBody, "smart", "gamma/m-g", 1), "x-api-key", k2)
		if code, typ, _ := anthropicErr(t, r); r.Status != 403 || code != "model_not_allowed" || typ != "permission_error" {
			t.Fatalf("gamma/m-g on /anthropic: status %d code %q type %q", r.Status, code, typ)
		}
		// The provider path is closed to it, too: inference and anything else.
		r = e.call(t, "POST", "/ai/alpha/v1/chat/completions", chat("m-a"), bearer(k2)...)
		if c := openAIErrCode(t, r); r.Status != 403 || c != "model_not_allowed" {
			t.Fatalf("provider path: status %d code %q", r.Status, c)
		}
		r = e.call(t, "POST", "/ai/gamma/v1/messages", strings.Replace(gwMessagesBody, "smart", "m-g", 1), "x-api-key", k2)
		if r.Status != 403 || r.code() != "model_not_allowed" {
			t.Fatalf("anthropic provider path: status %d code %q", r.Status, r.code())
		}
		// A model named in the URL instead of the body does not get past it.
		r = e.call(t, "POST", "/openai/v1/chat/completions?model=alpha/m-a", gwChatBody, bearer(k2)...)
		if r.Status != 403 || r.code() != "model_not_allowed" {
			t.Fatalf("model in the query: status %d code %q", r.Status, r.code())
		}
		if e.up.calls("alpha") != alpha || e.up.calls("gamma") != gamma {
			t.Fatal("a denied request reached an upstream")
		}
		// Both model lists show the key its one model, and nothing else.
		if ids := openAIModelIDs(t, e.call(t, "GET", "/openai/v1/models", "", bearer(k2)...)); strings.Join(ids, ",") != "smart" {
			t.Fatalf("openai list for the restricted key = %v", ids)
		}
		if ids := anthropicModelIDs(t, e.call(t, "GET", "/anthropic/v1/models", "", "x-api-key", k2)); strings.Join(ids, ",") != "smart" {
			t.Fatalf("anthropic list for the restricted key = %v", ids)
		}
		// The provider path's list is not forwarded for it and shows nothing.
		r = e.call(t, "GET", "/ai/alpha/v1/models", "", bearer(k2)...)
		t.Logf("provider-path model list for the restricted key: %d %s", r.Status, bytes.TrimSpace(r.Body))
		if r.Status == 200 {
			if ids := openAIModelIDs(t, r); len(ids) != 0 {
				t.Fatalf("provider-path list for the restricted key = %v", ids)
			}
		} else if r.Status != 403 {
			t.Fatalf("provider-path list: status %d body %s", r.Status, r.Body)
		}
		// The unrestricted key sees the synthetic models and the OpenAI
		// providers' catalogs, and nothing of gamma.
		ids := openAIModelIDs(t, e.call(t, "GET", "/openai/v1/models", "", bearer(k1)...))
		if strings.Join(ids, ",") != "only-openai,smart,alpha/m-a,beta/m-b" {
			t.Fatalf("openai list for the unrestricted key = %v", ids)
		}
	})

	// ---- 8. Revocation ---------------------------------------------------
	t.Run("08 Revocation", func(t *testing.T) {
		if r := e.call(t, "POST", "/openai/v1/chat/completions", gwChatBody, bearer(k2)...); r.Status != 200 {
			t.Fatalf("before revoking: status %d", r.Status)
		}
		if code, body := e.admin(t, "DELETE", "/api/v1/ai/keys/"+k2ID, nil); code != http.StatusNoContent {
			t.Fatalf("revoke: %d %s", code, body)
		}
		alpha := e.up.calls("alpha")
		// The very next request, on every door.
		r := e.call(t, "POST", "/openai/v1/chat/completions", gwChatBody, bearer(k2)...)
		if c := openAIErrCode(t, r); r.Status != 401 {
			t.Fatalf("revoked key on /openai: status %d code %q", r.Status, c)
		}
		r = e.call(t, "POST", "/anthropic/v1/messages", gwMessagesBody, "x-api-key", k2)
		if _, typ, _ := anthropicErr(t, r); r.Status != 401 || typ != "authentication_error" {
			t.Fatalf("revoked key on /anthropic: status %d type %q", r.Status, typ)
		}
		if r = e.call(t, "GET", "/openai/v1/models", "", bearer(k2)...); r.Status != 401 {
			t.Fatalf("revoked key on the model list: status %d", r.Status)
		}
		if r = e.call(t, "POST", "/ai/alpha/v1/chat/completions", gwChatBody, bearer(k2)...); r.Status != 401 {
			t.Fatalf("revoked key on the provider path: status %d", r.Status)
		}
		if e.up.calls("alpha") != alpha {
			t.Fatal("a request with a revoked key reached the upstream")
		}
	})

	// ---- 9. Concurrency limit ---------------------------------------------
	t.Run("09 Concurrency limit", func(t *testing.T) {
		inUse := func() int {
			t.Helper()
			code, body := e.admin(t, "GET", "/api/v1/ai/providers/alpha", nil)
			var p struct {
				MaxConcurrent int `json:"max_concurrent"`
				InUse         int `json:"in_use"`
			}
			if err := json.Unmarshal(body, &p); code != 200 || err != nil || p.MaxConcurrent != 1 {
				t.Fatalf("provider alpha: %d %s", code, body)
			}
			return p.InUse
		}
		one, none := 1, 0
		if code, b := e.admin(t, "PUT", "/api/v1/ai/providers/alpha", map[string]any{"slug": "alpha", "name": "Alpha", "max_concurrent": one}); code != 200 {
			t.Fatalf("set the limit: %d %s", code, b)
		}
		// A request waits for a place for as long as an attempt may take;
		// one second keeps the test short.
		quick := map[string]any{"name": "smart", "targets": smart["targets"], "attempt_timeout_s": 1, "total_timeout_s": 30}
		if code, b := e.admin(t, "PUT", "/api/v1/ai/models/smart", quick); code != 200 {
			t.Fatalf("shorten the attempt timeout: %d %s", code, b)
		}
		defer func() {
			if code, b := e.admin(t, "PUT", "/api/v1/ai/models/smart", smart); code != 200 {
				t.Errorf("restore the model: %d %s", code, b)
			}
			if code, b := e.admin(t, "PUT", "/api/v1/ai/providers/alpha", map[string]any{"slug": "alpha", "name": "Alpha", "max_concurrent": none}); code != 200 {
				t.Errorf("lift the limit: %d %s", code, b)
			}
		}()
		if n := inUse(); n != 0 {
			t.Fatalf("in use before any request = %d", n)
		}

		// The first request is a stream that has started and then stays
		// open: it holds alpha's one place until it is released.
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		e.up.set("alpha", func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if !bytes.Contains(body, []byte(`"stream":true`)) {
				return false
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one\"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
				return true
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return true
		})
		defer e.up.reset("alpha")
		first := e.open(t, "POST", "/openai/v1/chat/completions", `{"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`, bearer(k1)...)
		defer first.Body.Close()
		if first.StatusCode != 200 || first.Header.Get("Burrow-Provider") != "alpha" {
			t.Fatalf("first request: status %d headers %v", first.StatusCode, first.Header)
		}
		br := bufio.NewReader(first.Body)
		if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
			t.Fatalf("first chunk %q: %v", line, err)
		}
		if n := inUse(); n != 1 {
			t.Fatalf("in use while the first request runs = %d", n)
		}

		// The second finds alpha full and is served by beta, while the first
		// is still running.
		alpha := e.up.calls("alpha")
		second := e.call(t, "POST", "/openai/v1/chat/completions", gwChatBody, bearer(k1)...)
		if second.Status != 200 || second.Header.Get("Burrow-Provider") != "beta" || second.Header.Get("Burrow-Model") != "m-b" || second.Header.Get("Burrow-Attempts") != "2" {
			t.Fatalf("second request: status %d headers %v body %s", second.Status, second.Header, second.Body)
		}
		if e.up.calls("alpha") != alpha {
			t.Fatal("the second request was let in at alpha beyond its limit")
		}
		if n := inUse(); n != 1 {
			t.Fatalf("in use after the second request, the first still open = %d", n)
		}

		unblock()
		if rest, err := io.ReadAll(br); err != nil || !bytes.Contains(rest, []byte("[DONE]")) {
			t.Fatalf("rest of the first request %q: %v", rest, err)
		}
		// The place is given back when the handler has returned, a moment
		// after the client has the last byte.
		for end := time.Now().Add(20 * time.Second); inUse() != 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("in use after the first request ended = %d", inUse())
			}
		}
	})

	// ---- 12. Streaming (before the budget closes K1) ----------------------
	t.Run("12 Streaming", func(t *testing.T) {
		// The upstream holds its last chunk back until the client has read
		// the first one (or ten seconds have passed: then the relay buffered).
		firstRead := make(chan struct{})
		var buffered bool
		var done sync.WaitGroup
		done.Add(1)
		e.up.set("alpha", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
			defer done.Done()
			gwWriteChatStream(w, func() {
				select {
				case <-firstRead:
				case <-time.After(10 * time.Second):
					buffered = true
				}
			})
			return true
		})
		defer e.up.reset("alpha")
		resp := e.open(t, "POST", "/openai/v1/chat/completions", `{"model":"smart","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, bearer(k1)...)
		defer resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Burrow-Provider") != "alpha" {
			t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
		}
		br := bufio.NewReader(resp.Body)
		line, err := br.ReadString('\n')
		if err != nil || !strings.Contains(line, `"one"`) {
			t.Fatalf("first chunk %q: %v", line, err)
		}
		close(firstRead)
		rest, err := io.ReadAll(br)
		if err != nil || !bytes.Contains(rest, []byte("[DONE]")) || !bytes.Contains(rest, []byte(`"usage"`)) {
			t.Fatalf("rest of the stream %q: %v", rest, err)
		}
		done.Wait()
		if buffered {
			t.Fatal("the client got the first chunk only after the upstream had sent the last: the stream is buffered")
		}
		u := e.usageFor(t, resp.Header.Get("Burrow-Request-Id"))
		if u.Streamed != 1 || u.TokensIn != 21 || u.TokensOut != 4 || u.Provider != "alpha" {
			t.Fatalf("usage row %+v", u)
		}
	})

	// ---- 13. Accounting ----------------------------------------------------
	t.Run("13 Accounting", func(t *testing.T) {
		e.settled(t)
		type group struct {
			Key                 string
			Requests            int64
			TokensIn, TokensOut int64 `json:"-"`
		}
		groups := func(by string) map[string]int64 {
			t.Helper()
			code, body := e.admin(t, "GET", "/api/v1/cost/summary?window=today&group_by="+by, nil)
			var out struct {
				GroupBy string  `json:"group_by"`
				Groups  []group `json:"groups"`
			}
			if err := json.Unmarshal(body, &out); code != 200 || err != nil || out.GroupBy != by {
				t.Fatalf("summary by %s: %d %s", by, code, body)
			}
			m := map[string]int64{}
			for _, g := range out.Groups {
				m[g.Key] = g.Requests
			}
			return m
		}
		if m := groups("model"); m["smart"] < 3 {
			t.Fatalf("by model: %v", m)
		}
		if m := groups("dialect"); m["openai"] < 1 || m["anthropic"] < 1 {
			t.Fatalf("by dialect: %v", m)
		}
		if m := groups("gateway_key"); m[k1ID] < 3 || m[k2ID] < 1 {
			t.Fatalf("by gateway key: %v", m)
		}
		if m := groups("provider"); m["alpha"] < 1 || m["beta"] < 1 || m["gamma"] < 1 {
			t.Fatalf("by provider: %v", m)
		}
	})

	// ---- 10. Budget --------------------------------------------------------
	t.Run("10 Budget", func(t *testing.T) {
		e.settled(t)
		// K1 has used far more than one token today.
		code, body := e.admin(t, "POST", "/api/v1/budgets", map[string]any{
			"scope": "gateway_key", "subject_id": k1ID, "daily_tokens": 1, "action_on_exceed": "throttle_zero"})
		if code != http.StatusCreated {
			t.Fatalf("create budget: %d %s", code, body)
		}
		alpha, gamma := e.up.calls("alpha"), e.up.calls("gamma")
		r := e.call(t, "POST", "/openai/v1/chat/completions", gwChatBody, bearer(k1)...)
		if c := openAIErrCode(t, r); r.Status != 429 || c != "budget_exceeded" || r.Header.Get("Retry-After") == "" {
			t.Fatalf("/openai over budget: status %d code %q Retry-After %q body %s", r.Status, c, r.Header.Get("Retry-After"), r.Body)
		}
		r = e.call(t, "POST", "/anthropic/v1/messages", gwMessagesBody, "x-api-key", k1)
		if c, typ, _ := anthropicErr(t, r); r.Status != 429 || c != "budget_exceeded" || typ != "rate_limit_error" || r.Header.Get("Retry-After") == "" {
			t.Fatalf("/anthropic over budget: status %d code %q type %q", r.Status, c, typ)
		}
		if e.up.calls("alpha") != alpha || e.up.calls("gamma") != gamma {
			t.Fatal("a request over budget reached an upstream")
		}
		// Listing models costs nothing and still works.
		if r = e.call(t, "GET", "/openai/v1/models", "", bearer(k1)...); r.Status != 200 {
			t.Fatalf("model list over budget: status %d body %s", r.Status, r.Body)
		}
		// The budgets list shows the key over its limit.
		code, body = e.admin(t, "GET", "/api/v1/budgets", nil)
		var budgets []struct {
			Scope         string `json:"scope"`
			SubjectID     string `json:"subject_id"`
			CurrentTokens int64  `json:"current_tokens"`
			Exceeded      bool   `json:"exceeded"`
		}
		if err := json.Unmarshal(body, &budgets); code != 200 || err != nil || len(budgets) != 1 ||
			budgets[0].SubjectID != k1ID || !budgets[0].Exceeded || budgets[0].CurrentTokens <= 1 {
			t.Fatalf("budgets: %d %s", code, body)
		}
	})

	// ---- 11. Provider guard ------------------------------------------------
	t.Run("11 Provider guard", func(t *testing.T) {
		code, body := e.admin(t, "DELETE", "/api/v1/ai/providers/alpha", nil)
		if code != http.StatusConflict || !bytes.Contains(body, []byte("smart")) {
			t.Fatalf("delete a provider a model targets: %d %s", code, body)
		}
		if code, _ := e.admin(t, "GET", "/api/v1/ai/providers/alpha", nil); code != 200 {
			t.Fatalf("the provider is gone after a refused delete: %d", code)
		}
	})
}
