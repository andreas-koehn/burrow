package mcpgw

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/mcptest"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/store"
)

const testSecret = "sk-upstream-secret-123"

type mapVault map[string]string

func (v mapVault) Get(slot string) (string, bool) { s, ok := v[slot]; return s, ok }

type fixture struct {
	st     *store.Store
	userID string
	tr     *http.Transport
	pool   *Pool
}

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st := store.New(d)
	ctx := context.Background()
	if err := st.SeedAdmin(ctx, "admin@x", "pw"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(ctx, "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	return st, u.ID
}

// guarded returns the real guarded transport, trusting up's test
// certificate; allowPrivate lets it reach 127.0.0.1.
func guarded(up *mcptest.Server, allowPrivate bool) *http.Transport {
	tr := aiprovider.NewTransport(allowPrivate)
	tr.TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return tr
}

func inspectWith(e *guardrails.Engine) func(context.Context, []byte) (bool, string) {
	return func(_ context.Context, b []byte) (bool, string) { return e.InspectRequest(b, nil) }
}

func newFixture(t *testing.T, up *mcptest.Server, mods ...func(*Options)) *fixture {
	t.Helper()
	st, uid := newStore(t)
	f := &fixture{st: st, userID: uid, tr: guarded(up, true)}
	opts := Options{Store: st, Vault: mapVault{"UP": testSecret}, Remote: f.tr, InspectText: inspectWith(guardrails.NewEngine())}
	for _, m := range mods {
		m(&opts)
	}
	f.pool = New(opts)
	t.Cleanup(func() {
		f.pool.Close()
		f.tr.CloseIdleConnections()
	})
	return f
}

func (f *fixture) remote(t *testing.T, slug, rawURL string, mods ...func(*store.MCPSourceInput)) db.MCPSource {
	t.Helper()
	in := store.MCPSourceInput{Slug: slug, Name: slug, Kind: "remote", URL: rawURL, CredentialSlot: "UP"}
	for _, m := range mods {
		m(&in)
	}
	src, err := f.st.CreateMCPSource(context.Background(), f.userID, in)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func echo(t *testing.T, p *Pool, src db.MCPSource, text string) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.CallTool(ctx, src, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": text}})
}

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func countRPC(up *mcptest.Server, method string) int {
	n := 0
	for _, r := range up.Requests() {
		if r.RPC == method {
			n++
		}
	}
	return n
}

func TestCallTool_InjectsTheSourceCredential(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		t.Run(map[bool]string{false: "stateless", true: "stateful"}[stateful], func(t *testing.T) {
			up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: stateful, Authorize: func(r *http.Request) bool {
				return r.Header.Get("X-Upstream-Key") == "Token "+testSecret
			}})
			f := newFixture(t, up)
			src := f.remote(t, "gh", up.URL, func(in *store.MCPSourceInput) {
				in.AuthHeader, in.AuthFormat = "X-Upstream-Key", "Token {key}"
			})
			res, err := echo(t, f.pool, src, "hi")
			if err != nil {
				t.Fatal(err)
			}
			if got := resultText(res); got != "echo: hi" {
				t.Fatalf("result %q", got)
			}
			if len(up.Requests()) == 0 {
				t.Fatal("no request reached the upstream")
			}
			for _, r := range up.Requests() {
				if r.Header.Get("X-Upstream-Key") != "Token "+testSecret || r.Header.Get("Authorization") != "" {
					t.Fatalf("request %s %s carries headers %v", r.Method, r.RPC, r.Header)
				}
			}
		})
	}
}

func TestCallTool_StatelessGetsASessionPerCall(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL)
	for i := 0; i < 3; i++ {
		if _, err := echo(t, f.pool, src, "x"); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range up.Calls() {
		if c.SessionID != "" {
			t.Fatalf("stateless call carried session %q", c.SessionID)
		}
	}
	if n := len(up.CallsTo("echo")); n != 3 {
		t.Fatalf("%d calls", n)
	}
}

