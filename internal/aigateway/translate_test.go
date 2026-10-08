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
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aigw/translate"
	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/redact"
)

// --- helpers -----------------------------------------------------------------

const fixtures = "../aigw/translate/"

func fixture(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// frames splits an event stream into its frames.
func frames(b []byte) []string {
	var out []string
	for _, f := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n\n") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// seen records what an upstream was sent.
type seen struct {
	mu   sync.Mutex
	reqs []seenReq
}

type seenReq struct {
	path, query, body string
	header            http.Header
}

func (s *seen) then(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The scripted factory has read the body; it is kept in its log.
		s.mu.Lock()
		s.reqs = append(s.reqs, seenReq{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})
		s.mu.Unlock()
		h(w, r)
	}
}

func (s *seen) one(t *testing.T) seenReq {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) != 1 {
		t.Fatalf("the upstream was called %d times, want once", len(s.reqs))
	}
	return s.reqs[0]
}

func answer(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}
}

func sse(body []byte) http.HandlerFunc      { return answer("text/event-stream", body) }
func jsonBody(body string) http.HandlerFunc { return answer("application/json", []byte(body)) }

// Targets of the test model.
var (
	tZai  = db.AIModelTarget{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm-5.1"}
	tOR   = db.AIModelTarget{Dialect: "openai", ProviderSlug: "openrouter", TargetModel: "google/gemini-x"}
	tZaiA = db.AIModelTarget{Dialect: "anthropic", ProviderSlug: "zai-anthropic", TargetModel: "claude-x"}
)

// translateGateway is failoverGateway with model "smart" made of targets and
// the translate flag as given.
func translateGateway(s *scripted, on bool, targets ...db.AIModelTarget) (*Gateway, *memAttempts) {
	return failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Translate = on
		m.Targets = nil
		pos := map[string]int{}
		for _, t := range targets {
			t.Position = pos[t.Dialect]
			pos[t.Dialect]++
			m.Targets = append(m.Targets, t)
		}
	})
}

// anthropicPost is a request as Claude Code sends it.
func anthropicPost(path, body string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Api-Key", "bgw_all")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14")
	r.Header.Set("Accept-Encoding", "gzip, br")
	r.Header.Set("Cookie", "burrow_session=abc")
	r.Header.Set("User-Agent", "claude-cli/2.0")
	r.Header.Set("X-Stainless-Lang", "js")
	return r
}

const (
	messagesHi = `{"model":"smart","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	chatHi     = `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	chatAnswer = `{"id":"chatcmpl-1","object":"chat.completion","model":"m-chat","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there."},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`
)

func oneRow(t *testing.T, sink *recSink) aimeter.Sample {
	t.Helper()
	rows := sink.all()
	if len(rows) != 1 {
		t.Fatalf("usage rows: %+v", rows)
	}
	return rows[0]
}

// --- candidates --------------------------------------------------------------

func TestTranslate_Candidates(t *testing.T) {
	a := db.AIProvider{Slug: "a", Kind: "direct", APIFormat: "openai", SupportsResponses: true, CredentialSlot: "A"}
	b := db.AIProvider{Slug: "b", Kind: "direct", APIFormat: "openai", CredentialSlot: "B1, B2"}
	c := db.AIProvider{Slug: "c", Kind: "direct", APIFormat: "anthropic", CredentialSlot: "C"}
	A, B, C := Target{a, "ma"}, Target{b, "mb"}, Target{c, "mc"}
	model := func(on bool) db.AIModel { return db.AIModel{Name: "m", Enabled: true, Translate: on} }
	res := func(dialect string, on bool, targets, other []Target) Resolution {
		return Resolution{Requested: "m", Dialect: dialect, Synthetic: true, Model: model(on), Targets: targets, Other: other}
	}
	g := resolveGateway()
	cases := []struct {
		name string
		res  Resolution
		d    *Dialect
		path string
		want []string // "<slug>#<slot>" or "<slug>#<slot> via <pair>"
		why  string
	}{
		{"chat, flag off", res("openai", false, []Target{A, B}, nil), DialectOpenAI, "/v1/chat/completions", []string{"a#A", "b#B1", "b#B2"}, ""},
		// Native stays native: a translated candidate is no fallback of a native one.
		{"chat, flag on", res("openai", true, []Target{A, B}, []Target{C}), DialectOpenAI, "/v1/chat/completions", []string{"a#A", "b#B1", "b#B2"}, ""},
		{"responses, flag off", res("openai", false, []Target{A, B}, nil), DialectOpenAI, "/v1/responses", []string{"a#A"}, ""},
		{"responses, flag on, one offers it", res("openai", true, []Target{A, B}, []Target{C}), DialectOpenAI, "/v1/responses", []string{"a#A"}, ""},
		{"messages, flag on, native", res("anthropic", true, []Target{C}, []Target{A, B}), DialectAnthropic, "/v1/messages", []string{"c#C"}, ""},
		{"embeddings are never translated", res("openai", true, nil, []Target{C}), DialectOpenAI, "/v1/embeddings", nil, "format_mismatch"},
		{"completions are never translated", res("openai", true, nil, []Target{C}), DialectOpenAI, "/v1/completions", nil, "format_mismatch"},
		{"count_tokens is never sent on", res("anthropic", true, nil, []Target{A}), DialectAnthropic, "/v1/messages/count_tokens", nil, "format_mismatch"},

		{"messages on chat targets", res("anthropic", true, nil, []Target{A, B}), DialectAnthropic, "/v1/messages",
			[]string{"a#A via messages-chat", "b#B1 via messages-chat", "b#B2 via messages-chat"}, ""},
		{"chat on a messages target", res("openai", true, nil, []Target{C}), DialectOpenAI, "/v1/chat/completions", []string{"c#C via chat-messages"}, ""},
		{"responses: own dialect without the endpoint first, then the other", res("openai", true, []Target{B}, []Target{C}), DialectOpenAI, "/v1/responses",
			[]string{"b#B1 via responses-chat", "b#B2 via responses-chat", "c#C via responses-messages"}, ""},
		{"responses on a messages target only", res("openai", true, nil, []Target{C}), DialectOpenAI, "/v1/responses", []string{"c#C via responses-messages"}, ""},

		{"responses, flag off, nobody offers it", res("openai", false, []Target{B}, nil), DialectOpenAI, "/v1/responses", nil, "endpoint_unsupported"},
		{"only the other dialect, flag off", res("openai", false, nil, []Target{C}), DialectOpenAI, "/v1/chat/completions", nil, "format_mismatch"},
		{"nothing at all", res("openai", true, nil, nil), DialectOpenAI, "/v1/chat/completions", nil, "model_not_found"},
		{"a direct address is never translated", Resolution{Requested: "b/mb", Dialect: "openai", Targets: []Target{B}}, DialectOpenAI, "/v1/responses", nil, "endpoint_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cands, why := g.candidatesForRequest(tc.res, tc.d, tc.path)
			var got []string
			for i, c := range cands {
				s := c.provider.Slug + "#" + c.provider.CredentialSlot
				if c.pair != nil {
					s += " via " + c.pair.ID()
				}
				got = append(got, s)
				if c.pos != i {
					t.Errorf("candidate %d has position %d", i, c.pos)
				}
			}
			if !reflect.DeepEqual(got, tc.want) || why != tc.why {
				t.Fatalf("candidates %v why %q, want %v %q", got, why, tc.want, tc.why)
			}
		})
	}
}

