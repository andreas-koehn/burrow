package aigateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/observe"
)

// attemptSpy is a chain that reads, as the real one does when the gateway's
// handler has returned, what the gateway says it tried.
type attemptSpy struct {
	got    []observe.Attempt
	routed bool // the request had a route when the chain was dispatched
}

func (c *attemptSpy) Dispatch(http.ResponseWriter, *http.Request, string, string, string, string, http.Handler) {
}

func (c *attemptSpy) DispatchMetered(w http.ResponseWriter, r *http.Request, _, _, _, _ string, _ bool, up http.Handler) {
	_, c.routed = aigw.RouteFrom(r.Context())
	up.ServeHTTP(w, r)
	c.got = aigw.AttemptsFrom(r.Context())
}

// What was sent to an upstream is on the route when the gateway is done, in
// order, with the provider's service and whether the attempt counts against
// the provider.
func TestFailover_AttemptsReachTheRoute(t *testing.T) {
	zai := observe.Attempt{ProviderSlug: "zai", ServiceID: "prov-zai", Sent: true}
	or := observe.Attempt{ProviderSlug: "openrouter", ServiceID: "prov-or", Sent: true}
	with := func(a observe.Attempt, status int, failed bool) observe.Attempt {
		a.Status, a.Failed = status, failed
		return a
	}
	for _, tc := range []struct {
		name     string
		slots    string
		handlers map[string]http.HandlerFunc
		mut      func(*db.AIModel)
		offline  bool // no tunnel client is connected
		status   int
		want     []observe.Attempt
	}{
		{
			name: "the first target fails, the second answers", slots: "ZAI",
			handlers: map[string]http.HandlerFunc{"zai#ZAI": status(500, "boom"), "openrouter#OR": status(200, `{"ok":true}`)},
			status:   200,
			want:     []observe.Attempt{with(zai, 500, true), with(or, 200, false)},
		},
		{
			name: "another key of the same provider answers", slots: "ZAI,ZAI2",
			handlers: map[string]http.HandlerFunc{"zai#ZAI": status(503, "busy"), "zai#ZAI2": status(200, `{"ok":true}`)},
			status:   200,
			want:     []observe.Attempt{with(zai, 503, true), with(zai, 200, false)},
		},
		{
			name: "the request's own fault is no failure of the provider", slots: "ZAI",
			handlers: map[string]http.HandlerFunc{"zai#ZAI": status(400, `{"error":"bad"}`), "openrouter#OR": status(200, `{"ok":true}`)},
			status:   400,
			want:     []observe.Attempt{with(zai, 400, false)},
		},
		{
			name: "everything fails", slots: "ZAI",
			handlers: map[string]http.HandlerFunc{"zai#ZAI": status(500, "boom"), "openrouter#OR": status(502, "boom")},
			status:   502,
			want:     []observe.Attempt{with(zai, 500, true), with(or, 502, true)},
		},
		{
			// Refused by the gateway before the upstream was called, with a
			// 5xx: the breaker hears of it, so the observer does.
			name: "the credential is not configured, the second answers", slots: "ZAI",
			handlers: map[string]http.HandlerFunc{"openrouter#OR": status(200, `{"ok":true}`)},
			status:   200,
			want: []observe.Attempt{
				{ProviderSlug: "zai", ServiceID: "prov-zai", Status: 503, Failed: true},
				with(or, 200, false),
			},
		},
		{
			name: "the tunnel client is offline, the second answers", slots: "ZAI",
			handlers: map[string]http.HandlerFunc{"openrouter#OR": status(200, `{"ok":true}`)},
			mut: func(m *db.AIModel) {
				m.Targets = []db.AIModelTarget{
					{Dialect: "openai", Position: 0, ProviderSlug: "ollama", TargetModel: "mistral"},
					{Dialect: "openai", Position: 1, ProviderSlug: "openrouter", TargetModel: "google/gemini-x"},
				}
			},
			offline: true,
			status:  200,
			want: []observe.Attempt{
				{ProviderSlug: "ollama", ServiceID: "svc1", Status: 502, Failed: true},
				with(or, 200, false),
			},
		},
		{
			// A 429 is not the provider's failure (the breaker is not told):
			// no upstream error, and the move to the next target is a failover.
			name: "a rate limit with the flag off is the answer", slots: "ZAI",
			handlers: map[string]http.HandlerFunc{"zai#ZAI": status(429, "slow"), "openrouter#OR": status(200, "{}")},
			status:   429,
			want:     []observe.Attempt{with(zai, 429, false)},
		},
		{
			name: "a rate limit with the flag on moves to the next target", slots: "ZAI",
			handlers: map[string]http.HandlerFunc{"zai#ZAI": status(429, "slow"), "openrouter#OR": status(200, "{}")},
			mut:      func(m *db.AIModel) { m.FallbackOnRateLimit = true },
			status:   200,
			want:     []observe.Attempt{with(zai, 429, false), with(or, 200, false)},
		},
		{
			name: "a rate limit on one key moves to the next key of the provider", slots: "ZAI,ZAI2",
			handlers: map[string]http.HandlerFunc{"zai#ZAI": status(429, "slow"), "zai#ZAI2": status(200, "{}")},
			status:   200,
			want:     []observe.Attempt{with(zai, 429, false), with(zai, 200, false)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := failoverGateway(script(tc.handlers), tc.slots, tc.mut)
			if tc.offline {
				g.Tunnels = fakeTunnels{}
			}
			spy := &attemptSpy{}
			g.Chain = spy
			rec := call(g, smartBody)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if len(spy.got) != len(tc.want) {
				t.Fatalf("attempts %+v, want %+v", spy.got, tc.want)
			}
			for i := range tc.want {
				if spy.got[i] != tc.want[i] {
					t.Errorf("attempt %d: %+v, want %+v", i, spy.got[i], tc.want[i])
				}
			}
		})
	}
}

