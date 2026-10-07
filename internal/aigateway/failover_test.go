package aigateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/cache/semantic"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/redact"
)

// scripted is a Direct factory whose upstream for each "<slug>#<slot>" is
// looked up in handlers. Calls are counted per key; built counts how often a
// credential was asked for (the factory is what reads it).
type scripted struct {
	mu        sync.Mutex
	handlers  map[string]http.HandlerFunc
	calls     map[string]int
	built     map[string]int
	bodies    []string
	lengthErr string // an attempt whose Content-Length did not match its body
}

func script(handlers map[string]http.HandlerFunc) *scripted {
	return &scripted{handlers: handlers, calls: map[string]int{}, built: map[string]int{}}
}

func (s *scripted) factory() func(db.AIProvider, aiprovider.ErrorWriter) (http.Handler, error) {
	return func(p db.AIProvider, _ aiprovider.ErrorWriter) (http.Handler, error) {
		key := p.Slug + "#" + p.CredentialSlot
		s.mu.Lock()
		s.built[key]++
		s.mu.Unlock()
		h, ok := s.handlers[key]
		if !ok {
			return nil, aiprovider.ErrNotConfigured
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.calls[key]++
			s.bodies = append(s.bodies, key+" "+string(b))
			if r.ContentLength != int64(len(b)) || r.Header.Get("Content-Length") != strconv.Itoa(len(b)) {
				s.lengthErr = fmt.Sprintf("%s: Content-Length %d / %q for %d bytes", key, r.ContentLength, r.Header.Get("Content-Length"), len(b))
			}
			s.mu.Unlock()
			h(w, r)
		}), nil
	}
}

func (s *scripted) n(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[key]
}

func (s *scripted) credentialReads(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.built[key]
}

func (s *scripted) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code); _, _ = w.Write([]byte(body)) }
}

type memAttempts struct {
	mu    sync.Mutex
	rows  []db.UsageAttempt
	block chan struct{} // non-nil: RecordAttempts waits for it
}

func (m *memAttempts) RecordAttempts(_ context.Context, a []db.UsageAttempt) error {
	if m.block != nil {
		<-m.block
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, a...)
	return nil
}

func (m *memAttempts) all() []db.UsageAttempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]db.UsageAttempt(nil), m.rows...)
}

// drained waits until the gateway's attempt log has been written and its
// writer goroutine is gone.
func drained(g *Gateway) {
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Microsecond) {
		g.attempts.mu.Lock()
		idle := !g.attempts.running && len(g.attempts.pending) == 0
		g.attempts.mu.Unlock()
		if idle {
			return
		}
	}
	panic("the attempt log was not written")
}

// failoverGateway: model "smart" → zai (slots as given), then openrouter;
// in the anthropic dialect → zai-anthropic.
func failoverGateway(s *scripted, zaiSlots string, mut func(*db.AIModel)) (*Gateway, *memAttempts) {
	g := resolveGateway()
	g.Providers = fakeProviders{
		"zai":           {Slug: "zai", Kind: "direct", ServiceID: "prov-zai", APIFormat: "openai", CredentialSlot: zaiSlots},
		"openrouter":    {Slug: "openrouter", Kind: "direct", ServiceID: "prov-or", APIFormat: "openai", CredentialSlot: "OR"},
		"ollama":        {Slug: "ollama", Kind: "tunnel", ServiceID: "svc1", APIFormat: "openai"},
		"zai-anthropic": {Slug: "zai-anthropic", Kind: "direct", ServiceID: "prov-zai-a", APIFormat: "anthropic", CredentialSlot: "ZAIA"},
	}
	g.ServicePolicy = allowAll
	m := db.AIModel{Name: "smart", Enabled: true, AttemptTimeoutS: 60000, TotalTimeoutS: 120000, Targets: []db.AIModelTarget{
		{Dialect: "openai", Position: 0, ProviderSlug: "zai", TargetModel: "glm-5.1"},
		{Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
		{Dialect: "anthropic", Position: 0, ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"},
	}}
	if mut != nil {
		mut(&m)
	}
	g.Synthetic = fakeSynthetic{"smart": m}
	g.GatewayKeys = fakeGatewayKeys{"bgw_all": {ID: "gk"}}
	g.Direct = s.factory()
	g.Breaker = NewBreaker()
	att := &memAttempts{}
	g.Attempts = att
	g.timeUnit = time.Millisecond
	return g, att
}

const smartBody = `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`

func serve(g *Gateway, r *http.Request, d *Dialect) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rec.Header().Set("Burrow-Request-Id", "req-1")
	g.ServeDialect(rec, r, d)
	drained(g)
	return rec
}

func call(g *Gateway, body string) *httptest.ResponseRecorder {
	return serve(g, post("/v1/chat/completions", "bgw_all", body), DialectOpenAI)
}

func wantHeaders(t *testing.T, rec *httptest.ResponseRecorder, provider, model, attempts string) {
	t.Helper()
	h := rec.Header()
	if h.Get("Burrow-Provider") != provider || h.Get("Burrow-Model") != model || h.Get("Burrow-Attempts") != attempts {
		t.Fatalf("Burrow-Provider %q Burrow-Model %q Burrow-Attempts %q, want %q %q %q (status %d, body %s)",
			h.Get("Burrow-Provider"), h.Get("Burrow-Model"), h.Get("Burrow-Attempts"), provider, model, attempts, rec.Code, rec.Body.String())
	}
}

// --- which failures move on, and what every attempt is sent ------------------

// Review Focus 6: every attempt gets the client's bytes with its own model in
// place of the "model" value, spliced from the original, with a matching
// Content-Length.
func TestFailover_NextTargetOn5xx(t *testing.T) {
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Upstream-Secret", "sk-zai-secret")
			w.WriteHeader(500)
			_, _ = w.Write([]byte("zai exploded: internal detail"))
		},
		"openrouter#OR": status(200, `{"ok":true}`),
	})
	g, att := failoverGateway(s, "ZAI", nil)
	const pre, post = "{ \"x_new\":{\"model\":\"smart\"},\n\t\"model\" :  ", " ,\"messages\":[{\"role\":\"user\",\"content\":\"hi sk-client-text\"}]}\n"
	rec := call(g, pre+`"smart"`+post)

	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Upstream-Secret") != "" {
		t.Fatal("a header of the failed attempt reached the client")
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	want := []string{
		"zai#ZAI " + pre + `"glm-5.1"` + post,
		"openrouter#OR " + pre + `"google/gemini-x"` + post,
	}
	if got := s.sent(); !reflect.DeepEqual(got, want) {
		t.Fatalf("bodies:\n%s", strings.Join(got, "\n"))
	}
	if s.lengthErr != "" {
		t.Fatal(s.lengthErr)
	}
	rows := att.all()
	if len(rows) != 2 {
		t.Fatalf("attempts: %+v", rows)
	}
	a, b := rows[0], rows[1]
	if a.ProviderSlug != "zai" || a.TargetModel != "glm-5.1" || a.Status != 500 || a.ErrorCode != "http_500" || a.Position != 0 || a.RequestID != "req-1" || a.Ts.IsZero() || a.DurationMs < 0 ||
		b.ProviderSlug != "openrouter" || b.TargetModel != "google/gemini-x" || b.Status != 200 || b.ErrorCode != "" || b.Position != 1 || b.RequestID != "req-1" {
		t.Fatalf("attempts: %+v", rows)
	}
	// The log holds nothing of a body, a header or a key.
	for _, secret := range []string{"sk-zai-secret", "sk-client-text", "bgw_all", "internal detail", "ZAI", "OR"} {
		if strings.Contains(fmt.Sprintf("%+v", rows), secret) {
			t.Fatalf("attempt log contains %q: %+v", secret, rows)
		}
	}
}

// Review Focus 6: a body over the limit is refused before anything is called.
func TestFailover_BodyOverTheLimitCallsNoUpstream(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, "{}"), "openrouter#OR": status(200, "{}")})
	g, att := failoverGateway(s, "ZAI", nil)
	g.MaxBody = 64
	rec := call(g, `{"model":"smart","messages":[{"role":"user","content":"`+strings.Repeat("a", 64)+`"}]}`)
	if rec.Code != 413 || rec.Header().Get("Burrow-Error-Code") != "request_too_large" {
		t.Fatalf("status %d code %q", rec.Code, rec.Header().Get("Burrow-Error-Code"))
	}
	if len(s.sent()) != 0 || s.credentialReads("zai#ZAI") != 0 || len(att.all()) != 0 {
		t.Fatalf("an upstream was touched: sent %v", s.sent())
	}
}

// Review Focus 6, behind the real chain: what redaction made of the body is
// what every attempt is sent, and one usage row names the target that answered.
func TestFailover_RealChain_SameRedactedBodyAndOneUsageRow(t *testing.T) {
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI":       status(503, "down"),
		"openrouter#OR": status(200, `{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`),
	})
	g, att := failoverGateway(s, "ZAI", nil)
	sink := chained(t, g)
	rec := call(g, `{"model":"smart","messages":[{"role":"user","content":"my code is TOPSECRET-1 ok"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	sent := s.sent()
	if len(sent) != 2 || s.lengthErr != "" {
		t.Fatalf("sent %v lengthErr %q", sent, s.lengthErr)
	}
	first := strings.Replace(strings.TrimPrefix(sent[0], "zai#ZAI "), `"model":"glm-5.1"`, `"model":X`, 1)
	second := strings.Replace(strings.TrimPrefix(sent[1], "openrouter#OR "), `"model":"google/gemini-x"`, `"model":X`, 1)
	if first != second || !strings.Contains(first, `"model":X`) || strings.Contains(first, "TOPSECRET-1") {
		t.Fatalf("attempts differ in more than the model, or are not redacted:\n%s\n%s", sent[0], sent[1])
	}
	rows := sink.all()
	if len(rows) != 1 {
		t.Fatalf("usage rows: %+v", rows)
	}
	u := rows[0]
	if u.ProviderSlug != "openrouter" || u.TargetModel != "google/gemini-x" || u.RequestedModel != "smart" || u.RequestID != "req-1" ||
		u.GatewayKeyID != "gk" || u.UpstreamStatus != 200 || u.TokensIn != 3 || u.TokensOut != 4 {
		t.Fatalf("usage row: %+v", u)
	}
	if len(att.all()) != 2 {
		t.Fatalf("attempts: %+v", att.all())
	}
}

// chained puts g behind a real chain with one redaction rule and a usage sink.
func chained(t *testing.T, g *Gateway) *recSink {
	t.Helper()
	red, err := redact.NewEngine([]redact.Rule{{ID: "t-drop", Name: "t-drop", Pattern: `TOPSECRET-\d+`, Action: redact.ActionMask, Scope: redact.ScopeRequestBody}})
	if err != nil {
		t.Fatal(err)
	}
	sink := &recSink{}
	chain := aigw.NewChain(nil, nil, nil, red, guardrails.NewEngine(), nil, nil, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	chain.Loader = cfgLoader{Redaction: &aigw.RedactionConfig{Enabled: true}}
	g.Chain = chain
	return sink
}

func TestFailover_SingleSuccessIsNotLogged(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, `{"ok":1}`), "openrouter#OR": status(200, `{"ok":2}`)})
	g, att := failoverGateway(s, "ZAI", nil)
	rec := call(g, smartBody)
	if rec.Code != 200 || rec.Body.String() != `{"ok":1}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
	if s.n("openrouter#OR") != 0 || len(att.all()) != 0 {
		t.Fatalf("openrouter calls %d, attempts %+v", s.n("openrouter#OR"), att.all())
	}
}

func TestFailover_4xxIsNotRetried(t *testing.T) {
	for _, code := range []int{400, 404, 409, 422} {
		s := script(map[string]http.HandlerFunc{"zai#ZAI": status(code, `{"error":"bad request"}`), "openrouter#OR": status(200, "{}")})
		g, att := failoverGateway(s, "ZAI", nil)
		rec := call(g, smartBody)
		if rec.Code != code || rec.Body.String() != `{"error":"bad request"}` || s.n("openrouter#OR") != 0 {
			t.Fatalf("%d: status %d body %s openrouter calls %d", code, rec.Code, rec.Body.String(), s.n("openrouter#OR"))
		}
		wantHeaders(t, rec, "zai", "glm-5.1", "1")
		rows := att.all()
		if len(rows) != 1 || rows[0].ErrorCode != "http_"+strconv.Itoa(code) {
			t.Fatalf("%d: attempts %+v", code, rows)
		}
	}
}

func TestFailover_RateLimit(t *testing.T) {
	limited := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}
	// a) the model did not opt in: the 429 is the answer.
	s := script(map[string]http.HandlerFunc{"zai#ZAI": limited, "openrouter#OR": status(200, "{}")})
	g, _ := failoverGateway(s, "ZAI", nil)
	rec := call(g, smartBody)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "7" || rec.Body.String() != `{"error":"slow down"}` || s.n("openrouter#OR") != 0 {
		t.Fatalf("flag off: status %d headers %v openrouter calls %d", rec.Code, rec.Header(), s.n("openrouter#OR"))
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")

	// b) it did.
	s = script(map[string]http.HandlerFunc{"zai#ZAI": limited, "openrouter#OR": status(200, "{}")})
	g, _ = failoverGateway(s, "ZAI", func(m *db.AIModel) { m.FallbackOnRateLimit = true })
	rec = call(g, smartBody)
	if rec.Code != 200 || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("flag on: status %d headers %v", rec.Code, rec.Header())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
}