// A pair that is not released behaves as if translation were off.
func TestTranslate_UnreleasedPairIsOff(t *testing.T) {
	old := lookupPair
	t.Cleanup(func() { lookupPair = old })
	lookupPair = func(from, to translate.Format) (translate.Pair, bool) {
		if from == translate.Messages {
			return nil, false
		}
		return old(from, to)
	}
	s := script(map[string]http.HandlerFunc{"zai#ZAI": jsonBody(chatAnswer)})
	g, _ := translateGateway(s, true, tZai)
	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 400 || rec.Header().Get("Burrow-Error-Code") != "format_mismatch" || s.n("zai#ZAI") != 0 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	// The model list agrees with resolution.
	g.Catalog = fakeCatalog{
		models:    []db.AIModel{g.Synthetic.(fakeSynthetic)["smart"]},
		providers: []db.AIProvider{{Slug: "zai", APIFormat: "openai"}},
	}
	if ids := dialectModelIDs(t, g, DialectAnthropic); len(ids) != 0 {
		t.Fatalf("an unreleased pair lists %v", ids)
	}
	lookupPair = old
	if ids := dialectModelIDs(t, g, DialectAnthropic); !reflect.DeepEqual(ids, []string{"smart"}) {
		t.Fatalf("released: %v", ids)
	}
}

func dialectModelIDs(t *testing.T, g *Gateway, d *Dialect) []string {
	t.Helper()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer bgw_all")
	rec := httptest.NewRecorder()
	g.ServeDialect(rec, r, d)
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("model list: %d %s", rec.Code, rec.Body.String())
	}
	ids := []string{}
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// A dialect's model list shows a model that is served there by translation,
// and only then.
func TestTranslate_ModelList(t *testing.T) {
	g := globalGateway(http.NotFoundHandler(), nil)
	g.Catalog = fakeCatalog{
		models: []db.AIModel{
			{Name: "chat-only", Enabled: true, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "zai"}}},
			{Name: "chat-only-x", Enabled: true, Translate: true, Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "zai"}}},
			{Name: "claude-only", Enabled: true, Targets: []db.AIModelTarget{{Dialect: "anthropic", ProviderSlug: "zai-anthropic"}}},
			{Name: "claude-only-x", Enabled: true, Translate: true, Targets: []db.AIModelTarget{{Dialect: "anthropic", ProviderSlug: "zai-anthropic"}}},
			{Name: "off-x", Enabled: false, Translate: true, Targets: []db.AIModelTarget{{Dialect: "anthropic", ProviderSlug: "zai-anthropic"}}},
			// the row says anthropic, the provider speaks openai: no target at all
			{Name: "stale-x", Enabled: true, Translate: true, Targets: []db.AIModelTarget{{Dialect: "anthropic", ProviderSlug: "zai"}}},
			{Name: "gone-x", Enabled: true, Translate: true, Targets: []db.AIModelTarget{{Dialect: "anthropic", ProviderSlug: "gone"}}},
		},
		providers: []db.AIProvider{{Slug: "zai", APIFormat: "openai"}, {Slug: "zai-anthropic", APIFormat: "anthropic"}},
	}
	if ids := dialectModelIDs(t, g, DialectOpenAI); !reflect.DeepEqual(ids, []string{"chat-only", "chat-only-x", "claude-only-x"}) {
		t.Fatalf("openai: %v", ids)
	}
	if ids := dialectModelIDs(t, g, DialectAnthropic); !reflect.DeepEqual(ids, []string{"chat-only-x", "claude-only", "claude-only-x"}) {
		t.Fatalf("anthropic: %v", ids)
	}
}

// --- the three clients -------------------------------------------------------

func TestTranslate_ClaudeCodeOnAChatProvider(t *testing.T) {
	var up seen
	s := script(map[string]http.HandlerFunc{"zai#ZAI": up.then(sse(fixture(t, "chat/testdata/stream_tools.sse")))})
	g, att := translateGateway(s, true, tZai)
	sink := chained(t, g)
	body := strings.Replace(string(fixture(t, "messages/testdata/req_claude_code.json")), `"model":"burrow-medium"`, `"model":"smart"`, 1)

	rec := serve(g, anthropicPost("/v1/messages?beta=true", body), DialectAnthropic)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d Content-Type %q body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if got, want := frames(rec.Body.Bytes()), frames(fixture(t, "testdata/messages_chat_stream_tools.sse")); !reflect.DeepEqual(got, want) {
		t.Fatalf("the caller's stream:\n%s", rec.Body.String())
	}
	const dropped = "anthropic-beta,cache_control,metadata,thinking,thinking.signature,tool:web_search_20250305,top_k"
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
	if h := rec.Header(); h.Get("Burrow-Translated") != "messages-chat" || h.Get("Burrow-Dropped") != dropped || h.Get("Burrow-Error-Code") != "" {
		t.Fatalf("headers: %v", h)
	}

	// What the upstream got: the Chat Completions endpoint, no query, the
	// translated body with its own length, and nothing of the caller's headers.
	got := up.one(t)
	if got.path != "/v1/chat/completions" || got.query != "" {
		t.Fatalf("upstream path %q query %q", got.path, got.query)
	}
	sent := s.sent()
	want := strings.Replace(string(fixture(t, "testdata/messages_chat_request.json")), `"model":"gpt-x"`, `"model":"glm-5.1"`, 1)
	if len(sent) != 1 || !sameJSON([]byte(strings.TrimPrefix(sent[0], "zai#ZAI ")), []byte(want)) || s.lengthErr != "" {
		t.Fatalf("upstream body: %v (length: %q)", sent, s.lengthErr)
	}
	for name := range got.header {
		switch name {
		case "Content-Type", "Content-Length", "Accept":
		default:
			t.Errorf("header %s: %q reached the translated upstream", name, got.header[name])
		}
	}
	if got.header.Get("Accept") != "text/event-stream" || got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("upstream headers: %v", got.header)
	}

	// One usage row, with the upstream's figures and the translation.
	u := oneRow(t, sink)
	if u.Translated != "messages-chat" || u.Dropped != dropped || u.TokensIn != 50 || u.TokensOut != 21 || u.ProviderSlug != "zai" ||
		u.TargetModel != "glm-5.1" || u.RequestedModel != "smart" || u.Dialect != "anthropic" || u.UpstreamStatus != 200 || !u.Streamed || u.CostUSD != nil {
		t.Fatalf("usage row: %+v", u)
	}
	if len(att.all()) != 0 {
		t.Fatalf("a clean single attempt was logged: %+v", att.all())
	}
}

