package aigateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/credinject"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

// Upstream credentials of the rig. None of them may show up anywhere but in
// a header of the request to its own target.
const (
	credA      = "sk-tunnel-a-11111"
	credB      = "sk-tunnel-b-22222"
	credHosted = "sk-hosted-33333"
)

// seenHeaders records the headers each target's upstream received.
type seenHeaders struct {
	mu   sync.Mutex
	seen map[string][]http.Header
}

func (s *seenHeaders) note(who string, h http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string][]http.Header{}
	}
	s.seen[who] = append(s.seen[who], h.Clone())
}

// only returns the headers of the one request who received.
func (s *seenHeaders) only(t *testing.T, who string) http.Header {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen[who]) != 1 {
		t.Fatalf("%s received %d requests, want 1", who, len(s.seen[who]))
	}
	return s.seen[who][0]
}

func (s *seenHeaders) count(who string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen[who])
}

// tunnelsByService is a TunnelDialer with one upstream per service id.
type tunnelsByService map[string]http.Handler

func (m tunnelsByService) LookupByServiceID(_ context.Context, id string) (*proxy.Resolved, error) {
	if m[id] == nil {
		return nil, proxy.ErrNotFound
	}
	return &proxy.Resolved{ServiceID: id, AccessMode: "api_key", LocalHost: "127.0.0.1:11434"}, nil
}

func (m tunnelsByService) DialTunnelStreamByServiceID(_ context.Context, id string) (net.Conn, error) {
	h := m[id]
	if h == nil {
		return nil, proxy.ErrNotFound
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		_ = rec.Result().Write(server)
	}()
	return client, nil
}

// bindings is a credinject.Store held in memory.
type bindings map[string]credinject.Binding

func (b bindings) GetBinding(_ context.Context, id string) (credinject.Binding, bool, error) {
	bind, ok := b[id]
	return bind, ok, nil
}
func (b bindings) PutBinding(context.Context, credinject.Binding) error { return nil }
func (b bindings) DeleteBinding(context.Context, string) error          { return nil }

type slotVault map[string]string

func (v slotVault) Get(slot string) (string, bool) { s, ok := v[slot]; return s, ok }
func (v slotVault) Slots() []string                { return nil }

// credRig is a gateway with two tunnel providers ("tun-a" on service "svc-a",
// "tun-b" on "svc-b") and a hosted one ("hosted"), behind the real chain with
// the real credential injector. status says what each target answers.
type credRig struct {
	g      *Gateway
	seen   *seenHeaders
	att    *memAttempts
	log    *syncBuffer
	status map[string]int
}

