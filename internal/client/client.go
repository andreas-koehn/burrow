// Package client implements the burrow control client.
package client

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/backoff"
	"github.com/ankoehn/burrow/internal/proto"
)

// atomicCounter wraps atomic.Uint64 so callers can pass &atomicCounter.v as *atomic.Uint64.
type atomicCounter struct{ v atomic.Uint64 }

// TunnelSpec is one tunnel to register.
type TunnelSpec struct {
	Name       string
	Type       string
	RemotePort int
	LocalAddr  string
	// Slug and Access (a relay access mode: open, api_key, burrow_login) are
	// wishes for an http service that does not exist yet; the relay applies
	// them only when it creates the service. "" = no wish, nothing is sent.
	//
	// For a tcp service Access is open, ip or private. The relay creates the
	// service in that mode; for one that exists it serves the mode stored in
	// the dashboard when that is stricter, and refuses a wish that is
	// stricter than the stored mode (proto.CodeAccessInvalid). A relay that
	// cannot apply it (no proto.CapTCPAccess) is never sent a tcp tunnel
	// with ip or private: see AccessNotAppliedError.
	Slug   string
	Access string
	// AccessDefault (tcp only) is the mode a new service gets when Access is
	// empty: `burrow tcp` without --access and a burrow.yaml entry without
	// access: send "private". It is no wish: a service that exists is served
	// in its stored mode. It counts as the wish against a relay that cannot
	// apply it (see AccessNotAppliedError).
	AccessDefault string
	// MCP is set for the local MCP server of `burrow mcp` or of a burrow.yaml
	// service of type mcp. Such a tunnel is an ordinary http tunnel: the
	// command that runs it starts a bridge (internal/client/mcpbridge) and
	// sets LocalAddr to the bridge's address before the client starts.
	MCP *MCPSpec
	// ProxyProtocol (tcp only) is proto.ProxyProtocolV2 to write a PROXY
	// protocol v2 header to the local target before the bytes of every
	// connection, whatever the service's setting on the relay (which can ask
	// for the header too). "" leaves it to the relay. The relay is told, to
	// show it.
	ProxyProtocol string
}

// Options configures a Client.
type Options struct {
	Server     string
	Token      string
	Insecure   bool
	RootCAs    *x509.CertPool
	ServerName string
	// PinSHA256 is the fingerprint of the control certificate stored at login
	// (see pin.go); "" = none, and the system roots decide. Ignored under
	// Insecure and when RootCAs is set.
	PinSHA256 string
	// RefreshPin, when set, asks the relay's dashboard (over verified HTTPS)
	// for the fingerprint it names now. It is called after a pin mismatch; the
	// pin is replaced only when the answer equals what :7000 presented.
	RefreshPin func(ctx context.Context) (string, error)
	// PinRefreshed is told about the replacement, to store it.
	PinRefreshed func(pin string)
	Tunnels      []TunnelSpec
	// Forwards are the local listeners of `burrow forward` (forward.go). They
	// are opened when Run starts and stay open across reconnects.
	Forwards []ForwardSpec
	Logger   *slog.Logger
	// Observer is told what the client does, for a status view. nil = none.
	Observer Observer
	// RequestSummaries asks the relay for a summary of each request to an http
	// tunnel and hands them to Observer.Request. It is announced to the relay
	// only together with an Observer; without one nobody would read them. A
	// relay that does not know summaries sends none. `burrow connect` and
	// every run that prints log lines leave it off: their auth request is the
	// one it always was.
	RequestSummaries bool
	// StopOnRefusal makes Run return a *RefusedError when the relay refuses
	// for a reason that trying again does not change (see RefusedError.Final).
	// Without it Run keeps reconnecting whatever the relay says, as `burrow
	// connect` always has.
	StopOnRefusal bool
}

// Client maintains an authenticated control session with auto-reconnect.
type Client struct {
	opts       Options
	log        *slog.Logger
	bo         *backoff.Backoff
	registered atomic.Bool
	// events delivers to Options.Observer; nil without one.
	events       *notifier
	pingInterval time.Duration

	mu             sync.Mutex
	tunnelLocal    map[string]string      // tunnelID → localAddr
	tunnelProxy    map[string]bool        // tunnelID → the client writes a PROXY v2 header by itself
	live           map[string]*liveTunnel // the tunnels of the current session
	lastRemotePort int
	pingSent       time.Time       // when the ping that is still unanswered was sent
	localReported  map[string]bool // localAddr → what the observer was last told

	// The forwards (forward.go). fsess is the session local connections are
	// forwarded on, nil while there is none; fwdLive counts the forwards that
	// have not stopped. fwdEnd is closed when the last one stops in a run
	// without tunnels, which then ends with fwdErr.
	fwds    []*forwarder
	fwdWG   sync.WaitGroup
	fsess   *yamux.Session
	fwdLive int
	fwdEnd  chan struct{}
	fwdErr  error
}