func TestTranslate_CodexOnAChatProvider(t *testing.T) {
	var up seen
	s := script(map[string]http.HandlerFunc{"zai#ZAI": up.then(sse(fixture(t, "chat/testdata/stream_tools.sse")))})
	g, _ := translateGateway(s, true, tZai) // zai does not offer the Responses API
	sink := chained(t, g)
	body := strings.Replace(string(fixture(t, "responses/testdata/req_codex.json")), `"model":"burrow-medium"`, `"model":"smart"`, 1)
	r := post("/v1/responses", "bgw_all", body)
	r.Header.Set("OpenAI-Beta", "responses=experimental")
	r.Header.Set("OpenAI-Organization", "org-1")
	r.Header.Set("OpenAI-Project", "proj-1")
	r.Header.Set("Session_id", "019a0000")
	r.Header.Set("Conversation_id", "019a0001")
	r.Header.Set("Originator", "codex_cli_rs")

	rec := serve(g, r, DialectOpenAI)
	if rec.Code != 200 || rec.Header().Get("Burrow-Translated") != "responses-chat" {
		t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
	fr := frames(rec.Body.Bytes())
	if len(fr) == 0 || !strings.HasPrefix(fr[0], "event: response.created") || !strings.HasPrefix(fr[len(fr)-1], "event: response.completed") {
		t.Fatalf("the caller's stream:\n%s", rec.Body.String())
	}
	got := up.one(t)
	if got.path != "/v1/chat/completions" || len(got.header) != 3 {
		t.Fatalf("upstream path %q headers %v", got.path, got.header)
	}
	want := strings.Replace(string(fixture(t, "testdata/responses_chat_request.json")), `"model":"gpt-x"`, `"model":"glm-5.1"`, 1)
	if sent := s.sent(); !sameJSON([]byte(strings.TrimPrefix(sent[0], "zai#ZAI ")), []byte(want)) {
		t.Fatalf("upstream body: %v", sent)
	}
	if u := oneRow(t, sink); u.Translated != "responses-chat" || !strings.Contains(u.Dropped, "tool:local_shell") || u.TokensIn != 50 || u.TokensOut != 21 {
		t.Fatalf("usage row: %+v", u)
	}
	if rec.Header().Get("Burrow-Dropped") != oneRow(t, sink).Dropped {
		t.Fatalf("header %q, row %q", rec.Header().Get("Burrow-Dropped"), oneRow(t, sink).Dropped)
	}

	// A stored response has no pair: the sub-paths are no endpoint of the
	// gateway, with the flag as without.
	for _, path := range []string{"/v1/responses/resp_1", "/v1/responses/resp_1/cancel", "/v1/responses/resp_1/input_items", "/v1/responses/compact", "/v1/responses/input_tokens"} {
		rec := serve(g, post(path, "bgw_all", body), DialectOpenAI)
		if rec.Code != 404 || rec.Header().Get("Burrow-Error-Code") != "endpoint_not_found" {
			t.Fatalf("%s: status %d body %s", path, rec.Code, rec.Body.String())
		}
	}
	if s.n("zai#ZAI") != 1 {
		t.Fatalf("zai was called %d times", s.n("zai#ZAI"))
	}
}

func TestTranslate_ChatClientOnAMessagesProvider(t *testing.T) {
	var up seen
	s := script(map[string]http.HandlerFunc{"zai-anthropic#ZAIA": up.then(jsonBody(string(fixture(t, "messages/testdata/resp_text.json"))))})
	g, _ := translateGateway(s, true, tZaiA)
	sink := chained(t, g)
	r := post("/v1/chat/completions", "bgw_all", chatHi)
	r.Header.Set("OpenAI-Organization", "org-1")
	r.Header.Set("X-Api-Key", "sk-something-else")
	r.Header.Set("Anthropic-Version", "1999-01-01")

	rec := serve(g, r, DialectOpenAI)
	var out struct {
		Object  string
		Choices []struct{ Message struct{ Content string } }
		Usage   struct{ PromptTokens, CompletionTokens int } `json:"-"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Object != "chat.completion" ||
		len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello there." {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Burrow-Translated") != "chat-messages" || rec.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("headers: %v", rec.Header())
	}
	wantHeaders(t, rec, "zai-anthropic", "claude-x", "1")
	got := up.one(t)
	if got.path != "/v1/messages" || got.header.Get("Anthropic-Version") != "2023-06-01" || got.header.Get("Accept") != "application/json" || len(got.header) != 4 {
		t.Fatalf("upstream path %q headers %v", got.path, got.header)
	}
	var sent struct {
		Model     string
		MaxTokens int `json:"max_tokens"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(s.sent()[0], "zai-anthropic#ZAIA ")), &sent) != nil || sent.Model != "claude-x" || sent.MaxTokens != 32000 || s.lengthErr != "" {
		t.Fatalf("upstream body: %v (%q)", s.sent(), s.lengthErr)
	}
	// The upstream's figures, cache tokens counted as input: 12+3+5 and 4.
	u := oneRow(t, sink)
	if u.TokensIn != 20 || u.TokensOut != 4 || u.Translated != "chat-messages" || u.Dropped != "max_tokens.default" || u.Dialect != "openai" || u.Streamed {
		t.Fatalf("usage row: %+v", u)
	}
	if rec.Header().Get("Burrow-Dropped") != "max_tokens.default" {
		t.Fatalf("Burrow-Dropped %q", rec.Header().Get("Burrow-Dropped"))
	}
}

