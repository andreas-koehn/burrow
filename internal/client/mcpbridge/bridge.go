// Package mcpbridge publishes a local MCP server through an ordinary http
// tunnel (`burrow mcp`).
//
// A Bridge is an MCP client to the local server, which is either a child
// process speaking stdio or a Streamable HTTP endpoint, and at the same time a
// stateless Streamable HTTP server on 127.0.0.1:<random>. That address becomes
// the tunnel's local address, so the relay reaches the server like any other
// http service. Only tools pass the bridge: tools/list answers from the last
// list the local server gave, tools/call is forwarded. The list is fetched
// again when the server announces a change and every RefreshInterval.
//
// A child is supervised: started once, restarted with backoff whenever it
// ends, its stderr written to the logger line by line, and ended with SIGTERM
// (Windows: killed) and, after TerminateGrace, killed when the bridge closes.
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/version"
)

const (
	defaultRefresh    = 60 * time.Second
	defaultGrace      = 5 * time.Second
	defaultBackoffMin = 500 * time.Millisecond
	defaultBackoffMax = 30 * time.Second
	// connectTimeout bounds the MCP handshake with a fresh child or URL.
	connectTimeout = 30 * time.Second
	// listTimeout bounds one fetch of the tool list.
	listTimeout = 30 * time.Second
	// stableRun is how long a child has to run for the next restart to start
	// from the shortest wait again.
	stableRun = time.Minute
)

// Status is what a status view shows of a bridge.
type Status struct {
	Tools   int  // tools the bridge serves
	Running bool // the local server answers: the child runs, or the URL was reached
}

// Options configures a Bridge. Exactly one of Command and URL is set.
type Options struct {
	// Name is the source's name, for the log lines.
	Name string
	// Command is the child to run: the program and its arguments.
	Command []string
	// Env holds KEY=VALUE pairs added to this process's environment for the
	// child.
	Env []string
	// URL is the Streamable HTTP endpoint of a local MCP server.
	URL string
	// RestartMax is how often an ended child is started again; negative is
	// without limit. After the last restart the bridge gives up: Done closes
	// and Err says why.
	RestartMax int
	// Logger receives the bridge's lines and the child's stderr; nil discards.
	Logger *slog.Logger
	// OnStatus is told every change of the tool count or of Running. Calls
	// do not overlap.
	OnStatus func(Status)
	// RefreshInterval is how often the tool list is fetched again; 0 is 60 s.
	RefreshInterval time.Duration
	// BackoffMin and BackoffMax bound the wait before a restart or a new
	// attempt to reach the URL; 0 is 500 ms and 30 s.
	BackoffMin, BackoffMax time.Duration
	// TerminateGrace is how long a child has between SIGTERM and the kill;
	// 0 is 5 s.
	TerminateGrace time.Duration

	// sleep waits between two starts; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// errNotRunning: the child is between two runs.
var errNotRunning = errors.New("the local MCP server is not running")

// Bridge is a running bridge. Close it to end the child and the listener.
type Bridge struct {
	opts   Options
	log    *slog.Logger
	ln     net.Listener
	http   *http.Server
	server *mcp.Server
	client *mcp.Client
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	kick   chan struct{}

	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
	closeOnce sync.Once

	// dialMu serialises connecting to the URL.
	dialMu sync.Mutex
	// toolsMu serialises changes of the served tool list.
	toolsMu sync.Mutex
	served  map[string]string // tool name → its definition as JSON
	// statusMu serialises OnStatus calls.
	statusMu sync.Mutex

	mu       sync.Mutex
	sess     *mcp.ClientSession
	tools    int
	up       bool
	err      error
	starts   int
	restarts int
	pid      int
}

// Start validates o, listens on 127.0.0.1:0 and starts the child or the
// connection to the URL in the background. It does not wait for the local
// server: Ready closes once its first tool list is served.
func Start(o Options) (*Bridge, error) {
	if err := validate(&o); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcp bridge: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bridge{
		opts: o, log: o.Logger, ln: ln, ctx: ctx, cancel: cancel,
		kick:   make(chan struct{}, 1),
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
		served: map[string]string{},
	}
	b.client = mcp.NewClient(&mcp.Implementation{Name: "burrow", Version: version.Version}, &mcp.ClientOptions{
		Logger: b.log,
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case b.kick <- struct{}{}:
			default:
			}
		},
	})
	// Tools only: no prompts, no resources, no logging.
	b.server = mcp.NewServer(&mcp.Implementation{Name: "burrow-mcp-" + o.Name, Version: version.Version}, &mcp.ServerOptions{
		Logger:       b.log,
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
	})
	b.server.AddReceivingMiddleware(toolsOnly)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return b.server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The SDK's DNS-rebinding guard stays on: the relay's tunnel sends
		// Host = the local address, so only a foreign name is refused.
		Logger: b.log,
	})
	// No write timeout: a tool call may take as long as the source allows.
	b.http = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() { _ = b.http.Serve(ln) }()

	b.wg.Add(2)
	if len(o.Command) > 0 {
		go b.supervise()
	} else {
		go b.watchURL()
	}
	go b.refresher()
	return b, nil
}