func TestCallTool_StatefulSharesOneSession(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := echo(t, f.pool, src, "x"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	sessions := map[string]bool{}
	for _, c := range up.Calls() {
		sessions[c.SessionID] = true
	}
	if len(sessions) != 1 || sessions[""] {
		t.Fatalf("calls ran on sessions %v, want exactly one", sessions)
	}
	if n := countRPC(up, "initialize"); n != 1 {
		t.Fatalf("%d initialize requests, want 1", n)
	}
}

func TestCallTool_StatefulSessionIsRecreatedAfterUpstreamRestart(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL)
	if _, err := echo(t, f.pool, src, "before"); err != nil {
		t.Fatal(err)
	}
	up.Restart()
	res, err := echo(t, f.pool, src, "after")
	if err != nil {
		t.Fatalf("call after restart: %v", err)
	}
	if resultText(res) != "echo: after" {
		t.Fatalf("result %q", resultText(res))
	}
	calls := up.CallsTo("echo")
	if len(calls) != 2 || calls[0].SessionID == calls[1].SessionID || calls[1].SessionID == "" {
		t.Fatalf("calls %+v: want two calls on two sessions", calls)
	}
}

func TestCallTool_UpstreamDownThenBack(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL)
	if _, err := echo(t, f.pool, src, "x"); err != nil {
		t.Fatal(err)
	}
	up.SetDown(true)
	if _, err := echo(t, f.pool, src, "x"); err == nil {
		t.Fatal("call to a down upstream succeeded")
	}
	up.SetDown(false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := echo(t, f.pool, src, "x")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still failing after the upstream came back: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCallTool_PinsTheProtocolRevision(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL, func(in *store.MCPSourceInput) { in.ProtocolVersion = "2025-06-18" })
	if _, err := echo(t, f.pool, src, "x"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range up.Requests() {
		if r.RPC == "tools/call" {
			found = true
			if v := r.Header.Get("Mcp-Protocol-Version"); v != "2025-06-18" {
				t.Fatalf("tools/call sent revision %q", v)
			}
		}
	}
	if !found {
		t.Fatal("no tools/call recorded")
	}
}

func TestRemote_PrivateAddressRefusedAtDial(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	st, uid := newStore(t)
	tr := guarded(up, false) // the relay's default: private upstreams not allowed
	p := New(Options{Store: st, Vault: mapVault{"UP": testSecret}, Remote: tr})
	t.Cleanup(p.Close)
	src, err := st.CreateMCPSource(context.Background(), uid, store.MCPSourceInput{Slug: "gh", Name: "gh", Kind: "remote", URL: up.URL, CredentialSlot: "UP"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Sync(context.Background(), "gh")
	if !errors.Is(err, aiprovider.ErrBlockedAddress) {
		t.Fatalf("sync err = %v, want the dial guard's refusal", err)
	}
	if n := len(up.Requests()); n != 0 {
		t.Fatalf("%d requests reached the private upstream", n)
	}
	after, _ := st.MCPSourceBySlug(context.Background(), "gh")
	if !strings.Contains(after.LastSyncError, "private") || strings.Contains(after.LastSyncError, "127.0.0.1") || after.LastSyncAt == nil {
		t.Fatalf("last sync error %q", after.LastSyncError)
	}
	if _, err := echo(t, p, src, "x"); err == nil {
		t.Fatal("call through the guard succeeded")
	}
}

func TestRemote_MissingCredentialSendsNothing(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL, func(in *store.MCPSourceInput) { in.CredentialSlot = "UNSET" })
	if _, err := echo(t, f.pool, src, "x"); !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("err = %v", err)
	}
	if n := len(up.Requests()); n != 0 {
		t.Fatalf("%d requests went out without the credential", n)
	}
}

func TestRemote_PublicSourceWithoutSlot(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL, func(in *store.MCPSourceInput) { in.CredentialSlot = "" })
	if _, err := echo(t, f.pool, src, "x"); err != nil {
		t.Fatal(err)
	}
	for _, r := range up.Requests() {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("a public source got an Authorization header")
		}
	}
}

func TestCallTool_DisabledSourceRefused(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	f := newFixture(t, up)
	off := false
	src := f.remote(t, "gh", up.URL, func(in *store.MCPSourceInput) { in.Enabled = &off })
	if _, err := echo(t, f.pool, src, "x"); !errors.Is(err, ErrSourceDisabled) {
		t.Fatalf("err = %v", err)
	}
}

