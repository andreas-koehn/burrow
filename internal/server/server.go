package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
)

// TokenAuthenticator validates a client's plaintext token, returning its user id.
type TokenAuthenticator interface {
	Authenticate(ctx context.Context, token string) (userID string, err error)
}

// ServiceResolver maps a (user, tunnel-name, type) triple to a stable service
// identity and an assigned subdomain, creating the service record and
// generating/persisting the subdomain on first use. Collision retry (when two
// concurrent registrations race for the same random subdomain) is the
// responsibility of the concrete adapter (e.g. the serviceResolverAdapter in
// cmd/server that wraps the store). The server calls Resolve exactly once per
// http-tunnel registration and trusts the returned subdomain — it does NOT retry.
type ServiceResolver interface {
	Resolve(ctx context.Context, userID, name, typ string) (serviceID, subdomain string, err error)
}

// ResolveOptions are what a client wishes for an http service that does not
// exist yet. Both are optional; Access is a relay access mode.
type ResolveOptions struct{ Slug, Access string }

// Resolved is the service an http tunnel was bound to.
type Resolved struct {
	ServiceID, Slug, AccessMode string
	// Created says that this call created the service.
	Created bool
	// Ignored lists, sorted, which of "access" and "slug" were asked for and
	// not applied because the service existed with other values.
	Ignored []string
}

// OptionsResolver is a ServiceResolver that can create a service with a
// chosen slug and access mode. The options apply only when the service is
// created; an existing service is returned unchanged, with Ignored saying
// what was left out. It validates what it is given: the control loop passes
// the client's values on as they came. A refusal the client can act on is a
// *RefusalError.
type OptionsResolver interface {
	ResolveWithOptions(ctx context.Context, userID, name, typ string, o ResolveOptions) (Resolved, error)
}

// RefusalError is a refusal with one of the proto.Code… values. Message is
// sent to the client as it is, so it holds nothing internal.
type RefusalError struct{ Code, Message string }

func (e *RefusalError) Error() string { return e.Message }

// TunnelStore persists tunnel rows (best-effort; never blocks the data path).
// Implementations MUST be fast and non-blocking: SaveTunnel is called inline on
// the serial control loop, and MarkTunnelSeen is called at session/tunnel
// teardown to record the last-observed state (it is NOT a liveness heartbeat).
type TunnelStore interface {
	SaveTunnel(ctx context.Context, userID string, t *Tunnel) error
	MarkTunnelSeen(ctx context.Context, tunnelID string) error
}

// AuthFunc adapts a function to TokenAuthenticator.
type AuthFunc func(ctx context.Context, token string) (string, error)

// Authenticate implements TokenAuthenticator.
func (f AuthFunc) Authenticate(ctx context.Context, token string) (string, error) {
	return f(ctx, token)
}

// noopTunnelStore is the default TunnelStore: it persists nothing.
type noopTunnelStore struct{}

func (noopTunnelStore) SaveTunnel(context.Context, string, *Tunnel) error { return nil }
func (noopTunnelStore) MarkTunnelSeen(context.Context, string) error      { return nil }

// EventPublisher receives "this user's tunnels changed" notifications
// (best-effort, must never block the control loop). *events.Bus satisfies it.
type EventPublisher interface {
	PublishTunnelsChanged(userID string)
}

type noopEventPublisher struct{}

func (noopEventPublisher) PublishTunnelsChanged(string) {}

// TunnelView is a read-only snapshot of a live tunnel for the HTTP API.
type TunnelView struct {
	ID         string
	Name       string
	Type       string
	RemotePort int
	LocalAddr  string
	BytesIn    uint64
	BytesOut   uint64
	Connected  bool
	// ServiceID is the stable durable-service identity for http tunnels
	// (empty for tcp tunnels). HTTP tunnels are auto-promoted to a persisted
	// services row; the UI needs that ID — not the per-session tunnel.ID —
	// for per-service routes (/services/{id}/access-mode, /api-keys, etc.).
	ServiceID string
	// Hostname is the routable FQDN for http tunnels (subdomain.AuthDomain),
	// empty for tcp tunnels and for http tunnels when Options.AuthDomain is
	// unset. Surfaced to the dashboard so the Tunnels list can show users a
	// click-to-copy endpoint instead of a bare subdomain.
	Hostname string
	// AccessMode is the per-service access mode in effect on the live
	// http-tunnel registration ("open"/"api_key"/"burrow_login"/"mtls"),
	// empty for tcp tunnels or when no ServiceResolver was wired. The
	// dashboard renders an "Open" badge as the default when this is empty.
	AccessMode string
}