// A Chat caller that did not ask for usage gets a stream without a usage
// chunk; the row still holds what the upstream counted.
func TestTranslate_UsageIsTheUpstreamsWhenTheCallerSeesNone(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai-anthropic#ZAIA": sse(fixture(t, "messages/testdata/stream_tools.sse"))})
	g, _ := translateGateway(s, true, tZaiA)
	sink := chained(t, g)
	rec := serve(g, post("/v1/chat/completions", "bgw_all", `{"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`), DialectOpenAI)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"usage"`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if u := oneRow(t, sink); u.TokensIn != 131 || u.TokensOut != 17 || !u.Streamed {
		t.Fatalf("usage row: %+v", u)
	}
}

// A stream that is cut leaves the usage that had arrived, as a native one does.
func TestTranslate_CutStreamKeepsPartialUsage(t *testing.T) {
	whole := fixture(t, "messages/testdata/stream_tools.sse")
	cut := whole[:bytes.Index(whole, []byte("event: ping"))]
	s := script(map[string]http.HandlerFunc{"zai-anthropic#ZAIA": sse(cut), "openrouter#OR": jsonBody(chatAnswer)})
	g, att := translateGateway(s, true, tZaiA)
	sink := chained(t, g)
	rec := serve(g, post("/v1/chat/completions", "bgw_all", `{"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`), DialectOpenAI)
	fr := frames(rec.Body.Bytes())
	if rec.Code != 200 || len(fr) < 2 || !strings.Contains(fr[len(fr)-1], `"error"`) || strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if u := oneRow(t, sink); u.TokensIn != 131 || u.TokensOut != 1 || u.Translated != "chat-messages" {
		t.Fatalf("usage row: %+v", u)
	}
	rows := att.all()
	if len(rows) != 1 || rows[0].ErrorCode != "stream_aborted" || rows[0].Status != 200 {
		t.Fatalf("attempts: %+v", rows)
	}
}

// --- native stays native (Review Focus 4) ------------------------------------

func TestTranslate_NativeFirst(t *testing.T) {
	native := jsonBody(string(fixture(t, "messages/testdata/resp_text.json")))
	for _, tc := range []struct {
		name   string
		c      http.HandlerFunc
		status int
		body   string
	}{
		{"the native target answers", native, 200, ""},
		// A native target that fails is not followed by a translated one:
		// translation is for models with no target of the caller's format.
		{"the native target fails", status(500, `{"type":"error","error":{"type":"api_error","message":"down"}}`), 500, `{"type":"error","error":{"type":"api_error","message":"down"}}`},
		{"the native target refuses", status(400, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`), 400, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`},
		{"the native target has no answer", func(http.ResponseWriter, *http.Request) { panic("boom") }, 502, ""},
	} {
		for _, on := range []bool{true, false} {
			t.Run(tc.name+", flag "+strconv.FormatBool(on), func(t *testing.T) {
				s := script(map[string]http.HandlerFunc{"zai-anthropic#ZAIA": tc.c, "zai#ZAI": jsonBody(chatAnswer), "openrouter#OR": jsonBody(chatAnswer)})
				g, _ := translateGateway(s, on, tZaiA, tZai, tOR)
				g.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
				sink := chained(t, g)
				rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
				if rec.Code != tc.status || (tc.body != "" && rec.Body.String() != tc.body) {
					t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
				}
				if s.n("zai#ZAI") != 0 || s.n("openrouter#OR") != 0 || s.n("zai-anthropic#ZAIA") != 1 {
					t.Fatalf("calls: %v", s.sent())
				}
				if rec.Header().Get("Burrow-Translated") != "" || rec.Header().Get("Burrow-Dropped") != "" {
					t.Fatalf("a native answer says it was translated: %v", rec.Header())
				}
				if u := oneRow(t, sink); u.Translated != "" || u.Dropped != "" {
					t.Fatalf("usage row: %+v", u)
				}
				// The native request is the caller's, byte for byte but the model.
				if sent := s.sent(); sent[0] != "zai-anthropic#ZAIA "+strings.Replace(messagesHi, `"smart"`, `"claude-x"`, 1) {
					t.Fatalf("native body: %v", sent)
				}
			})
		}
	}

	// The same on the Responses API: a target that offers it is native, and
	// the ones that do not are not tried after it.
	s := script(map[string]http.HandlerFunc{"openrouter#OR": status(503, "down"), "zai#ZAI": jsonBody(chatAnswer), "zai-anthropic#ZAIA": native})
	g, _ := translateGateway(s, true, tZai, tOR, tZaiA)
	p := g.Providers.(fakeProviders)
	or := p["openrouter"]
	or.SupportsResponses = true
	p["openrouter"] = or
	rec := serve(g, post("/v1/responses", "bgw_all", `{"model":"smart","input":"hi"}`), DialectOpenAI)
	if rec.Code != 503 || rec.Body.String() != "down" || s.n("zai#ZAI") != 0 || s.n("zai-anthropic#ZAIA") != 0 || rec.Header().Get("Burrow-Translated") != "" {
		t.Fatalf("status %d body %s calls %v", rec.Code, rec.Body.String(), s.sent())
	}
}

// Without the flag nothing changes, byte for byte.
func TestTranslate_OptInOnly(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": jsonBody(chatAnswer), "zai-anthropic#ZAIA": jsonBody("{}")})
	g, att := translateGateway(s, false, tZai)
	chain := &spyChain{}
	g.Chain = chain

	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	const mismatch = `{"type":"error","error":{"type":"invalid_request_error","message":"model smart is not served in the anthropic format; use https://burrow.example.com/openai/v1"},"burrow_code":"format_mismatch"}` + "\n"
	if rec.Code != 400 || rec.Body.String() != mismatch || rec.Header().Get("Burrow-Error-Code") != "format_mismatch" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	rec = serve(g, post("/v1/responses", "bgw_all", `{"model":"smart","input":"hi"}`), DialectOpenAI)
	const unsupported = `{"error":{"message":"model smart is not available on the Responses API; use /v1/chat/completions","type":"burrow_error","code":"endpoint_unsupported"}}` + "\n"
	if rec.Code != 400 || rec.Body.String() != unsupported {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if s.n("zai#ZAI") != 0 || chain.serviceID != "" || len(att.all()) != 0 || rec.Header().Get("Burrow-Attempts") != "" {
		t.Fatal("a refused request went on")
	}

	// A direct address is never translated, whatever a model's flag says.
	g, _ = translateGateway(s, true, tZai)
	for _, tc := range []struct {
		r    *http.Request
		d    *Dialect
		code string
	}{
		{anthropicPost("/v1/messages", strings.Replace(messagesHi, "smart", "zai/glm-5.1", 1)), DialectAnthropic, "format_mismatch"},
		{post("/v1/chat/completions", "bgw_all", `{"model":"zai-anthropic/claude-x","messages":[]}`), DialectOpenAI, "format_mismatch"},
		{post("/v1/responses", "bgw_all", `{"model":"zai/glm-5.1","input":"hi"}`), DialectOpenAI, "endpoint_unsupported"},
		// Embeddings are never translated.
		{post("/v1/embeddings", "bgw_all", `{"model":"smart","input":"hi"}`), DialectAnthropic, "endpoint_not_found"},
	} {
		rec := serve(g, tc.r, tc.d)
		if rec.Header().Get("Burrow-Error-Code") != tc.code || rec.Header().Get("Burrow-Translated") != "" {
			t.Fatalf("%s: status %d body %s", tc.r.URL.Path, rec.Code, rec.Body.String())
		}
	}
	g, _ = translateGateway(s, true, tZaiA)
	if rec := serve(g, post("/v1/embeddings", "bgw_all", `{"model":"smart","input":"hi"}`), DialectOpenAI); rec.Code != 400 || rec.Header().Get("Burrow-Error-Code") != "format_mismatch" {
		t.Fatalf("embeddings: status %d body %s", rec.Code, rec.Body.String())
	}
	if n := len(s.sent()); n != 0 {
		t.Fatalf("%d upstream calls", n)
	}
}

// --- the order of checks -----------------------------------------------------

type budgetOver struct{ asked []string }

func (b *budgetOver) Blocked(_ context.Context, _, model string) (string, bool) {
	b.asked = append(b.asked, model)
	return "the model's daily budget is used up", true
}

// Whether a model is translated cannot be told before the allow-list passed;
// the budget is checked on the name the client asked for, before resolution,
// and answered in the caller's format.
func TestTranslate_ChecksBeforeResolution(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": jsonBody(chatAnswer)})
	g, _ := translateGateway(s, true, tZai)
	g.GatewayKeys = fakeGatewayKeys{"bgw_all": {ID: "gk"}, "bgw_other": {ID: "gk2", AllowedModels: []string{"something-else"}}}
	g.Synthetic = notForKey{g.Synthetic, t}

	denied := func(model string) string {
		r := anthropicPost("/v1/messages", strings.Replace(messagesHi, "smart", model, 1))
		r.Header.Set("X-Api-Key", "bgw_other")
		rec := serve(g, r, DialectAnthropic)
		if rec.Code != 403 || rec.Header().Get("Burrow-Error-Code") != "model_not_allowed" {
			t.Fatalf("%s: status %d body %s", model, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if a, b := denied("smart"), denied("no-such-model"); a != b {
		t.Fatalf("the denial tells a translated model from an unknown one:\n%s\n%s", a, b)
	}

	budget := &budgetOver{}
	g.Budgets = budget
	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	var body struct {
		Type       string
		BurrowCode string `json:"burrow_code"`
	}
	if rec.Code != 429 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Type != "error" || body.BurrowCode != "budget_exceeded" ||
		!reflect.DeepEqual(budget.asked, []string{"smart"}) || s.n("zai#ZAI") != 0 {
		t.Fatalf("status %d body %s asked %v", rec.Code, rec.Body.String(), budget.asked)
	}
}

// notForKey fails the test when a model is looked up while it must not be.
type notForKey struct {
	SyntheticModels
	t *testing.T
}

func (n notForKey) ModelByName(context.Context, string) (db.AIModel, error) {
	n.t.Error("a model was looked up for a request that was refused before resolution")
	return db.AIModel{}, errors.New("unused")
}

// --- the request itself is at fault ------------------------------------------

func TestTranslate_ClientErrorIsNotRetried(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": jsonBody(chatAnswer), "openrouter#OR": jsonBody(chatAnswer)})
	g, att := translateGateway(s, true, tZai, tOR)
	sink := chained(t, g)
	// A document block cannot be carried to a Chat Completions target.
	body := `{"model":"smart","max_tokens":64,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"x"}}]}]}`
	rec := serve(g, anthropicPost("/v1/messages", body), DialectAnthropic)
	var out struct {
		Type  string
		Error struct{ Type, Message string }
		Code  string `json:"burrow_code"`
	}
	if rec.Code != 400 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Type != "error" || out.Error.Type != "invalid_request_error" ||
		out.Code != "invalid_request" || !strings.Contains(out.Error.Message, "messages[0].content[0]") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if len(s.sent()) != 0 || s.credentialReads("zai#ZAI") != 0 || rec.Header().Get("Burrow-Attempts") != "0" ||
		rec.Header().Get("Burrow-Provider") != "" || rec.Header().Get("Burrow-Translated") != "" {
		t.Fatalf("calls %v headers %v", s.sent(), rec.Header())
	}
	if ok, failed := reports(g.Breaker, "zai"); ok != 0 || failed != 0 || len(att.all()) != 0 {
		t.Fatalf("breaker %d/%d attempts %+v", ok, failed, att.all())
	}
	if u := oneRow(t, sink); u.UpstreamStatus != 400 || u.Translated != "" {
		t.Fatalf("usage row: %+v", u)
	}
}

// --- failures of a translated attempt ----------------------------------------

func TestTranslate_UpstreamErrorKeepsStatusAndCallerShape(t *testing.T) {
	limited := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "9")
		w.Header().Set("Set-Cookie", "a=b")
		w.Header().Set("X-Upstream-Secret", "s")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down please","type":"rate_limit_error","code":"rate_limited"}}`))
	}
	s := script(map[string]http.HandlerFunc{"zai#ZAI": limited, "openrouter#OR": jsonBody(chatAnswer)})
	g, att := translateGateway(s, true, tZai)
	var logs syncBuffer
	g.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	var out struct {
		Type  string
		Error struct{ Type, Message string }
		Code  string `json:"burrow_code"`
	}
	if rec.Code != 429 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Type != "error" || out.Error.Type != "rate_limit_error" ||
		out.Error.Message != "slow down please" || out.Code != "upstream_error" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Retry-After") != "9" || h.Get("Burrow-Error-Code") != "upstream_error" || h.Get("Burrow-Translated") != "messages-chat" ||
		h.Get("Set-Cookie") != "" || h.Get("X-Upstream-Secret") != "" {
		t.Fatalf("headers: %v", h)
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
	// The provider's words reach the caller only.
	if strings.Contains(logs.String(), "slow down") {
		t.Fatalf("the provider's message was logged: %s", logs.String())
	}
	// A 4xx says nothing about the provider.
	if ok, failed := reports(g.Breaker, "zai"); ok != 0 || failed != 0 {
		t.Fatalf("breaker: %d ok, %d failed", ok, failed)
	}
	rows := att.all()
	if len(rows) != 1 || rows[0].ErrorCode != "http_429" || rows[0].Status != 429 {
		t.Fatalf("attempts: %+v", rows)
	}

	// With another candidate and the model's opt-in the next one is tried.
	g, att = failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Translate, m.FallbackOnRateLimit, m.Targets = true, true, []db.AIModelTarget{tZai, {Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"}}
	})
	rec = serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 200 || rec.Header().Get("Burrow-Translated") != "messages-chat" || rec.Header().Get("Retry-After") != "" || rec.Header().Get("Burrow-Error-Code") != "" {
		t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	if rows := att.all(); len(rows) != 2 || rows[0].ErrorCode != "http_429" || rows[1].ErrorCode != "" {
		t.Fatalf("attempts: %+v", rows)
	}
}

// A failure before the first event moves on to the next candidate exactly as
// a native one does; what failed is told apart in the attempt log, and the
// breaker hears what it would hear of a native attempt.
func TestTranslate_FailureBeforeTheFirstEventMovesOn(t *testing.T) {
	errorFrame := "data: {\"error\":{\"message\":\"model overloaded\",\"type\":\"server_error\"}}\n\n"
	for _, tc := range []struct {
		name      string
		zai       http.HandlerFunc
		code      string
		status    int
		failed    int // breaker reports against zai
		lastError string
	}{
		{"an upstream 500", status(500, `{"error":{"message":"down"}}`), "http_500", 500, 1, "upstream_error"},
		{"a 200 that is no answer", jsonBody(`<html>hello</html>`), "upstream_invalid", 200, 1, "upstream_invalid"},
		{"a 200 that is cut off", jsonBody(chatAnswer[:40]), "upstream_invalid", 200, 1, "upstream_invalid"},
		{"a stream that opens with an error", sse([]byte(errorFrame)), "upstream_error", 200, 1, "upstream_error"},
		{"a stream that ends before its first event", sse(nil), "upstream_invalid", 200, 1, "upstream_invalid"},
		{"a compressed answer", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte{0x1f, 0x8b})
		}, "upstream_invalid", 200, 1, "upstream_invalid"},
		{"a handler that says nothing", func(http.ResponseWriter, *http.Request) {}, "upstream_invalid", 0, 1, "upstream_invalid"},
		{"a panic", func(http.ResponseWriter, *http.Request) { panic("boom") }, "panic", 0, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := script(map[string]http.HandlerFunc{"zai#ZAI": tc.zai, "openrouter#OR": sse(fixture(t, "chat/testdata/stream_text.sse"))})
			g, att := translateGateway(s, true, tZai, tOR)
			g.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
			sink := chained(t, g)
			body := `{"model":"smart","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
			rec := serve(g, anthropicPost("/v1/messages", body), DialectAnthropic)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "event: message_stop") || rec.Header().Get("Burrow-Error-Code") != "" {
				t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
			}
			wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
			rows := att.all()
			if len(rows) != 2 || rows[0].ErrorCode != tc.code || rows[0].Status != tc.status || rows[0].ProviderSlug != "zai" || rows[1].ErrorCode != "" {
				t.Fatalf("attempts: %+v", rows)
			}
			if _, failed := reports(g.Breaker, "zai"); failed != tc.failed {
				t.Fatalf("breaker: %d failures of zai, want %d", failed, tc.failed)
			}
			if ok, failed := reports(g.Breaker, "openrouter"); ok != 1 || failed != 0 {
				t.Fatalf("breaker: openrouter %d ok %d failed", ok, failed)
			}
			// Every attempt is built from the caller's bytes: the two differ in the model only.
			sent := s.sent()
			if len(sent) == 2 {
				a := strings.Replace(strings.TrimPrefix(sent[0], "zai#ZAI "), `"glm-5.1"`, "X", 1)
				b := strings.Replace(strings.TrimPrefix(sent[1], "openrouter#OR "), `"google/gemini-x"`, "X", 1)
				if a != b || !strings.Contains(a, `"model":X`) {
					t.Fatalf("attempts differ in more than the model:\n%s\n%s", sent[0], sent[1])
				}
			}
			if u := oneRow(t, sink); u.ProviderSlug != "openrouter" || u.Translated != "messages-chat" || u.TokensIn != 12 || u.TokensOut != 2 {
				t.Fatalf("usage row: %+v", u)
			}

			// As the last candidate the same failure is the caller's answer, in its format.
			if tc.lastError == "" {
				return
			}
			g, att = translateGateway(s, true, tZai)
			g.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
			rec = serve(g, anthropicPost("/v1/messages", body), DialectAnthropic)
			var out struct {
				Type string
				Code string `json:"burrow_code"`
			}
			want := tc.status
			if want < 400 {
				want = 502
			}
			if rec.Code != want || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Type != "error" || out.Code != tc.lastError ||
				rec.Header().Get("Burrow-Error-Code") != tc.lastError || rec.Header().Get("Burrow-Translated") != "messages-chat" {
				t.Fatalf("last candidate: status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
			}
			if rows := att.all(); len(rows) != 1 || rows[0].ErrorCode != tc.code || rows[0].Status != tc.status {
				t.Fatalf("last candidate, attempts: %+v", rows)
			}
		})
	}
}

// After the first event nothing is retried: the caller's stream ends with its
// own error event, and the attempt is logged as a stream that broke off.
func TestTranslate_StreamFailureAfterFirstByte(t *testing.T) {
	text := fixture(t, "chat/testdata/stream_text.sse")
	cut := text[:bytes.Index(text, []byte(`"finish_reason":"stop"`))]
	cut = cut[:bytes.LastIndex(cut, []byte("data:"))]
	s := script(map[string]http.HandlerFunc{"zai#ZAI": sse(cut), "openrouter#OR": sse(text)})
	g, att := translateGateway(s, true, tZai, tOR)
	body := `{"model":"smart","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := serve(g, anthropicPost("/v1/messages", body), DialectAnthropic)
	fr := frames(rec.Body.Bytes())
	if rec.Code != 200 || len(fr) < 3 || !strings.HasPrefix(fr[len(fr)-1], "event: error") || strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if s.n("openrouter#OR") != 0 {
		t.Fatal("a second candidate was tried after the first byte")
	}
	wantHeaders(t, rec, "zai", "glm-5.1", "1")
	rows := att.all()
	if len(rows) != 1 || rows[0].ErrorCode != "stream_aborted" || rows[0].Status != 200 {
		t.Fatalf("attempts: %+v", rows)
	}
	// The start was the answer: the provider was up.
	if ok, failed := reports(g.Breaker, "zai"); ok != 1 || failed != 0 {
		t.Fatalf("breaker: %d ok %d failed", ok, failed)
	}
}

// --- timers ------------------------------------------------------------------

// An upstream that answers 200 and then says nothing leaves the caller without
// a header; the attempt's timer ends it, and the next candidate is tried.
func TestTranslate_StalledAfterTheHeaderTimesOut(t *testing.T) {
	stall := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}
	s := script(map[string]http.HandlerFunc{"zai#ZAI": stall, "openrouter#OR": jsonBody(chatAnswer)})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Translate, m.AttemptTimeoutS, m.TotalTimeoutS = true, 40, 60000
		m.Targets = []db.AIModelTarget{tZai, {Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"}}
	})
	start := time.Now()
	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 200 || time.Since(start) < 35*time.Millisecond {
		t.Fatalf("status %d after %s body %s", rec.Code, time.Since(start), rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	if rows := att.all(); len(rows) != 2 || rows[0].ErrorCode != "timeout" || rows[0].Status != 0 {
		t.Fatalf("attempts: %+v", rows)
	}
	if _, failed := reports(g.Breaker, "zai"); failed != 1 {
		t.Fatalf("breaker: %d failures", failed)
	}

	// Alone, it is the total timeout that answers.
	g, _ = failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Translate, m.AttemptTimeoutS, m.TotalTimeoutS, m.Targets = true, 30, 60, []db.AIModelTarget{tZai}
	})
	rec = serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 504 || rec.Header().Get("Burrow-Error-Code") != "gateway_timeout" {
		t.Fatalf("alone: status %d body %s", rec.Code, rec.Body.String())
	}
}

// --- per attempt: breaker, policy, credential, place -------------------------

func TestTranslate_ConcurrencyAndBreakerApply(t *testing.T) {
	hold := make(chan struct{})
	s := script(map[string]http.HandlerFunc{"zai#ZAI": holdFirst(hold), "openrouter#OR": jsonBody(chatAnswer)})
	g, att := failoverGateway(s, "ZAI", func(m *db.AIModel) {
		m.Translate, m.AttemptTimeoutS, m.TotalTimeoutS = true, 30, 60000
		m.Targets = []db.AIModelTarget{tZai, {Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"}}
	})
	limit(g, "zai", 1)
	a := inFlight(t, g, "zai", post("/v1/chat/completions", "bgw_all", zaiDirect), DialectOpenAI)
	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 200 || rec.Header().Get("Burrow-Translated") != "messages-chat" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "2")
	if rows := att.all(); len(rows) != 2 || rows[0].ErrorCode != "busy" || rows[0].Status != 429 {
		t.Fatalf("attempts: %+v", rows)
	}
	close(hold)
	<-a
	noPlaces(t, g)

	// A provider the breaker has opened is skipped.
	s = script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "down"), "openrouter#OR": jsonBody(chatAnswer)})
	g, att = translateGateway(s, true, tZai, tOR)
	for i := 0; i < 12 && !g.Breaker.Open("zai"); i++ {
		serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	}
	if !g.Breaker.Open("zai") {
		t.Fatal("translated failures never opened the breaker")
	}
	n := s.n("zai#ZAI")
	att.mu.Lock()
	att.rows = nil
	att.mu.Unlock()
	rec = serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 200 || s.n("zai#ZAI") != n {
		t.Fatalf("status %d, zai called %d more times", rec.Code, s.n("zai#ZAI")-n)
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "1")
	if rows := att.all(); len(rows) != 2 || rows[0].ErrorCode != "breaker_open" {
		t.Fatalf("attempts: %+v", rows)
	}
}

type headerCredentials struct{ applied []string }

func (c *headerCredentials) Apply(_ context.Context, serviceID string, r *http.Request) (bool, error) {
	c.applied = append(c.applied, serviceID)
	r.Header.Set("Authorization", "Bearer upstream-of-"+serviceID)
	return true, nil
}

// Each translated attempt is checked against its own target's policy and gets
// its own target's credential and nothing of the caller's.
func TestTranslate_PolicyAndCredentialPerAttempt(t *testing.T) {
	var gotAuth, gotPath, gotKey string
	tunnel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotKey = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("X-Api-Key")
		jsonBody(chatAnswer)(w, r)
	})
	s := script(map[string]http.HandlerFunc{"zai#ZAI": jsonBody(chatAnswer), "openrouter#OR": jsonBody(chatAnswer)})
	tOllama := db.AIModelTarget{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "mistral"}
	g, att := translateGateway(s, true, tZai, tOllama)
	g.Tunnels = fakeTunnels{res: &proxy.Resolved{ServiceID: "svc1", AccessMode: "api_key", LocalHost: "127.0.0.1:11434"}, upstream: tunnel}
	creds := &headerCredentials{}
	g.Credentials = creds
	var asked []string
	// zai's service is no longer in API-key mode: refused, a 403 is the answer.
	g.ServicePolicy = policyBy(map[string]string{"prov-zai": "public"}, &asked)
	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 403 || rec.Header().Get("Burrow-Error-Code") != "provider_unavailable" || s.credentialReads("zai#ZAI") != 0 || len(creds.applied) != 0 ||
		rec.Header().Get("Burrow-Translated") != "" || rec.Header().Get("Burrow-Provider") != "" {
		t.Fatalf("refused first target: status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
	}
	_ = att

	// The tunnelled target, translated: its bound credential and no other.
	g, _ = translateGateway(s, true, tOllama)
	g.Tunnels = fakeTunnels{res: &proxy.Resolved{ServiceID: "svc1", AccessMode: "api_key", LocalHost: "127.0.0.1:11434"}, upstream: tunnel}
	g.Credentials = creds
	rec = serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 200 || gotAuth != "Bearer upstream-of-svc1" || gotKey != "" || gotPath != "/v1/chat/completions" || !reflect.DeepEqual(creds.applied, []string{"svc1"}) {
		t.Fatalf("status %d auth %q key %q path %q applied %v body %s", rec.Code, gotAuth, gotKey, gotPath, creds.applied, rec.Body.String())
	}
	wantHeaders(t, rec, "ollama", "mistral", "1")
}

// --- what the chain sees -----------------------------------------------------

// Redaction and guardrails see the caller's body and are not bypassed; the
// refusal is the caller's format; a translated answer is never cached.
func TestTranslate_ChainSteps(t *testing.T) {
	raw, err := db.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	d := db.Wrap(raw)
	t.Cleanup(func() { _ = d.Close() })
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	answer := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(chatAnswer)))
		_, _ = w.Write([]byte(chatAnswer))
	}
	s := script(map[string]http.HandlerFunc{"zai#ZAI": answer})
	g, _ := translateGateway(s, true, tZai)
	g.Log = log
	red, err := redact.NewEngine([]redact.Rule{{ID: "r", Name: "r", Pattern: `TOPSECRET-\d+`, Action: redact.ActionMask, Scope: redact.ScopeRequestBody}})
	if err != nil {
		t.Fatal(err)
	}
	guard := guardrails.NewEngine()
	sink := &recSink{}
	chain := aigw.NewChain(exact.New(d, log), nil, nil, red, guard, nil, nil, sink, log)
	chain.Loader = cfgLoader{
		Redaction:  &aigw.RedactionConfig{Enabled: true},
		Guardrails: &guardrails.Settings{Enabled: true, Action: guardrails.ActionRefuseSafe},
		Cache:      &exact.Settings{Enabled: true, AppliesPer: "global", TTLSeconds: 300, MaxEntries: 100, MaxPerEntryKB: 64},
	}
	g.Chain = chain

	// Redaction: the translated request carries the masked prompt.
	body := `{"model":"smart","max_tokens":64,"messages":[{"role":"user","content":"my code is TOPSECRET-1 ok"}]}`
	rec := serve(g, anthropicPost("/v1/messages", body), DialectAnthropic)
	if rec.Code != 200 || rec.Header().Get("Burrow-Translated") != "messages-chat" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	sent := s.sent()
	if len(sent) != 1 || strings.Contains(sent[0], "TOPSECRET-1") || !strings.Contains(sent[0], "my code is") {
		t.Fatalf("upstream body: %v", sent)
	}

	// Cache: the same request again reaches the upstream again.
	rec = serve(g, anthropicPost("/v1/messages", body), DialectAnthropic)
	if rec.Code != 200 || rec.Header().Get("Burrow-Cache") == "HIT" || s.n("zai#ZAI") != 2 || rec.Header().Get("Burrow-Translated") != "messages-chat" {
		t.Fatalf("second call: status %d Burrow-Cache %q, upstream calls %d", rec.Code, rec.Header().Get("Burrow-Cache"), s.n("zai#ZAI"))
	}
	// ... and a native request for the same target is not answered with a
	// translated answer either.
	rec = call(g, `{"model":"zai/glm-5.1","messages":[{"role":"user","content":"my code is TOPSECRET-1 ok"}]}`)
	if rec.Code != 200 || rec.Header().Get("Burrow-Cache") == "HIT" || rec.Body.String() != chatAnswer {
		t.Fatalf("native call: status %d Burrow-Cache %q body %s", rec.Code, rec.Header().Get("Burrow-Cache"), rec.Body.String())
	}

	// Guardrail: matched on the caller's prompt, answered in the caller's format, no upstream.
	n := s.n("zai#ZAI")
	rec = serve(g, anthropicPost("/v1/messages", `{"model":"smart","max_tokens":64,"messages":[{"role":"user","content":"`+injection+`"}]}`), DialectAnthropic)
	var refusal struct {
		Type, Role, Model string
		Content           []struct{ Type, Text string }
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &refusal) != nil || refusal.Type != "message" || refusal.Model != "smart" ||
		len(refusal.Content) != 1 || refusal.Content[0].Text == "" || s.n("zai#ZAI") != n {
		t.Fatalf("refusal: status %d body %s", rec.Code, rec.Body.String())
	}

	// Nothing of a request or an answer is in a log line.
	for _, leak := range []string{"TOPSECRET", "my code is", "Hello there", "ignore previous"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("%q was logged:\n%s", leak, logs.String())
		}
	}
	_ = sink
}

