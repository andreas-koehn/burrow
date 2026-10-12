package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proxy"
)

// trafficObserver records what the proxy tells its observer.
type trafficObserver struct {
	mu  sync.Mutex
	got []observe.Traffic
}

func (o *trafficObserver) ObserveTraffic(e observe.Traffic) {
	o.mu.Lock()
	o.got = append(o.got, e)
	o.mu.Unlock()
}

// one waits for the event of the request that was just answered: the proxy
// reports when the handler returns, a moment after the visitor has the
// response. A second event for one request is a fault.
func (o *trafficObserver) one(t *testing.T) observe.Traffic {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		n := len(o.got)
		o.mu.Unlock()
		if n > 0 {
			time.Sleep(30 * time.Millisecond)
			o.mu.Lock()
			defer o.mu.Unlock()
			if len(o.got) != 1 {
				t.Fatalf("%d events for one request: %+v", len(o.got), o.got)
			}
			return o.got[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no traffic event")
	return observe.Traffic{}
}

func TestObserver_OneRequestOneEvent(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Set-Cookie", "app=response-secret")
		_, _ = w.Write([]byte("body-secret"))
	})
	d := newFakeDialer(upstream)
	d.register("abc123", &proxy.Resolved{
		ServiceID: "svc1", TunnelID: "tun1", UserID: "user1", ClientSessionID: "sess1",
		AccessMode: "open", LocalHost: "127.0.0.1:3000",
	})
	obs := &trafficObserver{}
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithObserver(obs)))
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/users?token=query-secret", bytes.NewReader([]byte("request-secret")))
	req.Host = "abc123." + authDomain
	req.Header.Set("Authorization", "Bearer bur_test_0000")
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	got := obs.one(t)
	if got.Duration <= 0 || got.Duration > 5*time.Second || got.Started.IsZero() {
		t.Errorf("started %v, duration %s", got.Started, got.Duration)
	}
	if got.SourceIP != "127.0.0.1" && got.SourceIP != "::1" {
		t.Errorf("source ip %q", got.SourceIP)
	}
	got.Started, got.Duration, got.SourceIP = time.Time{}, 0, ""
	want := observe.Traffic{
		Kind: observe.TrafficHTTP, ServiceID: "svc1", TunnelID: "tun1", UserID: "user1", SessionID: "sess1",
		Method: "POST", Status: 200, Outcome: observe.OutcomeClean,
		BytesIn: int64(len("request-secret")), BytesOut: int64(len("body-secret")),
	}
	if got != want {
		t.Errorf("event:\n got %+v\nwant %+v", got, want)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "bur_test") || strings.Contains(string(raw), "/api/users") {
		t.Errorf("content of the request or the response in the event: %s", raw)
	}
}

