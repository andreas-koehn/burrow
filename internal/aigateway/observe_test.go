package aigateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/observe"
)

// attemptSpy is a chain that reads, as the real one does when the gateway's
// handler has returned, what the gateway says it tried.
type attemptSpy struct{ got []observe.Attempt }

func (c *attemptSpy) Dispatch(http.ResponseWriter, *http.Request, string, string, string, string, http.Handler) {
}

func (c *attemptSpy) DispatchMetered(w http.ResponseWriter, r *http.Request, _, _, _, _ string, _ bool, up http.Handler) {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := failoverGateway(script(tc.handlers), tc.slots, nil)
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

// A request without a route has no attempts, and a nil route takes none.
func TestAttemptsWithoutARoute(t *testing.T) {
	var none *aigw.Route
	none.SetAttempts([]observe.Attempt{{ProviderSlug: "x"}})
	req, _ := http.NewRequest("POST", "/", nil)
	if got := aigw.AttemptsFrom(req.Context()); got != nil {
		t.Fatalf("attempts without a route: %+v", got)
	}
}