// A reported cost is not believed on a translated request; the chain runs once.
func TestTranslate_ChainRunsOnceAndCostIsNotTrusted(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": status(500, "x"), "openrouter#OR": jsonBody(chatAnswer)})
	g, _ := translateGateway(s, true, tZai, tOR)
	chain := &countingChain{}
	g.Chain = chain
	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 200 || chain.n != 1 || chain.trust || !chain.noCache || chain.serviceID != "prov-zai" {
		t.Fatalf("status %d chain %+v", rec.Code, chain)
	}
	// Natively, the same providers are trusted and cached as before.
	rec = call(g, smartBody)
	if rec.Code != 200 || chain.n != 2 || !chain.trust || chain.noCache {
		t.Fatalf("native: status %d chain %+v", rec.Code, chain)
	}
}

type countingChain struct {
	n         int
	trust     bool
	noCache   bool
	serviceID string
}

func (c *countingChain) Dispatch(http.ResponseWriter, *http.Request, string, string, string, string, http.Handler) {
	panic("an inference call was not metered")
}

func (c *countingChain) DispatchMetered(w http.ResponseWriter, r *http.Request, serviceID, _, _, _ string, trust bool, up http.Handler) {
	c.n++
	c.trust, c.noCache, c.serviceID = trust, aigw.CacheBypassed(r.Context()), serviceID
	up.ServeHTTP(w, r)
}

