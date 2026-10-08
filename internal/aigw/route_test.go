package aigw_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
)

func TestRoute_ContextRoundTrip(t *testing.T) {
	if _, ok := aigw.RouteFrom(context.Background()); ok {
		t.Fatal("empty context reported a route")
	}
	r := aigw.NewRoute("gk1", "anthropic", "burrow-medium", "req-1")
	ctx := aigw.WithRoute(context.Background(), r)
	r.SetTarget("openrouter", "google/gemini-x") // set after the context was built
	got, ok := aigw.RouteFrom(ctx)
	want := aigw.RouteInfo{GatewayKeyID: "gk1", Dialect: "anthropic", ProviderSlug: "openrouter", RequestedModel: "burrow-medium", TargetModel: "google/gemini-x", RequestID: "req-1"}
	if !ok || got != want {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

// A nil route in the context counts as no route.
func TestRoute_NilRouteInContext(t *testing.T) {
	if _, ok := aigw.RouteFrom(aigw.WithRoute(context.Background(), nil)); ok {
		t.Fatal("nil route reported as a route")
	}
}

func TestRoute_ConcurrentSetAndSnapshot(t *testing.T) {
	r := aigw.NewRoute("gk1", "openai", "m", "req")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.SetTarget("p", "t") }()
		go func() { defer wg.Done(); _ = r.Snapshot() }()
	}
	wg.Wait() // meaningful under -race
}

func TestRoute_Translation(t *testing.T) {
	r := aigw.NewRoute("gk", "anthropic", "burrow-medium", "req")
	if ri := r.Snapshot(); ri.Translated != "" || ri.Dropped != "" {
		t.Fatalf("fresh route: %+v", ri)
	}
	r.SetTranslation("messages-chat", []string{"top_k", "cache_control", "top_k"})
	ri := r.Snapshot()
	if ri.Translated != "messages-chat" || ri.Dropped != "cache_control,top_k" { // sorted, de-duplicated
		t.Fatalf("after SetTranslation: %+v", ri)
	}
	r.SetTranslation("", nil) // a later native attempt clears it
	if ri := r.Snapshot(); ri.Translated != "" || ri.Dropped != "" {
		t.Fatalf("after clearing: %+v", ri)
	}
	// Without a pair nothing was translated, so nothing was dropped either.
	r.SetTranslation("", []string{"top_k"})
	if ri := r.Snapshot(); ri.Translated != "" || ri.Dropped != "" {
		t.Fatalf("dropped without a pair: %+v", ri)
	}
}

// Dropped names come from field names of the request, which the client
// chooses: what is stored is cleaned and bounded.
func TestRoute_TranslationIsBounded(t *testing.T) {
	r := aigw.NewRoute("gk", "anthropic", "m", "req")
	r.SetTranslation("messages-chat", []string{"unknown:a,b", "x\r\ny\x00", " ", "", "tool:web search", "unknown:é\"<"})
	if got := r.Snapshot().Dropped; got != "tool:websearch,unknown:,unknown:ab,xy" {
		t.Fatalf("cleaned names = %q", got)
	}

	// One name is cut to 64 bytes, and the list says that something was cut.
	r.SetTranslation("messages-chat", []string{strings.Repeat("n", 500)})
	if got := r.Snapshot().Dropped; got != strings.Repeat("n", 64)+",more" {
		t.Fatalf("long name stored as %d bytes: %q", len(got), got)
	}

	// Many names: at most 512 bytes, cut at a comma, at most 32 entries, the
	// last of them "more" when names were left out.
	var many []string
	for i := 0; i < 500; i++ {
		many = append(many, fmt.Sprintf("unknown:field_%03d_%s", i, strings.Repeat("x", 30)))
	}
	r.SetTranslation("messages-chat", many)
	got := r.Snapshot().Dropped
	parts := strings.Split(got, ",")
	if len(got) > 512 || len(parts) > 32 || len(parts) < 5 || parts[len(parts)-1] != "more" {
		t.Fatalf("dropped is %d bytes in %d entries: %q", len(got), len(parts), got)
	}
	for i, p := range parts[:len(parts)-1] {
		if p != many[i] {
			t.Fatalf("entry %d = %q, want the whole name %q", i, p, many[i])
		}
	}
	short := make([]string, 100)
	for i := range short {
		short[i] = fmt.Sprintf("f%03d", i)
	}
	r.SetTranslation("messages-chat", short)
	if parts := strings.Split(r.Snapshot().Dropped, ","); len(parts) != 32 || parts[30] != "f030" || parts[31] != "more" {
		t.Fatalf("%d entries stored, want 31 names and \"more\": %v", len(parts), parts)
	}

	// The pair id is one of a few constants; a wrong caller still cannot
	// store more than a short name.
	r.SetTranslation("messages-chat\n"+strings.Repeat("z", 300), nil)
	if got := r.Snapshot().Translated; len(got) != 64 || strings.ContainsAny(got, "\n ") {
		t.Fatalf("pair stored as %q", got)
	}
}

// The upstream's figures of a translated answer travel on the route; without
// them the chain meters what it sees itself.
func TestRoute_UpstreamUsage(t *testing.T) {
	r := aigw.NewRoute("gk", "openai", "smart", "req-1")
	if ri := r.Snapshot(); ri.UpstreamUsage || ri.TokensIn != 0 || ri.TokensOut != 0 {
		t.Fatalf("a new route: %+v", ri)
	}
	r.SetUpstreamUsage(12, 0)
	if ri := r.Snapshot(); !ri.UpstreamUsage || ri.TokensIn != 12 || ri.TokensOut != 0 {
		t.Fatalf("after SetUpstreamUsage: %+v", ri)
	}
}

// UpstreamUsage reads an answer the way the chain reads a native one of that
// kind: usage frames of a stream, the body of an answer in one piece, the
// byte estimate when neither names any.
func TestUpstreamUsage(t *testing.T) {
	stream := http.Header{"Content-Type": {"text/event-stream"}}
	plain := http.Header{"Content-Type": {"application/json"}}

	u := aigw.NewUpstreamUsage(aigw.KindAnthropic)
	_, _ = u.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":3,\"output_tokens\":1}}}\n\n"))
	if in, out := u.Tokens(stream); in != 10 || out != 1 {
		t.Fatalf("a cut Anthropic stream: %d in, %d out", in, out)
	}

	u = aigw.NewUpstreamUsage(aigw.KindOpenAI)
	_, _ = u.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":5,`))
	_, _ = u.Write([]byte(`"completion_tokens":2,"total_tokens":7}}`))
	if in, out := u.Tokens(plain); in != 5 || out != 2 {
		t.Fatalf("an OpenAI body: %d in, %d out", in, out)
	}

	u = aigw.NewUpstreamUsage(aigw.KindOpenAI)
	_, _ = u.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"12345678\"}}]}\n\n"))
	if in, out := u.Tokens(stream); in != 0 || out == 0 {
		t.Fatalf("a stream without usage: %d in, %d out, want the byte estimate", in, out)
	}
}