func TestFailover_CredentialSlots(t *testing.T) {
	for name, first := range map[string]http.HandlerFunc{
		"rate limit on the first key": status(429, "slow"),
		"first key rejected":          status(502, `{"error":{"code":"upstream_auth_failed"}}`),
	} {
		s := script(map[string]http.HandlerFunc{"zai#ZAI": first, "zai#ZAI2": status(200, `{"ok":true}`), "openrouter#OR": status(200, "{}")})
		g, att := failoverGateway(s, "ZAI, ZAI2", nil) // rate-limit fallback is off: another key of the same provider is still tried
		rec := call(g, smartBody)
		if rec.Code != 200 || rec.Body.String() != `{"ok":true}` || s.n("openrouter#OR") != 0 {
			t.Fatalf("%s: status %d body %s openrouter calls %d", name, rec.Code, rec.Body.String(), s.n("openrouter#OR"))
		}
		wantHeaders(t, rec, "zai", "glm-5.1", "2")
		rows := att.all()
		if len(rows) != 2 || rows[0].Position != 0 || rows[1].Position != 1 || rows[0].ProviderSlug != "zai" || rows[1].ProviderSlug != "zai" {
			t.Fatalf("%s: attempts %+v", name, rows)
		}
	}
}

// One dead key of a provider whose other key works is not a failing provider.
func TestFailover_BreakerCountsAProviderOncePerRequest(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(502, "bad key"), "zai#ZAI2": status(200, "{}"), "openrouter#OR": status(200, "{}")})
	g, _ := failoverGateway(s, "ZAI,ZAI2", nil)
	for i := 0; i < 12; i++ {
		rec := call(g, smartBody)
		wantHeaders(t, rec, "zai", "glm-5.1", "2")
	}
	if g.Breaker.Open("zai") || s.n("openrouter#OR") != 0 {
		t.Fatal("a provider that answers on its second key was taken out of service")
	}
}

func TestFailover_AllFail(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "zai down"), "openrouter#OR": status(503, "or down")})
	g, att := failoverGateway(s, "ZAI", nil)
	rec := call(g, smartBody)
	// The last answer is the client's: nothing of an earlier one is mixed in.
	if rec.Code != 503 || rec.Body.String() != "or down" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	rows := att.all()
	if len(rows) != 2 || rows[0].ErrorCode != "http_500" || rows[1].ErrorCode != "http_503" {
		t.Fatalf("attempts: %+v", rows)
	}
}

// When no target produced a response the gateway writes the error itself, in
// the request's dialect, and names no provider.
func TestFailover_NoAnswerIsAGatewayErrorInTheDialect(t *testing.T) {
	broken := func(http.ResponseWriter, *http.Request) { panic("boom: sk-internal 10.0.0.7") }
	s := script(map[string]http.HandlerFunc{"zai#ZAI": broken, "openrouter#OR": broken, "zai-anthropic#ZAIA": broken})
	g, att := failoverGateway(s, "ZAI", nil)
	rec := call(g, smartBody)
	if rec.Code != 502 || errCode(t, rec) != "upstream_unavailable" || rec.Header().Get("Burrow-Error-Code") != "upstream_unavailable" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "", "", "2")
	for _, leak := range []string{"zai", "openrouter", "glm", "gemini", "prov-", "sk-internal", "10.0.0.7", "boom"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("the error names %q: %s", leak, rec.Body.String())
		}
	}
	rows := att.all()
	if len(rows) != 2 || rows[0].ErrorCode != "panic" || rows[0].Status != 0 || rows[1].ErrorCode != "panic" || strings.Contains(fmt.Sprintf("%+v", rows), "boom") {
		t.Fatalf("attempts: %+v", rows)
	}

	rec = serve(g, msg("/v1/messages", "bgw_all", smartBody), DialectAnthropic)
	if typ, code := anthropicErr(t, rec); rec.Code != 502 || typ != "api_error" || code != "upstream_unavailable" {
		t.Fatalf("anthropic: status %d type %q code %q body %s", rec.Code, typ, code, rec.Body.String())
	}
	wantHeaders(t, rec, "", "", "1")
}

func TestFailover_ProviderNotConfiguredFallsThrough(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"openrouter#OR": status(200, "{}")}) // no credential for zai
	g, att := failoverGateway(s, "ZAI", nil)
	rec := call(g, smartBody)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	if rows := att.all(); len(rows) != 2 || rows[0].Status != 503 || rows[0].ErrorCode != "http_503" {
		t.Fatalf("attempts: %+v", rows)
	}
}

func TestFailover_OfflineTunnelFallsThrough(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"openrouter#OR": status(200, "{}")})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "ollama", TargetModel: "mistral"},
			{Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
		}
	})
	g.Tunnels = fakeTunnels{} // no client connected
	rec := call(g, smartBody)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	if rows := att.all(); len(rows) != 2 || rows[0].ProviderSlug != "ollama" || rows[0].Status != 502 {
		t.Fatalf("attempts: %+v", rows)
	}
}

// A panic in one attempt is that attempt's failure: its timer and context are
// released and the next target is tried on a clean slate.
func TestFailover_PanicInAnAttemptMovesOn(t *testing.T) {
	var zaiCtx context.Context
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, r *http.Request) {
			zaiCtx = r.Context()
			w.Header().Set("X-Half", "written")
			panic("boom")
		},
		"openrouter#OR": status(200, `{"ok":true}`),
	})
	g, att := failoverGateway(s, "ZAI", nil)
	rec := call(g, smartBody)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` || rec.Header().Get("X-Half") != "" {
		t.Fatalf("status %d body %s headers %v", rec.Code, rec.Body.String(), rec.Header())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	if zaiCtx.Err() == nil {
		t.Fatal("the context of the attempt that panicked was not released")
	}
	if rows := att.all(); len(rows) != 2 || rows[0].ErrorCode != "panic" || rows[1].ErrorCode != "" {
		t.Fatalf("attempts: %+v", rows)
	}
}

// --- Review Focus 1: a response that has started --------------------------------

func TestFailover_NeverAfterFirstByte(t *testing.T) {
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: one\n\n"))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // what a ReverseProxy does when the upstream dies mid-stream
		},
		"openrouter#OR": status(200, "data: from openrouter\n\n"),
	})
	g, att := failoverGateway(s, "ZAI", nil)
	sink := chained(t, g)
	for i := 1; i <= 5; i++ {
		rec := httptest.NewRecorder()
		rec.Header().Set("Burrow-Request-Id", "req-"+strconv.Itoa(i))
		func() {
			defer func() {
				// The failure is not hidden from the server: the connection is aborted.
				if p := recover(); p != http.ErrAbortHandler {
					t.Fatalf("panic = %v, want http.ErrAbortHandler", p)
				}
			}()
			g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", smartBody), DialectOpenAI)
			t.Fatal("a stream that died mid-way ended as if it were complete")
		}()
		drained(g)
		if rec.Code != 200 || rec.Body.String() != "data: one\n\n" || !rec.Flushed {
			t.Fatalf("client got status %d body %q flushed %v", rec.Code, rec.Body.String(), rec.Flushed)
		}
		wantHeaders(t, rec, "zai", "glm-5.1", "1")
		if s.n("openrouter#OR") != 0 {
			t.Fatal("a second target was tried after the response had started")
		}
	}
	// Recorded as what it was in the log, and in one usage row each.
	rows := att.all()
	if len(rows) != 5 || rows[0].ErrorCode != "stream_aborted" || rows[0].Status != 200 || rows[0].ProviderSlug != "zai" {
		t.Fatalf("attempts: %+v", rows)
	}
	// For the breaker a response that started is a success, and what happens
	// to its body afterwards changes nothing: one OK per request, no failure.
	if ok, failed := reports(g.Breaker, "zai"); ok != 5 || failed != 0 || g.Breaker.Open("zai") {
		t.Fatalf("reports for zai: %d ok, %d failed; open %v", ok, failed, g.Breaker.Open("zai"))
	}
	if usage := sink.all(); len(usage) != 5 || usage[0].ProviderSlug != "zai" {
		t.Fatalf("usage rows: %+v", usage)
	}
}

// streamRig is the whole stack over real connections: a client, the relay's
// dialect endpoint behind the real chain, and one TLS upstream that plays zai
// (/zai/v1) and openrouter (/or/v1) through the real direct upstream handler.
type streamRig struct {
	g     *Gateway
	att   *memAttempts
	sink  *recSink
	front *httptest.Server
	// transport reaches the upstream; set its timeouts before the first request.
	transport *http.Transport
	mu        sync.Mutex
	hits      map[string]int
	release   chan struct{}
}

func newStreamRig(t *testing.T, zai, or http.HandlerFunc) *streamRig {
	t.Helper()
	rig := &streamRig{hits: map[string]int{}, release: make(chan struct{})}
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		who := strings.Split(r.URL.Path, "/")[1]
		rig.mu.Lock()
		rig.hits[who]++
		rig.mu.Unlock()
		if who == "zai" {
			zai(w, r)
			return
		}
		or(w, r)
	}))
	t.Cleanup(up.Close)
	rig.g, rig.att = failoverGateway(script(nil), "ZAI", nil)
	rig.transport = up.Client().Transport.(*http.Transport).Clone()
	rig.g.Direct = DirectUpstreams(vaultMap{"ZAI": "sk-zai", "OR": "sk-or"}, rig.transport)
	p := rig.g.Providers.(fakeProviders)
	zp, op := p["zai"], p["openrouter"]
	zp.BaseURL, op.BaseURL = up.URL+"/zai/v1", up.URL+"/or/v1"
	p["zai"], p["openrouter"] = zp, op
	rig.sink = chained(t, rig.g)
	rig.front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Burrow-Request-Id", "req-1")
		rig.g.ServeDialect(w, r, DialectOpenAI)
	}))
	t.Cleanup(rig.front.Close)
	t.Cleanup(func() {
		select {
		case <-rig.release:
		default:
			close(rig.release)
		}
	})
	return rig
}

func (rig *streamRig) n(who string) int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return rig.hits[who]
}

func (rig *streamRig) post(t *testing.T) *http.Response {
	t.Helper()
	return rig.postModel(t, "smart")
}

// timeouts sets the timeouts of model "smart", in milliseconds.
func (rig *streamRig) timeouts(attempt, total int) {
	m := rig.g.Synthetic.(fakeSynthetic)["smart"]
	m.AttemptTimeoutS, m.TotalTimeoutS = attempt, total
	rig.g.Synthetic.(fakeSynthetic)["smart"] = m
}

func (rig *streamRig) postModel(t *testing.T, model string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", rig.front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","stream":true}`))
	req.Header.Set("Authorization", "Bearer bgw_all")
	resp, err := rig.front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// readEvent reads one SSE event; it fails the test instead of hanging when the
// event is being held back somewhere.
func readEvent(t *testing.T, br *bufio.Reader) (string, error) {
	t.Helper()
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if err != nil || line == "\n" {
				ch <- result{sb.String(), err}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("no event arrived: the stream is being buffered")
		return "", nil
	}
}

func sseFirstThenWait(release <-chan struct{}, then func(w http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
		w.(http.Flusher).Flush()
		<-release
		then(w)
	}
}

// Review Focus 1: deciding costs the stream nothing. The first event reaches
// the client while the upstream is still producing, also when the target that
// streams is a fallback, and flushing works through the commit writer.
func TestFailover_FirstEventIsNotHeldBack(t *testing.T) {
	done := func(w http.ResponseWriter) { _, _ = w.Write([]byte("data: [DONE]\n\n")) }
	for name, c := range map[string]struct {
		zaiFails           bool
		provider, attempts string
	}{
		"first target streams":    {false, "zai", "1"},
		"fallback target streams": {true, "openrouter", "2"},
	} {
		t.Run(name, func(t *testing.T) {
			var rig *streamRig
			stream := func(w http.ResponseWriter, r *http.Request) { sseFirstThenWait(rig.release, done)(w, r) }
			zai := http.HandlerFunc(stream)
			if c.zaiFails {
				zai = status(503, "zai down")
			}
			rig = newStreamRig(t, zai, stream)
			resp := rig.post(t)
			if resp.StatusCode != 200 || resp.Header.Get("Burrow-Provider") != c.provider || resp.Header.Get("Burrow-Attempts") != c.attempts {
				t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
			}
			br := bufio.NewReader(resp.Body)
			if ev, err := readEvent(t, br); ev != "data: one\n\n" || err != nil {
				t.Fatalf("first event %q, %v", ev, err)
			}
			close(rig.release) // only now does the upstream go on
			rest, err := io.ReadAll(br)
			if string(rest) != "data: [DONE]\n\n" || err != nil {
				t.Fatalf("rest %q, %v", rest, err)
			}
		})
	}
}

// Review Focus 1, over real connections: the upstream dies after the first
// event. The client's response ends there, truncated; nothing is appended and
// the second target is never called.
func TestFailover_RealStack_UpstreamDiesMidStream(t *testing.T) {
	var rig *streamRig
	rig = newStreamRig(t,
		func(w http.ResponseWriter, r *http.Request) {
			sseFirstThenWait(rig.release, func(http.ResponseWriter) { panic(http.ErrAbortHandler) })(w, r)
		},
		status(200, "data: from openrouter\n\n"))
	resp := rig.post(t)
	br := bufio.NewReader(resp.Body)
	if ev, err := readEvent(t, br); ev != "data: one\n\n" || err != nil {
		t.Fatalf("first event %q, %v", ev, err)
	}
	close(rig.release)
	rest, err := io.ReadAll(br)
	if err == nil || len(rest) != 0 {
		t.Fatalf("after the upstream died the client read %q, err %v; want a truncated response and nothing more", rest, err)
	}
	if resp.Header.Get("Burrow-Provider") != "zai" || resp.Header.Get("Burrow-Attempts") != "1" {
		t.Fatalf("headers %v", resp.Header)
	}
	// The relay's handler finishes its bookkeeping after the connection is cut.
	for end := time.Now().Add(10 * time.Second); len(rig.sink.all()) == 0 || len(rig.att.all()) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("no usage row or no attempt row for the aborted stream")
		}
	}
	drained(rig.g)
	if rig.n("or") != 0 || rig.n("zai") != 1 {
		t.Fatalf("upstream calls: zai %d, openrouter %d", rig.n("zai"), rig.n("or"))
	}
	if rows := rig.att.all(); len(rows) != 1 || rows[0].ErrorCode != "stream_aborted" || rows[0].ProviderSlug != "zai" {
		t.Fatalf("attempts: %+v", rows)
	}
	if usage := rig.sink.all(); len(usage) != 1 || usage[0].ProviderSlug != "zai" || usage[0].TargetModel != "glm-5.1" {
		t.Fatalf("usage rows: %+v", usage)
	}
}

