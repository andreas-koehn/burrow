package main

// Request summaries, the whole way: a visitor, the dashboard origin's
// /svc/<slug> route and the proxy ingress, the control server, and the client
// of this code base with an observer, as `burrow http` runs it on a terminal.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/server"
)

// lateSink is the proxy's summary sink for a control server that is built
// after the proxy options are.
type lateSink struct{ srv atomic.Pointer[server.Server] }

func (l *lateSink) RequestSummary(serviceID string, s proto.RequestSummary) {
	if srv := l.srv.Load(); srv != nil {
		srv.RequestSummary(serviceID, s)
	}
}

// requestObserver keeps the registration and the requests it is told.
type requestObserver struct {
	lastRegistration
	rmu      sync.Mutex
	requests []string
	at       []time.Time
}

func (o *requestObserver) Request(id string, at time.Time, method, path string, status int) {
	o.rmu.Lock()
	defer o.rmu.Unlock()
	o.requests = append(o.requests, fmt.Sprintf("%s %s %s %d", id, method, path, status))
	o.at = append(o.at, at)
}

func (o *requestObserver) seen() []string {
	o.rmu.Lock()
	defer o.rmu.Unlock()
	return append([]string(nil), o.requests...)
}

func (o *requestObserver) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := o.seen(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d summaries, want %d: %q", len(o.seen()), n, o.seen())
	return nil
}

func TestE2ERequestSummaries(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}
	sink := &lateSink{}
	s := bootE2EStack(t, func(c *bootE2ECfg) {
		c.extraProxyOpts = append(c.extraProxyOpts, proxy.WithSummarySink(sink))
	})
	sink.srv.Store(s.server)
	s.setUpstreamHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Set-Cookie", "app=response-secret")
		fmt.Fprint(w, "body-secret")
	})

	// A second client of the same user, with its own service, that asks.
	tok, err := s.store.IssueClientToken(context.Background(), s.userID, "e2e-summaries")
	must(t, err, "mint token")
	obs := &requestObserver{}
	c := client.New(client.Options{
		Server: s.server.Addr(), Token: tok, Insecure: true, ServerName: "localhost",
		Tunnels:  []client.TunnelSpec{{Name: "summaries", Type: "http", LocalAddr: s.upstreamAddr}},
		Observer: obs, RequestSummaries: true, Logger: s.log,
	})
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	var reg client.RegisteredTunnel
	deadline := time.Now().Add(e2eClientReady)
	for time.Now().Before(deadline) {
		obs.mu.Lock()
		if obs.reg != nil {
			reg = *obs.reg
		}
		obs.mu.Unlock()
		if reg.TunnelID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reg.TunnelID == "" {
		t.Fatal("the second client never registered")
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(reg.URL, "https://"+e2eAuthDomain+"/svc/"), "/")
	if slug == "" || strings.Contains(slug, "/") || slug == s.subdomain {
		t.Fatalf("public address %q", reg.URL)
	}
	var serviceID string
	for _, tn := range s.server.HTTPTunnels() {
		if tn.ID == reg.TunnelID {
			serviceID = tn.ServiceID
		}
	}

	do := func(hc *http.Client, method, url string) int {
		t.Helper()
		req, err := http.NewRequest(method, url, nil)
		must(t, err, "request")
		req.Header.Set("X-Custom", "header-secret")
		req.Header.Set("Cookie", "app=cookie-secret")
		resp, err := hc.Do(req)
		must(t, err, method+" "+url)
		_ = readAllString(t, resp)
		return resp.StatusCode
	}
	before := time.Now().Add(-2 * time.Second)

	// 1. The path route: the summary names the path the app saw.
	if code := do(s.pathClient(t), "GET", "https://"+e2eAuthDomain+"/svc/"+slug+"/api/users?token=query-secret"); code != 200 {
		t.Fatalf("path route: %d", code)
	}
	// 2. The host route, and a status of the app's own.
	if code := do(s.visitorClient(t), "POST", "https://"+slug+"."+e2eAuthDomain+":"+s.proxyPort+"/missing?x=query-secret"); code != 404 {
		t.Fatalf("host route: %d", code)
	}
	// 3. A request to the other client's service is that client's business.
	if code := do(s.pathClient(t), "GET", s.pathURL("/other")); code != 200 {
		t.Fatalf("other service: %d", code)
	}
	// 4. Refused by the access check: nothing reaches the app, the summary
	// says 401.
	must(t, s.store.SetServiceAccessMode(context.Background(), s.userID, "admin", serviceID, "api_key", "Authorization", nil), "SetServiceAccessMode")
	if code := do(s.pathClient(t), "GET", "https://"+e2eAuthDomain+"/svc/"+slug+"/locked"); code != 401 {
		t.Fatalf("locked: %d", code)
	}

	got := obs.wait(t, 3)
	time.Sleep(100 * time.Millisecond)
	got = obs.seen()
	want := []string{
		reg.TunnelID + " GET /api/users 200",
		reg.TunnelID + " POST /missing 404",
		reg.TunnelID + " GET /locked 401",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the client was told\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if all := strings.Join(got, "\n"); strings.Contains(all, "secret") || strings.Contains(all, "?") {
		t.Fatalf("content of a request in a summary: %s", all)
	}
	obs.rmu.Lock()
	defer obs.rmu.Unlock()
	for _, at := range obs.at {
		if at.Before(before) || at.After(time.Now().Add(2*time.Second)) {
			t.Errorf("time of a request: %v", at)
		}
	}
}