func newCredRig(t *testing.T, binds bindings, targets ...string) *credRig {
	t.Helper()
	rig := &credRig{seen: &seenHeaders{}, log: &syncBuffer{}, status: map[string]int{}}
	answer := func(who string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			rig.seen.note(who, r.Header)
			code := rig.status[who]
			if code == 0 {
				code = 200
			}
			body := `{"from":"` + who + `"}`
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		}
	}
	hosted := httptest.NewTLSServer(answer("hosted"))
	t.Cleanup(hosted.Close)

	log := slog.New(slog.NewTextHandler(rig.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g := resolveGateway()
	g.Log = log
	g.Providers = fakeProviders{
		"tun-a":  {Slug: "tun-a", Kind: "tunnel", ServiceID: "svc-a", APIFormat: "openai"},
		"tun-b":  {Slug: "tun-b", Kind: "tunnel", ServiceID: "svc-b", APIFormat: "openai"},
		"hosted": {Slug: "hosted", Kind: "direct", ServiceID: "prov-h", APIFormat: "openai", CredentialSlot: "H", BaseURL: hosted.URL + "/v1"},
	}
	g.Keys = &fakeKeys{service: "svc-a", good: "sk-good", id: "key-1"}
	g.Tunnels = tunnelsByService{"svc-a": answer("tun-a"), "svc-b": answer("tun-b")}
	g.ServicePolicy = allowAll
	g.Direct = DirectUpstreams(vaultMap{"H": credHosted}, hosted.Client().Transport)
	m := db.AIModel{Name: "smart", Enabled: true, AttemptTimeoutS: 60000, TotalTimeoutS: 120000}
	for i, slug := range targets {
		m.Targets = append(m.Targets, db.AIModelTarget{Dialect: "openai", Position: i, ProviderSlug: slug, TargetModel: "m-" + slug})
	}
	g.Synthetic = fakeSynthetic{"smart": m}
	g.GatewayKeys = fakeGatewayKeys{"bgw_all": {ID: "gk"}}
	g.Breaker = NewBreaker()
	rig.att = &memAttempts{}
	g.Attempts = rig.att
	g.timeUnit = 1e6 // milliseconds

	inj := credinject.New(slotVault{"SA": credA, "SB": credB}, binds, log)
	g.Credentials = inj
	chain := aigw.NewChain(nil, nil, inj, nil, nil, nil, nil, &recSink{}, log)
	chain.Loader = cfgLoader{}
	g.Chain = chain
	rig.g = g
	return rig
}

// call sends a chat request with every kind of client credential on it.
func (rig *credRig) call(model string) *httptest.ResponseRecorder {
	r := post("/v1/chat/completions", "bgw_all", `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
	r.Header.Set("X-Api-Key", "client-x-api-key")
	r.Header.Set("Cookie", "burrow_session=client-cookie")
	r.Header.Set("Proxy-Authorization", "Basic client-proxy")
	return serve(rig.g, r, DialectOpenAI)
}

// noSecretsOutside fails when a credential shows up in the response, the
// attempt log or the relay's own log.
func (rig *credRig) noSecretsOutside(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	out := rec.Body.String() + fmt.Sprint(rec.Header()) + fmt.Sprintf("%+v", rig.att.all()) + rig.log.String()
	for _, secret := range []string{credA, credB, credHosted, "bgw_all"} {
		if strings.Contains(out, secret) {
			t.Fatalf("a credential (%s…) is in the response, the attempt log or the log", secret[:6])
		}
	}
}

// clientCredentialsGone fails when h carries something the client sent as a
// credential.
func clientCredentialsGone(t *testing.T, who string, h http.Header) {
	t.Helper()
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization"} {
		for _, v := range h.Values(name) {
			if strings.Contains(v, "client-") || strings.Contains(v, "bgw_") {
				t.Fatalf("%s received the client's %s", who, name)
			}
		}
	}
}

// wantOnly fails unless h has exactly the given header values (name, value
// pairs) among every header a credential of this rig could travel in.
func wantOnly(t *testing.T, who string, h http.Header, pairs ...string) {
	t.Helper()
	want := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		want[http.CanonicalHeaderKey(pairs[i])] = pairs[i+1]
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Upstream-A", "X-Upstream-B", "Proxy-Authorization", "Cookie"} {
		got := h.Values(name)
		if w, ok := want[name]; ok {
			if len(got) != 1 || got[0] != w {
				t.Fatalf("%s: %s has %d value(s), want exactly its own", who, name, len(got))
			}
			continue
		}
		if len(got) != 0 {
			t.Fatalf("%s received a %s header (%d value(s)) that is not its own", who, name, len(got))
		}
	}
	// Whatever the name: no value of another target's credential.
	for name, vals := range h {
		for _, v := range vals {
			for _, secret := range []string{credA, credB, credHosted} {
				if strings.Contains(v, secret) && want[name] != v {
					t.Fatalf("%s received another target's credential in %s", who, name)
				}
			}
		}
	}
}

var (
	bindA     = credinject.Binding{ServiceID: "svc-a", Slot: "SA", HeaderName: "X-Upstream-A", HeaderFormat: "A {key}"}
	bindAAuth = credinject.Binding{ServiceID: "svc-a", Slot: "SA", HeaderName: "Authorization", HeaderFormat: "Bearer {key}"}
	bindB     = credinject.Binding{ServiceID: "svc-b", Slot: "SB", HeaderName: "Authorization", HeaderFormat: "Bearer {key}"}
	bindBOwn  = credinject.Binding{ServiceID: "svc-b", Slot: "SB", HeaderName: "X-Upstream-B", HeaderFormat: "B {key}"}
)

// Every attempt of a fallback sends the credential of its own target and
// nothing of another target's, whatever header a binding uses.
func TestFailover_CredentialIsTheTargetsOwn(t *testing.T) {
	cases := []struct {
		name          string
		binds         bindings
		first, second string
		wantFirst     []string
		wantSecond    []string
	}{
		{
			name: "tunnel then tunnel, different headers", binds: bindings{"svc-a": bindA, "svc-b": bindB},
			first: "tun-a", second: "tun-b",
			wantFirst:  []string{"X-Upstream-A", "A " + credA},
			wantSecond: []string{"Authorization", "Bearer " + credB},
		},
		{
			name: "tunnel then tunnel, the first binds Authorization", binds: bindings{"svc-a": bindAAuth, "svc-b": bindBOwn},
			first: "tun-a", second: "tun-b",
			wantFirst:  []string{"Authorization", "Bearer " + credA},
			wantSecond: []string{"X-Upstream-B", "B " + credB},
		},
		{
			name: "tunnel then tunnel, the same header", binds: bindings{"svc-a": bindAAuth, "svc-b": bindB},
			first: "tun-a", second: "tun-b",
			wantFirst:  []string{"Authorization", "Bearer " + credA},
			wantSecond: []string{"Authorization", "Bearer " + credB},
		},
		{
			name: "fallback target without a binding", binds: bindings{"svc-a": bindAAuth},
			first: "tun-a", second: "tun-b",
			wantFirst:  []string{"Authorization", "Bearer " + credA},
			wantSecond: nil,
		},
		{
			name: "fallback target without a binding, other header", binds: bindings{"svc-a": bindA},
			first: "tun-a", second: "tun-b",
			wantFirst:  []string{"X-Upstream-A", "A " + credA},
			wantSecond: nil,
		},
		{
			name: "first target without a binding", binds: bindings{"svc-b": bindBOwn},
			first: "tun-a", second: "tun-b",
			wantFirst:  nil,
			wantSecond: []string{"X-Upstream-B", "B " + credB},
		},
		{
			name: "tunnel then direct", binds: bindings{"svc-a": bindA},
			first: "tun-a", second: "hosted",
			wantFirst:  []string{"X-Upstream-A", "A " + credA},
			wantSecond: []string{"Authorization", "Bearer " + credHosted},
		},
		{
			name: "direct then tunnel", binds: bindings{"svc-a": bindA},
			first: "hosted", second: "tun-a",
			wantFirst:  []string{"Authorization", "Bearer " + credHosted},
			wantSecond: []string{"X-Upstream-A", "A " + credA},
		},
		{
			name: "direct then tunnel without a binding", binds: bindings{},
			first: "hosted", second: "tun-b",
			wantFirst:  []string{"Authorization", "Bearer " + credHosted},
			wantSecond: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCredRig(t, tc.binds, tc.first, tc.second)
			rig.status[tc.first] = 500
			rec := rig.call("smart")
			if rec.Code != 200 || rec.Body.String() != `{"from":"`+tc.second+`"}` {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			wantHeaders(t, rec, tc.second, "m-"+tc.second, "2")
			first, second := rig.seen.only(t, tc.first), rig.seen.only(t, tc.second)
			wantOnly(t, tc.first, first, tc.wantFirst...)
			wantOnly(t, tc.second, second, tc.wantSecond...)
			clientCredentialsGone(t, tc.first, first)
			clientCredentialsGone(t, tc.second, second)
			rig.noSecretsOutside(t, rec)
		})
	}
}

// With one target nothing changes: the bound header, its value, set once, on
// a synthetic model, a direct address and the provider path alike.
func TestFailover_CredentialOfASingleTargetIsUnchanged(t *testing.T) {
	want := []string{"X-Upstream-A", "A " + credA}
	t.Run("synthetic model", func(t *testing.T) {
		rig := newCredRig(t, bindings{"svc-a": bindA, "svc-b": bindB}, "tun-a")
		rec := rig.call("smart")
		if rec.Code != 200 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		h := rig.seen.only(t, "tun-a")
		wantOnly(t, "tun-a", h, want...)
		clientCredentialsGone(t, "tun-a", h)
		rig.noSecretsOutside(t, rec)
	})
	t.Run("direct address", func(t *testing.T) {
		rig := newCredRig(t, bindings{"svc-a": bindA, "svc-b": bindB}, "tun-b")
		rec := rig.call("tun-a/mistral")
		if rec.Code != 200 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		wantOnly(t, "tun-a", rig.seen.only(t, "tun-a"), want...)
		if rig.seen.count("tun-b") != 0 {
			t.Fatal("tun-b was called for a request addressed to tun-a")
		}
	})
	t.Run("direct address of a hosted provider", func(t *testing.T) {
		rig := newCredRig(t, bindings{"svc-a": bindA}, "tun-a")
		rec := rig.call("hosted/gpt")
		if rec.Code != 200 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		wantOnly(t, "hosted", rig.seen.only(t, "hosted"), "Authorization", "Bearer "+credHosted)
	})
	t.Run("provider path", func(t *testing.T) {
		rig := newCredRig(t, bindings{"svc-a": bindA, "svc-b": bindB}, "tun-a")
		r := post("/v1/chat/completions", "sk-good", `{"model":"mistral"}`)
		rec := httptest.NewRecorder()
		rig.g.Serve(rec, r, "tun-a")
		if rec.Code != 200 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		h := rig.seen.only(t, "tun-a")
		wantOnly(t, "tun-a", h, want...)
		clientCredentialsGone(t, "tun-a", h)
	})
	t.Run("provider path, binding on Authorization", func(t *testing.T) {
		rig := newCredRig(t, bindings{"svc-a": bindAAuth}, "tun-a")
		rec := httptest.NewRecorder()
		rig.g.Serve(rec, post("/v1/chat/completions", "sk-good", `{"model":"mistral"}`), "tun-a")
		if rec.Code != 200 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		wantOnly(t, "tun-a", rig.seen.only(t, "tun-a"), "Authorization", "Bearer "+credA)
	})
}

// The credential is read last: a target whose policy refuses the caller gets
// no binding lookup, and the next target still gets only its own.
func TestFailover_CredentialIsReadAfterTheTargetsPolicy(t *testing.T) {
	rig := newCredRig(t, nil, "tun-a", "tun-b")
	var looked []string
	store := countingBindings{bindings: bindings{"svc-a": bindA, "svc-b": bindB}, looked: &looked}
	inj := credinject.New(slotVault{"SA": credA, "SB": credB}, store, rig.g.Log)
	rig.g.Credentials = inj
	rig.g.Chain.(*aigw.Chain).CredInjector = inj
	// tun-a's tunnel is gone: its policy cannot be read, so it is not called.
	tun := rig.g.Tunnels.(tunnelsByService)
	delete(tun, "svc-a")
	rec := rig.call("smart")
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if len(looked) != 1 || looked[0] != "svc-b" {
		t.Fatalf("bindings read for %v, want svc-b only", looked)
	}
	wantOnly(t, "tun-b", rig.seen.only(t, "tun-b"), "Authorization", "Bearer "+credB)
}

type countingBindings struct {
	bindings
	looked *[]string
}

func (c countingBindings) GetBinding(ctx context.Context, id string) (credinject.Binding, bool, error) {
	*c.looked = append(*c.looked, id)
	return c.bindings.GetBinding(ctx, id)
}

// When the first target's policy cannot be read the cache is left alone, and
// the upstream is not told so: Burrow adds no request header of its own.
func TestFailover_CacheBypassDoesNotReachTheUpstream(t *testing.T) {
	raw, err := db.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	d := db.Wrap(raw)
	t.Cleanup(func() { _ = d.Close() })

	rig := newCredRig(t, bindings{}, "tun-a", "tun-b")
	log := rig.g.Log
	chain := aigw.NewChain(exact.New(d, log), nil, nil, nil, nil, nil, nil, &recSink{}, log)
	chain.Loader = cfgLoader{Cache: &exact.Settings{Enabled: true, AppliesPer: "global", TTLSeconds: 300, MaxEntries: 100, MaxPerEntryKB: 64}}
	rig.g.Chain = chain

	// The first target answers and its answer is cached.
	if rec := rig.call("smart"); rec.Code != 200 || rec.Body.String() != `{"from":"tun-a"}` {
		t.Fatalf("first: status %d body %s", rec.Code, rec.Body.String())
	}
	if rec := rig.call("smart"); rec.Header().Get("Burrow-Cache") != "HIT" || rig.seen.count("tun-a") != 1 {
		t.Fatalf("second: Burrow-Cache %q, tun-a called %d times", rec.Header().Get("Burrow-Cache"), rig.seen.count("tun-a"))
	}
	// Its tunnel goes away: the caller cannot be checked against its policy,
	// so its cache is not used, and the next target answers.
	tun := rig.g.Tunnels.(tunnelsByService)
	delete(tun, "svc-a")
	rec := rig.call("smart")
	if rec.Code != 200 || rec.Body.String() != `{"from":"tun-b"}` || rec.Header().Get("Burrow-Cache") == "HIT" {
		t.Fatalf("offline: status %d body %s Burrow-Cache %q", rec.Code, rec.Body.String(), rec.Header().Get("Burrow-Cache"))
	}
	if got := rig.seen.only(t, "tun-b").Values("Burrow-Cache"); len(got) != 0 {
		t.Fatalf("the upstream received Burrow-Cache: %v", got)
	}
}