// liveTunnel is what the client knows of a tunnel of the current session.
type liveTunnel struct {
	http        bool
	open, total int // visitor connections
}

// New builds a Client.
func New(o Options) *Client {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	c := &Client{
		opts:          o,
		log:           o.Logger,
		bo:            backoff.New(500*time.Millisecond, 30*time.Second),
		pingInterval:  60 * time.Second,
		tunnelLocal:   map[string]string{},
		tunnelProxy:   map[string]bool{},
		localReported: map[string]bool{},
	}
	if o.Observer != nil {
		c.events = newNotifier(o.Observer)
	}
	return c
}

// lastRemotePortForTest returns the remote port from the last successful registration (test helper).
func (c *Client) lastRemotePortForTest() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRemotePort
}

// Registered reports whether at least one tunnel is currently registered.
func (c *Client) Registered() bool { return c.registered.Load() }

func (c *Client) resetRegisteredForTest() { c.registered.Store(false) }

// Run connects and keeps reconnecting until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	if c.events != nil {
		defer c.events.start()()
	}
	if len(c.opts.Forwards) > 0 {
		if err := c.listenForwards(); err != nil {
			return err
		}
		defer c.closeForwards()
	}
	for {
		c.events.emit(func(o Observer) { o.State(StateConnecting, "", 0) })
		err := c.connectOnce(ctx)
		if c.forwardsDone() {
			c.registered.Store(false)
			return c.fwdErrValue()
		}
		var mismatch *PinMismatchError
		if errors.As(err, &mismatch) && ctx.Err() == nil {
			// Trying again cannot make the certificate the pinned one.
			c.registered.Store(false)
			return err
		}
		var notApplied *AccessNotAppliedError
		if errors.As(err, &notApplied) && ctx.Err() == nil {
			// Never tried again, whatever the caller asked for: every attempt
			// would leave the service open.
			c.registered.Store(false)
			return err
		}
		if c.opts.StopOnRefusal && ctx.Err() == nil {
			var re *RefusedError
			if errors.As(err, &re) && re.Final() {
				c.registered.Store(false)
				return err
			}
		}
		if err != nil && ctx.Err() == nil {
			c.log.Warn("connection ended", "err", err)
		}
		c.registered.Store(false)
		wait := c.bo.NextBackOff()
		if ctx.Err() == nil {
			detail := ""
			if err != nil {
				detail = err.Error()
			}
			c.events.emit(func(o Observer) { o.State(StateReconnecting, detail, wait) })
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.forwardsEnded():
			return c.fwdErrValue()
		case <-time.After(wait):
		}
	}
}

// forwardsDone reports whether every forward of a run without tunnels has
// stopped.
func (c *Client) forwardsDone() bool {
	if c.fwdEnd == nil {
		return false
	}
	select {
	case <-c.fwdEnd:
		return true
	default:
		return false
	}
}

func (c *Client) fwdErrValue() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fwdErr
}