// A handler that returns without writing has answered 200, as under net/http.
func TestFailover_SilentHandlerIsAnEmpty200(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": func(http.ResponseWriter, *http.Request) {}, "openrouter#OR": status(200, "{}")})
	g, att := failoverGateway(s, "ZAI", nil)
	rec := call(g, smartBody)
	if rec.Code != 200 || rec.Body.Len() != 0 || s.n("openrouter#OR") != 0 || len(att.all()) != 0 {
		t.Fatalf("status %d body %q openrouter calls %d", rec.Code, rec.Body.String(), s.n("openrouter#OR"))
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
}

// --- timeouts ----------------------------------------------------------------

func untilCancelled(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

func TestFailover_AttemptTimeout(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": untilCancelled, "openrouter#OR": status(200, `{"ok":true}`)})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 30, 60000 })
	start := time.Now()
	rec := call(g, smartBody)
	if rec.Code != 200 || time.Since(start) > 10*time.Second {
		t.Fatalf("status %d after %s", rec.Code, time.Since(start))
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	rows := att.all()
	if len(rows) != 2 || rows[0].ErrorCode != "timeout" || rows[0].Status != 0 || rows[0].DurationMs < 25 {
		t.Fatalf("attempts: %+v", rows)
	}
}

// A status that arrives after the attempt's time is up is not the answer.
func TestFailover_LateAnswerAfterTheTimeoutIsDiscarded(t *testing.T) {
	late := func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(200)
		_, _ = w.Write([]byte("too late"))
	}
	s := script(map[string]http.HandlerFunc{"zai#ZAI": late, "openrouter#OR": late})
	// Each attempt ends by its own timer and answers only after it: nothing
	// here depends on how fast the machine is. The request's total time is
	// far away, so the second attempt is certainly started (that the total
	// time ends a request is TestFailover_TotalTimeout's subject).
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 30, 120000 })
	rec := call(g, smartBody)
	if rec.Code != 504 || strings.Contains(rec.Body.String(), "too late") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if rows := att.all(); len(rows) != 2 || rows[0].ErrorCode != "timeout" || rows[1].ErrorCode != "timeout" {
		t.Fatalf("attempts: %+v", rows)
	}
}

func TestFailover_TotalTimeout(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": untilCancelled, "openrouter#OR": untilCancelled})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 40, 50 })
	start := time.Now()
	rec := call(g, smartBody)
	if rec.Code != 504 || errCode(t, rec) != "gateway_timeout" || rec.Header().Get("Burrow-Error-Code") != "gateway_timeout" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if el := time.Since(start); el < 45*time.Millisecond || el > 10*time.Second {
		t.Fatalf("answered after %s", el)
	}
	if rec.Header().Get("Burrow-Provider") != "" || rec.Header().Get("Burrow-Model") != "" {
		t.Fatalf("a timeout names a provider: %v", rec.Header())
	}
	rows := att.all()
	if len(rows) == 0 || len(rows) > 2 || rows[0].ErrorCode != "timeout" {
		t.Fatalf("attempts: %+v", rows)
	}
	// A timeout is the provider's failure.
	for i := 0; i < 5; i++ {
		_ = call(g, smartBody)
	}
	if !g.Breaker.Open("zai") {
		t.Fatal("timeouts were not counted against the provider")
	}
}

func TestFailover_StreamIsNotCutByTimeouts(t *testing.T) {
	var ctxErr error
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("data: one\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(450 * time.Millisecond) // past both timeouts
			ctxErr = r.Context().Err()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		},
		"openrouter#OR": status(200, "{}"),
	})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 200, 300 })
	rec := call(g, smartBody)
	if rec.Code != 200 || rec.Body.String() != "data: one\n\ndata: [DONE]\n\n" || ctxErr != nil {
		t.Fatalf("status %d body %q upstream context %v", rec.Code, rec.Body.String(), ctxErr)
	}
	if s.n("openrouter#OR") != 0 || len(att.all()) != 0 {
		t.Fatalf("openrouter calls %d attempts %+v", s.n("openrouter#OR"), att.all())
	}
}

// --- client disconnect -------------------------------------------------------

func TestFailover_ClientGone(t *testing.T) {
	var cancel context.CancelFunc
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, _ *http.Request) {
			cancel() // the client hangs up while zai is working
			w.WriteHeader(500)
		},
		"openrouter#OR": status(200, "{}"),
	})
	g, att := failoverGateway(s, "ZAI", nil)
	for i := 0; i < 6; i++ {
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		_ = serve(g, post("/v1/chat/completions", "bgw_all", smartBody).WithContext(ctx), DialectOpenAI)
		cancel()
	}
	if s.n("openrouter#OR") != 0 {
		t.Fatal("the chain went on after the client was gone")
	}
	if g.Breaker.Open("zai") {
		t.Fatal("a client hanging up was counted against the provider")
	}
	if rows := att.all(); len(rows) != 6 || rows[0].ErrorCode != "client_closed" {
		t.Fatalf("attempts: %+v", rows)
	}

	// Gone before the first attempt: nothing is called at all.
	s2 := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, "{}"), "openrouter#OR": status(200, "{}")})
	g2, _ := failoverGateway(s2, "ZAI", nil)
	ctx, stop := context.WithCancel(context.Background())
	stop()
	_ = serve(g2, post("/v1/chat/completions", "bgw_all", smartBody).WithContext(ctx), DialectOpenAI)
	if len(s2.sent()) != 0 {
		t.Fatalf("an upstream was called for a client that was gone: %v", s2.sent())
	}
}

// --- breaker -----------------------------------------------------------------

func TestFailover_BreakerSkipsFailingProviderAndUsesItAgain(t *testing.T) {
	zaiStatus := 500
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(zaiStatus) },
		"openrouter#OR": status(200, "{}"),
	})
	g, att := failoverGateway(s, "ZAI", nil)
	now := time.Now()
	g.Breaker.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		wantHeaders(t, call(g, smartBody), "openrouter", "google/gemini-x", "2")
	}
	before := len(att.all())
	rec := call(g, smartBody)
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "1")
	if s.n("zai#ZAI") != 5 || s.credentialReads("zai#ZAI") != 5 {
		t.Fatalf("an open breaker let a call through: calls %d, credential reads %d", s.n("zai#ZAI"), s.credentialReads("zai#ZAI"))
	}
	// The skip is in the log, next to the attempt that answered.
	rows := att.all()[before:]
	if len(rows) != 2 || rows[0].ProviderSlug != "zai" || rows[0].ErrorCode != "breaker_open" || rows[0].Status != 0 || rows[0].Position != 0 ||
		rows[1].ProviderSlug != "openrouter" || rows[1].ErrorCode != "" || rows[1].Position != 1 {
		t.Fatalf("attempts of the skipping request: %+v", rows)
	}

	// The provider recovers: after the cool-down it is tried, answers, and is
	// the first choice again.
	zaiStatus = 200
	now = now.Add(31 * time.Second)
	wantHeaders(t, call(g, smartBody), "zai", "glm-5.1", "1")
	wantHeaders(t, call(g, smartBody), "zai", "glm-5.1", "1")
	if g.Breaker.Open("zai") || g.Breaker.State("zai") != BreakerClosed {
		t.Fatal("the breaker did not close after the provider answered")
	}
}

func TestFailover_AllBreakersOpenStillTries(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, `{"ok":true}`)})
	g, _ := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.Targets = m.Targets[:1] })
	for i := 0; i < 5; i++ {
		g.Breaker.Report("zai", false, 0)
	}
	if !g.Breaker.Open("zai") {
		t.Fatal("setup: breaker not open")
	}
	rec := call(g, smartBody)
	if rec.Code != 200 || s.n("zai#ZAI") != 1 {
		t.Fatalf("a model with nowhere else to go failed closed: status %d", rec.Code)
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
}

// Requests that are the client's own fault do not take a provider away from
// everyone else.
func TestFailover_ClientErrorsDoNotOpenTheBreaker(t *testing.T) {
	for _, code := range []int{400, 404, 413, 422, 429} {
		s := script(map[string]http.HandlerFunc{"zai#ZAI": status(code, "no"), "openrouter#OR": status(200, "{}")})
		g, _ := failoverGateway(s, "ZAI", nil)
		for i := 0; i < 20; i++ {
			_ = call(g, smartBody)
		}
		if g.Breaker.Open("zai") || s.n("zai#ZAI") != 20 {
			t.Fatalf("%d: breaker open %v after %d calls", code, g.Breaker.Open("zai"), s.n("zai#ZAI"))
		}
	}
}

// --- dialects and endpoints (Review Focus 4) -----------------------------------

func TestFailover_DirectAddressHasNoFallback(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "zai down"), "openrouter#OR": status(200, "{}")})
	g, _ := failoverGateway(s, "ZAI", nil)
	rec := call(g, `{"model":"zai/glm-5.1"}`)
	if rec.Code != 500 || rec.Body.String() != "zai down" || s.n("openrouter#OR") != 0 {
		t.Fatalf("status %d body %s openrouter calls %d", rec.Code, rec.Body.String(), s.n("openrouter#OR"))
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
}

func TestFailover_NeverAcrossDialects(t *testing.T) {
	handlers := map[string]http.HandlerFunc{
		"zai#ZAI": status(500, "zai down"), "openrouter#OR": status(500, "or down"), "zai-anthropic#ZAIA": status(200, `{"type":"message"}`),
	}
	s := script(handlers)
	g, _ := failoverGateway(s, "ZAI", nil)
	rec := call(g, smartBody)
	if rec.Code != 500 || rec.Body.String() != "or down" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	if s.n("zai-anthropic#ZAIA") != 0 || s.credentialReads("zai-anthropic#ZAIA") != 0 {
		t.Fatal("an anthropic target was a fallback for an openai request")
	}

	// The mirror: an anthropic request never falls over to an openai target.
	handlers["zai-anthropic#ZAIA"], handlers["zai#ZAI"], handlers["openrouter#OR"] = status(500, "za down"), status(200, "{}"), status(200, "{}")
	s = script(handlers)
	g, _ = failoverGateway(s, "ZAI", nil)
	rec = serve(g, msg("/v1/messages", "bgw_all", smartBody), DialectAnthropic)
	if rec.Code != 500 || rec.Body.String() != "za down" {
		t.Fatalf("anthropic: status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "zai-anthropic", "glm-5.1", "1")
	if s.n("zai#ZAI") != 0 || s.n("openrouter#OR") != 0 || s.credentialReads("zai#ZAI") != 0 || s.credentialReads("openrouter#OR") != 0 {
		t.Fatal("an openai target was a fallback for an anthropic request")
	}
}

// A target row may say "openai" while its provider has become an anthropic
// one (the store's format lock is check-then-write). The provider's format as
// it is now decides, on every request.
func TestFailover_ProviderFormatIsCheckedAtRequestTime(t *testing.T) {
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": status(500, "zai down"), "zai-anthropic#ZAIA": status(200, "wrong format"), "openrouter#OR": status(200, `{"ok":true}`),
	})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "zai", TargetModel: "glm-5.1"},
			{Dialect: "openai", Position: 1, ProviderSlug: "zai-anthropic", TargetModel: "glm-5.1"}, // stale row
			{Dialect: "openai", Position: 2, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
		}
	})
	rec := call(g, smartBody)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	if s.n("zai-anthropic#ZAIA") != 0 || s.credentialReads("zai-anthropic#ZAIA") != 0 {
		t.Fatal("an openai request was sent to a provider that speaks anthropic")
	}

	// The failover handler holds the line by itself, too: a candidate of
	// another format is skipped and the skip is recorded, whoever built the list.
	p := g.Providers.(fakeProviders)
	fo := g.newFailover(Resolution{Requested: "smart", Dialect: "openai"}, []candidate{
		{provider: p["zai-anthropic"], model: "glm-5.1", pos: 0},
		{provider: p["openrouter"], model: "google/gemini-x", pos: 1},
	}, aigw.NewRoute("gk", "openai", "smart", "req-2"), "req-2")
	rec = httptest.NewRecorder()
	fo.ServeHTTP(rec, post("/v1/chat/completions", "", smartBody))
	drained(g)
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "1")
	if s.n("zai-anthropic#ZAIA") != 0 || s.credentialReads("zai-anthropic#ZAIA") != 0 {
		t.Fatal("the failover handler sent an openai request to an anthropic provider")
	}
	var skip []db.UsageAttempt
	for _, r := range att.all() {
		if r.RequestID == "req-2" {
			skip = append(skip, r)
		}
	}
	if len(skip) != 2 || skip[0].ErrorCode != "dialect_mismatch" || skip[0].ProviderSlug != "zai-anthropic" || skip[0].Status != 0 {
		t.Fatalf("attempts: %+v", skip)
	}
}

