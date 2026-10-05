package aigateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
)

type fakeProviders map[string]db.AIProvider

func (f fakeProviders) ProviderBySlug(_ context.Context, slug string) (db.AIProvider, error) {
	p, ok := f[slug]
	if !ok {
		return db.AIProvider{}, db.ErrNotFound
	}
	return p, nil
}

// fakeKeys holds one key, valid for one service only.
type fakeKeys struct {
	service, good, id string
	calls             atomic.Int32
}

func (f *fakeKeys) ValidateAPIKey(_ context.Context, serviceID, presented string) (string, bool, error) {
	f.calls.Add(1)
	if serviceID == f.service && presented == f.good {
		return f.id, true, nil
	}
	return "", false, nil
}

// fakeTunnels serves each dialled stream with upstream, over a net.Pipe.
type fakeTunnels struct {
	res       *proxy.Resolved
	upstream  http.Handler
	lookupErr error
}

func (f fakeTunnels) LookupByServiceID(context.Context, string) (*proxy.Resolved, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if f.res == nil {
		return nil, proxy.ErrNotFound
	}
	cp := *f.res
	return &cp, nil
}

func (f fakeTunnels) DialTunnelStreamByServiceID(context.Context, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		rec := httptest.NewRecorder()
		f.upstream.ServeHTTP(rec, req)
		_ = rec.Result().Write(server)
	}()
	return client, nil
}

type spyChain struct {
	serviceID, keyID string
	metered          bool
}

func (c *spyChain) Dispatch(w http.ResponseWriter, r *http.Request, serviceID, _, _, apiKeyID string, up http.Handler) {
	c.serviceID, c.keyID, c.metered = serviceID, apiKeyID, false
	up.ServeHTTP(w, r)
}

func (c *spyChain) DispatchMetered(w http.ResponseWriter, r *http.Request, serviceID, _, _, apiKeyID string, up http.Handler) {
	c.serviceID, c.keyID, c.metered = serviceID, apiKeyID, true
	up.ServeHTTP(w, r)
}

func newGateway(up http.Handler, chain Chain) *Gateway {
	return &Gateway{
		Providers: fakeProviders{
			"ollama": {Slug: "ollama", Name: "Ollama", Kind: "tunnel", ServiceID: "svc1", APIFormat: "openai"},
			"vllm":   {Slug: "vllm", Name: "vLLM", Kind: "tunnel", ServiceID: "svc2", APIFormat: "openai"},
		},
		Keys: &fakeKeys{service: "svc1", good: "sk-good", id: "key-1"},
		Tunnels: fakeTunnels{
			res:      &proxy.Resolved{ServiceID: "svc1", AccessMode: "api_key", LocalHost: "127.0.0.1:11434"},
			upstream: up,
		},
		Chain:      chain,
		PublicHost: "burrow.example.com",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not the /ai/ shape: %s", rec.Body.String())
	}
	if body.Error.Type != "burrow_error" || body.Error.Message == "" {
		t.Fatalf("error body = %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	return body.Error.Code
}

func TestServe_ForwardsToTunnelAndStripsBurrowKey(t *testing.T) {
	var gotAuth, gotCookie, gotPath, gotHost string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		gotPath, gotHost = r.URL.Path, r.Host
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	chain := &spyChain{}
	g := newGateway(up, chain)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-good")
	req.Header.Set("Cookie", "burrow_session=abc")
	rec := httptest.NewRecorder()
	g.Serve(rec, req, "ollama")

	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if gotAuth != "" || gotCookie != "" {
		t.Fatalf("credentials reached the upstream: Authorization=%q Cookie=%q", gotAuth, gotCookie)
	}
	if gotPath != "/v1/chat/completions" || gotHost != "127.0.0.1:11434" {
		t.Fatalf("upstream saw path %q host %q", gotPath, gotHost)
	}
	if chain.serviceID != "svc1" || chain.keyID != "key-1" {
		t.Fatalf("chain got service %q key %q", chain.serviceID, chain.keyID)
	}
	if !chain.metered {
		t.Fatal("an inference call (POST) must be metered")
	}
	if rec.Header().Get("Burrow-Provider") != "ollama" {
		t.Fatalf("Burrow-Provider = %q", rec.Header().Get("Burrow-Provider"))
	}
}

// Listing models is not inference: it must not create usage rows.
func TestServe_GETIsNotMetered(t *testing.T) {
	chain := &spyChain{metered: true}
	g := newGateway(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }), chain)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	g.Serve(httptest.NewRecorder(), req, "ollama")
	if chain.serviceID != "svc1" || chain.metered {
		t.Fatalf("GET went through DispatchMetered (service %q metered %v)", chain.serviceID, chain.metered)
	}
}