func (c *Client) connectOnce(ctx context.Context) error {
	conn, err := dialControl(ctx, &c.opts)
	if err != nil {
		return err
	}
	defer conn.Close()

	var caps []string
	if c.opts.RequestSummaries && c.events != nil {
		caps = []string{proto.CapRequestSummaries}
	}
	ar, err := authenticate(conn, c.opts.Token, caps)
	if err != nil {
		return err
	}
	if !ar.OK {
		return newRefused(stageAuth, ar.Code, ar.Error)
	}
	// The tunnels of the session before are gone, and their counts with them.
	c.mu.Lock()
	c.live = map[string]*liveTunnel{}
	c.mu.Unlock()
	c.log.Info("connected", "session_id", ar.SessionID)
	c.events.emit(func(o Observer) { o.State(StateConnected, "", 0) })
	info := SessionInfo{RelayVersion: ar.RelayVersion}
	c.events.emit(func(o Observer) {
		if so, ok := o.(SessionObserver); ok {
			so.Session(info)
		}
	})

	// yamux.DefaultConfig has EnableKeepAlive=true, KeepAliveInterval=30s.
	// Dead-peer detection relies on this keepalive; do not override it.
	ysess, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		return err
	}
	defer ysess.Close()
	// The relay takes the first stream of a session as the control stream,
	// also of a client that only forwards.
	ctrl, err := ysess.OpenStream()
	if err != nil {
		return err
	}
	defer ctrl.Close()
	if len(c.fwds) > 0 {
		if !slices.Contains(ar.Capabilities, proto.CapForward) {
			c.stopForwards(msgRelayTooOld, func(f *forwarder) error { return &ForwardUnsupportedError{Target: f.spec.Target} })
		} else {
			c.setForwardSession(ysess)
			defer c.setForwardSession(nil)
		}
	}
	tcpAccess := slices.Contains(ar.Capabilities, proto.CapTCPAccess)
	var env proto.Envelope
	// A refusal that will not change (see RefusedError.Final) ends its own
	// tunnel: it is logged and skipped, and the others are served. Only a
	// session in which every tunnel was refused so ends, with the first
	// refusal. Any other refusal ends the session as it always has, so that
	// the reconnect tries every tunnel again.
	var firstFinal error
	served := 0
	skip := func(name string, err error) {
		if firstFinal == nil {
			firstFinal = err
		}
		c.events.emit(func(o Observer) {
			if ro, ok := o.(RefusalObserver); ok {
				ro.Refused(name, err)
			}
		})
	}
	for _, tn := range c.opts.Tunnels {
		// A wish for ip or private that the relay cannot apply is never
		// sent: an older relay would open a public port for it (D9).
		// The default for a new service counts here as a wish: an older
		// relay would create the service open.
		want := tn.Access
		var accessDefault string
		if tn.Type == "tcp" {
			accessDefault = tn.AccessDefault
			if want == "" {
				want = accessDefault
			}
		}
		restricted := tn.Type == "tcp" && want != "" && want != "open"
		if restricted && !tcpAccess {
			ae := &AccessNotAppliedError{Name: tn.Name, Access: want, Type: "tcp", NotSent: true}
			c.log.Error("tunnel not registered: the relay cannot apply its access mode", "name", tn.Name, "access", want)
			skip(tn.Name, ae)
			continue
		}
		if err := proto.WriteMessage(ctrl, proto.MsgTunnelRegister, proto.TunnelRegister{
			Name: tn.Name, Type: tn.Type, RemotePort: tn.RemotePort, LocalAddr: tn.LocalAddr,
			Slug: tn.Slug, Access: tn.Access, AccessDefault: accessDefault, ProxyProtocol: tcpProxyProtocol(tn),
		}); err != nil {
			return err
		}
		if err := proto.ReadFrame(ctrl, &env); err != nil {
			return err
		}
		var rr proto.TunnelRegisterResponse
		if env.Type != proto.MsgTunnelRegisterResp || proto.DecodePayload(env, &rr) != nil {
			return fmt.Errorf("register failed: %s", rr.Error)
		}
		if !rr.OK {
			re := newRefused(stageRegister, rr.Code, rr.Error)
			if !re.Final() {
				return re
			}
			c.log.Warn("tunnel refused", "name", tn.Name, "code", re.Code, "err", re.Message)
			skip(tn.Name, re)
			continue
		}
		reg := RegisteredTunnel{
			TunnelID: rr.TunnelID, Name: tn.Name, Type: tn.Type, LocalAddr: tn.LocalAddr,
			AccessMode: rr.AccessMode, Created: rr.Created, DashboardURL: rr.DashboardURL, GatewayOnly: rr.GatewayOnly,
		}
		if tn.Type == "tcp" {
			reg.Wished = tn.Access
			if tn.Access == "" {
				reg.Default = accessDefault
			}
		}
		for _, name := range rr.Ignored {
			switch name {
			case "slug":
				reg.Ignored.Slug = true
			case "access":
				reg.Ignored.Access = true
			}
		}
		// A mode less strict than the wish, or than the default of a service
		// the relay says it created, is a relay that did not do as asked (it
		// serves the stricter of wish and stored mode, or refuses).
		looser := tn.Type == "tcp" && rr.AccessMode != "" &&
			((tn.Access != "" && tcpAccessRank(rr.AccessMode) < tcpAccessRank(tn.Access)) ||
				(tn.Access == "" && rr.Created && accessDefault != "" && tcpAccessRank(rr.AccessMode) < tcpAccessRank(accessDefault)))
		if (restricted && (rr.AccessMode == "" || reg.Ignored.Access)) || looser {
			// The relay said it applies access modes to tcp and then did not
			// confirm this one: its port may be public. Returning closes the
			// session before a visitor can be sent.
			ae := &AccessNotAppliedError{Name: tn.Name, Access: want, Type: "tcp", RemotePort: rr.RemotePort}
			if n := len(c.opts.Tunnels); n > 1 {
				ae.Services = n
			}
			return ae
		}
		served++
		// A relay that knows slug and access says what the access mode is,
		// or that it ignored them. One that says neither is older: it never
		// saw the wishes and made, or found, the service its own way.
		if tn.Type == "http" && rr.AccessMode == "" && !reg.Ignored.Any() {
			if tn.Access != "" && tn.Access != "open" {
				// Such a relay creates a service open to everyone. Serving it
				// would give the person the opposite of what was asked for:
				// returning closes the session before a visitor can be sent.
				public := rr.URL
				if public == "" {
					public = rr.Hostname
				}
				ae := &AccessNotAppliedError{Name: tn.Name, Access: tn.Access, URL: public}
				if n := len(c.opts.Tunnels); n > 1 {
					// The session ends for all of them, not for this one alone.
					ae.Services = n
				}
				return ae
			}
			reg.SlugUnacknowledged = tn.Slug != ""
		}
		if tn.Type == "http" {
			// Older relays report only the hostname; keep printing that.
			tunnelURL := rr.URL
			if tunnelURL == "" {
				tunnelURL = rr.Hostname
			}
			c.log.Info("tunnel registered", "name", tn.Name, "tunnel_id", rr.TunnelID, "url", tunnelURL)
			reg.URL = tunnelURL
		} else {
			args := []any{"tunnel_id", rr.TunnelID, "remote_port", rr.RemotePort}
			if rr.AccessMode != "" {
				// A relay with tcp services (proto.CapTCPAccess) says more.
				args = append(args, "name", tn.Name, "access_mode", rr.AccessMode, "slug", rr.Slug)
			}
			c.log.Info("tunnel registered", args...)
			reg.RemotePort, reg.Slug = rr.RemotePort, rr.Slug
		}
		c.events.emit(func(o Observer) { o.Registered(reg) })
		c.mu.Lock()
		c.tunnelLocal[rr.TunnelID] = tn.LocalAddr
		c.tunnelProxy[rr.TunnelID] = tcpProxyProtocol(tn) == proto.ProxyProtocolV2
		c.live[rr.TunnelID] = &liveTunnel{http: tn.Type == "http"}
		c.lastRemotePort = rr.RemotePort
		c.mu.Unlock()
	}
	if served == 0 && firstFinal != nil && c.fwdLiveCount() == 0 {
		return firstFinal
	}
	c.registered.Store(true)
	// Reset backoff only after auth AND all tunnels are registered so that a
	// partial failure (auth ok but registration rejected) keeps the accumulated
	// delay and does not cause a tight-retry storm (B14).
	c.bo.Reset()

	go c.pingLoop(ctx, ctrl)
	readErr := make(chan error, 1)
	go func() { readErr <- c.controlReadLoop(ysess, ctrl) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ysess.CloseChan():
		return fmt.Errorf("session closed")
	case err := <-readErr:
		return err
	case <-c.forwardsEnded():
		return c.fwdErrValue()
	}
}