// ControlSessionSink is the interface the server uses to record a
// kind=control connection-log entry on yamux session close. The concrete
// *connlog.SQLSink satisfies it via a thin cmd/server adapter. Declared here
// (not in internal/connlog) to keep the server package import-free of
// connlog on the data-plane path.
//
// Full instrumentation is wired by Task 17 (cmd/server). In v0.5.0 Task 8
// this field is declared in Options so Task 17 can inject the sink without
// changes to this file.
type ControlSessionSink interface {
	RecordControl(ctx context.Context, sessionID, userID, remoteAddr string, startedAt, endedAt time.Time)
}

// Options configures a Server.
type Options struct {
	Listen     string
	TLSCert    string
	TLSKey     string
	Auth       TokenAuthenticator
	Tunnels    TunnelStore
	Events     EventPublisher
	Logger     *slog.Logger
	PublicBind string
	PortMin    int
	PortMax    int
	// Services resolves http-tunnel service identity + subdomain. If nil,
	// http tunnel registrations are rejected with a clear error.
	Services ServiceResolver
	// AuthDomain is the base domain appended to a subdomain to form the
	// routable Hostname returned in TunnelRegisterResponse (e.g. "tunnels.example.com"
	// → hostname "k7p2qx.tunnels.example.com"). When empty, Hostname is returned
	// as "" (degraded mode — subdomain is still assigned on the tunnel).
	AuthDomain string
	// ConnLog is the optional connection-log sink for control sessions.
	// When non-nil, handleConn emits a kind=control entry on yamux session
	// close. Wired by Task 17 (cmd/server); nil means no logging.
	ConnLog ControlSessionSink
	// GetCertificate, when non-nil, supplies the control-channel certificate
	// (e.g. from the ACME manager) instead of loading TLSCert/TLSKey files.
	// Only GetCertificate is used (no TLS-ALPN NextProtos) since the control
	// channel speaks yamux, not an ACME challenge protocol.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// MinClientVersion is the oldest client version let in (MAJOR.MINOR.PATCH,
	// a leading v allowed). Empty lets every client in, and so does a client
	// that names no version or one that cannot be compared.
	MinClientVersion string
}

// Server is the burrowd relay control server.
type Server struct {
	opts  Options
	log   *slog.Logger
	reg   *Registry
	tlsC  *tls.Config
	ports *portAllocator

	mu sync.Mutex
	ln net.Listener
	wg sync.WaitGroup
}

