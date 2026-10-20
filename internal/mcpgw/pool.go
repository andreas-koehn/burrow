// Package mcpgw is the upstream side of the MCP gateway: it reaches the tool
// sources (remote MCP servers over the guarded transport, and MCP servers
// behind a client through the tunnel), keeps their sessions, and syncs their
// tool definitions into mcp_tools (D5, D9, D10, D14, D15).
//
// # Pool
//
//	p := mcpgw.New(mcpgw.Options{Store: st, Vault: vault, Remote: guarded, Tunnels: dialer, InspectText: …})
//	p.Start(ctx)                       // background sync, every 10 minutes per enabled source
//	defer p.Close()                    // stops the sync, closes every upstream session
//	res, err := p.CallTool(ctx, src, &mcp.CallToolParams{Name: native, Arguments: rawArgs})
//	sr, err := p.Sync(ctx, slug)       // what the "Sync tools" button does
//	p.Invalidate(slug)                 // a source was changed or deleted: drop its session
//
// The caller's credential never goes upstream: a remote source gets the
// value of its vault slot, injected into the header and format the source
// names, and only toward the source's own host. Remote sources are dialled
// through the guarded transport (Options.Remote), so a URL that resolves to a
// private address is refused at dial time unless the operator allowed
// private upstreams; redirects are never followed. Tunnel sources are
// dialled through the client's tunnel and addressed as
// http://<local address>/mcp.
//
// A stateful upstream (one that hands out Mcp-Session-Id) gets one
// long-lived session per source, shared by all callers; it is re-created on
// session loss, and a call that failed because the upstream no longer knew
// the session is sent once more on the new session. A stateless upstream gets
// a fresh session per call. Failed connects back off (0.5 s to 30 s, with
// jitter); while backing off, calls fail at once.
//
// # Served tools
//
// Served(row) turns an mcp_tools row into the tool the gateway may serve: only
// an approved, not hidden tool, from its approved definition with the
// overrides applied. The current (possibly changed) definition is never
// served.
package mcpgw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/backoff"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/version"
)

// Errors of the pool. Their messages are written for an admin and never
// carry a URL, a header value or a credential.
var (
	// ErrSourceNotFound: no source has the slug.
	ErrSourceNotFound = errors.New("tool source not found")
	// ErrSourceDisabled: the source is switched off.
	ErrSourceDisabled = errors.New("the tool source is disabled")
	// ErrSyncRunning: a sync of this source is already running.
	ErrSyncRunning = errors.New("a sync of this tool source is already running")
	// ErrCredentialMissing: the source names a vault slot that holds no value.
	ErrCredentialMissing = errors.New("the credential slot of this tool source is not set")
	// ErrTunnelOffline: no client is serving the tunnel source right now.
	ErrTunnelOffline = errors.New("the client serving this tool source is not connected")
	// ErrBackingOff: connecting failed a moment ago; the pool waits before it
	// tries again.
	ErrBackingOff = errors.New("the tool source could not be reached a moment ago; retrying shortly")
	// ErrResponseTooLarge: an upstream response exceeded Options.MaxResponseBytes.
	ErrResponseTooLarge = errors.New("the tool source sent a response that is too large")
	// ErrClosed: the pool was closed.
	ErrClosed = errors.New("the tool gateway is shutting down")
	// errForeignHost: a request to another host than the source's own (never
	// sent, so the credential cannot follow it).
	errForeignHost = errors.New("request to a host other than the tool source's")
)

// SourceStore is what the pool reads and writes. *store.Store satisfies it.
type SourceStore interface {
	MCPSourceBySlug(ctx context.Context, slug string) (db.MCPSource, error)
	ListMCPSources(ctx context.Context) ([]db.MCPSource, error)
	ApplyMCPToolSync(ctx context.Context, slug string, seen []db.MCPToolSeen, at time.Time, warning string) (db.MCPSyncCounts, error)
	RecordMCPSyncError(ctx context.Context, slug string, at time.Time, msg string) error
}

// Vault reads a credential slot (credinject.Vault satisfies it).
type Vault interface {
	Get(slot string) (string, bool)
}