func TestFailover_ResponsesSkipsTargetsWithoutTheEndpoint(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, "from zai"), "openrouter#OR": status(200, "from openrouter")})
	g, att := failoverGateway(s, "ZAI", nil)
	p := g.Providers.(fakeProviders)
	or := p["openrouter"]
	or.SupportsResponses = true
	p["openrouter"] = or

	rec := serve(g, post("/v1/responses", "bgw_all", `{"model":"smart","input":"hi"}`), DialectOpenAI)
	if rec.Code != 200 || rec.Body.String() != "from openrouter" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "1")
	if s.n("zai#ZAI") != 0 || s.credentialReads("zai#ZAI") != 0 || len(att.all()) != 0 {
		t.Fatal("a target without the Responses API was sent a Responses request")
	}

	// No target offers it: refused before any upstream, and the answer for a
	// synthetic name does not say what stands behind it.
	or.SupportsResponses = false
	p["openrouter"] = or
	s.mu.Lock()
	s.built = map[string]int{}
	s.mu.Unlock()
	rec = serve(g, post("/v1/responses", "bgw_all", `{"model":"smart","input":"hi"}`), DialectOpenAI)
	if rec.Code != 400 || errCode(t, rec) != "endpoint_unsupported" || s.n("zai#ZAI") != 0 || s.n("openrouter#OR") != 1 || len(s.built) != 0 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{"zai", "openrouter", "glm", "gemini"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("the error names %q: %s", leak, rec.Body.String())
		}
	}
	wantHeaders(t, rec, "", "", "")
}

// --- policy and credentials per target ---------------------------------------

// policyBy gives each backing service its own access mode.
func policyBy(modes map[string]string, calls *[]string) func(context.Context, string) (*proxy.Resolved, error) {
	return func(_ context.Context, serviceID string) (*proxy.Resolved, error) {
		*calls = append(*calls, serviceID)
		mode, ok := modes[serviceID]
		switch {
		case mode == "error":
			return nil, errors.New("db down: secret-detail")
		case !ok:
			mode = "api_key"
		}
		return &proxy.Resolved{ServiceID: serviceID, AccessMode: mode}, nil
	}
}

// cacheSpy is a chain that notes whether it was told to stay away from the cache.
type cacheSpy struct {
	dispatched  int
	cacheHeader string
	cacheOff    bool
}

func (c *cacheSpy) Dispatch(http.ResponseWriter, *http.Request, string, string, string, string, http.Handler) {
}

func (c *cacheSpy) DispatchMetered(w http.ResponseWriter, r *http.Request, _, _, _, _ string, _ bool, up http.Handler) {
	c.dispatched++
	c.cacheHeader = r.Header.Get("Burrow-Cache")
	c.cacheOff = aigw.CacheBypassed(r.Context())
	up.ServeHTTP(w, r)
}

func TestFailover_PolicyBeforeCredential_PerTarget(t *testing.T) {
	// a) The first target's service refuses the caller: the request ends
	// there, before the chain (so no cached answer either), and no credential
	// is read, as for a single target.
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, "{}"), "openrouter#OR": status(200, "{}")})
	g, _ := failoverGateway(s, "ZAI", nil)
	var asked []string
	g.ServicePolicy = policyBy(map[string]string{"prov-zai": "open"}, &asked)
	chain := &cacheSpy{}
	g.Chain = chain
	rec := call(g, smartBody)
	if rec.Code != 403 || errCode(t, rec) != "provider_unavailable" || chain.dispatched != 0 {
		t.Fatalf("first target refused: status %d body %s dispatched %d", rec.Code, rec.Body.String(), chain.dispatched)
	}
	if len(s.built) != 0 || len(s.sent()) != 0 {
		t.Fatalf("a credential was read or an upstream called: %v", s.built)
	}
	for _, leak := range []string{"zai", "openrouter", "glm", "prov-"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("the refusal names %q: %s", leak, rec.Body.String())
		}
	}

	// b) A fallback target's service refuses the caller: its policy is read
	// when its turn comes, its credential never, and the refusal is the answer.
	s = script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "zai down"), "openrouter#OR": status(200, "{}")})
	g, att := failoverGateway(s, "ZAI", nil)
	asked = nil
	g.ServicePolicy = policyBy(map[string]string{"prov-or": "open"}, &asked)
	rec = call(g, smartBody)
	if rec.Code != 403 || errCode(t, rec) != "provider_unavailable" {
		t.Fatalf("fallback refused: status %d body %s", rec.Code, rec.Body.String())
	}
	if s.credentialReads("openrouter#OR") != 0 || s.n("openrouter#OR") != 0 || s.n("zai#ZAI") != 1 {
		t.Fatalf("credential reads %v", s.built)
	}
	if asked[len(asked)-1] != "prov-or" {
		t.Fatalf("policy lookups: %v", asked)
	}
	if rows := att.all(); len(rows) != 2 || rows[1].Status != 403 || rows[1].ErrorCode != "http_403" {
		t.Fatalf("attempts: %+v", rows)
	}
	if g.Breaker.State("openrouter") != BreakerClosed {
		t.Fatal("a policy refusal was counted against the provider")
	}

	// c) The first target's policy cannot be read: that is no refusal, the
	// chain goes on to the next target, but the first service's cache is not
	// used for a caller its policy never saw.
	s = script(map[string]http.HandlerFunc{"zai#ZAI": status(200, "{}"), "openrouter#OR": status(200, `{"ok":true}`)})
	g, _ = failoverGateway(s, "ZAI", nil)
	asked = nil
	g.ServicePolicy = policyBy(map[string]string{"prov-zai": "error"}, &asked)
	chain = &cacheSpy{}
	g.Chain = chain
	rec = call(g, smartBody)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` || chain.dispatched != 1 || !chain.cacheOff || chain.cacheHeader != "" {
		t.Fatalf("policy unreadable: status %d body %s cache off %v cache header %q", rec.Code, rec.Body.String(), chain.cacheOff, chain.cacheHeader)
	}
	if strings.Contains(rec.Body.String(), "secret-detail") || s.credentialReads("zai#ZAI") != 0 {
		t.Fatal("a target whose policy could not be read was used")
	}

	// d) And when it can be read, the cache is left alone.
	s = script(map[string]http.HandlerFunc{"zai#ZAI": status(200, "{}")})
	g, _ = failoverGateway(s, "ZAI", nil)
	chain = &cacheSpy{}
	g.Chain = chain
	if rec = call(g, smartBody); rec.Code != 200 || chain.cacheOff || chain.cacheHeader != "" {
		t.Fatalf("status %d cache off %v cache header %q", rec.Code, chain.cacheOff, chain.cacheHeader)
	}
}

// --- accounting --------------------------------------------------------------

func TestFailover_TrustsReportedCostOnlyWhenEveryCandidateIsDirect(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, "{}"), "openrouter#OR": status(200, "{}")})
	g, _ := failoverGateway(s, "ZAI", nil)
	chain := &spyChain{}
	g.Chain = chain
	if rec := call(g, smartBody); rec.Code != 200 || !chain.trustCost || !chain.metered || chain.serviceID != "prov-zai" || chain.keyID != "" {
		t.Fatalf("all direct: status %d chain %+v", rec.Code, chain)
	}

	g, _ = failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "zai", TargetModel: "glm-5.1"},
			{Dialect: "openai", Position: 1, ProviderSlug: "ollama", TargetModel: "mistral"},
		}
	})
	chain = &spyChain{}
	g.Chain = chain
	if rec := call(g, smartBody); rec.Code != 200 || chain.trustCost {
		t.Fatalf("with a tunnel target: status %d chain %+v", rec.Code, chain)
	}
}

// A request that is not metered leaves no usage row, whatever it took to
// answer it; what was tried is still in the attempt log.
func TestFailover_UnmeteredRequestWritesNoUsageRow(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai-anthropic#ZAIA": status(503, "down"), "second#SEC": status(200, `{"input_tokens":7}`)})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = append(m.Targets, db.AIModelTarget{Dialect: "anthropic", Position: 1, ProviderSlug: "second", TargetModel: "claude-x"})
	})
	g.Providers.(fakeProviders)["second"] = db.AIProvider{Slug: "second", Kind: "direct", ServiceID: "prov-second", APIFormat: "anthropic", CredentialSlot: "SEC"}
	sink := chained(t, g)

	rec := serve(g, msg("/v1/messages/count_tokens", "bgw_all", smartBody), DialectAnthropic)
	if rec.Code != 200 || rec.Body.String() != `{"input_tokens":7}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "second", "claude-x", "2")
	if len(sink.all()) != 0 {
		t.Fatalf("usage rows for an unmetered request: %+v", sink.all())
	}
	if rows := att.all(); len(rows) != 2 || rows[1].ProviderSlug != "second" {
		t.Fatalf("attempts: %+v", rows)
	}

	// The same chain, metered: exactly one row for two attempts.
	rec = serve(g, msg("/v1/messages", "bgw_all", smartBody), DialectAnthropic)
	if usage := sink.all(); rec.Code != 200 || len(usage) != 1 || usage[0].ProviderSlug != "second" || usage[0].TargetModel != "claude-x" || usage[0].Dialect != "anthropic" {
		t.Fatalf("status %d usage rows %+v", rec.Code, usage)
	}
}

// The attempt log is written behind the response: a slow store holds nothing
// up, a stuck one cannot pile up work without bound, and the writer goroutine
// ends when there is nothing left to write.
func TestFailover_AttemptLogDoesNotBlockTheResponse(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "down"), "openrouter#OR": status(200, `{"ok":true}`)})
	g, att := failoverGateway(s, "ZAI", nil)
	att.block = make(chan struct{})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		rec.Header().Set("Burrow-Request-Id", "req-1")
		g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", smartBody), DialectOpenAI)
		done <- rec
	}()
	select {
	case rec := <-done:
		if rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the response waited for the attempt log")
	}

	// While the store is stuck, more logs queue up to a bound and no further,
	// and the overflow is reported in one line, not one per request.
	var logged syncBuffer
	g.Log = slog.New(slog.NewTextHandler(&logged, nil))
	for i := 0; i < 3*maxPendingAttemptLogs; i++ {
		g.logAttempts([]db.UsageAttempt{{RequestID: "r" + strconv.Itoa(i), ErrorCode: "http_500"}})
	}
	g.attempts.mu.Lock()
	pending, running := len(g.attempts.pending), g.attempts.running
	g.attempts.mu.Unlock()
	if pending > maxPendingAttemptLogs || !running {
		t.Fatalf("pending %d (limit %d), writer running %v", pending, maxPendingAttemptLogs, running)
	}
	if n := strings.Count(logged.String(), "attempt logs dropped"); n != 1 || !strings.Contains(logged.String(), "dropped=1\n") {
		t.Fatalf("%d warnings for the dropped logs: %s", n, logged.String())
	}
	// A flush gives up when its context ends...
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := g.FlushAttempts(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush with a stuck store: %v", err)
	}
	cancel()
	// ...and returns once everything queued is written.
	close(att.block)
	if err := g.FlushAttempts(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.attempts.mu.Lock()
	pending, running = len(g.attempts.pending), g.attempts.running
	g.attempts.mu.Unlock()
	if pending != 0 || running {
		t.Fatalf("after the flush: pending %d, writer running %v", pending, running)
	}
	if err := g.FlushAttempts(context.Background()); err != nil { // nothing to do
		t.Fatal(err)
	}
	if n := len(att.all()); n < 2 || n > 2+maxPendingAttemptLogs+1 {
		t.Fatalf("%d rows written", n)
	}
}

// A failing store does not disturb the request path.
type failingAttempts struct{}

func (failingAttempts) RecordAttempts(context.Context, []db.UsageAttempt) error {
	return errors.New("db down")
}

func TestFailover_AttemptLogFailureIsNotTheClientsProblem(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "down"), "openrouter#OR": status(200, `{"ok":true}`)})
	g, _ := failoverGateway(s, "ZAI", nil)
	g.Attempts = failingAttempts{}
	if rec := call(g, smartBody); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	g.Attempts = nil // not wired: nothing is queued
	if rec := call(g, smartBody); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
}

// Many requests at once share the breaker, the attempt log and nothing else.
func TestFailover_Concurrent(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "down"), "openrouter#OR": status(200, `{"ok":true}`)})
	g, att := failoverGateway(s, "ZAI", nil)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			rec.Header().Set("Burrow-Request-Id", "req-"+strconv.Itoa(i))
			g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", smartBody), DialectOpenAI)
			if rec.Code != 200 || rec.Header().Get("Burrow-Provider") != "openrouter" {
				t.Errorf("request %d: status %d provider %q", i, rec.Code, rec.Header().Get("Burrow-Provider"))
			}
		}(i)
	}
	wg.Wait()
	drained(g)
	seen := map[string]int{}
	for _, r := range att.all() {
		seen[r.RequestID]++
	}
	if len(seen) != 40 {
		t.Fatalf("attempt logs for %d of 40 requests", len(seen))
	}
	for id, n := range seen {
		if n != 2 {
			t.Fatalf("request %s has %d attempt rows", id, n)
		}
	}
}

// --- provider paths ----------------------------------------------------------