// --- counting tokens ---------------------------------------------------------

func TestTranslate_CountTokensEstimate(t *testing.T) {
	s := script(map[string]http.HandlerFunc{"zai#ZAI": jsonBody(chatAnswer), "zai-anthropic#ZAIA": jsonBody(`{"input_tokens":7}`)})
	g, _ := translateGateway(s, true, tZai)
	chain := &countingChain{}
	g.Chain = chain
	// 11 bytes of system, 5 of text, 4 + 11 + 17 of the tool: 48 bytes, 12 tokens.
	body := `{"model":"smart","system":"Be concise.","messages":[{"role":"user","content":"hello"}],` +
		`"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object"}}]}`
	rec := serve(g, anthropicPost("/v1/messages/count_tokens", body), DialectAnthropic)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"input_tokens":12}` || rec.Header().Get("Burrow-Estimated") != "1" ||
		rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if len(s.sent()) != 0 || chain.n != 0 {
		t.Fatalf("upstream calls %v, chain runs %d", s.sent(), chain.n)
	}
	// A body that cannot be read is the caller's fault.
	rec = serve(g, anthropicPost("/v1/messages/count_tokens", `{"model":"smart","messages":"nope"}`), DialectAnthropic)
	if rec.Code != 400 || rec.Header().Get("Burrow-Error-Code") != "invalid_request" {
		t.Fatalf("bad body: status %d body %s", rec.Code, rec.Body.String())
	}

	// Without the flag: the mismatch, as before.
	g, _ = translateGateway(s, false, tZai)
	rec = serve(g, anthropicPost("/v1/messages/count_tokens", body), DialectAnthropic)
	if rec.Code != 400 || rec.Header().Get("Burrow-Error-Code") != "format_mismatch" || rec.Header().Get("Burrow-Estimated") != "" {
		t.Fatalf("flag off: status %d body %s", rec.Code, rec.Body.String())
	}
	// With a native target the provider counts.
	g, _ = translateGateway(s, true, tZaiA, tZai)
	rec = serve(g, anthropicPost("/v1/messages/count_tokens", body), DialectAnthropic)
	if rec.Code != 200 || rec.Body.String() != `{"input_tokens":7}` || rec.Header().Get("Burrow-Estimated") != "" || s.n("zai-anthropic#ZAIA") != 1 {
		t.Fatalf("native: status %d body %s", rec.Code, rec.Body.String())
	}
}

// --- through a real server ---------------------------------------------------

// The upstream dies in the middle of a translated stream: the caller's stream
// ends with its own error event and is a complete HTTP response.
func TestTranslate_RealStack_UpstreamDiesMidStream(t *testing.T) {
	text := fixture(t, "chat/testdata/stream_text.sse")
	head := text[:bytes.Index(text, []byte(`"finish_reason":"stop"`))]
	head = head[:bytes.LastIndex(head, []byte("data:"))]
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(head)
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler) // the connection is cut
	}))
	t.Cleanup(up.Close)
	g, att := translateGateway(script(nil), true, tZai)
	g.Direct = DirectUpstreams(vaultMap{"ZAI": "sk-zai"}, up.Client().Transport)
	p := g.Providers.(fakeProviders)
	zp := p["zai"]
	zp.BaseURL = up.URL + "/v1"
	p["zai"] = zp
	sink := chained(t, g)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Burrow-Request-Id", "req-1")
		g.ServeDialect(w, r, DialectAnthropic)
	}))
	t.Cleanup(front.Close)

	req, _ := http.NewRequest("POST", front.URL+"/v1/messages", strings.NewReader(`{"model":"smart","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Api-Key", "bgw_all")
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	all, err := io.ReadAll(bufio.NewReader(resp.Body))
	if err != nil {
		t.Fatalf("the caller's response was cut: %v\n%s", err, all)
	}
	fr := frames(all)
	if resp.StatusCode != 200 || resp.Header.Get("Burrow-Translated") != "messages-chat" || len(fr) < 3 ||
		!strings.HasPrefix(fr[0], "event: message_start") || !strings.HasPrefix(fr[len(fr)-1], "event: error") {
		t.Fatalf("status %d headers %v\n%s", resp.StatusCode, resp.Header, all)
	}
	drained(g)
	if rows := att.all(); len(rows) != 1 || rows[0].ErrorCode != "stream_aborted" {
		t.Fatalf("attempts: %+v", rows)
	}
	waitFor(t, "the usage row", func() bool { return len(sink.all()) == 1 })
	if u := oneRow(t, sink); u.Translated != "messages-chat" || u.UpstreamStatus != 200 {
		t.Fatalf("usage row: %+v", u)
	}
}