// TunnelDialer reaches the live tunnel of a service (the AI gateway's dialer
// satisfies it). Errors wrapping proxy.ErrNotFound mean no client serves it.
type TunnelDialer interface {
	LookupByServiceID(ctx context.Context, serviceID string) (*proxy.Resolved, error)
	DialTunnelStreamByServiceID(ctx context.Context, serviceID string) (net.Conn, error)
}

// Options configures a Pool.
type Options struct {
	Store SourceStore
	Vault Vault
	// Remote is the transport for remote sources: the guarded transport of
	// the AI gateway (aiprovider.NewTransport). Without it remote sources
	// cannot be reached; there is no fallback to an unguarded transport.
	Remote http.RoundTripper
	// Tunnels reaches tunnel sources; nil = tunnel sources cannot be reached.
	Tunnels TunnelDialer
	// InspectText runs text through the guardrail request patterns (bundled
	// and custom) and returns the first pattern id that matched. nil = no
	// description check.
	InspectText func(ctx context.Context, text []byte) (hit bool, id string)
	// OnBackgroundSync, when set, is called after a background sync that
	// changed something (tools added, changed or removed).
	OnBackgroundSync func(slug string, res SyncResult)
	Log              *slog.Logger
	// SyncInterval is the background sync period per source (default 10 min).
	SyncInterval time.Duration
	// SyncTimeout bounds one sync (default 60 s).
	SyncTimeout time.Duration
	// MaxResponseBytes caps one upstream HTTP response body (default 8 MiB).
	MaxResponseBytes int64
	// MaxCallResponseBytes caps the response body of a tools/call (default
	// 2 MiB: the gateway's 1 MiB result cap plus room for the envelope), so
	// the memory one call holds stays close to what may be delivered.
	MaxCallResponseBytes int64
}

// Defaults of Options.
const (
	DefaultSyncInterval         = 10 * time.Minute
	DefaultSyncTimeout          = 60 * time.Second
	DefaultMaxResponseBytes     = 8 << 20
	DefaultMaxCallResponseBytes = 2 << 20
)

// Pool holds the upstream access of every tool source. Safe for concurrent
// use.
type Pool struct {
	opts   Options
	client *mcp.Client
	log    *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
	syncing map[string]bool
	closed  bool

	// bg tracks goroutines the pool started (session closes, the sync loop);
	// bgMu and bgDone keep Add from racing with Close's Wait.
	bg       sync.WaitGroup
	bgMu     sync.Mutex
	bgDone   bool
	stopLoop context.CancelFunc
}

// entry is the upstream state of one source under one configuration.
type entry struct {
	key  string
	lock chan struct{} // held while connecting a stateful session

	mu        sync.Mutex
	cs        *mcp.ClientSession // the long-lived session of a stateful upstream
	stateless bool               // learned on the first connect
	bo        *backoff.Backoff
	retryAt   time.Time
	lastErr   error // why the last connect failed
	dead      bool  // replaced or invalidated: the session must not be kept
}

// New returns a pool. Call Start for the background sync and Close at
// shutdown.
func New(opts Options) *Pool {
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = DefaultSyncInterval
	}
	if opts.SyncTimeout <= 0 {
		opts.SyncTimeout = DefaultSyncTimeout
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if opts.MaxCallResponseBytes <= 0 {
		opts.MaxCallResponseBytes = DefaultMaxCallResponseBytes
	}
	// The pool's own copy of the guarded transport (same dialer and guard):
	// a tool call may run up to 300 s and is bounded by its context, so the
	// shared response-header timeout must not cut it off.
	if tr, ok := opts.Remote.(*http.Transport); ok && tr != nil {
		c := tr.Clone()
		c.ResponseHeaderTimeout = 0
		opts.Remote = c
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Pool{
		opts: opts,
		log:  log,
		// Tools only (D3): no roots, sampling or elicitation is offered to
		// the upstream.
		client: mcp.NewClient(&mcp.Implementation{Name: "burrow-gateway", Version: version.Version},
			&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}}),
		entries: map[string]*entry{},
		syncing: map[string]bool{},
	}
}

// sourceKey fingerprints what a session depends on; a change gives a new
// entry (and a new session).
func sourceKey(s db.MCPSource) string {
	return strings.Join([]string{s.Kind, s.URL, s.ServiceID, s.CredentialSlot, s.AuthHeader, s.AuthFormat, s.ProtocolVersion}, "\x00")
}