// /ai/<provider>/… names one provider: no fallback, no attempt header, and a
// provider with several credential slots is called with its first.
func TestFailover_ProviderPathIsUnchanged(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "zai down"), "zai#ZAI2": status(200, "{}"), "openrouter#OR": status(200, "{}")})
	g, att := failoverGateway(s, "ZAI, ZAI2", nil)
	rec := httptest.NewRecorder()
	g.Serve(rec, post("/v1/chat/completions", "bgw_all", `{"model":"glm-5.1"}`), "zai")
	drained(g)
	if rec.Code != 500 || rec.Body.String() != "zai down" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Burrow-Provider") != "zai" || rec.Header().Get("Burrow-Attempts") != "" {
		t.Fatalf("headers: %v", rec.Header())
	}
	if s.n("zai#ZAI") != 1 || s.n("zai#ZAI2") != 0 || s.n("openrouter#OR") != 0 || len(s.built) != 1 || len(att.all()) != 0 {
		t.Fatalf("calls %v, credential reads %v, attempts %+v", s.calls, s.built, att.all())
	}
}

// --- review follow-ups ---------------------------------------------------------

// endless500 answers 500 and then never stops sending.
func endless500(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(500)
	chunk := []byte(strings.Repeat("x", 1024))
	for r.Context().Err() == nil {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		time.Sleep(time.Millisecond)
	}
}

// A discarded answer is not read to its end: the move to the next target does
// not wait for a body nobody will see, and a status that did arrive stays the
// attempt's outcome.
func TestFailover_DiscardedBodyDoesNotDelayTheNextTarget(t *testing.T) {
	rig := newStreamRig(t, endless500, status(503, "or down"))
	rig.timeouts(3000, 6000)
	start := time.Now()
	resp := rig.post(t)
	body, _ := io.ReadAll(resp.Body)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the next target waited %s for the discarded body", el)
	}
	// The last answer, not a timeout of the gateway's.
	if resp.StatusCode != 503 || string(body) != "or down" || resp.Header.Get("Burrow-Attempts") != "2" {
		t.Fatalf("status %d body %q headers %v", resp.StatusCode, body, resp.Header)
	}
	for end := time.Now().Add(10 * time.Second); len(rig.att.all()) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("no attempt rows")
		}
	}
	rows := rig.att.all()
	if rows[0].ErrorCode != "http_500" || rows[0].Status != 500 || rows[1].ErrorCode != "http_503" {
		t.Fatalf("attempts: %+v", rows)
	}
}

// A direct address has one target and no model row an operator could set
// timeouts on: the failover handler puts no clock on it. A synthetic model
// with the very same target keeps its timeouts.
func TestFailover_DirectAddressHasNoTimeout(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond): // past the 60 ms and 120 ms a unit of 1 ms would give
			_, _ = w.Write([]byte(`{"ok":true}`))
		case <-r.Context().Done():
		}
	}
	s := script(map[string]http.HandlerFunc{"zai#ZAI": slow})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = m.Targets[:1]
		m.AttemptTimeoutS, m.TotalTimeoutS = 60, 120
	})
	rec := call(g, `{"model":"zai/glm-5.1"}`)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("direct address: status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")

	rec = call(g, smartBody)
	if rec.Code != 504 || errCode(t, rec) != "gateway_timeout" {
		t.Fatalf("synthetic model: status %d body %s", rec.Code, rec.Body.String())
	}
	if rows := att.all(); len(rows) != 1 || rows[0].ErrorCode != "timeout" {
		t.Fatalf("attempts: %+v", rows)
	}
}

// A hosted provider's transport gives up waiting for response headers on its
// own clock. That is a timeout in the log, whatever status the client is
// given, and the chain moves on.
func TestFailover_TransportHeaderTimeoutIsATimeout(t *testing.T) {
	stall := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}
	rig := newStreamRig(t, stall, status(200, `{"ok":true}`))
	rig.transport.ResponseHeaderTimeout = 50 * time.Millisecond
	rows := func(n int) []db.UsageAttempt {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); len(rig.att.all()) < n; time.Sleep(time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("attempt rows: %+v", rig.att.all())
			}
		}
		return rig.att.all()
	}

	resp := rig.post(t)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != `{"ok":true}` || resp.Header.Get("Burrow-Provider") != "openrouter" {
		t.Fatalf("synthetic: status %d body %s", resp.StatusCode, body)
	}
	if r := rows(2); r[0].ProviderSlug != "zai" || r[0].ErrorCode != "timeout" || r[0].Status != 0 {
		t.Fatalf("attempts: %+v", r)
	}

	// A direct address: nothing to move on to; the answer is the provider
	// handler's own 502, and the row still says what happened.
	resp = rig.postModel(t, "zai/glm-5.1")
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 502 || resp.Header.Get("Burrow-Error-Code") != "upstream_unavailable" {
		t.Fatalf("direct address: status %d headers %v", resp.StatusCode, resp.Header)
	}
	if r := rows(3); r[2].ErrorCode != "timeout" {
		t.Fatalf("attempts: %+v", r)
	}
}

// The cache is the first target's: its key is that service and that model. An
// answer another target gave after a fallover is not stored under it, or a
// request for the first target itself would be served the other one's answer.
func TestFailover_FalloverAnswerIsNotCached(t *testing.T) {
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

	zaiDown := true
	answer := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write([]byte(body))
		}
	}
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, r *http.Request) {
			if zaiDown {
				w.WriteHeader(503)
				return
			}
			answer(`{"from":"zai"}`)(w, r)
		},
		"openrouter#OR": answer(`{"from":"openrouter"}`),
	})
	g, _ := failoverGateway(s, "ZAI", nil)
	sem := &semStub{}
	chain := aigw.NewChain(exact.New(d, log), sem, nil, nil, nil, nil, nil, &recSink{}, log)
	chain.Loader = cfgLoader{
		Cache:    &exact.Settings{Enabled: true, AppliesPer: "global", TTLSeconds: 300, MaxEntries: 100, MaxPerEntryKB: 64},
		Semantic: &semantic.Settings{Enabled: true, FallbackPolicy: "treat_as_miss", PromoteOnMiss: true},
	}
	g.Chain = chain
	const rest = `,"messages":[{"role":"user","content":"hi"}]}`

	// zai is down, openrouter answers.
	rec := call(g, `{"model":"smart"`+rest)
	if rec.Code != 200 || rec.Body.String() != `{"from":"openrouter"}` {
		t.Fatalf("fallover: status %d body %s", rec.Code, rec.Body.String())
	}
	if len(sem.promoted) != 0 {
		t.Fatalf("a fallover answer was promoted to the semantic cache: %v", sem.promoted)
	}
	// zai is back. A request for zai itself gets zai's answer, not a HIT.
	zaiDown = false
	rec = call(g, `{"model":"zai/glm-5.1"`+rest)
	if rec.Code != 200 || rec.Body.String() != `{"from":"zai"}` || rec.Header().Get("Burrow-Cache") == "HIT" {
		t.Fatalf("after the fallover: status %d body %s Burrow-Cache %q", rec.Code, rec.Body.String(), rec.Header().Get("Burrow-Cache"))
	}
	// The first target's own answer is cached as before.
	n := s.n("zai#ZAI")
	rec = call(g, `{"model":"smart"`+rest)
	if rec.Body.String() != `{"from":"zai"}` || rec.Header().Get("Burrow-Cache") != "HIT" || s.n("zai#ZAI") != n {
		t.Fatalf("first target's answer: body %s Burrow-Cache %q", rec.Body.String(), rec.Header().Get("Burrow-Cache"))
	}
}

// The policy of a tunnelled first target is checked before the chain, too.
func TestFailover_TunnelFirstTargetPolicyBeforeTheChain(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"openrouter#OR": status(200, "{}")})
	g, _ := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "ollama", TargetModel: "mistral"},
			{Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
		}
	})
	g.Tunnels = fakeTunnels{res: &proxy.Resolved{ServiceID: "svc1", AccessMode: "open", LocalHost: "127.0.0.1:11434"}, upstream: http.NotFoundHandler()}
	chain := &cacheSpy{}
	g.Chain = chain
	rec := call(g, smartBody)
	if rec.Code != 403 || errCode(t, rec) != "provider_unavailable" || chain.dispatched != 0 || len(s.built) != 0 {
		t.Fatalf("status %d body %s dispatched %d", rec.Code, rec.Body.String(), chain.dispatched)
	}
	// Offline: no refusal, no cache, on to the next target.
	g.Tunnels = fakeTunnels{}
	rec = call(g, smartBody)
	// The bypass travels in the context: a header would go on to the upstream.
	if rec.Code != 200 || chain.dispatched != 1 || !chain.cacheOff || chain.cacheHeader != "" {
		t.Fatalf("offline tunnel: status %d dispatched %d cache off %v cache header %q", rec.Code, chain.dispatched, chain.cacheOff, chain.cacheHeader)
	}
}

// A recovered panic is logged with the stack that led to it.
func TestFailover_RecoveredPanicIsLoggedWithItsStack(t *testing.T) {
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI":       func(http.ResponseWriter, *http.Request) { panic("boom") },
		"openrouter#OR": status(200, "{}"),
	})
	g, _ := failoverGateway(s, "ZAI", nil)
	var buf syncBuffer
	g.Log = slog.New(slog.NewTextHandler(&buf, nil))
	if rec := call(g, smartBody); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if out := buf.String(); !strings.Contains(out, "boom") || !strings.Contains(out, "goroutine ") || !strings.Contains(out, "failover") {
		t.Fatalf("log: %s", out)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A provider with several keys takes its trial as one: when the first key
// fails, the second is still tried, and its answer decides.
func TestFailover_TrialCoversAllKeysOfAProvider(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "bad key"), "zai#ZAI2": status(200, `{"from":"zai"}`), "openrouter#OR": status(200, `{"from":"or"}`)})
	g, att := failoverGateway(s, "ZAI,ZAI2", nil)
	now := time.Now()
	g.Breaker.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		g.Breaker.Report("zai", false, 0)
	}
	now = now.Add(31 * time.Second)
	rec := call(g, smartBody)
	if rec.Body.String() != `{"from":"zai"}` {
		t.Fatalf("body %s attempts %+v", rec.Body.String(), att.all())
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "2")
	if g.Breaker.State("zai") != BreakerClosed || s.n("openrouter#OR") != 0 {
		t.Fatalf("state %s after the second key answered", g.Breaker.State("zai"))
	}
	if rows := att.all(); len(rows) != 2 || rows[0].ErrorCode != "http_500" || rows[1].ErrorCode != "" {
		t.Fatalf("attempts: %+v", rows)
	}

	// The same for a model that lists one provider twice.
	s = script(map[string]http.HandlerFunc{"openrouter#OR": status(200, "{}")})
	calls := 0
	s.handlers["zai#ZAI"] = func(w http.ResponseWriter, _ *http.Request) {
		if calls++; calls == 1 {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(`{"from":"zai"}`))
	}
	g, _ = failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "zai", TargetModel: "glm-5.1"},
			{Dialect: "openai", Position: 1, ProviderSlug: "zai", TargetModel: "glm-4"},
			{Dialect: "openai", Position: 2, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
		}
	})
	g.Breaker.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		g.Breaker.Report("zai", false, 0)
	}
	now = now.Add(31 * time.Second)
	rec = call(g, smartBody)
	wantHeaders(t, rec, "zai", "glm-4", "2")
	if g.Breaker.State("zai") != BreakerClosed {
		t.Fatalf("state %s", g.Breaker.State("zai"))
	}
}

// zaiTwice lists zai, then openrouter, then zai again.
func zaiTwice(m *db.AIModel) {
	m.Targets = []db.AIModelTarget{
		{Dialect: "openai", Position: 0, ProviderSlug: "zai", TargetModel: "glm-5.1"},
		{Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
		{Dialect: "openai", Position: 2, ProviderSlug: "zai", TargetModel: "glm-4"},
	}
}

// A provider is judged once per request, wherever in the chain its candidates
// stand: by its answer, or by its failure once nothing later in the request
// can say otherwise.
func TestFailover_ProviderListedTwiceIsReportedOnce(t *testing.T) {
	// halfOpen opens zai's breaker and moves past the cool-down: the next
	// request is zai's trial.
	halfOpen := func(g *Gateway) {
		now := time.Now()
		g.Breaker.now = func() time.Time { return now }
		for i := 0; i < 5; i++ {
			g.Breaker.Report("zai", false, 0)
		}
		now = now.Add(31 * time.Second)
	}
	failing := func() *scripted {
		return script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "zai down"), "openrouter#OR": status(500, "or down")})
	}
	orAnswers := func() *scripted {
		return script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "zai down"), "openrouter#OR": status(200, `{"from":"or"}`)})
	}

	t.Run("trial: the second listing answers and closes", func(t *testing.T) {
		calls := 0
		s := failing()
		s.handlers["zai#ZAI"] = func(w http.ResponseWriter, _ *http.Request) {
			if calls++; calls == 1 {
				w.WriteHeader(500)
				return
			}
			_, _ = w.Write([]byte(`{"from":"zai"}`))
		}
		g, _ := failoverGateway(s, "ZAI", zaiTwice)
		halfOpen(g)
		wantHeaders(t, call(g, smartBody), "zai", "glm-4", "3")
		if g.Breaker.State("zai") != BreakerClosed {
			t.Fatalf("zai answered its trial, state %s", g.Breaker.State("zai"))
		}
	})
	t.Run("closed: everything fails, not open after four requests", func(t *testing.T) {
		g, _ := failoverGateway(failing(), "ZAI", zaiTwice)
		for i := 0; i < 4; i++ {
			wantHeaders(t, call(g, smartBody), "zai", "glm-4", "3")
		}
		if g.Breaker.Open("zai") || g.Breaker.Open("openrouter") {
			t.Fatal("four failing requests opened a breaker: a provider was counted twice in one request")
		}
	})
	t.Run("closed: everything fails, open after five requests", func(t *testing.T) {
		g, _ := failoverGateway(failing(), "ZAI", zaiTwice)
		for i := 0; i < 5; i++ {
			_ = call(g, smartBody)
		}
		if !g.Breaker.Open("zai") || !g.Breaker.Open("openrouter") {
			t.Fatal("five failing requests did not open the breakers")
		}
	})
	// zai fails and openrouter answers: zai's second listing is never tried,
	// so its failure stands and counts, once.
	t.Run("closed: zai fails, openrouter answers, not open after four requests", func(t *testing.T) {
		g, _ := failoverGateway(orAnswers(), "ZAI", zaiTwice)
		for i := 0; i < 4; i++ {
			wantHeaders(t, call(g, smartBody), "openrouter", "google/gemini-x", "2")
		}
		if g.Breaker.Open("zai") {
			t.Fatal("four requests opened zai's breaker")
		}
	})
	t.Run("closed: zai fails, openrouter answers, open after five requests", func(t *testing.T) {
		s := orAnswers()
		g, _ := failoverGateway(s, "ZAI", zaiTwice)
		for i := 0; i < 5; i++ {
			wantHeaders(t, call(g, smartBody), "openrouter", "google/gemini-x", "2")
		}
		if !g.Breaker.Open("zai") || g.Breaker.State("openrouter") != BreakerClosed {
			t.Fatalf("zai failed in five requests and was not counted: zai %s, openrouter %s", g.Breaker.State("zai"), g.Breaker.State("openrouter"))
		}
		// From now on zai is skipped.
		wantHeaders(t, call(g, smartBody), "openrouter", "google/gemini-x", "1")
		if s.n("zai#ZAI") != 5 {
			t.Fatalf("zai calls: %d", s.n("zai#ZAI"))
		}
	})
	t.Run("trial: zai fails, openrouter answers, reopens", func(t *testing.T) {
		g, _ := failoverGateway(orAnswers(), "ZAI", zaiTwice)
		halfOpen(g)
		wantHeaders(t, call(g, smartBody), "openrouter", "google/gemini-x", "2")
		if g.Breaker.State("zai") != BreakerOpen {
			t.Fatalf("zai failed its trial, state %s", g.Breaker.State("zai"))
		}
	})
}