// brokenPair fails to write the request, for a reason that is not the caller's.
type brokenPair struct{ translate.Pair }

func (brokenPair) Request([]byte, http.Header, string) ([]byte, bool, []string, error) {
	return nil, false, nil, errors.New("encoder broke")
}

// A request that cannot be written for one target, through no fault of the
// caller, is no attempt and says nothing about the provider: the next
// candidate is tried.
func TestTranslate_TranslatorFaultSkipsTheCandidate(t *testing.T) {
	old := lookupPair
	t.Cleanup(func() { lookupPair = old })
	n := 0
	lookupPair = func(from, to translate.Format) (translate.Pair, bool) {
		p, ok := old(from, to)
		if n++; ok && n == 1 {
			return brokenPair{p}, true
		}
		return p, ok
	}
	s := script(map[string]http.HandlerFunc{"zai#ZAI": jsonBody(chatAnswer), "openrouter#OR": jsonBody(chatAnswer)})
	g, att := translateGateway(s, true, tZai, tOR)
	var logs syncBuffer
	g.Log = slog.New(slog.NewTextHandler(&logs, nil))
	rec := serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 200 || s.n("zai#ZAI") != 0 || s.credentialReads("zai#ZAI") != 0 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	wantHeaders(t, rec, "openrouter", "google/gemini-x", "1")
	rows := att.all()
	if len(rows) != 2 || rows[0].ErrorCode != "translate_error" || rows[0].ProviderSlug != "zai" || rows[0].Status != 0 || rows[1].ErrorCode != "" {
		t.Fatalf("attempts: %+v", rows)
	}
	if ok, failed := reports(g.Breaker, "zai"); ok != 0 || failed != 0 {
		t.Fatalf("breaker: zai %d ok %d failed", ok, failed)
	}
	if !strings.Contains(logs.String(), "request translation failed") || strings.Contains(logs.String(), `"hi"`) {
		t.Fatalf("log: %s", logs.String())
	}

	// With nothing else to try the caller gets the gateway's own error.
	n = 0
	g, _ = translateGateway(s, true, tZai)
	g.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	rec = serve(g, anthropicPost("/v1/messages", messagesHi), DialectAnthropic)
	if rec.Code != 502 || rec.Header().Get("Burrow-Error-Code") != "upstream_unavailable" || rec.Header().Get("Burrow-Attempts") != "0" || s.n("zai#ZAI") != 0 {
		t.Fatalf("alone: status %d headers %v body %s", rec.Code, rec.Header(), rec.Body.String())
	}
}
