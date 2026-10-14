// cmd/server/gateway_attempts_test.go
//
// What the model gateway tried reaches the observer through the chain, and
// reaches it once: the failover and the upstream error of a failed attempt
// are counted by the attempts of the gateway and by no other route, and a
// caller who hangs up is no upstream error. Real wiring as in
// e2e_gateway_test.go; the recorder is the stack's own, the event sink and a
// tap on the chain's observer are the test's.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/metrics"
	"github.com/ankoehn/burrow/internal/observe"
)

// tapObserver passes every model request on to the real observer and then
// says it did: a barrier for assertions about what did not happen.
type tapObserver struct {
	inner interface{ ObserveModelRequest(observe.ModelRequest) }
	done  chan observe.ModelRequest
}

func (t *tapObserver) ObserveModelRequest(e observe.ModelRequest) {
	t.inner.ObserveModelRequest(e)
	t.done <- e
}

// gatewayAttemptsEnv boots the gateway with providers alpha and beta behind
// one model, "smart", and a gateway key.
func gatewayAttemptsEnv(t *testing.T) (e *gwEnv, key string, rec *metrics.Recorder, events *recEmitter, taps chan observe.ModelRequest) {
	t.Helper()
	e = bootGatewayE2E(t)
	base := e.up.srv.URL
	for _, p := range []map[string]any{
		{"slug": "alpha", "name": "Alpha", "kind": "direct", "api_format": "openai", "base_url": base + "/alpha/v1", "credential_slot": "ALPHA"},
		{"slug": "beta", "name": "Beta", "kind": "direct", "api_format": "openai", "base_url": base + "/beta/v1", "credential_slot": "BETA"},
	} {
		if code, body := e.admin(t, "POST", "/api/v1/ai/providers", p); code != http.StatusCreated {
			t.Fatalf("create provider %v: %d %s", p["slug"], code, body)
		}
	}
	for slug, id := range map[string]string{"alpha": "m-a", "beta": "m-b"} {
		if code, body := e.admin(t, "POST", "/api/v1/ai/providers/"+slug+"/models", map[string]string{"id": id}); code/100 != 2 {
			t.Fatalf("add catalog entry %s/%s: %d %s", slug, id, code, body)
		}
	}
	if code, body := e.admin(t, "POST", "/api/v1/ai/models", map[string]any{"name": "smart", "targets": []map[string]string{
		{"dialect": "openai", "provider": "alpha", "model": "m-a"},
		{"dialect": "openai", "provider": "beta", "model": "m-b"},
	}}); code != http.StatusCreated {
		t.Fatalf("create model: %d %s", code, body)
	}
	code, b := e.admin(t, "POST", "/api/v1/ai/keys", map[string]any{"name": "all"})
	var out struct{ Key string }
	if code != http.StatusCreated || json.Unmarshal(b, &out) != nil || out.Key == "" {
		t.Fatalf("create key: %d %s", code, b)
	}
	// Before the first request: the events go to the test, and the chain's
	// observer is tapped.
	events = &recEmitter{}
	e.v04.Observer.Events = events
	taps = make(chan observe.ModelRequest, 16)
	e.v04.AIChain.Observer = &tapObserver{inner: e.v04.Observer, done: taps}
	return e, out.Key, e.v04.Metrics, events, taps
}

func awaitModelRequest(t *testing.T, taps chan observe.ModelRequest) observe.ModelRequest {
	t.Helper()
	select {
	case ev := <-taps:
		return ev
	case <-time.After(20 * time.Second):
		t.Fatal("the observer was never told of the model request")
		return observe.ModelRequest{}
	}
}