func (c *Client) fwdLiveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fwdLive
}

func (c *Client) pingLoop(ctx context.Context, ctrl *yamux.Stream) {
	ping := func() error {
		// The time is taken before the write, so that the answer cannot be
		// read before it is set.
		c.mu.Lock()
		c.pingSent = time.Now()
		c.mu.Unlock()
		return proto.WriteMessage(ctrl, proto.MsgPing, proto.Ping{Nonce: "hb"})
	}
	c.mu.Lock()
	c.pingSent = time.Time{} // a ping of an earlier session gets no answer
	c.mu.Unlock()
	// An observer shows the round-trip time, so it gets one measurement at
	// once instead of after the first interval.
	if c.events != nil {
		if err := ping(); err != nil {
			return
		}
	}
	t := time.NewTicker(c.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ping(); err != nil {
				return
			}
		}
	}
}

// pongReceived reports the round-trip time of the ping that was answered.
func (c *Client) pongReceived() {
	c.mu.Lock()
	sent := c.pingSent
	c.pingSent = time.Time{}
	c.mu.Unlock()
	if sent.IsZero() {
		return
	}
	rtt := time.Since(sent)
	c.events.emit(func(o Observer) { o.Latency(rtt) })
}

// localTarget tells the observer whether the local service at addr took a
// visitor connection, when that differs from what it was told last.
func (c *Client) localTarget(addr string, reachable bool) {
	if c.events == nil {
		return
	}
	c.mu.Lock()
	last, known := c.localReported[addr]
	c.localReported[addr] = reachable
	c.mu.Unlock()
	if known && last == reachable {
		return
	}
	c.events.emit(func(o Observer) { o.LocalTarget(addr, reachable) })
}