// When the total timeout ends the chain before a provider's next key is
// tried, the failure that was seen still counts, once. When the client leaves
// instead, nothing does.
func TestFailover_FailureIsReportedWhenTheDeadlineEndsTheChain(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": untilCancelled, "zai#ZAI2": status(200, "{}"), "openrouter#OR": status(200, "{}")})
	g, _ := failoverGateway(s, "ZAI,ZAI2", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 40, 40 })
	for i := 0; i < 4; i++ {
		if rec := call(g, smartBody); rec.Code != 504 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	}
	if g.Breaker.Open("zai") {
		t.Fatal("four timeouts opened the breaker: counted more than once per request")
	}
	_ = call(g, smartBody)
	if !g.Breaker.Open("zai") || s.n("zai#ZAI2") != 0 || s.n("openrouter#OR") != 0 {
		t.Fatalf("a provider that hangs on its first key was never counted: open %v, second key calls %d", g.Breaker.Open("zai"), s.n("zai#ZAI2"))
	}

	var cancel context.CancelFunc
	s = script(map[string]http.HandlerFunc{
		"zai#ZAI":       func(w http.ResponseWriter, _ *http.Request) { cancel(); w.WriteHeader(500) },
		"zai#ZAI2":      status(200, "{}"),
		"openrouter#OR": status(200, "{}"),
	})
	g, _ = failoverGateway(s, "ZAI,ZAI2", nil)
	for i := 0; i < 8; i++ {
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		_ = serve(g, post("/v1/chat/completions", "bgw_all", smartBody).WithContext(ctx), DialectOpenAI)
		cancel()
	}
	if g.Breaker.Open("zai") || s.n("zai#ZAI2") != 0 {
		t.Fatal("a client that left was counted against the provider")
	}
}

// reports returns what the breaker has been told about key in its window.
func reports(b *Breaker, key string) (ok, failed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.states[key]; s != nil {
		for _, c := range s.buckets {
			ok += c.ok
			failed += c.failed
		}
	}
	return ok, failed
}

// The breaker hears about a request at the moment a response starts: the
// answering provider is up, and the ones that failed before it failed. Neither
// waits for the end of a long stream, and a client leaving mid-stream finds
// both already said.
func TestFailover_ReportsAtCommit(t *testing.T) {
	for name, clientLeaves := range map[string]bool{"stream runs to its end": false, "client leaves mid-stream": true} {
		t.Run(name, func(t *testing.T) {
			var rig *streamRig
			rig = newStreamRig(t, status(500, "zai down"), func(w http.ResponseWriter, r *http.Request) {
				sseFirstThenWait(rig.release, func(w http.ResponseWriter) { _, _ = w.Write([]byte("data: [DONE]\n\n")) })(w, r)
			})
			m := rig.g.Synthetic.(fakeSynthetic)["smart"]
			zaiTwice(&m)
			rig.g.Synthetic.(fakeSynthetic)["smart"] = m

			resp := rig.post(t)
			br := bufio.NewReader(resp.Body)
			if ev, err := readEvent(t, br); ev != "data: one\n\n" || err != nil {
				t.Fatalf("first event %q, %v", ev, err)
			}
			check := func(when string) {
				t.Helper()
				zok, zfailed := reports(rig.g.Breaker, "zai")
				ook, ofailed := reports(rig.g.Breaker, "openrouter")
				if zok != 0 || zfailed != 1 || ook != 1 || ofailed != 0 {
					t.Fatalf("%s: zai %d ok / %d failed, openrouter %d ok / %d failed; want 0/1 and 1/0", when, zok, zfailed, ook, ofailed)
				}
			}
			check("while the stream is open")
			if clientLeaves {
				_ = resp.Body.Close()
			}
			close(rig.release)
			if !clientLeaves {
				if rest, err := io.ReadAll(br); string(rest) != "data: [DONE]\n\n" || err != nil {
					t.Fatalf("rest %q, %v", rest, err)
				}
			}
			// The relay's handler has finished when its usage row is written.
			for end := time.Now().Add(10 * time.Second); len(rig.sink.all()) == 0; time.Sleep(time.Millisecond) {
				if time.Now().After(end) {
					t.Fatal("the request did not finish")
				}
			}
			drained(rig.g)
			check("after the request")
		})
	}
}

// A half-open trial is decided by the first byte: the provider answered. That
// the stream then broke off does not reopen the breaker.
func TestFailover_TrialIsDecidedAtCommit(t *testing.T) {
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("data: one\n\n"))
			panic(http.ErrAbortHandler)
		},
	})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.Targets = m.Targets[:1] })
	now := time.Now()
	g.Breaker.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		g.Breaker.Report("zai", false, 0)
	}
	now = now.Add(31 * time.Second)
	func() {
		defer func() { _ = recover() }()
		rec := httptest.NewRecorder()
		rec.Header().Set("Burrow-Request-Id", "req-1")
		g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", smartBody), DialectOpenAI)
	}()
	drained(g)
	if g.Breaker.State("zai") != BreakerClosed {
		t.Fatalf("state %s after a trial that started its response", g.Breaker.State("zai"))
	}
	if rows := att.all(); len(rows) != 1 || rows[0].ErrorCode != "stream_aborted" {
		t.Fatalf("attempts: %+v", rows)
	}
}

// A provider the breaker refused may still be tried as a last resort, but
// that attempt tells the breaker nothing: it was not asked for.
func TestFailover_RefusedProviderIsNeverReported(t *testing.T) {
	open := func(g *Gateway) {
		for i := 0; i < 5; i++ {
			g.Breaker.Report("zai", false, 0)
		}
	}
	// zai open behind openrouter: skipped, rows say so, only openrouter is reported.
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, "{}"), "zai#ZAI2": status(200, "{}"), "openrouter#OR": status(200, `{"from":"or"}`)})
	g, att := failoverGateway(s, "ZAI,ZAI2", func(m *db.AIModel) {
		m.Targets = []db.AIModelTarget{
			{Dialect: "openai", Position: 0, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
			{Dialect: "openai", Position: 1, ProviderSlug: "zai", TargetModel: "glm-5.1"},
		}
	})
	open(g)
	wantHeaders(t, call(g, smartBody), "openrouter", "google/gemini-x", "1")
	rows := att.all()
	if len(rows) != 3 || rows[0].ErrorCode != "breaker_open" || rows[1].ErrorCode != "breaker_open" || rows[2].ProviderSlug != "openrouter" {
		t.Fatalf("attempts: %+v", rows)
	}
	if ok, failed := reports(g.Breaker, "zai"); ok != 0 || failed != 5 {
		t.Fatalf("zai was reported: %d ok, %d failed", ok, failed)
	}
	if ok, failed := reports(g.Breaker, "openrouter"); ok != 1 || failed != 0 {
		t.Fatalf("openrouter: %d ok, %d failed", ok, failed)
	}

	// zai open and the only provider: both keys are tried as a last resort,
	// whatever they return, and nothing is reported.
	for name, second := range map[string]http.HandlerFunc{"answers": status(200, `{"from":"zai"}`), "fails": status(500, "down")} {
		s = script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "down"), "zai#ZAI2": second})
		g, _ = failoverGateway(s, "ZAI,ZAI2", func(m *db.AIModel) { m.Targets = m.Targets[:1] })
		open(g)
		rec := call(g, smartBody)
		wantHeaders(t, rec, "zai", "glm-5.1", "2")
		if ok, failed := reports(g.Breaker, "zai"); ok != 0 || failed != 5 || s.n("zai#ZAI2") != 1 {
			t.Fatalf("last resort %s: zai was reported: %d ok, %d failed", name, ok, failed)
		}
	}
}

// --- Review Focus 7: the concurrency limit of a provider -----------------------

// limit gives provider slug a concurrency limit on g.
func limit(g *Gateway, slug string, n int) {
	p := g.Providers.(fakeProviders)
	v := p[slug]
	v.MaxConcurrent = n
	p[slug] = v
	if g.Limiter == nil {
		g.Limiter = NewLimiter()
	}
}

// holdFirst is an upstream whose first call waits for hold (or for its request
// to be cancelled); every call answers 200.
func holdFirst(hold <-chan struct{}) http.HandlerFunc {
	var n atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte(`{"from":"first"}`))
	}
}

// inFlight sends r and returns once it holds the one place of provider slug.
func inFlight(t *testing.T, g *Gateway, slug string, r *http.Request, d *Dialect) <-chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(g, r, d) }()
	waitFor(t, "the first request holds the place", func() bool { return g.Limiter.InUse(slug) == 1 })
	return done
}

// zaiDirect addresses zai directly: no fallback and no timer.
const zaiDirect = `{"model":"zai/glm-5.1"}`

// noPlaces fails unless the limiter is empty: nothing held, nobody waiting, no
// entry left.
func noPlaces(t *testing.T, g *Gateway) {
	t.Helper()
	for _, slug := range []string{"zai", "openrouter", "zai-anthropic", "ollama"} {
		if in, wait := g.Limiter.InUse(slug), g.Limiter.Waiting(slug); in != 0 || wait != 0 {
			t.Fatalf("%s: %d in use, %d waiting after the request ended", slug, in, wait)
		}
	}
	if n := g.Limiter.entries(); n != 0 {
		t.Fatalf("%d limiter entries left behind", n)
	}
}

func TestFailover_BusyProviderFallsThrough(t *testing.T) {
	hold := make(chan struct{})
	s := script(map[string]http.HandlerFunc{"zai#ZAI": holdFirst(hold), "openrouter#OR": status(200, `{"from":"or"}`)})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 30, 60000 })
	limit(g, "zai", 1)
	a := inFlight(t, g, "zai", post("/v1/chat/completions", "bgw_all", zaiDirect), DialectOpenAI)

	start := time.Now()
	rec := call(g, smartBody)
	if rec.Code != 200 || rec.Body.String() != `{"from":"or"}` || time.Since(start) < 25*time.Millisecond {
		t.Fatalf("status %d body %s after %s", rec.Code, rec.Body.String(), time.Since(start))
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	rows := att.all()
	if len(rows) != 2 || rows[0].ErrorCode != "busy" || rows[0].Status != 429 || rows[0].ProviderSlug != "zai" ||
		rows[0].Position != 0 || rows[0].DurationMs < 25 || rows[1].ErrorCode != "" || rows[1].ProviderSlug != "openrouter" {
		t.Fatalf("attempts: %+v", rows)
	}
	// A full provider is a healthy one: waiting in vain is never its failure.
	for i := 0; i < 6; i++ {
		if rec := call(g, smartBody); rec.Header().Get("Burrow-Provider") != "openrouter" {
			t.Fatalf("request %d: %v", i, rec.Header())
		}
	}
	if _, failed := reports(g.Breaker, "zai"); failed != 0 || g.Breaker.Open("zai") {
		t.Fatalf("a busy provider was reported to the breaker: %d failures, open %v", failed, g.Breaker.Open("zai"))
	}
	if s.n("zai#ZAI") != 1 || g.Limiter.InUse("zai") != 1 {
		t.Fatalf("zai calls %d, in use %d", s.n("zai#ZAI"), g.Limiter.InUse("zai"))
	}
	close(hold)
	if ra := <-a; ra.Code != 200 {
		t.Fatalf("the request that held the place: status %d", ra.Code)
	}
	noPlaces(t, g)
}