func TestPool_ConfigChangeUsesANewSession(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: true, Authorize: func(r *http.Request) bool {
		return strings.HasSuffix(r.Header.Get("Authorization"), testSecret) || r.Header.Get("X-Other") != ""
	}})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL)
	if _, err := echo(t, f.pool, src, "x"); err != nil {
		t.Fatal(err)
	}
	h := "X-Other"
	src2, err := f.st.UpdateMCPSource(context.Background(), "gh", store.MCPSourceUpdate{AuthHeader: &h})
	if err != nil {
		t.Fatal(err)
	}
	up.ResetRecords()
	if _, err := echo(t, f.pool, src2, "x"); err != nil {
		t.Fatal(err)
	}
	if countRPC(up, "initialize") != 1 {
		t.Fatal("the changed source reused the old session")
	}
	for _, r := range up.Requests() {
		if r.Method == http.MethodPost && (r.Header.Get("X-Other") == "" || r.Header.Get("Authorization") != "") {
			t.Fatalf("request %s used the old credential header: %v", r.RPC, r.Header)
		}
	}
	// Invalidate drops the session too.
	f.pool.Invalidate("gh")
	up.ResetRecords()
	if _, err := echo(t, f.pool, src2, "x"); err != nil {
		t.Fatal(err)
	}
	if countRPC(up, "initialize") != 1 {
		t.Fatal("Invalidate kept the session")
	}
}

func TestPool_RestartsDoNotLeakGoroutines(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: true})
	st, uid := newStore(t)
	tr := guarded(up, true)
	src, err := st.CreateMCPSource(context.Background(), uid, store.MCPSourceInput{Slug: "gh", Name: "gh", Kind: "remote", URL: up.URL, CredentialSlot: "UP"})
	if err != nil {
		t.Fatal(err)
	}
	settle := func() int {
		tr.CloseIdleConnections()
		up.CloseClientConnections()
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	before := settle()
	p := New(Options{Store: st, Vault: mapVault{"UP": testSecret}, Remote: tr})
	for i := 0; i < 20; i++ {
		if _, err := echo(t, p, src, "x"); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		up.Restart()
	}
	p.Close()
	var after int
	for i := 0; i < 50; i++ {
		if after = settle(); after <= before+2 {
			return
		}
	}
	buf := make([]byte, 1<<20)
	t.Fatalf("goroutines before %d, after %d\n%s", before, after, buf[:runtime.Stack(buf, true)])
}

// fakeTunnels reaches an upstream over plain TCP as if through a tunnel.
type fakeTunnels struct {
	addr   string // the listener the "client" forwards to
	local  string // the tunnel's local address (Host header)
	online bool
	mu     sync.Mutex
	dials  int
}

func (f *fakeTunnels) LookupByServiceID(_ context.Context, id string) (*proxy.Resolved, error) {
	if !f.online {
		return nil, proxy.ErrNotFound
	}
	return &proxy.Resolved{ServiceID: id, LocalHost: f.local}, nil
}

func (f *fakeTunnels) DialTunnelStreamByServiceID(ctx context.Context, _ string) (net.Conn, error) {
	if !f.online {
		return nil, proxy.ErrNotFound
	}
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", f.addr)
}

func TestTunnelSource_SyncAndCallThroughTheTunnel(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	st, uid := newStore(t)
	svc := db.Service{ID: "svc-notes", UserID: uid, Name: "notes", Type: "http", Subdomain: "notes", AccessMode: "api_key"}
	if err := st.CreateService(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(up.URL)
	tun := &fakeTunnels{addr: u.Host, local: "127.0.0.1:8931", online: true}
	p := New(Options{Store: st, Tunnels: tun})
	t.Cleanup(p.Close)
	src, err := st.CreateMCPSource(context.Background(), uid, store.MCPSourceInput{Slug: "notes", Name: "notes", Kind: "tunnel", ServiceID: svc.ID})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Sync(context.Background(), "notes")
	if err != nil || res.Added != 5 || res.Pending != 5 {
		t.Fatalf("sync: %+v %v", res, err)
	}
	if _, err := echo(t, p, src, "x"); err != nil {
		t.Fatal(err)
	}
	for _, r := range up.Requests() {
		if r.Path != "/mcp" || r.Header.Get("Authorization") != "" {
			t.Fatalf("tunnel request path %q headers %v", r.Path, r.Header)
		}
	}
	if tun.dials == 0 {
		t.Fatal("the tunnel was not dialled")
	}
	tun.online = false
	if _, err := echo(t, p, src, "x"); !errors.Is(err, ErrTunnelOffline) {
		t.Fatalf("offline err = %v", err)
	}
	if _, err := p.Sync(context.Background(), "notes"); !errors.Is(err, ErrTunnelOffline) {
		t.Fatalf("offline sync err = %v", err)
	}
	after, _ := st.MCPSourceBySlug(context.Background(), "notes")
	if after.LastSyncError != ErrTunnelOffline.Error() || after.ToolCount != 5 {
		t.Fatalf("after offline sync: %q, %d tools", after.LastSyncError, after.ToolCount)
	}
}

func TestUpstreamRT_RefusesAForeignHost(t *testing.T) {
	sent := 0
	rt := &upstreamRT{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}), host: "mcp.example.com", header: "Authorization", value: "Bearer " + testSecret, max: 10}
	req, _ := http.NewRequest(http.MethodPost, "https://evil.example.net/mcp", nil)
	if _, err := rt.RoundTrip(req); !errors.Is(err, errForeignHost) || sent != 0 {
		t.Fatalf("foreign host: err %v, sent %d", err, sent)
	}
}