func TestObserver_Outcomes(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	cases := []struct {
		name    string
		res     proxy.Resolved
		checker proxy.AccessChecker
		broken  bool
		opts    []proxy.Option
		target  string
		status  int
		outcome string
		reason  string
		key     bool
	}{
		{name: "answered", target: "/x", status: 200, outcome: observe.OutcomeClean},
		{name: "refused by the access check", checker: statusChecker{401}, target: "/x", status: 401, outcome: observe.OutcomeRejected, reason: observe.RejectedAccess},
		{name: "refused by the IP policy", res: proxy.Resolved{IPBlockCIDRs: []string{"127.0.0.0/8", "::1/128"}}, target: "/x", status: 403, outcome: observe.OutcomeRejected, reason: observe.RejectedIPGeo},
		{name: "the upstream failed", broken: true, target: "/x", status: 502, outcome: observe.OutcomeError},
		{name: "the upstream answered 502", target: "/boom", status: 502, outcome: observe.OutcomeError},
		{name: "a key service whose upstream failed", res: proxy.Resolved{APIKeyHeader: "Authorization"}, broken: true, target: "/x", status: 502, outcome: observe.OutcomeError, key: true},
		{name: "answered by the AI chain", opts: []proxy.Option{proxy.WithAIChain(answerChain{})}, target: "/v1/chat", status: 200, outcome: observe.OutcomeClean},
		{name: "taken over", opts: []proxy.Option{proxy.WithAIChain(answerChain{hijack: true})}, target: "/ws", status: 101, outcome: observe.OutcomeClean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.res
			res.ServiceID, res.TunnelID, res.AccessMode, res.LocalHost = "svc1", "tun1", "open", "127.0.0.1:3000"
			fd := newFakeDialer(ok)
			fd.register("abc123", &res)
			var d proxy.StreamDialer = fd
			if tc.broken {
				d = brokenDialer{fd}
			}
			var checker proxy.AccessChecker = openChecker{}
			if tc.checker != nil {
				checker = tc.checker
			}
			obs := &trafficObserver{}
			ts := httptest.NewServer(proxy.New(d, checker, authDomain, testLog(), append(tc.opts, proxy.WithObserver(obs))...))
			defer ts.Close()

			req, _ := http.NewRequest("GET", ts.URL+tc.target, nil)
			req.Host = "abc123." + authDomain
			resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			got := obs.one(t)
			if got.Status != tc.status || got.Outcome != tc.outcome || got.RejectReason != tc.reason || got.KeyService != tc.key {
				t.Errorf("status %d outcome %q reason %q key %v, want %d %q %q %v",
					got.Status, got.Outcome, got.RejectReason, got.KeyService, tc.status, tc.outcome, tc.reason, tc.key)
			}
		})
	}
}

// A request for a host that is no service reaches none: no event.
func TestObserver_NothingForAnUnknownHost(t *testing.T) {
	obs := &trafficObserver{}
	ts := httptest.NewServer(proxy.New(newFakeDialer(http.NotFoundHandler()), openChecker{}, authDomain, testLog(), proxy.WithObserver(obs)))
	defer ts.Close()
	if code := get(t, ts.URL, "nobody."+authDomain, "/"); code != http.StatusNotFound {
		t.Fatalf("status %d", code)
	}
	time.Sleep(50 * time.Millisecond)
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.got) != 0 {
		t.Fatalf("events for a request that reached no service: %+v", obs.got)
	}
}

// The visitor hangs up while the upstream is still answering: the request's
// context is cancelled, and the event is delivered all the same, with what
// was sent until then.
func TestObserver_EventAfterTheVisitorHungUp(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	d := newFakeDialer(upstream)
	d.register("abc123", &proxy.Resolved{ServiceID: "svc1", TunnelID: "tun1", AccessMode: "open", LocalHost: "127.0.0.1:3000"})
	obs := &trafficObserver{}
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithObserver(obs)))
	defer ts.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream", nil)
	req.Host = "abc123." + authDomain
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel() // the visitor is gone
	_ = resp.Body.Close()

	got := obs.one(t)
	if got.ServiceID != "svc1" || got.Status != 200 || got.BytesOut < int64(len("first")) {
		t.Fatalf("event after the visitor hung up: %+v", got)
	}
}

// The model gateway answers /ai/ on the dashboard listener and is counted by
// its attempts (observe.ModelRequest); it never reaches the proxy. A request
// for the auth domain itself, the host of /ai/, must stay unobserved here, or
// a failed gateway request would count as an upstream error twice. A
// gateway-only service is hidden from the proxy the same way.
func TestObserver_NothingForAGatewayRequest(t *testing.T) {
	d := newFakeDialer(http.NotFoundHandler())
	d.register("abc123", &proxy.Resolved{
		ServiceID: "svc1", TunnelID: "tun1", AccessMode: "open", LocalHost: "127.0.0.1:3000",
		APIKeyHeader: "Authorization", GatewayOnly: true,
	})
	obs := &trafficObserver{}
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithObserver(obs)))
	defer ts.Close()
	for _, host := range []string{authDomain, "abc123." + authDomain} {
		if code := get(t, ts.URL, host, "/ai/provider/v1/chat/completions"); code != http.StatusNotFound {
			t.Fatalf("%s: status %d", host, code)
		}
	}
	time.Sleep(50 * time.Millisecond)
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.got) != 0 {
		t.Fatalf("the proxy observed a gateway request: %+v", obs.got)
	}
}
