package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxy"
)

// Request summaries are made here, for every request the proxy answers for an
// http service: host-routed, path-routed (/svc/<slug>) and custom-domain. The
// /ai/ gateway does not pass through the proxy and makes none; tcp tunnels
// have no requests.

type sentSummary struct {
	serviceID string
	s         proto.RequestSummary
}

// summarySink records what the proxy reports.
type summarySink struct {
	mu   sync.Mutex
	got  []sentSummary
	hook func() // called on each summary, before it is recorded
}

func (k *summarySink) RequestSummary(serviceID string, s proto.RequestSummary) {
	if k.hook != nil {
		k.hook()
	}
	k.mu.Lock()
	k.got = append(k.got, sentSummary{serviceID, s})
	k.mu.Unlock()
}

func (k *summarySink) all() []sentSummary {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]sentSummary(nil), k.got...)
}

// one waits for the summary of the request that was just answered. The proxy
// reports when the handler returns, which may be a moment after the visitor
// has the whole response.
func (k *summarySink) one(t *testing.T) sentSummary {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := k.all(); len(got) > 0 {
			time.Sleep(30 * time.Millisecond) // a second one would be a fault
			if got = k.all(); len(got) != 1 {
				t.Fatalf("%d summaries for one request: %+v", len(got), got)
			}
			return got[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no summary")
	return sentSummary{}
}

type statusChecker struct{ status int }

func (c statusChecker) Allow(context.Context, *proxy.Resolved, *http.Request) (bool, int, string, http.Header) {
	return false, c.status, "refused", nil
}

// brokenDialer finds the tunnel and cannot reach its client.
type brokenDialer struct{ *fakeDialer }

func (brokenDialer) DialTunnelStreamByServiceID(context.Context, string) (net.Conn, error) {
	return nil, errors.New("the client is gone")
}

// answerChain answers itself, as the AI chain does for a cache hit.
type answerChain struct{ hijack bool }

func (c answerChain) Dispatch(w http.ResponseWriter, _ *http.Request, _, _, _, _ string, _ http.Handler) {
	if c.hijack {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = conn.Close()
		return
	}
	_, _ = io.WriteString(w, "from the cache") // no WriteHeader: an implicit 200
}

func get(t *testing.T, base, host, target string) int {
	t.Helper()
	req, err := http.NewRequest("GET", base+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Authorization", "Bearer bur_test_0000")
	req.Header.Set("Cookie", "session=cookie-secret")
	req.Header.Set("X-Api-Key", "header-secret")
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestSummary_OneRequestOneSummaryWithoutItsContent(t *testing.T) {
	var answered atomic.Bool
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "app=response-secret")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body-secret"))
		answered.Store(true)
	})
	d := newFakeDialer(upstream)
	d.register("abc123", &proxy.Resolved{ServiceID: "svc1", TunnelID: "tun1", AccessMode: "open", LocalHost: "127.0.0.1:3000"})
	sink := &summarySink{}
	sink.hook = func() {
		if !answered.Load() {
			t.Error("the summary was made before the response was complete")
		}
	}
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithSummarySink(sink)))
	defer ts.Close()

	before := time.Now().Add(-time.Second)
	if code := get(t, ts.URL, "abc123."+authDomain, "/api/users?token=query-secret&x=1#frag-secret"); code != 200 {
		t.Fatalf("status %d", code)
	}
	got := sink.one(t)
	at, err := time.Parse(time.RFC3339, got.s.Time)
	if err != nil || !strings.HasSuffix(got.s.Time, "Z") || at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("time %q: %v", got.s.Time, err)
	}
	if got.s.DurationMs < 0 || got.s.DurationMs > 5000 {
		t.Errorf("duration %d ms", got.s.DurationMs)
	}
	got.s.Time, got.s.DurationMs = "", 0
	if want := (sentSummary{"svc1", proto.RequestSummary{TunnelID: "tun1", Method: "GET", Path: "/api/users", Status: 200}}); got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	raw, _ := json.Marshal(sink.all())
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "bur_test") {
		t.Errorf("content of the request or the response in the summary: %s", raw)
	}
}