func validate(o *Options) error {
	if (len(o.Command) > 0) == (o.URL != "") {
		return errors.New("mcp bridge: exactly one of a command and a URL is needed")
	}
	if len(o.Command) > 0 && strings.TrimSpace(o.Command[0]) == "" {
		return errors.New("mcp bridge: the command is empty")
	}
	if o.URL != "" {
		u, err := url.Parse(o.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("mcp bridge: %q is not an http:// or https:// URL", o.URL)
		}
	}
	for _, kv := range o.Env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return errors.New("mcp bridge: an environment entry is not KEY=VALUE") // the value may be a secret
		}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.RefreshInterval <= 0 {
		o.RefreshInterval = defaultRefresh
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = defaultBackoffMin
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = defaultBackoffMax
	}
	if o.TerminateGrace <= 0 {
		o.TerminateGrace = defaultGrace
	}
	if o.sleep == nil {
		o.sleep = sleepCtx
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Addr is the bridge's address, 127.0.0.1:<port>: the tunnel's local address.
func (b *Bridge) Addr() string { return b.ln.Addr().String() }

// Ready closes when the first tool list of the local server is served, or
// when the bridge has given up or is closed.
func (b *Bridge) Ready() <-chan struct{} { return b.ready }

// Done closes when the bridge stops for good: after Close, or when the child
// ended once more than RestartMax allows (Err then says so).
func (b *Bridge) Done() <-chan struct{} { return b.done }

// Err is why the bridge gave up; nil while it runs and after Close.
func (b *Bridge) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// Close ends the child (SIGTERM, then a kill after TerminateGrace), the
// connection to the URL and the listener, and waits for them.
func (b *Bridge) Close() error {
	b.closeOnce.Do(func() {
		b.cancel()
		// Requests under way lose their connection and with it their context.
		_ = b.http.Close()
		b.wg.Wait()
		b.setSession(nil, true)
		b.markReady()
		if len(b.opts.Command) == 0 {
			close(b.done) // the supervisor closes it for a command
		}
	})
	return nil
}

func (b *Bridge) markReady() { b.readyOnce.Do(func() { close(b.ready) }) }

// setSession makes cs the session calls go to; nil while there is none. The
// session it replaces is closed when closeOld is set.
func (b *Bridge) setSession(cs *mcp.ClientSession, closeOld bool) {
	b.mu.Lock()
	old := b.sess
	b.sess = cs
	changed := b.up != (cs != nil)
	b.up = cs != nil
	b.mu.Unlock()
	if closeOld && old != nil && old != cs {
		_ = old.Close()
	}
	if changed {
		b.emit()
	}
}

// emit tells OnStatus the current status.
func (b *Bridge) emit() {
	if b.opts.OnStatus == nil {
		return
	}
	b.statusMu.Lock()
	defer b.statusMu.Unlock()
	b.mu.Lock()
	s := Status{Tools: b.tools, Running: b.up}
	b.mu.Unlock()
	b.opts.OnStatus(s)
}

func (b *Bridge) running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.up
}