// entryFor returns the entry of the source's current configuration,
// replacing (and closing) one of an older configuration.
func (p *Pool) entryFor(src db.MCPSource) (*entry, error) {
	key := sourceKey(src)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	e := p.entries[src.Slug]
	if e != nil && e.key == key {
		return e, nil
	}
	if e != nil {
		p.retireLocked(e)
	}
	e = &entry{key: key, lock: make(chan struct{}, 1), bo: backoff.New(500*time.Millisecond, 30*time.Second)}
	p.entries[src.Slug] = e
	return e, nil
}

// retireLocked marks e dead and closes its session in the background.
// p.mu is held.
func (p *Pool) retireLocked(e *entry) {
	e.mu.Lock()
	cs := e.cs
	e.cs, e.dead = nil, true
	e.mu.Unlock()
	p.closeLater(cs)
}

// closeLater closes a session off the caller's path: closing a stateful
// session sends a DELETE that may take up to a few seconds. Close waits for it.
func (p *Pool) closeLater(cs *mcp.ClientSession) {
	if cs == nil {
		return
	}
	p.bgMu.Lock()
	if p.bgDone {
		p.bgMu.Unlock()
		_ = cs.Close() // after Close: nobody waits any more, close here
		return
	}
	p.bg.Add(1)
	p.bgMu.Unlock()
	go func() {
		defer p.bg.Done()
		_ = cs.Close()
	}()
}

func (p *Pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Invalidate drops the session of a source; the next call connects anew with
// the source's settings as they are then. Call it after a source was changed
// or deleted.
func (p *Pool) Invalidate(slug string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[slug]; e != nil {
		p.retireLocked(e)
		delete(p.entries, slug)
	}
}

// Removed forgets a deleted source: its upstream session, as Invalidate.
func (p *Pool) Removed(slug string) { p.Invalidate(slug) }

// Close stops the background sync and closes every upstream session. Calls
// after Close fail with ErrClosed.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.stopLoop != nil {
		p.stopLoop()
	}
	for slug, e := range p.entries {
		p.retireLocked(e)
		delete(p.entries, slug)
	}
	p.mu.Unlock()
	p.bgMu.Lock()
	p.bgDone = true
	p.bgMu.Unlock()
	p.bg.Wait()
	if tr, ok := p.opts.Remote.(interface{ CloseIdleConnections() }); ok {
		tr.CloseIdleConnections()
	}
}

// CallTool calls a tool of the source by its upstream (native) name. The
// caller bounds the call with ctx (the source's timeout). A tool that failed
// is a result with IsError set, not an error; errors are about reaching the
// upstream, and their messages are fit for an admin (see Describe).
func (p *Pool) CallTool(ctx context.Context, src db.MCPSource, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if !src.Enabled {
		return nil, ErrSourceDisabled
	}
	// The SDK sends the call's request with this context: upstreamRT reads
	// the smaller cap from it.
	ctx = context.WithValue(ctx, ctxKeyBodyCap{}, p.opts.MaxCallResponseBytes)
	var res *mcp.CallToolResult
	err := p.withSession(ctx, src, func(cs *mcp.ClientSession) error {
		var err error
		res, err = cs.CallTool(ctx, params)
		return err
	})
	return res, err
}

// withSession runs fn on a session of the source. On a stateful upstream a
// failure drops the shared session (unless the caller gave up), and a
// failure that says the upstream lost the session, or that the connection
// was already closed, runs fn once more on a new session: the upstream did
// not act on the first attempt.
func (p *Pool) withSession(ctx context.Context, src db.MCPSource, fn func(*mcp.ClientSession) error) error {
	e, err := p.entryFor(src)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		cs, shared, err := p.acquire(ctx, e, src)
		if err != nil {
			return err
		}
		err = fn(cs)
		if !shared {
			_ = cs.Close() // a stateless upstream's session ends with the call
			return mapErr(err)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !sessionBroken(err) {
			// The caller gave up, or the upstream answered with a JSON-RPC
			// error: the session itself is fine and stays for the others.
			return mapErr(err)
		}
		p.drop(e, cs)
		if attempt == 0 && (errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed)) {
			p.log.Info("mcpgw: upstream session lost, reconnecting", "source", src.Slug)
			continue
		}
		return mapErr(err)
	}
}

