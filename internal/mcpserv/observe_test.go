package mcpserv

import (
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/observe"
)

type toolCallSink struct {
	mu    sync.Mutex
	calls []observe.ToolCall
}

func (s *toolCallSink) ObserveToolCall(c observe.ToolCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}

func (s *toolCallSink) all() []observe.ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observe.ToolCall(nil), s.calls...)
}

// Every tools/call on the management server is observed once, with source
// "burrow", the token and user, and an outcome of ok, denied or error. The
// arguments and the answer are not part of the event.
func TestMCP_ToolCallsAreObserved(t *testing.T) {
	newServed := func(t *testing.T, api *fakeAPI, perms []string) (*toolCallSink, string) {
		t.Helper()
		s := newTestServer(t, nil, nil, quietLog())
		s.SetAPIHandler(api)
		sink := &toolCallSink{}
		s.SetObserver(sink)
		hs := serveFor(t, s, &authedToken{tokenID: "tok1", userID: "u1", role: "admin", perms: perms, bearer: "Bearer bua_test"})
		return sink, hs.URL
	}

	t.Run("ok", func(t *testing.T) {
		sink, url := newServed(t, &fakeAPI{reply: `{}`}, []string{"services:configure:own"})
		cs := connectMCP(t, url, "", "2026-07-28")
		if res, err := callTool(t, cs, "services_list", nil); err != nil || res.IsError {
			t.Fatalf("call: %v %+v", err, res)
		}
		got := sink.all()
		if len(got) != 1 {
			t.Fatalf("observed %d calls, want 1: %+v", len(got), got)
		}
		c := got[0]
		if c.Server != "management" || c.Source != "burrow" || c.Tool != "services_list" || c.TokenID != "tok1" || c.UserID != "u1" ||
			c.Outcome != "ok" || c.GatewayKeyID != "" || c.LatencyMs != c.Duration.Milliseconds() {
			t.Errorf("event = %+v", c)
		}
	})

	t.Run("denied", func(t *testing.T) {
		api := &fakeAPI{}
		sink, url := newServed(t, api, []string{"audit:read"})
		cs := connectMCP(t, url, "", "2026-07-28")
		_, _ = callTool(t, cs, "provider_delete", map[string]any{"slug": "zai", "confirm": "zai"})
		got := sink.all()
		if len(got) != 1 || got[0].Outcome != "denied" || got[0].Tool != "provider_delete" || got[0].Source != "burrow" {
			t.Fatalf("events = %+v", got)
		}
	})

	t.Run("error", func(t *testing.T) {
		sink, url := newServed(t, &fakeAPI{status: 500, reply: `{"error":"boom"}`}, []string{"services:configure:own"})
		cs := connectMCP(t, url, "", "2026-07-28")
		_, _ = callTool(t, cs, "services_list", nil)
		got := sink.all()
		if len(got) != 1 || got[0].Outcome != "error" {
			t.Fatalf("events = %+v", got)
		}
	})
}