func TestSummary_Statuses(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	lookup := func(_ context.Context, host string) (string, bool, error) {
		return "svc1", host == "app.example.org", nil
	}
	cases := []struct {
		name    string
		res     proxy.Resolved
		checker proxy.AccessChecker
		broken  bool
		opts    []proxy.Option
		host    string
		target  string
		prefix  string
		want    int
		path    string
	}{
		{name: "the app's own 404", target: "/missing", want: 404, path: "/missing"},
		{name: "refused by the access check", checker: statusChecker{401}, target: "/x", want: 401, path: "/x"},
		{name: "refused by the IP policy", res: proxy.Resolved{IPBlockCIDRs: []string{"127.0.0.0/8", "::1/128"}}, target: "/x", want: 403, path: "/x"},
		{name: "the upstream failed", broken: true, target: "/x", want: 502, path: "/x"},
		{name: "answered by the AI chain", opts: []proxy.Option{proxy.WithAIChain(answerChain{})}, target: "/v1/chat", want: 200, path: "/v1/chat"},
		{name: "the upstream failed behind the AI chain", broken: true, opts: []proxy.Option{proxy.WithAIChain(passChain{})}, target: "/x", want: 502, path: "/x"},
		{name: "custom domain", opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup)}, host: "app.example.org", target: "/c?q=1", want: 200, path: "/c"},
		{name: "custom domain, refused", checker: statusChecker{403}, opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup)}, host: "app.example.org", target: "/c", want: 403, path: "/c"},
		// The /svc/<slug> adapter has taken the prefix off; the summary names
		// the path the app saw.
		{name: "path-routed", prefix: "/svc/abc123", target: "/api/users?a=b", want: 200, path: "/api/users"},
		{name: "no path", prefix: "/svc/abc123", target: "", want: 200, path: "/"},
		{name: "escape sequence in the path", target: "/a%1B%5B2Jb%0D%0A%E2%80%AEc", want: 200, path: "/a?[2Jb???c"},
		{name: "a question mark that is part of the path", target: "/a%3Fb?real=query", want: 200, path: "/a?b"},
		{name: "long path", target: "/" + strings.Repeat("a", 400), want: 200, path: "/" + strings.Repeat("a", 255)},
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
			checker := tc.checker
			if checker == nil {
				checker = openChecker{}
			}
			sink := &summarySink{}
			p := proxy.New(d, checker, authDomain, testLog(), append(tc.opts, proxy.WithSummarySink(sink))...)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.prefix != "" {
					r = r.WithContext(proxy.WithPathPrefix(r.Context(), tc.prefix))
				}
				p.ServeHTTP(w, r)
			}))
			defer ts.Close()
			host := tc.host
			if host == "" {
				host = "abc123." + authDomain
			}
			if code := get(t, ts.URL, host, tc.target); code != tc.want {
				t.Fatalf("the visitor got %d, want %d", code, tc.want)
			}
			got := sink.one(t)
			if got.serviceID != "svc1" || got.s.TunnelID != "tun1" || got.s.Method != "GET" || got.s.Status != tc.want || got.s.Path != tc.path {
				t.Errorf("summary %+v, want status %d and path %q", got, tc.want, tc.path)
			}
		})
	}
}

// A connection that was taken over (a WebSocket) is reported as what it is.
func TestSummary_HijackedIs101(t *testing.T) {
	d := newFakeDialer(http.NotFoundHandler())
	d.register("abc123", &proxy.Resolved{ServiceID: "svc1", TunnelID: "tun1", AccessMode: "open"})
	sink := &summarySink{}
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithSummarySink(sink), proxy.WithAIChain(answerChain{hijack: true})))
	defer ts.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: abc123."+authDomain+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	_, _ = io.ReadAll(conn)
	if got := sink.one(t); got.s.Status != 101 || got.s.Path != "/ws" {
		t.Errorf("summary %+v", got)
	}
}

// What has no service has nobody to tell: an unknown slug, the gate, a method
// made of anything.
func TestSummary_NothingWithoutAService(t *testing.T) {
	d := newFakeDialer(http.NotFoundHandler())
	sink := &summarySink{}
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithSummarySink(sink)))
	defer ts.Close()
	if code := get(t, ts.URL, "nobody."+authDomain, "/x"); code != 404 {
		t.Fatalf("status %d", code)
	}
	if code := get(t, ts.URL, authDomain, "/__burrow/login"); code != 404 {
		t.Fatalf("status %d", code)
	}
	time.Sleep(50 * time.Millisecond)
	if got := sink.all(); len(got) != 0 {
		t.Fatalf("summaries: %+v", got)
	}
}

// The method is the visitor's word too.
func TestSummary_MethodIsBounded(t *testing.T) {
	d := newFakeDialer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	d.register("abc123", &proxy.Resolved{ServiceID: "svc1", TunnelID: "tun1", AccessMode: "open"})
	sink := &summarySink{}
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithSummarySink(sink)))
	defer ts.Close()
	req, _ := http.NewRequest(strings.Repeat("X", 40), ts.URL+"/", nil)
	req.Host = "abc123." + authDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := sink.one(t); got.s.Method != strings.Repeat("X", 16) || got.s.Status != 204 {
		t.Errorf("summary %+v", got)
	}
}

// A proxy without a sink does what it did.
func TestSummary_NoSink(t *testing.T) {
	d := newFakeDialer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	d.register("abc123", &proxy.Resolved{ServiceID: "svc1", AccessMode: "open"})
	ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog()))
	defer ts.Close()
	if code := get(t, ts.URL, "abc123."+authDomain, "/"); code != 200 {
		t.Fatalf("status %d", code)
	}
}