// sessionBroken reports whether err means the shared session can no longer
// be used: the upstream lost it, the connection is closed, a response was
// over the cap, or the request failed in transport. A JSON-RPC error the
// upstream answered with (unknown tool, invalid params, …) leaves it intact.
func sessionBroken(err error) bool {
	if errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, ErrResponseTooLarge) {
		return true
	}
	var rpc *jsonrpcError
	return !errors.As(err, &rpc)
}

// drop forgets cs as the entry's session (if it still is) and closes it.
func (p *Pool) drop(e *entry, cs *mcp.ClientSession) {
	e.mu.Lock()
	if e.cs == cs {
		e.cs = nil
	}
	e.mu.Unlock()
	p.closeLater(cs)
}

// acquire returns a session: the shared one of a stateful upstream (shared
// = true), or a fresh one the caller closes.
func (p *Pool) acquire(ctx context.Context, e *entry, src db.MCPSource) (cs *mcp.ClientSession, shared bool, err error) {
	if p.isClosed() {
		return nil, false, ErrClosed
	}
	e.mu.Lock()
	cs, stateless := e.cs, e.stateless
	e.mu.Unlock()
	if cs != nil {
		return cs, true, nil
	}
	if !stateless {
		// One connect at a time for a (possibly) stateful upstream, so that
		// concurrent callers end up on one session.
		select {
		case e.lock <- struct{}{}:
			defer func() { <-e.lock }()
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		e.mu.Lock()
		cs = e.cs
		e.mu.Unlock()
		if cs != nil {
			return cs, true, nil
		}
	}
	e.mu.Lock()
	wait, last := time.Until(e.retryAt), e.lastErr
	e.mu.Unlock()
	if wait > 0 {
		return nil, false, &backingOff{cause: last}
	}
	cs, err = p.connect(ctx, src)
	if err == nil && p.isClosed() {
		// Closed while connecting: close the new session here, not under
		// e.mu (closing may wait on the upstream).
		p.closeLater(cs)
		return nil, false, ErrClosed
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		err = mapErr(err)
		if ctx.Err() == nil {
			e.retryAt, e.lastErr = time.Now().Add(e.bo.NextBackOff()), err
		}
		return nil, false, err
	}
	e.bo.Reset()
	e.retryAt, e.lastErr = time.Time{}, nil
	if cs.ID() == "" {
		e.stateless = true
		return cs, false, nil
	}
	e.stateless = false
	if e.dead {
		// The source was changed while this connect ran: serve this one call
		// and close the session after it.
		return cs, false, nil
	}
	e.cs = cs
	return cs, true, nil
}

// connect opens a session to the source's upstream.
func (p *Pool) connect(ctx context.Context, src db.MCPSource) (*mcp.ClientSession, error) {
	endpoint, rt, err := p.transport(ctx, src)
	if err != nil {
		return nil, err
	}
	t := &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{
			Transport: rt,
			// A redirect could lead the credential, or the request, to
			// another host; the gateway never follows one.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		MaxRetries: -1,
		// Tools only, request and response: no standing stream the upstream
		// could push into. Changed tools are found by the sync.
		DisableStandaloneSSE: true,
		MaxEventSize:         int(p.opts.MaxResponseBytes),
	}
	return p.client.Connect(ctx, t, &mcp.ClientSessionOptions{ProtocolVersion: src.ProtocolVersion})
}

// transport returns the endpoint and the HTTP transport of a source.
func (p *Pool) transport(ctx context.Context, src db.MCPSource) (string, http.RoundTripper, error) {
	switch src.Kind {
	case "remote":
		if p.opts.Remote == nil {
			return "", nil, errors.New("remote tool sources are not available on this relay")
		}
		u, err := aiprovider.ValidateBaseURL(src.URL)
		if err != nil {
			return "", nil, errors.New("the tool source's url is not a valid https URL")
		}
		rt := &upstreamRT{base: p.opts.Remote, host: u.Host, max: p.opts.MaxResponseBytes}
		if src.CredentialSlot != "" {
			key, ok := "", false
			if p.opts.Vault != nil {
				key, ok = p.opts.Vault.Get(src.CredentialSlot)
			}
			if !ok || key == "" {
				return "", nil, ErrCredentialMissing
			}
			header, format := src.AuthHeader, src.AuthFormat
			if header == "" {
				header = "Authorization"
			}
			if format == "" {
				format = "Bearer {key}"
			}
			value := strings.Replace(format, "{key}", key, 1)
			if strings.ContainsAny(value, "\r\n\x00") {
				return "", nil, errors.New("the credential of this tool source cannot be sent in a header")
			}
			rt.header, rt.value = http.CanonicalHeaderKey(header), value
		}
		return u.String(), rt, nil
	case "tunnel":
		if p.opts.Tunnels == nil {
			return "", nil, ErrTunnelOffline
		}
		res, err := p.opts.Tunnels.LookupByServiceID(ctx, src.ServiceID)
		if err != nil {
			if errors.Is(err, proxy.ErrNotFound) {
				return "", nil, ErrTunnelOffline
			}
			return "", nil, fmt.Errorf("look up the tunnel: %w", err)
		}
		// The request carries the tunnel's local address as its Host, as the
		// proxy does: the client's bridge keeps the SDK's localhost
		// protection and accepts only a loopback Host.
		host := res.LocalHost
		if host == "" {
			host = "127.0.0.1"
		}
		serviceID, tunnels := src.ServiceID, p.opts.Tunnels
		base := &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return tunnels.DialTunnelStreamByServiceID(ctx, serviceID)
			},
			DisableCompression: true,
			DisableKeepAlives:  true, // one tunnel stream per request
		}
		endpoint := (&url.URL{Scheme: "http", Host: host, Path: "/mcp"}).String()
		return endpoint, &upstreamRT{base: base, host: host, max: p.opts.MaxResponseBytes}, nil
	}
	return "", nil, fmt.Errorf("unknown tool source kind %q", src.Kind)
}

