// Package client implements the burrow control client.
package client

import (
	"context"
	"crypto/x509"
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
	tunnelLocal    map[string]string // tunnelID → localAddr
	lastRemotePort int
	pingSent       time.Time       // when the ping that is still unanswered was sent
	localReported  map[string]bool // localAddr → what the observer was last told
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

	ar, err := authenticate(conn, c.opts.Token)
	if err != nil {
		return err
	}
	if !ar.OK {
		return fmt.Errorf("auth failed: %s", ar.Error)
	}
	c.log.Info("connected", "session_id", ar.SessionID)
	c.events.emit(func(o Observer) { o.State(StateConnected, "", 0) })

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
		}); err != nil {
			return err
		}
		if err := proto.ReadFrame(ctrl, &env); err != nil {
			return err
		}
		var rr proto.TunnelRegisterResponse
		if env.Type != proto.MsgTunnelRegisterResp || proto.DecodePayload(env, &rr) != nil || !rr.OK {
			return fmt.Errorf("register failed: %s", rr.Error)
		}
		reg := RegisteredTunnel{TunnelID: rr.TunnelID, Name: tn.Name, Type: tn.Type, LocalAddr: tn.LocalAddr}
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
		}
	}
}