// A real failure of the first provider followed by an answer from the second
// is one failover and one upstream error: no other route counts the request.
func TestGatewayAttempts_FailoverIsObservedOnce(t *testing.T) {
	e, key, rec, events, taps := gatewayAttemptsEnv(t)
	e.up.set("alpha", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		gwJSON(w, 500, map[string]any{"error": map[string]string{"message": "boom"}})
		return true
	})
	r := e.call(t, "POST", "/openai/v1/chat/completions", gwChatBody, bearer(key)...)
	if r.Status != 200 || r.Header.Get("Burrow-Provider") != "beta" {
		t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
	}
	ev := awaitModelRequest(t, taps)
	if len(ev.Attempts) != 2 || !ev.Attempts[0].Failed || ev.Attempts[1].Failed {
		t.Fatalf("attempts %+v, want a failed one and an answered one", ev.Attempts)
	}
	text := expo(t, rec)
	wantLines(t, text,
		`burrow_ai_failover_events_total{service="`+ev.ServiceID+`",from="alpha",to="beta",cross_provider="true"} 1`)
	if n, _ := familySum(text, "burrow_ai_failover_events_total"); n != 1 {
		t.Errorf("failovers counted: %v, want 1", n)
	}
	if n, _ := familySum(text, "burrow_ai_upstream_errors_total"); n != 1 {
		t.Errorf("upstream errors counted: %v, want 1 for the one failed attempt:\n%s", n, grepBurrow(text, "burrow_ai_upstream_errors_total"))
	}
	wantLines(t, text, `burrow_ai_upstream_errors_total{service="`+serviceOfProvider(t, e, "alpha")+`",status="5xx"} 1`)
	if up := events.named("ai.upstream_error"); len(up) != 1 {
		t.Errorf("ai.upstream_error events: %v, want 1", up)
	}

	// Every attempt fails: two attempts, one failover, two upstream errors.
	e.up.set("beta", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		gwJSON(w, 502, map[string]any{"error": map[string]string{"message": "boom"}})
		return true
	})
	if r := e.call(t, "POST", "/openai/v1/chat/completions", gwChatBody, bearer(key)...); r.Status != http.StatusBadGateway {
		t.Fatalf("everything fails: status %d body %s", r.Status, r.Body)
	}
	awaitModelRequest(t, taps)
	text = expo(t, rec)
	if n, _ := familySum(text, "burrow_ai_failover_events_total"); n != 2 {
		t.Errorf("failovers counted: %v, want 2 (one per request)", n)
	}
	if n, _ := familySum(text, "burrow_ai_upstream_errors_total"); n != 3 {
		t.Errorf("upstream errors counted: %v, want 3 (one per failed attempt)", n)
	}
}