// upstreamRT sends the gateway's requests to one source: only to the
// source's own host, with the source's credential (if any), and with the
// response body capped.
type upstreamRT struct {
	base          http.RoundTripper
	host          string
	header, value string
	max           int64
}

func (t *upstreamRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != t.host {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errForeignHost
	}
	out := req.Clone(req.Context())
	// Nothing that could be mistaken for a caller's credential leaves with
	// the request; the source's own is the only one.
	out.Header.Del("Authorization")
	out.Header.Del("X-Api-Key")
	out.Header.Del("Cookie")
	if t.header != "" {
		out.Header.Set(t.header, t.value)
	}
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound && req.Header.Get("Mcp-Session-Id") != "" {
		// The upstream no longer knows the session (§2.5.3), whatever its
		// body says. Some servers send a JSON-RPC error with the 404, which
		// the SDK would report as an ordinary JSON-RPC error and keep the
		// dead session; without the body it reports ErrSessionMissing, the
		// session is dropped and the call, which the upstream did not act
		// on, is sent again on a new one.
		_ = resp.Body.Close()
		resp.Body = http.NoBody
		resp.ContentLength = 0
		resp.Header.Del("Content-Type")
		return resp, nil
	}
	limit := t.max
	if c, ok := req.Context().Value(ctxKeyBodyCap{}).(int64); ok && c > 0 && c < limit {
		limit = c
	}
	resp.Body = &cappedBody{rc: resp.Body, left: limit}
	return resp, nil
}

// ctxKeyBodyCap carries a smaller response cap for the requests of one call.
type ctxKeyBodyCap struct{}

// cappedBody fails a read past the cap instead of truncating silently.
type cappedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// One byte more tells a body of exactly the cap from a larger one.
		var one [1]byte
		if n, _ := b.rc.Read(one[:]); n > 0 {
			return 0, ErrResponseTooLarge
		}
		return b.rc.Read(p)
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

// backingOff is the error of a call made while the pool waits before it
// connects again: it is ErrBackingOff and the cause of the last failure.
type backingOff struct{ cause error }

func (b *backingOff) Error() string {
	if b.cause == nil {
		return ErrBackingOff.Error()
	}
	return ErrBackingOff.Error() + ": " + b.cause.Error()
}

