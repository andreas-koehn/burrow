// Package client implements the burrow control client.
package client

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	Slug   string
	Access string
}

// Options configures a Client.
type Options struct {
	Server     string
	Token      string
	Insecure   bool
	RootCAs    *x509.CertPool
	ServerName string
	Tunnels    []TunnelSpec
	Logger     *slog.Logger
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
	live           map[string]*liveTunnel // the tunnels of the current session
	lastRemotePort int
	pingSent       time.Time       // when the ping that is still unanswered was sent
	localReported  map[string]bool // localAddr → what the observer was last told
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
	for {
		c.events.emit(func(o Observer) { o.State(StateConnecting, "", 0) })
		err := c.connectOnce(ctx)
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
		case <-time.After(wait):
		}
	}
}

func (c *Client) connectOnce(ctx context.Context) error {
	conn, err := dialControl(ctx, c.opts)
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
	ctrl, err := ysess.OpenStream()
	if err != nil {
		return err
	}
	defer ctrl.Close()
	var env proto.Envelope
	for _, tn := range c.opts.Tunnels {
		if err := proto.WriteMessage(ctrl, proto.MsgTunnelRegister, proto.TunnelRegister{
			Name: tn.Name, Type: tn.Type, RemotePort: tn.RemotePort, LocalAddr: tn.LocalAddr,
			Slug: tn.Slug, Access: tn.Access,
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
			return newRefused(stageRegister, rr.Code, rr.Error)
		}
		reg := RegisteredTunnel{
			TunnelID: rr.TunnelID, Name: tn.Name, Type: tn.Type, LocalAddr: tn.LocalAddr,
			AccessMode: rr.AccessMode, Created: rr.Created, DashboardURL: rr.DashboardURL,
		}
		for _, name := range rr.Ignored {
			switch name {
			case "slug":
				reg.Ignored.Slug = true
			case "access":
				reg.Ignored.Access = true
			}
		}
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
			c.log.Info("tunnel registered", "tunnel_id", rr.TunnelID, "remote_port", rr.RemotePort)
			reg.RemotePort = rr.RemotePort
		}
		c.events.emit(func(o Observer) { o.Registered(reg) })
		c.mu.Lock()
		c.tunnelLocal[rr.TunnelID] = tn.LocalAddr
		c.live[rr.TunnelID] = &liveTunnel{http: tn.Type == "http"}
		c.lastRemotePort = rr.RemotePort
		c.mu.Unlock()
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
	}
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
			c.mu.Unlock()
			if local == "" {
				continue
			}
			go c.handleNewConnection(sess, nc, local)
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
	tn := c.live[tunnelID]
	if tn == nil {
		c.mu.Unlock()
		return
	}
	if tn.open += d; tn.open < 0 {
		tn.open = 0
	}
	if d > 0 {
		tn.total++
	}
	open, total := tn.open, tn.total
	c.mu.Unlock()
	c.events.counts(tunnelID, open, total)
}