func (c *Client) controlReadLoop(sess *yamux.Session, ctrl io.Reader) error {
	for {
		var env proto.Envelope
		if err := proto.ReadFrame(ctrl, &env); err != nil {
			return err
		}
		switch env.Type {
		case proto.MsgNewConnection:
			var nc proto.NewConnection
			if proto.DecodePayload(env, &nc) != nil {
				continue
			}
			c.mu.Lock()
			local := c.tunnelLocal[nc.TunnelID]
			ownProxy := c.tunnelProxy[nc.TunnelID]
			c.mu.Unlock()
			if local == "" {
				continue
			}
			go c.handleNewConnection(sess, nc, local, ownProxy || nc.ProxyProtocol == proto.ProxyProtocolV2)
		case proto.MsgRequestSummary:
			c.requestSummary(env)
		case proto.MsgPong, proto.MsgError:
			// Pong is informational only. Dead-peer detection is provided by
			// yamux's built-in keepalive (EnableKeepAlive=true, KeepAliveInterval=30s
			// in yamux.DefaultConfig). The Ping/Pong messages are retained as a
			// lightweight application-level liveness signal; yamux keepalive is the
			// authoritative liveness mechanism.
			if env.Type == proto.MsgError {
				c.log.Warn("server error message")
			} else {
				c.pongReceived()
			}
		default:
			// A message of a relay newer than this client. It is passed over
			// without a word: a relay may only send what a client can do
			// without, and a line in the log for each would help nobody.
		}
	}
}

// requestSummary hands a request summary to the observer. Nothing here waits:
// this is the goroutine that reads the control stream.
//
// A summary is shown only when this client asked for summaries, and only for
// an http tunnel of the current session. One that cannot be read, or that
// carries no time, no method or no status of an HTTP answer, is dropped.
// Method and path are bounded and made printable again here, whatever the
// relay did: they end up on a terminal.
func (c *Client) requestSummary(env proto.Envelope) {
	if c.events == nil || !c.opts.RequestSummaries {
		return
	}
	var s proto.RequestSummary
	if proto.DecodePayload(env, &s) != nil || s.Method == "" || s.Status < 100 || s.Status > 599 {
		return
	}
	at, err := time.Parse(time.RFC3339, s.Time)
	if err != nil {
		return
	}
	c.mu.Lock()
	tn := c.live[s.TunnelID]
	c.mu.Unlock()
	if tn == nil || !tn.http {
		return
	}
	id, method, path, status := s.TunnelID, proto.SummaryMethod(s.Method), proto.SummaryPath(s.Path), s.Status
	at = at.Local()
	c.events.emitLine(func(o Observer) { o.Request(id, at, method, path, status) })
}

// countConnection adds d (1 or -1) to the open visitor connections of a
// tunnel of the current session and tells the observer the new counts. The
// end of a connection of an earlier session changes nothing: that session's
// counts are gone.
func (c *Client) countConnection(tunnelID string, d int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tn := c.live[tunnelID]
	if tn == nil {
		return
	}
	if tn.open += d; tn.open < 0 {
		tn.open = 0
	}
	if d > 0 {
		tn.total++
	}
	// Handed over under the lock the numbers were made under: otherwise two
	// connections that end at the same moment could deliver theirs the other
	// way round, and the older numbers would be the last the observer hears.
	// The notifier takes its own lock for a few instructions and never calls
	// out while it holds it, and nothing takes that lock before this one.
	c.events.counts(tunnelID, tn.open, tn.total)
}