// session returns the session to use. For a URL it connects when there is
// none; for a child nil means it is not running.
func (b *Bridge) session(ctx context.Context) (*mcp.ClientSession, error) {
	b.mu.Lock()
	cs := b.sess
	b.mu.Unlock()
	if cs != nil {
		return cs, nil
	}
	if b.opts.URL == "" {
		return nil, errNotRunning
	}
	b.dialMu.Lock()
	defer b.dialMu.Unlock()
	b.mu.Lock()
	cs = b.sess
	b.mu.Unlock()
	if cs != nil {
		return cs, nil
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cs, err := b.client.Connect(cctx, &mcp.StreamableClientTransport{Endpoint: b.opts.URL}, nil)
	if err != nil {
		return nil, err
	}
	b.setSession(cs, true)
	return cs, nil
}

// drop forgets a session that failed, so that the next request to a URL
// connects again.
func (b *Bridge) drop(cs *mcp.ClientSession) {
	if b.opts.URL == "" {
		return // the supervisor owns the child's session
	}
	b.mu.Lock()
	mine := b.sess == cs
	b.mu.Unlock()
	if mine {
		b.setSession(nil, true)
	}
}

// isProtocolError reports whether err is an answer of the local server
// rather than a failed connection.
func isProtocolError(err error) bool {
	var we *jsonrpc.Error
	return errors.As(err, &we)
}

// withSession runs f on the current session. A URL whose session was lost
// gets a new one; f runs again on it when the server said it does not know
// the session (it did not run the request), or when f is a read (again).
func (b *Bridge) withSession(ctx context.Context, again bool, f func(*mcp.ClientSession) error) error {
	for attempt := 0; ; attempt++ {
		cs, err := b.session(ctx)
		if err != nil {
			return err
		}
		err = f(cs)
		if err == nil || isProtocolError(err) || ctx.Err() != nil {
			return err
		}
		b.drop(cs)
		if attempt > 0 || b.opts.URL == "" || (!again && !errors.Is(err, mcp.ErrSessionMissing)) {
			return err
		}
	}
}

// refresh fetches the tool list and serves it.
func (b *Bridge) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var tools []*mcp.Tool
	err := b.withSession(ctx, true, func(cs *mcp.ClientSession) error {
		tools = tools[:0]
		for tl, err := range cs.Tools(ctx, nil) {
			if err != nil {
				return err
			}
			tools = append(tools, tl)
		}
		return nil
	})
	if err != nil {
		return err
	}
	b.serve(tools)
	b.markReady()
	return nil
}

// serve makes tools the served list: new and changed tools are added, the
// ones the server no longer has are removed.
func (b *Bridge) serve(tools []*mcp.Tool) {
	b.toolsMu.Lock()
	keep := map[string]bool{}
	for _, tl := range tools {
		def, err := json.Marshal(tl)
		if err != nil || tl.Name == "" {
			continue
		}
		if b.served[tl.Name] == string(def) {
			keep[tl.Name] = true
			continue
		}
		if b.add(tl) {
			b.served[tl.Name] = string(def)
			keep[tl.Name] = true
		}
	}
	var gone []string
	for name := range b.served {
		if !keep[name] {
			gone = append(gone, name)
			delete(b.served, name)
		}
	}
	if len(gone) > 0 {
		b.server.RemoveTools(gone...)
	}
	n := len(b.served)
	b.toolsMu.Unlock()

	b.mu.Lock()
	changed := b.tools != n
	b.tools = n
	b.mu.Unlock()
	if changed {
		b.emit()
	}
}