// New validates options and loads the TLS keypair.
func New(o Options) (*Server, error) {
	if o.Auth == nil {
		return nil, fmt.Errorf("server: Auth (TokenAuthenticator) is required")
	}
	if o.Tunnels == nil {
		o.Tunnels = noopTunnelStore{}
	}
	if o.Events == nil {
		o.Events = noopEventPublisher{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PublicBind == "" {
		o.PublicBind = "0.0.0.0"
	}
	if o.PortMin == 0 {
		o.PortMin = 9000
	}
	if o.PortMax == 0 {
		o.PortMax = 9100
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.GetCertificate != nil {
		tlsCfg.GetCertificate = o.GetCertificate
	} else {
		cert, err := tls.LoadX509KeyPair(o.TLSCert, o.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("load tls keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	s := &Server{
		opts: o, log: o.Logger, reg: NewRegistry(),
		tlsC: tlsCfg,
	}
	s.ports = newPortAllocator(o.PortMin, o.PortMax)
	return s, nil
}

// Addr returns the bound listen address ("" until listening).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Wait blocks until all connection goroutines have exited.
func (s *Server) Wait() { s.wg.Wait() }

// Serve listens until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := tls.Listen("tcp", s.opts.Listen, s.tlsC)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.opts.Listen, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.log.Info("control server listening", "addr", ln.Addr().String())

	go func() { <-ctx.Done(); _ = ln.Close() }()

	s.wg.Add(1)
	go s.byteTicker(ctx)

	// B11: capped exponential backoff on transient accept errors (EMFILE etc.)
	// mirrors the net/http tempDelay pattern. Exits promptly on ctx cancel.
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				s.wg.Wait()
				return nil
			default:
			}
			s.log.Warn("accept", "err", err)
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else {
				delay *= 2
			}
			if delay > time.Second {
				delay = time.Second
			}
			time.Sleep(delay)
			continue
		}
		delay = 0 // reset on success
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

// yamuxConfig returns the yamux session configuration used by the server.
// Dead-peer detection relies on yamux's built-in keepalive; callers MUST NOT
// disable EnableKeepAlive (a CI test in keepalive_test.go enforces this).
func yamuxConfig() *yamux.Config {
	return yamux.DefaultConfig()
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	sid := uuid.NewString()
	cs, err := HandleHandshakeWith(conn, s.opts.Auth, sid, HandshakeOptions{MinClientVersion: s.opts.MinClientVersion})
	if err != nil {
		s.log.Warn("handshake failed", "remote_addr", conn.RemoteAddr().String(), "err", err)
		return
	}
	ysess, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		s.log.Warn("yamux server", "err", err)
		return
	}
	defer ysess.Close()
	cs.Yamux = ysess
	s.reg.AddSession(cs)
	defer s.reg.RemoveSession(cs)
	s.log.Info("client authenticated", "session_id", cs.SessionID, "remote_addr", cs.RemoteAddr)

	defer func() {
		for _, tn := range s.reg.snapshotTunnels(cs) {
			if tn.Listener != nil {
				_ = tn.Listener.Close()
			}
			s.ports.Release(tn.RemotePort)
			_ = s.opts.Tunnels.MarkTunnelSeen(context.Background(), tn.ID)
		}
		if cs.UserID != "" {
			s.opts.Events.PublishTunnelsChanged(cs.UserID)
		}
	}()

	ctrl, err := ysess.AcceptStream()
	if err != nil {
		return
	}
	cs.SetControl(ctrl)
	if cs.WantsRequestSummaries() {
		// Ends with the session: closing it ends a write that waits, too.
		go cs.runSummaries(ysess.CloseChan())
	}
	go func() {
		for {
			st, e := ysess.AcceptStream()
			if e != nil {
				return
			}
			go s.handleDataStream(cs, st)
		}
	}()
	go s.heartbeat(ctx, ysess, cs)
	s.RunControlLoop(ctrl, s.reg, cs)
	if n := cs.SummariesDropped(); n > 0 {
		s.log.Debug("request summaries dropped for a slow client", "session_id", cs.SessionID, "dropped", n)
	}
	s.log.Info("client disconnected", "session_id", cs.SessionID)
}

// heartbeat closes the yamux session when ctx is cancelled; the session's own
// keepalive (EnableKeepAlive=true, KeepAliveInterval=30s — see yamuxConfig) will
// close it autonomously if the peer goes silent, so no additional ping/pong
// deadline logic is needed here.
func (s *Server) heartbeat(ctx context.Context, y *yamux.Session, _ *ClientSession) {
	select {
	case <-ctx.Done():
		_ = y.Close()
	case <-y.CloseChan():
	}
}

// userByteSum returns the total in+out bytes summed across all tunnels of all
// sessions owned by userID, and whether the user has any live tunnels at all.
// It is a pure helper so the delta logic can be unit-tested independently.
func userByteSum(sessions []*ClientSession, reg *Registry, userID string) (sum uint64, hasTunnels bool) {
	for _, cs := range sessions {
		if cs.UserID != userID {
			continue
		}
		for _, tn := range reg.snapshotTunnels(cs) {
			sum += tn.BytesIn.Load() + tn.BytesOut.Load()
			hasTunnels = true
		}
	}
	return sum, hasTunnels
}

// byteTicker publishes a per-user "tunnels changed" ping ~1/s, but only when
// the total byte sum for that user has changed since the last publish. This
// avoids spurious SSE wakeups and dashboard refetches for idle tunnels (B15).
// It is WaitGroup-tracked and exits on ctx cancellation.
func (s *Server) byteTicker(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	// lastSum tracks the last-published total byte sum (in+out) per userID.
	// The map is cleaned up when a user's tunnels all disconnect, preventing leaks.
	lastSum := map[string]uint64{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sessions := s.reg.Sessions()
			// Collect distinct userIDs with live tunnels.
			activeUsers := map[string]struct{}{}
			for _, cs := range sessions {
				if cs.UserID != "" && len(s.reg.snapshotTunnels(cs)) > 0 {
					activeUsers[cs.UserID] = struct{}{}
				}
			}
			// Remove stale entries for users who no longer have live tunnels.
			for uid := range lastSum {
				if _, ok := activeUsers[uid]; !ok {
					delete(lastSum, uid)
				}
			}
			// Publish only when byte sum changed (or on first observation).
			for userID := range activeUsers {
				cur, _ := userByteSum(sessions, s.reg, userID)
				prev, known := lastSum[userID]
				if !known || cur != prev {
					lastSum[userID] = cur
					// Best-effort invalidate ping; a publish for a mid-teardown
					// session is harmless (client refetches and sees the update).
					s.opts.Events.PublishTunnelsChanged(userID)
				}
			}
		}
	}
}

// SessionSnapshot is a read-only view of one live client session for the
// clients overview (cmd/server adapts this to api.ClientView).
type SessionSnapshot struct {
	SessionID, UserID, RemoteAddr  string
	OS, Arch, ClientVersion, Token string
	Tunnels                        []TunnelView
}

// LookupSessionByTunnelID returns the sessionID and userID that own the tunnel
// with the given runtime ID, or ("", "", false) on miss. O(1) — probes the
// Registry's tunnel index (v0.5.2 BACKLOG #1). Replaces the
// O(N sessions x M tunnels-per-session) SnapshotSessions scan that
// cmd/server/proxy_wiring.go::lookupSessionFields ran per proxied request.
func (s *Server) LookupSessionByTunnelID(tunnelID string) (sessionID, userID string, ok bool) {
	cs, found := s.reg.SessionByTunnelID(tunnelID)
	if !found {
		return "", "", false
	}
	return cs.SessionID, cs.UserID, true
}

// hostnameFor returns the routable FQDN for an http tunnel
// (subdomain.AuthDomain) or "" for tcp tunnels / when AuthDomain is unset.
// Centralised so SnapshotSessions and ListUserTunnels stay in sync (P0-5).
func (s *Server) hostnameFor(tn *Tunnel) string {
	if !tn.IsHTTP || tn.Subdomain == "" || s.opts.AuthDomain == "" {
		return ""
	}
	return tn.Subdomain + "." + s.opts.AuthDomain
}

// SnapshotSessions returns all live client sessions with their tunnels.
func (s *Server) SnapshotSessions() []SessionSnapshot {
	var out []SessionSnapshot
	for _, cs := range s.reg.Sessions() {
		ss := SessionSnapshot{
			SessionID: cs.SessionID, UserID: cs.UserID, RemoteAddr: cs.RemoteAddr,
			OS: cs.OS, Arch: cs.Arch, ClientVersion: cs.ClientVersion, Token: cs.TokenName,
		}
		for _, tn := range s.reg.snapshotTunnels(cs) {
			ss.Tunnels = append(ss.Tunnels, TunnelView{
				ID: tn.ID, Name: tn.Name, Type: tn.Type, RemotePort: tn.RemotePort,
				LocalAddr: tn.LocalAddr, BytesIn: tn.BytesIn.Load(), BytesOut: tn.BytesOut.Load(),
				Connected: true,
				ServiceID: tn.ServiceID,
				Hostname:  s.hostnameFor(tn),
			})
		}
		out = append(out, ss)
	}
	return out
}

// ListUserTunnels returns a snapshot of the live tunnels owned by userID.
func (s *Server) ListUserTunnels(userID string) []TunnelView {
	var out []TunnelView
	for _, cs := range s.reg.Sessions() {
		if cs.UserID != userID {
			continue
		}
		for _, tn := range s.reg.snapshotTunnels(cs) {
			out = append(out, TunnelView{
				ID: tn.ID, Name: tn.Name, Type: tn.Type, RemotePort: tn.RemotePort,
				LocalAddr: tn.LocalAddr, BytesIn: tn.BytesIn.Load(), BytesOut: tn.BytesOut.Load(),
				Connected: true, ServiceID: tn.ServiceID,
				Hostname: s.hostnameFor(tn),
			})
		}
	}
	return out
}

// LookupHTTPTunnel finds the live http tunnel with the given subdomain.
// It iterates all sessions and their tunnels under a per-session mutex-safe
// snapshot and returns the first match where t.IsHTTP && t.Subdomain == sub.
// Subdomains are unique (enforced by ServiceResolver collision retry), so the
// first match is always the correct one. Returns (nil, false) when not found.
func (s *Server) LookupHTTPTunnel(sub string) (*Tunnel, bool) {
	for _, cs := range s.reg.Sessions() {
		for _, tn := range s.reg.snapshotTunnels(cs) {
			if tn.IsHTTP && tn.Subdomain == sub {
				return tn, true
			}
		}
	}
	return nil, false
}

// HTTPTunnels returns a snapshot slice of all live http tunnels across all
// sessions. The slice is a copy collected under per-session locks so callers
// may iterate it safely after this call returns.
func (s *Server) HTTPTunnels() []*Tunnel {
	var out []*Tunnel
	for _, cs := range s.reg.Sessions() {
		for _, tn := range s.reg.snapshotTunnels(cs) {
			if tn.IsHTTP {
				out = append(out, tn)
			}
		}
	}
	return out
}

// LookupHTTPTunnelByServiceID finds the live http tunnel with the given
// serviceID. Returns (nil, false) when not found. Used by the custom-domain
// routing path (v0.5.0 Task 7) where the request Host is not a subdomain of
// authDomain.
func (s *Server) LookupHTTPTunnelByServiceID(serviceID string) (*Tunnel, bool) {
	for _, cs := range s.reg.Sessions() {
		for _, tn := range s.reg.snapshotTunnels(cs) {
			if tn.IsHTTP && tn.ServiceID == serviceID {
				return tn, true
			}
		}
	}
	return nil, false
}