func TestCappedBody(t *testing.T) {
	read := func(body string, max int64) error {
		_, err := io.ReadAll(&cappedBody{rc: io.NopCloser(bytes.NewReader([]byte(body))), left: max})
		return err
	}
	if err := read("12345", 5); err != nil {
		t.Fatalf("body at the cap: %v", err)
	}
	if err := read("123456", 5); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("body over the cap: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemote_RedirectIsNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	st, uid := newStore(t)
	tr := aiprovider.NewTransport(true)
	tr.TLSClientConfig = redirector.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer tr.CloseIdleConnections()
	p := New(Options{Store: st, Vault: mapVault{"UP": testSecret}, Remote: tr})
	defer p.Close()
	if _, err := st.CreateMCPSource(context.Background(), uid, store.MCPSourceInput{Slug: "gh", Name: "gh", Kind: "remote", URL: redirector.URL + "/mcp", CredentialSlot: "UP"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sync(context.Background(), "gh"); err == nil {
		t.Fatal("sync through a redirect succeeded")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests", n)
	}
}

// The tunnel request's Host is the tunnel's local address (the client
// bridge accepts only a loopback Host), or 127.0.0.1 when it has none.
func TestTunnelSource_HostIsTheLocalAddress(t *testing.T) {
	for _, tc := range []struct{ local, want string }{{"127.0.0.1:8931", "127.0.0.1:8931"}, {"", "127.0.0.1"}} {
		var mu sync.Mutex
		var hosts []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hosts = append(hosts, r.Host)
			mu.Unlock()
			http.Error(w, "no", http.StatusInternalServerError)
		}))
		st, uid := newStore(t)
		svc := db.Service{ID: "svc-notes", UserID: uid, Name: "notes", Type: "http", Subdomain: "notes", AccessMode: "api_key"}
		if err := st.CreateService(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateMCPSource(context.Background(), uid, store.MCPSourceInput{Slug: "notes", Name: "notes", Kind: "tunnel", ServiceID: svc.ID}); err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(srv.URL)
		p := New(Options{Store: st, Tunnels: &fakeTunnels{addr: u.Host, local: tc.local, online: true}})
		_, _ = p.Sync(context.Background(), "notes")
		p.Close()
		srv.Close()
		mu.Lock()
		if len(hosts) == 0 || hosts[0] != tc.want {
			t.Errorf("local %q: Host %v, want %q", tc.local, hosts, tc.want)
		}
		mu.Unlock()
	}
}

// A JSON-RPC error answered on the shared session is the tool's business,
// not a broken session: the session is kept for the next caller.
func TestCallTool_JSONRPCErrorKeepsTheSession(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL)
	if _, err := echo(t, f.pool, src, "a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.pool.CallTool(ctx, src, &mcp.CallToolParams{Name: "no_such_tool", Arguments: map[string]any{}})
	var rpc *jsonrpcError
	if !errors.As(err, &rpc) {
		t.Fatalf("unknown tool err = %v, want a JSON-RPC error", err)
	}
	if _, err := echo(t, f.pool, src, "b"); err != nil {
		t.Fatal(err)
	}
	calls := up.CallsTo("echo")
	if len(calls) != 2 || calls[0].SessionID != calls[1].SessionID || countRPC(up, "initialize") != 1 {
		t.Fatalf("session changed after a JSON-RPC error: %+v, %d initialize", calls, countRPC(up, "initialize"))
	}
}

// The call's context bounds a call (up to 300 s), not the shared transport's
// response-header timeout.
func TestCallTool_SlowToolOutlivesTheHeaderTimeout(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true})
	f := newFixture(t, up, func(o *Options) {
		tr := o.Remote.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 200 * time.Millisecond
		o.Remote = tr
	})
	src := f.remote(t, "gh", up.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := f.pool.CallTool(ctx, src, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{"ms": 600}})
	if err != nil || res.IsError {
		t.Fatalf("slow call: %v %+v", err, res)
	}
}