func TestFailover_BusyWithNoOtherTarget(t *testing.T) {
	hold := make(chan struct{})
	s := script(map[string]http.HandlerFunc{"zai#ZAI": holdFirst(hold)})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = m.Targets[:1]
		m.AttemptTimeoutS, m.TotalTimeoutS = 30, 60000
	})
	limit(g, "zai", 1)
	a := inFlight(t, g, "zai", post("/v1/chat/completions", "bgw_all", zaiDirect), DialectOpenAI)

	busy := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != 429 || errCode(t, rec) != "provider_busy" || rec.Header().Get("Burrow-Error-Code") != "provider_busy" {
			t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
		}
		if n, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || n < 1 {
			t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
		}
		wantHeaders(t, rec, "", "", "1")
		if strings.Contains(rec.Body.String(), "zai") {
			t.Fatalf("the error names the provider: %s", rec.Body.String())
		}
	}
	busy(call(g, smartBody))
	if rows := att.all(); len(rows) != 1 || rows[0].ErrorCode != "busy" || rows[0].Status != 429 {
		t.Fatalf("attempts: %+v", rows)
	}
	// A direct address has no timer of its own; its wait ends all the same
	// (60 units, here milliseconds).
	start := time.Now()
	busy(call(g, zaiDirect))
	if el := time.Since(start); el < 50*time.Millisecond || el > 10*time.Second {
		t.Fatalf("a direct address waited %s for a place", el)
	}
	if s.n("zai#ZAI") != 1 {
		t.Fatalf("zai was called %d times", s.n("zai#ZAI"))
	}
	if _, failed := reports(g.Breaker, "zai"); failed != 0 {
		t.Fatalf("%d failures reported for a busy provider", failed)
	}
	close(hold)
	<-a
	noPlaces(t, g)
}

// The total timeout ends a wait like the attempt's does, and the answer says
// what the request met: a provider with no place, not one that was silent.
func TestFailover_BusyUntilTheTotalTimeout(t *testing.T) {
	hold := make(chan struct{})
	s := script(map[string]http.HandlerFunc{"zai#ZAI": holdFirst(hold)})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Targets = m.Targets[:1]
		m.AttemptTimeoutS, m.TotalTimeoutS = 60000, 30
	})
	limit(g, "zai", 1)
	a := inFlight(t, g, "zai", post("/v1/chat/completions", "bgw_all", zaiDirect), DialectOpenAI)
	start := time.Now()
	rec := call(g, smartBody)
	if rec.Code != 429 || errCode(t, rec) != "provider_busy" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if rows := att.all(); len(rows) != 1 || rows[0].ErrorCode != "busy" {
		t.Fatalf("attempts: %+v", rows)
	}
	if el := time.Since(start); el < 25*time.Millisecond || el > 10*time.Second {
		t.Fatalf("the total timeout of 30 ms ended the wait after %s", el)
	}
	if _, failed := reports(g.Breaker, "zai"); failed != 0 {
		t.Fatalf("%d failures reported for a wait the total timeout ended", failed)
	}
	close(hold)
	<-a
	noPlaces(t, g)
}

// The attempt's clock starts when the place is obtained, not when the wait
// begins: a full but healthy provider answers everybody it admits in time.
// Requests come faster than a limit of 1 serves them; each either gets its
// answer from the provider or, after waiting an attempt's time in vain, moves
// on. None is cut short by a clock that ran while it queued, so no attempt is
// a "timeout" and the breaker hears of no failure.
func TestFailover_WaitDoesNotEatTheUpstreamsTime(t *testing.T) {
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(15 * time.Millisecond)
			_, _ = w.Write([]byte(`{"from":"zai"}`))
		},
		"openrouter#OR": status(200, `{"from":"or"}`),
	})
	// 55 ms for an answer that takes 15: a request admitted after 50 ms of
	// waiting needs its own 15 ms. (The wait itself ends after 55 ms: the
	// attempt timeout, which is below the 60 units every wait is capped at.)
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 55, 60000 })
	limit(g, "zai", 1)
	const n = 30
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			rec.Header().Set("Burrow-Request-Id", "req-"+strconv.Itoa(i))
			g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", smartBody), DialectOpenAI)
			codes[i] = rec.Code
		}(i)
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()
	drained(g)
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: status %d", i, c)
		}
	}
	busy := 0
	for _, row := range att.all() {
		switch row.ErrorCode {
		case "busy":
			busy++
		case "":
		default:
			t.Fatalf("an attempt on a healthy provider ended as %q: %+v", row.ErrorCode, row)
		}
	}
	if busy == 0 || s.n("zai#ZAI") < 2 {
		t.Fatalf("the test did not queue: %d busy rows, %d zai calls", busy, s.n("zai#ZAI"))
	}
	if ok, failed := reports(g.Breaker, "zai"); failed != 0 || ok == 0 || g.Breaker.Open("zai") {
		t.Fatalf("breaker for zai: %d ok, %d failed, open %v", ok, failed, g.Breaker.Open("zai"))
	}
	noPlaces(t, g)
}

// An upstream that is slow once the request is in still runs out of time, on
// a clock that started at admission, and that is the provider's failure.
func TestFailover_SlowAfterAdmissionStillTimesOut(t *testing.T) {
	hold := make(chan struct{})
	var calls atomic.Int32
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				<-hold
				_, _ = w.Write([]byte("{}"))
				return
			}
			<-r.Context().Done() // admitted, and then silent
		},
		"openrouter#OR": status(200, `{"from":"or"}`),
	})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 55, 60000 })
	limit(g, "zai", 1)
	a := inFlight(t, g, "zai", post("/v1/chat/completions", "bgw_all", zaiDirect), DialectOpenAI)

	b := make(chan *httptest.ResponseRecorder, 1)
	go func() { b <- call(g, smartBody) }()
	waitFor(t, "the second request waits", func() bool { return g.Limiter.Waiting("zai") == 1 })
	time.Sleep(30 * time.Millisecond) // it waits 30 of the 55 ms a wait may take
	close(hold)
	rec := <-b
	if rec.Code != 200 || rec.Body.String() != `{"from":"or"}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	rows := att.all()
	if len(rows) != 2 || rows[0].ErrorCode != "timeout" || rows[0].ProviderSlug != "zai" || rows[0].Status != 0 {
		t.Fatalf("attempts: %+v", rows)
	}
	// 30 ms in the queue and then the whole 55 ms at the upstream.
	if rows[0].DurationMs < 80 {
		t.Fatalf("the attempt ended after %d ms: its clock ran while it waited", rows[0].DurationMs)
	}
	if _, failed := reports(g.Breaker, "zai"); failed != 1 {
		t.Fatalf("failures reported for zai = %d, want 1", failed)
	}
	<-a
	noPlaces(t, g)
}

func TestFailover_BusyInTheAnthropicDialect(t *testing.T) {
	hold := make(chan struct{})
	s := script(map[string]http.HandlerFunc{"zai-anthropic#ZAIA": holdFirst(hold)})
	g, _ := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 30, 60000 })
	limit(g, "zai-anthropic", 1)
	a := inFlight(t, g, "zai-anthropic", msg("/v1/messages", "bgw_all", `{"model":"zai-anthropic/glm-5.1"}`), DialectAnthropic)
	rec := serve(g, msg("/v1/messages", "bgw_all", smartBody), DialectAnthropic)
	if typ, code := anthropicErr(t, rec); rec.Code != 429 || typ != "rate_limit_error" || code != "provider_busy" ||
		rec.Header().Get("Burrow-Error-Code") != "provider_busy" || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("status %d type %q code %q headers %v", rec.Code, typ, code, rec.Header())
	}
	close(hold)
	<-a
	noPlaces(t, g)
}

// Every credential slot of a provider shares the provider's places, and a
// provider that had none for this request is not waited for a second time.
func TestFailover_BusyCoversAllKeysOfAProvider(t *testing.T) {
	hold := make(chan struct{})
	s := script(map[string]http.HandlerFunc{
		"zai#A": holdFirst(hold), "zai#B": status(200, `{"from":"zai-b"}`), "openrouter#OR": status(200, `{"from":"or"}`),
	})
	g, att := failoverGateway(s, "A,B", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 30, 60000 })
	limit(g, "zai", 1)
	a := inFlight(t, g, "zai", post("/v1/chat/completions", "bgw_all", zaiDirect), DialectOpenAI)

	rec := call(g, smartBody)
	if rec.Code != 200 || rec.Body.String() != `{"from":"or"}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	rows := att.all()
	if len(rows) != 3 || rows[0].ErrorCode != "busy" || rows[1].ErrorCode != "busy" || rows[1].ProviderSlug != "zai" ||
		rows[1].Position != 1 || rows[1].DurationMs != 0 || rows[2].ErrorCode != "" {
		t.Fatalf("attempts: %+v", rows)
	}
	if s.n("zai#B") != 0 || s.credentialReads("zai#B") != 0 {
		t.Fatalf("the second key was used past the limit: %d calls, %d credential reads", s.n("zai#B"), s.credentialReads("zai#B"))
	}
	close(hold)
	<-a
	noPlaces(t, g)
}