// add registers one tool of the local server. A definition the SDK refuses
// (an input schema that is not an object) is left out and logged.
func (b *Bridge) add(tl *mcp.Tool) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Warn("mcp server: tool left out", "source", b.opts.Name, "tool", tl.Name, "reason", fmt.Sprint(r))
			ok = false
		}
	}()
	b.server.AddTool(tl, b.call)
	return true
}

// call forwards a tools/call to the local server.
func (b *Bridge) call(reqCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, done := b.detach(reqCtx)
	defer done()
	// _meta is not forwarded: it carries the protocol details of the relay's request.
	params := &mcp.CallToolParams{Name: req.Params.Name}
	if raw := req.Params.Arguments; len(raw) > 0 {
		params.Arguments = raw
	}
	var res *mcp.CallToolResult
	err := b.withSession(ctx, false, func(cs *mcp.ClientSession) error {
		r, err := cs.CallTool(ctx, params)
		res = r
		return err
	})
	switch {
	case err == nil:
		return res, nil
	case isProtocolError(err):
		return nil, err
	case errors.Is(err, errNotRunning):
		return errorResult("The local MCP server is not running; burrow is starting it again."), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	b.log.Warn("mcp server: call failed", "source", b.opts.Name, "tool", req.Params.Name, "error", err.Error())
	return errorResult("The local MCP server did not answer: " + err.Error()), nil
}

// detach returns a context that ends with reqCtx or the bridge but carries
// none of reqCtx's values: the SDK keeps the protocol version of the
// relay's request there, which is not the one spoken with the local server.
func (b *Bridge) detach(reqCtx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(b.ctx)
	stop := context.AfterFunc(reqCtx, cancel)
	if dl, ok := reqCtx.Deadline(); ok {
		var cancelDL context.CancelFunc
		ctx, cancelDL = context.WithDeadline(ctx, dl)
		return ctx, func() { stop(); cancelDL(); cancel() }
	}
	return ctx, func() { stop(); cancel() }
}

// refusedPrefixes are the method families beyond tools (D3).
var refusedPrefixes = []string{"resources/", "prompts/", "completion/", "logging/", "tasks/", "sampling/", "elicitation/", "roots/"}

// toolsOnly answers a method outside tools with -32601 and a message that
// names the limit. Where the SDK refuses a method itself (it knows the
// server has no resources), its own -32601 comes first.
func toolsOnly(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		for _, p := range refusedPrefixes {
			if strings.HasPrefix(method, p) {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "burrow mcp publishes tools only; " + method + " is not supported"}
			}
		}
		return next(ctx, method, req)
	}
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// refresher fetches the tool list every RefreshInterval and when the local
// server says it changed.
func (b *Bridge) refresher() {
	defer b.wg.Done()
	t := time.NewTicker(b.opts.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		case <-b.kick:
		}
		if err := b.refresh(b.ctx); err != nil && b.ctx.Err() == nil && !errors.Is(err, errNotRunning) {
			b.log.Debug("mcp server: tool list not fetched", "source", b.opts.Name, "error", err.Error())
		}
	}
}

// watchURL reaches the URL for the first tool list, trying again with
// backoff; the refresher keeps the list from then on.
func (b *Bridge) watchURL() {
	defer b.wg.Done()
	bo := newBackoff(b.opts)
	for {
		err := b.refresh(b.ctx)
		if err == nil || b.ctx.Err() != nil {
			return
		}
		d := bo.NextBackOff()
		b.log.Warn("mcp server: cannot reach the URL", "source", b.opts.Name, "url", b.opts.URL, "error", err.Error(), "retry_in", d.Round(time.Millisecond).String())
		if b.opts.sleep(b.ctx, d) != nil {
			return
		}
	}
}

// Test helpers.

func (b *Bridge) restartsForTest() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.restarts
}

func (b *Bridge) startsForTest() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts
}