func (b *backingOff) Unwrap() []error {
	if b.cause == nil {
		return []error{ErrBackingOff}
	}
	return []error{ErrBackingOff, b.cause}
}

// mapErr turns a few transport-level causes into the pool's own errors.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, proxy.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrTunnelOffline, err)
	case !errors.Is(err, ErrResponseTooLarge) && !isJSONRPC(err) && strings.Contains(err.Error(), ErrResponseTooLarge.Error()):
		// The SDK reports a failed body read with %v, which loses the
		// cappedBody's error; its text is still there.
		return fmt.Errorf("%w: %w", ErrResponseTooLarge, err)
	}
	return err
}

func isJSONRPC(err error) bool {
	var rpc *jsonrpcError
	return errors.As(err, &rpc)
}

// Describe returns a short message about err that is fit for an admin (the
// dashboard, last_sync_error): it never carries a URL, a header value or a
// credential, nor upstream text other than a JSON-RPC error message.
func Describe(err error) string {
	var rpc *jsonrpcError
	var bo *backingOff
	switch {
	case err == nil:
		return ""
	case errors.As(err, &bo) && bo.cause != nil:
		// The reason of the failure the pool is backing off from.
		return Describe(bo.cause)
	case errors.Is(err, ErrSourceNotFound), errors.Is(err, ErrSourceDisabled), errors.Is(err, ErrSyncRunning),
		errors.Is(err, ErrCredentialMissing), errors.Is(err, ErrTunnelOffline), errors.Is(err, ErrBackingOff),
		errors.Is(err, ErrResponseTooLarge), errors.Is(err, ErrClosed), errors.Is(err, errTooManyTools):
		return rootMessage(err)
	case errors.Is(err, aiprovider.ErrBlockedAddress):
		return "the tool source's address is private or loopback; private upstreams are not allowed on this relay"
	case errors.Is(err, context.DeadlineExceeded):
		return "the tool source did not answer in time"
	case errors.Is(err, mcp.ErrSessionMissing):
		return "the tool source lost the session; it is re-created on the next call"
	case asJSONRPC(err, &rpc):
		return "the tool source answered with an error: " + clip(rpc.Message, 200)
	case strings.Contains(err.Error(), http.StatusText(http.StatusUnauthorized)), strings.Contains(err.Error(), http.StatusText(http.StatusForbidden)):
		return "the tool source refused the credential"
	}
	return "the tool source could not be reached"
}

// Classify cuts a message Describe made (or a sync warning) down to a short
// fixed class with no upstream text in it, for callers that may be agents:
// a JSON-RPC error message and the names of skipped tools are written by
// the upstream and could be worded to steer whoever reads them.
func Classify(msg string) string {
	switch {
	case msg == "":
		return ""
	case strings.HasPrefix(msg, "the tool source answered with an error"):
		return "upstream error"
	case strings.Contains(msg, "tool(s) skipped"):
		return "tools skipped"
	case msg == "the tool source refused the credential":
		return "upstream refused the credential"
	case msg == ErrCredentialMissing.Error():
		return "credential missing"
	case msg == ErrTunnelOffline.Error():
		return "client not connected"
	case msg == ErrSourceDisabled.Error():
		return "source disabled"
	case msg == ErrSyncRunning.Error():
		return "sync already running"
	case msg == errTooManyTools.Error(), msg == ErrResponseTooLarge.Error():
		return "upstream response too large"
	case msg == "the tool source could not be reached", msg == "the tool source did not answer in time",
		msg == ErrBackingOff.Error(), strings.HasPrefix(msg, "the tool source's address is private"),
		strings.HasPrefix(msg, "the tool source lost the session"):
		return "upstream unreachable"
	}
	return "sync failed"
}

// rootMessage is the message of the first of the pool's own errors in err's
// chain.
func rootMessage(err error) string {
	for _, own := range []error{ErrSourceNotFound, ErrSourceDisabled, ErrSyncRunning, ErrCredentialMissing, ErrTunnelOffline,
		ErrBackingOff, ErrResponseTooLarge, ErrClosed, errTooManyTools} {
		if errors.Is(err, own) {
			return own.Error()
		}
	}
	return err.Error()
}

func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