// A caller who hangs up while an attempt is in flight is no upstream error:
// the attempt is on the list, sent, but not failed, and nothing follows it.
func TestFailover_ClientGoneIsNoFailedAttempt(t *testing.T) {
	var cancel context.CancelFunc
	s := script(map[string]http.HandlerFunc{
		"zai#ZAI": func(w http.ResponseWriter, _ *http.Request) {
			cancel() // the caller hangs up while zai is working
			w.WriteHeader(500)
		},
		"openrouter#OR": status(200, "{}"),
	})
	g, _ := failoverGateway(s, "ZAI", nil)
	spy := &attemptSpy{}
	g.Chain = spy
	var ctx context.Context
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	_ = serve(g, post("/v1/chat/completions", "bgw_all", smartBody).WithContext(ctx), DialectOpenAI)
	if len(spy.got) != 1 || spy.got[0].Failed || !spy.got[0].Sent || spy.got[0].ProviderSlug != "zai" {
		t.Fatalf("attempts %+v, want one sent, not failed attempt at zai", spy.got)
	}
}

// serveOwnPath sends an inference request to the provider's own path
// (/ai/<slug>/v1/...) with a gateway key and returns what the chain's
// observer is handed.
func serveOwnPath(t *testing.T, g *Gateway, ctx context.Context) []observe.Attempt {
	t.Helper()
	spy := &attemptSpy{}
	g.Chain = spy
	req := post("/v1/chat/completions", "bgw_all", `{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`).WithContext(ctx)
	g.Serve(httptest.NewRecorder(), req, "zai")
	drained(g)
	return spy.got
}

// The provider's own path has no fallback: one attempt, which counts against
// the provider as a 5xx does on a dialect endpoint, and is no failure when the
// caller hung up.
func TestOwnPath_AttemptIsReported(t *testing.T) {
	zai := observe.Attempt{ProviderSlug: "zai", ServiceID: "prov-zai", Sent: true}
	with := func(status int, failed bool) []observe.Attempt {
		a := zai
		a.Status, a.Failed = status, failed
		return []observe.Attempt{a}
	}
	run := func(h http.HandlerFunc, ctx context.Context) []observe.Attempt {
		g, _ := failoverGateway(script(map[string]http.HandlerFunc{"zai#ZAI": h}), "ZAI", nil)
		return serveOwnPath(t, g, ctx)
	}
	for name, tc := range map[string]struct {
		h    http.HandlerFunc
		want []observe.Attempt
	}{
		"an answer":                  {status(200, `{"ok":true}`), with(200, false)},
		"the request's own fault":    {status(400, `{"error":"bad"}`), with(400, false)},
		"a rate limit":               {status(429, "slow"), with(429, false)},
		"an upstream failure":        {status(503, "down"), with(503, true)},
		"a handler that writes none": {func(http.ResponseWriter, *http.Request) {}, with(200, false)},
	} {
		t.Run(name, func(t *testing.T) {
			got := run(tc.h, context.Background())
			if len(got) != 1 || got[0] != tc.want[0] {
				t.Fatalf("attempts %+v, want %+v", got, tc.want)
			}
		})
	}
	t.Run("the caller hangs up", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := run(func(w http.ResponseWriter, _ *http.Request) {
			cancel()
			w.WriteHeader(500)
		}, ctx)
		if len(got) != 1 || got[0].Failed {
			t.Fatalf("attempts %+v, want one that is not failed", got)
		}
	})
}

// A key that is not a gateway key (a service key, as every client from before
// gateway keys has) gives the request no route, so its usage row and its
// rate-limit subject stay what they were; its attempt is reported all the
// same.
func TestOwnPath_ServiceKeyAttemptIsReported(t *testing.T) {
	for name, tc := range map[string]struct {
		h      http.HandlerFunc
		failed bool
		status int
	}{
		"an upstream failure": {status(503, "down"), true, 503},
		"an answer":           {status(200, `{"ok":true}`), false, 200},
	} {
		t.Run(name, func(t *testing.T) {
			spy := &attemptSpy{}
			g := newGateway(tc.h, spy)
			req := post("/v1/chat/completions", "sk-good", `{"model":"m","messages":[]}`)
			g.Serve(httptest.NewRecorder(), req, "ollama")
			if spy.routed {
				t.Fatal("a service-key request got a route")
			}
			want := observe.Attempt{ProviderSlug: "ollama", ServiceID: "svc1", Status: tc.status, Failed: tc.failed, Sent: true}
			if len(spy.got) != 1 || spy.got[0] != want {
				t.Fatalf("attempts %+v, want %+v", spy.got, want)
			}
		})
	}
}

// A request without a holder has no attempts, and setting them is a no-op.
func TestAttemptsWithoutAHolder(t *testing.T) {
	req, _ := http.NewRequest("POST", "/", nil)
	aigw.SetAttemptsIn(req.Context(), []observe.Attempt{{ProviderSlug: "x"}})
	if got := aigw.AttemptsFrom(req.Context()); got != nil {
		t.Fatalf("attempts without a holder: %+v", got)
	}
}