// The guard survives the pool's copy of the transport.
func TestRemote_GuardSurvivesTheTransportCopy(t *testing.T) {
	tr := aiprovider.NewTransport(false)
	p := New(Options{Remote: tr})
	defer p.Close()
	c, ok := p.opts.Remote.(*http.Transport)
	if !ok || c == tr || c.ResponseHeaderTimeout != 0 || c.DialContext == nil || c.Proxy != nil {
		t.Fatalf("pool transport %+v", c)
	}
	if _, err := c.DialContext(context.Background(), "tcp", "127.0.0.1:1"); !errors.Is(err, aiprovider.ErrBlockedAddress) {
		t.Fatalf("dial through the copy: %v", err)
	}
}

func TestPool_UseAfterCloseFails(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: true})
	f := newFixture(t, up)
	src := f.remote(t, "gh", up.URL)
	if _, err := echo(t, f.pool, src, "x"); err != nil {
		t.Fatal(err)
	}
	f.pool.Close()
	if _, err := echo(t, f.pool, src, "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close: %v", err)
	}
	// A drop after Close closes at once instead of joining the wait group.
	f.pool.closeLater(nil)
}

// A stateful upstream that answers a stale session with 404 and a JSON-RPC
// body (as the TypeScript SDK does) gets a new session, and the call is
// sent again on it.
func TestCallTool_StaleSession404WithJSONRPCBody(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","error":{"code":-32001,"message":"Session not found"},"id":null}`,
		`{"jsonrpc":"2.0","error":{"code":-32001,"message":"Session not found"},"id":1}`,
	} {
		t.Run(body[len(body)-8:], func(t *testing.T) { staleSession404(t, body) })
	}
}

func staleSession404(t *testing.T, body string) {
	sdk := mcptest.NewSDKServer("ts-like", nil, nil)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk },
		&mcp.StreamableHTTPOptions{JSONResponse: true, DisableLocalhostProtection: true})
	var mu sync.Mutex
	stale := map[string]bool{}
	var sessions []string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		gone := stale[id]
		mu.Unlock()
		if id != "" && gone {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, body)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(readPeek(r), `"tools/call"`) {
			mu.Lock()
			sessions = append(sessions, id)
			mu.Unlock()
		}
		h.ServeHTTP(w, r)
	}))
	defer up.Close()
	st, uid := newStore(t)
	tr := aiprovider.NewTransport(true)
	tr.TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	p := New(Options{Store: st, Remote: tr})
	defer p.Close()
	src, err := st.CreateMCPSource(context.Background(), uid, store.MCPSourceInput{Slug: "ts", Name: "ts", Kind: "remote", URL: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := echo(t, p, src, "a"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for _, s := range sessions {
		stale[s] = true
	}
	mu.Unlock()
	res, err := echo(t, p, src, "b")
	if err != nil || resultText(res) != "echo: b" {
		t.Fatalf("call after the stale-session 404: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 2 || sessions[0] == sessions[1] {
		t.Fatalf("tools/call sessions %v, want two different ones", sessions)
	}
}

// readPeek reads the request body and puts it back.
func readPeek(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return string(b)
}

// A tools/call response is capped near the gateway's result cap (2 MiB by
// default), not at the 8 MiB a tool list may need, so the memory one call
// holds stays bounded; the tool list keeps the larger cap.
func TestCallTool_ResponseCapNearTheResultCap(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		t.Run(map[bool]string{false: "stateless", true: "stateful"}[stateful], func(t *testing.T) {
			up := mcptest.NewServer(t, mcptest.Opts{TLS: true, Stateful: stateful})
			f := newFixture(t, up)
			src := f.remote(t, "gh", up.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := f.pool.CallTool(ctx, src, &mcp.CallToolParams{Name: "big", Arguments: map[string]any{"bytes": 3 << 20}}); !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("a 3 MiB result: err = %v, want ErrResponseTooLarge", err)
			}
			res, err := f.pool.CallTool(ctx, src, &mcp.CallToolParams{Name: "big", Arguments: map[string]any{"bytes": 1 << 20}})
			if err != nil || res.IsError {
				t.Fatalf("a 1 MiB result: %v %v", res, err)
			}
			if _, err := f.pool.Sync(ctx, "gh"); err != nil {
				t.Fatalf("the tool list after a refused result: %v", err)
			}
		})
	}
}