func TestServe_AcceptsXAPIKeyHeader(t *testing.T) {
	g := newGateway(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("X-Api-Key", "sk-good")
	rec := httptest.NewRecorder()
	g.Serve(rec, req, "ollama")
	if rec.Code != 204 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestServe_Errors(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	withKey := func(k string) *http.Request {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if k != "" {
			r.Header.Set("Authorization", "Bearer "+k)
		}
		return r
	}

	t.Run("unknown provider", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newGateway(ok, nil).Serve(rec, withKey("sk-good"), "nope")
		if rec.Code != 404 || errCode(t, rec) != "provider_not_found" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newGateway(ok, nil).Serve(rec, withKey(""), "ollama")
		if rec.Code != 401 || errCode(t, rec) != "invalid_api_key" {
			t.Fatalf("status %d", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("missing WWW-Authenticate")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newGateway(ok, nil).Serve(rec, withKey("sk-bad"), "ollama")
		if rec.Code != 401 || errCode(t, rec) != "invalid_api_key" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("client offline", func(t *testing.T) {
		g := newGateway(ok, nil)
		g.Tunnels = fakeTunnels{}
		rec := httptest.NewRecorder()
		g.Serve(rec, withKey("sk-good"), "ollama")
		if rec.Code != 502 || errCode(t, rec) != "provider_offline" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("service not key protected", func(t *testing.T) {
		g := newGateway(ok, nil)
		g.Tunnels = fakeTunnels{res: &proxy.Resolved{ServiceID: "svc1", AccessMode: "open"}, upstream: ok}
		rec := httptest.NewRecorder()
		g.Serve(rec, withKey("sk-good"), "ollama")
		if rec.Code != 403 || errCode(t, rec) != "provider_unavailable" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("ip policy denies", func(t *testing.T) {
		g := newGateway(ok, nil)
		g.IPGeoDeny = func(*proxy.Resolved, *http.Request) bool { return true }
		rec := httptest.NewRecorder()
		g.Serve(rec, withKey("sk-good"), "ollama")
		if rec.Code != 403 || errCode(t, rec) != "forbidden" {
			t.Fatalf("status %d", rec.Code)
		}
		// A blocked address must not be able to probe keys.
		if n := g.Keys.(*fakeKeys).calls.Load(); n != 0 {
			t.Fatalf("key validator called %d times for a blocked address", n)
		}
		if rec.Header().Get("Burrow-Provider") != "" {
			t.Fatal("Burrow-Provider set for an unauthenticated caller")
		}
	})
	t.Run("key of another provider", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newGateway(ok, nil).Serve(rec, withKey("sk-good"), "vllm")
		if rec.Code != 401 || errCode(t, rec) != "invalid_api_key" {
			t.Fatalf("status %d", rec.Code)
		}
	})
	// The provider row outlived its service: the lookup fails for a reason
	// other than "client offline".
	t.Run("backing service gone", func(t *testing.T) {
		g := newGateway(ok, nil)
		g.Tunnels = fakeTunnels{lookupErr: errors.New("service by id: secret-detail")}
		rec := httptest.NewRecorder()
		g.Serve(rec, withKey("sk-good"), "ollama")
		if rec.Code != 500 || errCode(t, rec) != "internal_error" {
			t.Fatalf("status %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret-detail") {
			t.Fatal("internal error text leaked to the client")
		}
	})
}

type errKeys struct{}

func (errKeys) ValidateAPIKey(context.Context, string, string) (string, bool, error) {
	return "", false, errors.New("db down: secret-detail")
}

func TestServe_ValidatorErrorDoesNotLeak(t *testing.T) {
	g := newGateway(http.NotFoundHandler(), nil)
	g.Keys = errKeys{}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	rec := httptest.NewRecorder()
	g.Serve(rec, req, "ollama")
	if rec.Code != 500 || errCode(t, rec) != "internal_error" {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret-detail") {
		t.Fatal("internal error text leaked to the client")
	}
}

// streamTunnels dials a real HTTP server, so a response reaches the gateway
// chunk by chunk instead of all at once.
type streamTunnels struct {
	res  *proxy.Resolved
	addr string
}

func (f streamTunnels) LookupByServiceID(context.Context, string) (*proxy.Resolved, error) {
	cp := *f.res
	return &cp, nil
}

func (f streamTunnels) DialTunnelStreamByServiceID(ctx context.Context, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", f.addr)
}

// The caller must see the first chunk while the upstream is still holding the
// response open.
func TestServe_StreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
		_ = http.NewResponseController(w).Flush()
		<-release
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	g := newGateway(nil, nil)
	g.Tunnels = streamTunnels{
		res:  &proxy.Resolved{ServiceID: "svc1", AccessMode: "api_key", LocalHost: "127.0.0.1:11434"},
		addr: up.Listener.Addr().String(),
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.Serve(w, r, "ollama") }))
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	first := make(chan string, 1)
	br := bufio.NewReader(resp.Body)
	go func() {
		line, _ := br.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: one\n" {
			t.Fatalf("first chunk = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first chunk was held back until the upstream finished")
	}
	unblock()
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestServe_QueryStringReachesUpstream(t *testing.T) {
	var gotQuery string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotQuery = r.URL.RawQuery })
	g := newGateway(up, nil)
	req := httptest.NewRequest("GET", "/v1/models?limit=5&q=a%20b&x=%2F", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	g.Serve(httptest.NewRecorder(), req, "ollama")
	if gotQuery != "limit=5&q=a%20b&x=%2F" {
		t.Fatalf("upstream query = %q", gotQuery)
	}
}

// Errors the AI chain writes itself must have the /ai/ shape too.
func TestServe_ChainErrorsUseAIShape(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstreamHit := false
	up := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamHit = true })
	post := func(g *Gateway) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		req.Header.Set("Authorization", "Bearer sk-good")
		rec := httptest.NewRecorder()
		g.Serve(rec, req, "ollama")
		return rec
	}

	t.Run("request too large", func(t *testing.T) {
		chain := aigw.NewChain(nil, nil, nil, nil, nil, nil, nil, nil, log)
		chain.MaxRequestBodyBytes = 8
		rec := post(newGateway(up, chain))
		if rec.Code != 413 || errCode(t, rec) != "request_too_large" {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("rate limited", func(t *testing.T) {
		chain := aigw.NewChain(nil, nil, nil, nil, nil, nil, nil, nil, log)
		// Stands in for the quota middleware: it answers through the
		// request's error writer, as that middleware does.
		chain.RateLimit = func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				aigw.ErrorWriterFrom(r.Context())(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
			})
		}
		rec := post(newGateway(up, chain))
		if rec.Code != 429 || errCode(t, rec) != "rate_limited" {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
	if upstreamHit {
		t.Fatal("a refused request reached the upstream")
	}
}