func TestFailover_WaitsForAFreePlace(t *testing.T) {
	hold := make(chan struct{})
	s := script(map[string]http.HandlerFunc{"zai#ZAI": holdFirst(hold), "openrouter#OR": status(200, `{"from":"or"}`)})
	g, att := failoverGateway(s, "ZAI", nil) // attempt timeout 60 s
	limit(g, "zai", 1)
	a := inFlight(t, g, "zai", post("/v1/chat/completions", "bgw_all", zaiDirect), DialectOpenAI)

	b := make(chan *httptest.ResponseRecorder, 1)
	go func() { b <- call(g, smartBody) }()
	waitFor(t, "the second request waits", func() bool { return g.Limiter.Waiting("zai") == 1 })
	if s.n("zai#ZAI") != 1 {
		t.Fatal("the waiting request reached the upstream")
	}
	close(hold)
	rec := <-b
	if rec.Code != 200 || rec.Body.String() != `{"from":"first"}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
	if s.n("openrouter#OR") != 0 || s.n("zai#ZAI") != 2 || len(att.all()) != 0 {
		t.Fatalf("openrouter calls %d, zai calls %d, attempts %+v", s.n("openrouter#OR"), s.n("zai#ZAI"), att.all())
	}
	<-a
	noPlaces(t, g)
}

// Review Focus 7: a request cancelled while it waits leaves the queue, takes
// no place later, and is nobody's failure.
func TestFailover_CancelledWhileWaitingForAPlace(t *testing.T) {
	hold := make(chan struct{})
	s := script(map[string]http.HandlerFunc{"zai#ZAI": holdFirst(hold), "openrouter#OR": status(200, `{"from":"or"}`)})
	g, att := failoverGateway(s, "ZAI", nil)
	limit(g, "zai", 1)
	a := inFlight(t, g, "zai", post("/v1/chat/completions", "bgw_all", zaiDirect), DialectOpenAI)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		b <- serve(g, post("/v1/chat/completions", "bgw_all", smartBody).WithContext(ctx), DialectOpenAI)
	}()
	waitFor(t, "the second request waits", func() bool { return g.Limiter.Waiting("zai") == 1 })
	cancel()
	<-b
	if g.Limiter.Waiting("zai") != 0 || g.Limiter.InUse("zai") != 1 {
		t.Fatalf("after the waiter left: %d waiting, %d in use", g.Limiter.Waiting("zai"), g.Limiter.InUse("zai"))
	}
	if rows := att.all(); len(rows) != 1 || rows[0].ErrorCode != "client_closed" || rows[0].Status != 0 {
		t.Fatalf("attempts: %+v", rows)
	}
	if s.n("openrouter#OR") != 0 {
		t.Fatal("the chain went on for a client that was gone")
	}
	if _, failed := reports(g.Breaker, "zai"); failed != 0 {
		t.Fatalf("%d failures reported", failed)
	}
	close(hold)
	<-a
	noPlaces(t, g)
	if s.n("zai#ZAI") != 1 {
		t.Fatalf("zai calls = %d: the cancelled request took a place afterwards", s.n("zai#ZAI"))
	}
}

// Review Focus 7: whatever way an attempt ends, its place is back before the
// next thing happens.
func TestFailover_PlaceIsReturnedOnEveryExit(t *testing.T) {
	// zaiFree, used as openrouter's handler, proves zai's place was returned
	// before the next target was called.
	setup := func(t *testing.T, zai http.HandlerFunc, mut func(*db.AIModel)) (*Gateway, *scripted, *atomic.Int32) {
		t.Helper()
		atNext := &atomic.Int32{}
		atNext.Store(-1)
		var g *Gateway
		s := script(map[string]http.HandlerFunc{
			"zai#ZAI": func(w http.ResponseWriter, r *http.Request) {
				if n := g.Limiter.InUse("zai"); n != 1 {
					t.Errorf("in use while zai is serving = %d", n)
				}
				zai(w, r)
			},
			"openrouter#OR": func(w http.ResponseWriter, _ *http.Request) {
				atNext.Store(int32(g.Limiter.InUse("zai")))
				_, _ = w.Write([]byte(`{"from":"or"}`))
			},
		})
		g, _ = failoverGateway(s, "ZAI", mut)
		limit(g, "zai", 1)
		limit(g, "openrouter", 1)
		return g, s, atNext
	}
	nextSawFree := func(t *testing.T, rec *httptest.ResponseRecorder, atNext *atomic.Int32) {
		t.Helper()
		if rec.Code != 200 || rec.Header().Get("Burrow-Provider") != "openrouter" {
			t.Fatalf("status %d headers %v", rec.Code, rec.Header())
		}
		if n := atNext.Load(); n != 0 {
			t.Fatalf("zai places in use when the next target was called = %d", n)
		}
	}

	t.Run("success", func(t *testing.T) {
		g, _, _ := setup(t, status(200, "{}"), nil)
		if rec := call(g, smartBody); rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
		noPlaces(t, g)
	})
	t.Run("upstream error that is the answer", func(t *testing.T) {
		g, _, _ := setup(t, status(400, "{}"), nil)
		if rec := call(g, smartBody); rec.Code != 400 {
			t.Fatalf("status %d", rec.Code)
		}
		noPlaces(t, g)
	})
	t.Run("attempt discarded for the next target", func(t *testing.T) {
		g, _, atNext := setup(t, endless500, nil)
		nextSawFree(t, call(g, smartBody), atNext)
		noPlaces(t, g)
	})
	t.Run("panic before a response", func(t *testing.T) {
		g, _, atNext := setup(t, func(http.ResponseWriter, *http.Request) { panic("boom") }, nil)
		nextSawFree(t, call(g, smartBody), atNext)
		noPlaces(t, g)
	})
	t.Run("panic after the response started", func(t *testing.T) {
		g, _, _ := setup(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("data: one\n\n"))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}, nil)
		rec := httptest.NewRecorder()
		rec.Header().Set("Burrow-Request-Id", "req-1")
		func() {
			defer func() {
				if p := recover(); p != http.ErrAbortHandler {
					t.Fatalf("panic = %v, want http.ErrAbortHandler", p)
				}
			}()
			g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", smartBody), DialectOpenAI)
		}()
		drained(g)
		if rec.Body.String() != "data: one\n\n" {
			t.Fatalf("body %q", rec.Body.String())
		}
		noPlaces(t, g)
	})
	t.Run("attempt timeout", func(t *testing.T) {
		g, _, atNext := setup(t, untilCancelled, func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 30, 60000 })
		nextSawFree(t, call(g, smartBody), atNext)
		noPlaces(t, g)
	})
	t.Run("total timeout", func(t *testing.T) {
		g, s, _ := setup(t, untilCancelled, func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 40, 50 })
		s.handlers["openrouter#OR"] = untilCancelled
		if rec := call(g, smartBody); rec.Code != 504 {
			t.Fatalf("status %d", rec.Code)
		}
		noPlaces(t, g)
	})
	t.Run("client gone before a response", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g, s, _ := setup(t, func(_ http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done()
		}, nil)
		_ = serve(g, post("/v1/chat/completions", "bgw_all", smartBody).WithContext(ctx), DialectOpenAI)
		if s.n("openrouter#OR") != 0 {
			t.Fatal("the chain went on")
		}
		noPlaces(t, g)
	})
	t.Run("client gone mid-stream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("data: one\n\n"))
			w.(http.Flusher).Flush()
			cancel()
			<-r.Context().Done()
		}, nil)
		rec := serve(g, post("/v1/chat/completions", "bgw_all", smartBody).WithContext(ctx), DialectOpenAI)
		if rec.Body.String() != "data: one\n\n" {
			t.Fatalf("body %q", rec.Body.String())
		}
		noPlaces(t, g)
	})
	t.Run("target refused by its policy takes no place", func(t *testing.T) {
		g, s, atNext := setup(t, status(200, "{}"), nil)
		g.ServicePolicy = policyBy(map[string]string{"prov-zai": "public"}, &[]string{})
		_ = call(g, smartBody)
		if s.n("zai#ZAI") != 0 || atNext.Load() > 0 {
			t.Fatalf("zai calls %d, places in use at the next target %d", s.n("zai#ZAI"), atNext.Load())
		}
		noPlaces(t, g)
	})
}

// Review Focus 7, over real connections: a stream holds its place until its
// last byte, and a stream the client walks away from gives it back.
func TestFailover_StreamHoldsItsPlaceUntilTheEnd(t *testing.T) {
	for _, abandon := range []bool{false, true} {
		name := "read to the end"
		if abandon {
			name = "abandoned by the client"
		}
		t.Run(name, func(t *testing.T) {
			var rig *streamRig
			var zaiCalls atomic.Int32
			rig = newStreamRig(t,
				func(w http.ResponseWriter, r *http.Request) {
					if zaiCalls.Add(1) > 1 {
						_, _ = w.Write([]byte("data: late\n\n"))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: one\n\n"))
					w.(http.Flusher).Flush()
					select {
					case <-rig.release:
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
					case <-r.Context().Done():
					}
				},
				status(200, "data: from openrouter\n\n"))
			limit(rig.g, "zai", 1)
			// Long enough that a loaded machine still gets the first event
			// (and the third request its answer) within the attempt's time;
			// the test then waits past it once, and the second request
			// waits it out in the queue.
			const attemptMs = 1000
			rig.timeouts(attemptMs, 120000)

			resp := rig.post(t)
			br := bufio.NewReader(resp.Body)
			if ev, err := readEvent(t, br); ev != "data: one\n\n" || err != nil {
				t.Fatalf("first event %q, %v", ev, err)
			}
			// Well past the attempt timeout the stream still holds the place.
			time.Sleep(attemptMs * time.Millisecond * 3 / 2)
			if n := rig.g.Limiter.InUse("zai"); n != 1 {
				t.Fatalf("in use while the stream is open = %d", n)
			}
			// A second request is not let in on zai.
			second := rig.post(t)
			body, _ := io.ReadAll(second.Body)
			if second.Header.Get("Burrow-Provider") != "openrouter" || second.Header.Get("Burrow-Attempts") != "2" || string(body) != "data: from openrouter\n\n" {
				t.Fatalf("second request: headers %v body %q", second.Header, body)
			}
			if n := rig.g.Limiter.InUse("zai"); n != 1 || rig.n("zai") != 1 {
				t.Fatalf("in use = %d, zai calls = %d", n, rig.n("zai"))
			}

			if abandon {
				_ = resp.Body.Close()
			} else {
				close(rig.release)
				if rest, err := io.ReadAll(br); string(rest) != "data: [DONE]\n\n" || err != nil {
					t.Fatalf("rest %q, %v", rest, err)
				}
			}
			waitFor(t, "the stream's place is returned", func() bool { return rig.g.Limiter.InUse("zai") == 0 })
			waitFor(t, "the limiter is empty", func() bool { return rig.g.Limiter.entries() == 0 })
			// And the provider serves again.
			third := rig.post(t)
			body, _ = io.ReadAll(third.Body)
			if third.Header.Get("Burrow-Provider") != "zai" || string(body) != "data: late\n\n" {
				t.Fatalf("third request: headers %v body %q", third.Header, body)
			}
			waitFor(t, "the limiter is empty", func() bool { return rig.g.Limiter.entries() == 0 })
		})
	}
}

// Review Focus 7: a burst through the whole request path never has more than
// the limit in flight at the provider, and every request is served.
func TestFailover_BurstNeverExceedsTheLimit(t *testing.T) {
	const n, k = 24, 3
	var cur, peak atomic.Int32
	s := script(map[string]http.HandlerFunc{"zai#ZAI": func(w http.ResponseWriter, _ *http.Request) {
		c := cur.Add(1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		cur.Add(-1)
		_, _ = w.Write([]byte("{}"))
	}})
	g, _ := failoverGateway(s, "ZAI", nil)
	g.timeUnit = time.Second // nobody gives up waiting in this test
	limit(g, "zai", k)
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			body := smartBody
			if i%2 == 0 {
				body = zaiDirect
			}
			g.ServeDialect(rec, post("/v1/chat/completions", "bgw_all", body), DialectOpenAI)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	drained(g)
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: status %d", i, c)
		}
	}
	if p := peak.Load(); p > k || p < 2 {
		t.Fatalf("peak in flight = %d, limit %d", p, k)
	}
	noPlaces(t, g)
}

// The limit is read from the provider row of every request: raising it lets
// more in at once, without a restart, while requests admitted earlier keep
// their places.
func TestFailover_LimitChangeAppliesToTheNextRequest(t *testing.T) {
	hold := make(chan struct{})
	var calls atomic.Int32
	s := script(map[string]http.HandlerFunc{"zai#ZAI": func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-hold:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("{}"))
	}})
	g, _ := failoverGateway(s, "ZAI", nil)
	g.timeUnit = time.Second
	// Each request reads the provider row afresh; the map is swapped whole so
	// the readers race with nothing.
	var providers atomic.Value
	providers.Store(g.Providers)
	setLimit := func(n int) {
		next := fakeProviders{}
		for k, v := range providers.Load().(fakeProviders) {
			if k == "zai" {
				v.MaxConcurrent = n
			}
			next[k] = v
		}
		providers.Store(next)
	}
	g.Providers = providerFunc(func(ctx context.Context, slug string) (db.AIProvider, error) {
		return providers.Load().(fakeProviders).ProviderBySlug(ctx, slug)
	})
	g.Limiter = NewLimiter()
	setLimit(1)

	done := make(chan int, 3)
	send := func() {
		go func() { done <- call(g, zaiDirect).Code }()
	}
	send()
	waitFor(t, "first in", func() bool { return calls.Load() == 1 })
	send()
	waitFor(t, "second waits", func() bool { return g.Limiter.Waiting("zai") == 1 })
	setLimit(2)
	send() // brings the new limit: the waiter goes in ahead of it, and it waits
	waitFor(t, "the waiter is let in under the raised limit", func() bool { return calls.Load() == 2 })
	if in, waiting := g.Limiter.InUse("zai"), g.Limiter.Waiting("zai"); in != 2 || waiting != 1 {
		t.Fatalf("%d in use, %d waiting under a limit of 2", in, waiting)
	}
	setLimit(0)
	send() // no limit any more: nobody is left waiting
	waitFor(t, "everybody is in", func() bool { return calls.Load() == 4 })
	close(hold)
	for i := 0; i < 4; i++ {
		if c := <-done; c != 200 {
			t.Fatalf("status %d", c)
		}
	}
	noPlaces(t, g)
}

type providerFunc func(ctx context.Context, slug string) (db.AIProvider, error)

func (f providerFunc) ProviderBySlug(ctx context.Context, slug string) (db.AIProvider, error) {
	return f(ctx, slug)
}

// --- before admission: the target's policy and credential lookup ----------------

// The lookup that precedes an attempt's upstream call has the attempt's time,
// too: one that hangs is cut, the next target answers, and the provider,
// which was never called, is not blamed.
func TestFailover_BlockingLookupMovesOn(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, `{"from":"zai"}`), "openrouter#OR": status(200, `{"from":"or"}`)})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) { m.AttemptTimeoutS, m.TotalTimeoutS = 30, 60000 })
	limit(g, "zai", 1)
	// A request reads the first target's policy twice: once before the chain
	// (for the cache; that read is not the failover's) and once in the
	// attempt. The second one hangs.
	var zaiLookups atomic.Int32
	g.ServicePolicy = func(ctx context.Context, serviceID string) (*proxy.Resolved, error) {
		if serviceID == "prov-zai" && zaiLookups.Add(1)%2 == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(1500 * time.Millisecond):
			}
		}
		return &proxy.Resolved{ServiceID: serviceID, AccessMode: "api_key"}, nil
	}
	for i := 0; i < 6; i++ {
		start := time.Now()
		rec := call(g, smartBody)
		if el := time.Since(start); rec.Code != 200 || rec.Body.String() != `{"from":"or"}` || el < 25*time.Millisecond || el > time.Second {
			t.Fatalf("request %d: status %d body %s after %s", i, rec.Code, rec.Body.String(), el)
		}
		wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	}
	rows := att.all()
	if len(rows) != 12 || rows[0].ErrorCode != "timeout" || rows[0].ProviderSlug != "zai" || rows[0].Status != 0 || rows[1].ErrorCode != "" {
		t.Fatalf("attempts: %+v", rows)
	}
	if s.n("zai#ZAI") != 0 || s.credentialReads("zai#ZAI") != 0 {
		t.Fatalf("zai: %d calls, %d credential reads", s.n("zai#ZAI"), s.credentialReads("zai#ZAI"))
	}
	if ok, failed := reports(g.Breaker, "zai"); ok != 0 || failed != 0 || g.Breaker.Open("zai") {
		t.Fatalf("a provider that was never called was reported: %d ok, %d failed", ok, failed)
	}
	noPlaces(t, g)
}

// The request's total time ends during the lookup, which then says yes: the
// request is out of time, not turned away by a full provider. No upstream is
// called and nothing is reported.
func TestFailover_TotalTimeoutDuringTheLookupIsATimeout(t *testing.T) {
	for name, setup := range map[string]func(g *Gateway){
		"no limit":     func(g *Gateway) { limit(g, "zai", 0) },
		"no limiter":   func(g *Gateway) { g.Limiter = nil },
		"a free place": func(g *Gateway) { limit(g, "zai", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			s := script(map[string]http.HandlerFunc{"zai#ZAI": status(200, `{"from":"zai"}`)})
			g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
				m.Targets = m.Targets[:1]
				m.AttemptTimeoutS, m.TotalTimeoutS = 60000, 30
			})
			setup(g)
			g.ServicePolicy = func(_ context.Context, serviceID string) (*proxy.Resolved, error) {
				time.Sleep(60 * time.Millisecond) // past the total timeout, whatever the context says
				return &proxy.Resolved{ServiceID: serviceID, AccessMode: "api_key"}, nil
			}
			rec := call(g, smartBody)
			if rec.Code != 504 || errCode(t, rec) != "gateway_timeout" || rec.Header().Get("Retry-After") != "" {
				t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
			}
			if rows := att.all(); len(rows) != 1 || rows[0].ErrorCode != "timeout" || rows[0].Status != 0 {
				t.Fatalf("attempts: %+v", rows)
			}
			if s.n("zai#ZAI") != 0 {
				t.Fatal("the upstream was called after the request's time was up")
			}
			if ok, failed := reports(g.Breaker, "zai"); ok != 0 || failed != 0 {
				t.Fatalf("reported: %d ok, %d failed", ok, failed)
			}
			if g.Limiter != nil {
				noPlaces(t, g)
			}
		})
	}
}