// A caller who hangs up while an attempt is in flight is no upstream error
// and no failover, however the upstream ends.
func TestGatewayAttempts_CallerGoneIsNoUpstreamError(t *testing.T) {
	e, key, rec, events, taps := gatewayAttemptsEnv(t)
	inFlight := make(chan struct{}, 1)
	e.up.set("alpha", func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		inFlight <- struct{}{}
		<-r.Context().Done() // the caller hangs up while alpha works
		w.WriteHeader(http.StatusInternalServerError)
		return true
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/openai/v1/chat/completions", strings.NewReader(gwChatBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	errc := make(chan error, 1)
	go func() {
		resp, err := e.plain.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	select {
	case <-inFlight:
	case <-time.After(20 * time.Second):
		t.Fatal("the request never reached the first provider")
	}
	cancel()
	<-errc
	ev := awaitModelRequest(t, taps)
	if len(ev.Attempts) != 1 || ev.Attempts[0].Failed {
		t.Fatalf("attempts %+v, want the one that was in flight, not failed", ev.Attempts)
	}
	text := expo(t, rec)
	for _, family := range []string{"burrow_ai_failover_events_total", "burrow_ai_upstream_errors_total"} {
		if n, ok := familySum(text, family); ok && n != 0 {
			t.Errorf("%s = %v for a caller who hung up:\n%s", family, n, grepBurrow(text, family))
		}
	}
	if up := events.named("ai.upstream_error"); len(up) != 0 {
		t.Errorf("ai.upstream_error for a caller who hung up: %v", up)
	}
	if e.up.calls("beta") != 0 {
		t.Error("the chain went on after the caller was gone")
	}
}

// A provider's own path (/ai/<slug>/v1/...) has no fallback chain: its one
// attempt counts one upstream error when the upstream fails, no failover, and
// nothing for an answer or for a caller who hung up.
func TestGatewayAttempts_ProviderPathIsObserved(t *testing.T) {
	e, key, rec, events, taps := gatewayAttemptsEnv(t)
	const body = `{"model":"m-a","messages":[{"role":"user","content":"hi"}]}`
	const path = "/ai/alpha/v1/chat/completions"
	count := func(family string) float64 { n, _ := familySum(expo(t, rec), family); return n }

	// An answer counts nothing.
	if r := e.call(t, "POST", path, body, bearer(key)...); r.Status != 200 {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
	if ev := awaitModelRequest(t, taps); len(ev.Attempts) != 1 || ev.Attempts[0].Failed {
		t.Fatalf("attempts %+v, want one answered", ev.Attempts)
	}
	if n := count("burrow_ai_upstream_errors_total") + count("burrow_ai_failover_events_total"); n != 0 {
		t.Fatalf("an answer counted %v", n)
	}

	// A failing upstream: one upstream error, no failover, one event.
	e.up.set("alpha", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		gwJSON(w, 500, map[string]any{"error": map[string]string{"message": "boom"}})
		return true
	})
	if r := e.call(t, "POST", path, body, bearer(key)...); r.Status != 500 {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
	awaitModelRequest(t, taps)
	text := expo(t, rec)
	wantLines(t, text, `burrow_ai_upstream_errors_total{service="`+serviceOfProvider(t, e, "alpha")+`",status="5xx"} 1`)
	if n := count("burrow_ai_failover_events_total"); n != 0 {
		t.Errorf("failovers counted on a path without fallback: %v", n)
	}
	if up := events.named("ai.upstream_error"); len(up) != 1 {
		t.Errorf("ai.upstream_error events: %v, want 1", up)
	}

	// A caller who hangs up while the upstream works counts nothing more.
	inFlight := make(chan struct{}, 1)
	e.up.set("alpha", func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		inFlight <- struct{}{}
		<-r.Context().Done()
		w.WriteHeader(http.StatusInternalServerError)
		return true
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	errc := make(chan error, 1)
	go func() {
		resp, err := e.plain.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	select {
	case <-inFlight:
	case <-time.After(20 * time.Second):
		t.Fatal("the request never reached the provider")
	}
	cancel()
	<-errc
	awaitModelRequest(t, taps)
	if n := count("burrow_ai_upstream_errors_total"); n != 1 {
		t.Errorf("upstream errors after a caller hung up: %v, want still 1", n)
	}
	if up := events.named("ai.upstream_error"); len(up) != 1 {
		t.Errorf("ai.upstream_error events: %v, want still 1", up)
	}
}

// serviceOfProvider is the id of the service behind a provider.
func serviceOfProvider(t *testing.T, e *gwEnv, slug string) string {
	t.Helper()
	code, b := e.admin(t, "GET", "/api/v1/ai/providers/"+slug, nil)
	var out struct {
		ServiceID string `json:"service_id"`
	}
	if code != http.StatusOK || json.Unmarshal(b, &out) != nil || out.ServiceID == "" {
		t.Fatalf("provider %s: %d %s", slug, code, b)
	}
	return out.ServiceID
}

// The same failure on the provider's own path, reached with a service key (the
// api_key service behind the provider; every client from before gateway keys
// has one) is counted once as well, and the request keeps having no route: its
// usage row names no gateway key, dialect, provider or model.
func TestGatewayAttempts_ProviderPathServiceKey(t *testing.T) {
	e, _, rec, events, taps := gatewayAttemptsEnv(t)
	svc := serviceOfProvider(t, e, "alpha")
	code, b := e.admin(t, "POST", "/api/v1/services/"+svc+"/api-keys", map[string]any{"name": "legacy"})
	var out struct{ Key string }
	if code != http.StatusCreated || json.Unmarshal(b, &out) != nil || out.Key == "" {
		t.Fatalf("create a service key: %d %s", code, b)
	}
	e.up.set("alpha", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		gwJSON(w, 502, map[string]any{"error": map[string]string{"message": "boom"}})
		return true
	})
	const body = `{"model":"m-a","messages":[{"role":"user","content":"hi"}]}`
	if r := e.call(t, "POST", "/ai/alpha/v1/chat/completions", body, bearer(out.Key)...); r.Status != 502 {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
	if ev := awaitModelRequest(t, taps); len(ev.Attempts) != 1 || !ev.Attempts[0].Failed {
		t.Fatalf("attempts %+v, want one failed", ev.Attempts)
	}
	text := expo(t, rec)
	wantLines(t, text, `burrow_ai_upstream_errors_total{service="`+svc+`",status="5xx"} 1`)
	if n, _ := familySum(text, "burrow_ai_failover_events_total"); n != 0 {
		t.Errorf("failovers counted: %v, want none", n)
	}
	if up := events.named("ai.upstream_error"); len(up) != 1 {
		t.Errorf("ai.upstream_error events: %v, want 1", up)
	}
	// No route: the usage row carries none of its fields.
	var gk, dialect, provider, requested, target, reqID string
	for end := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		err := e.sqldb.QueryRow(`SELECT gateway_key_id, dialect, provider_slug, requested_model, target_model, request_id
			FROM usage_events WHERE service_id = ?`, svc).Scan(&gk, &dialect, &provider, &requested, &target, &reqID)
		if err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("no usage row for the service key request: %v", err)
		}
	}
	if gk != "" || dialect != "" || provider != "" || requested != "" || target != "" || reqID != "" {
		t.Errorf("usage row has route fields: %q %q %q %q %q %q", gk, dialect, provider, requested, target, reqID)
	}
}
