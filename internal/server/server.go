package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proto"
)

// TokenAuthenticator validates a client's plaintext token, returning its user id.
// An error that matches ErrTokenInvalid says the token itself is not valid
// (unknown, expired, of a suspended user): the control port counts it as a
// failed sign-in of the source. Any other error (the store failed) is not
// counted.
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

// ResolveOptions are what a client wishes for a service that does not exist
// yet. All are optional; Access is a relay access mode. AccessDefault (tcp
// only) is the mode a service created without Access gets; a service that
// exists keeps its stored mode.
type ResolveOptions struct{ Slug, Access, AccessDefault string }

// Resolved is the service an http tunnel was bound to.
type Resolved struct {
	ServiceID, Slug, AccessMode string
	// Created says that this call created the service.
	Created bool
	// GatewayOnly says that the service has no direct address: it answers
	// through the AI gateway only, so no URL is reported for it.
	GatewayOnly bool
	// Ignored lists, sorted, which of "access" and "slug" were asked for and
	// not applied because the service existed with other values.
	Ignored []string
	// TCPPort is the public port a tcp service holds (TCPServiceResolver);
	// 0 when it holds none. TCPPortPinned: the client asked for that port.
	TCPPort       int
	TCPPortPinned bool
	// TCPPortLimit is how many ports the owner may hold; 0 is no limit (an
	// administrator, or a relay without one).
	TCPPortLimit int
	// MaxConnections is a tcp service's cap on connected visitors; 0 is the
	// relay's default (DefaultTCPMaxConnections).
	MaxConnections int
}

// TCPPortHold is a service's hold on a public port.
type TCPPortHold struct {
	ServiceID, UserID string
	// Pinned: the client asked for the port. An automatic hold of an offline
	// service may be taken over; a pinned one only goes with the service.
	Pinned bool
	// LastSeen is when a client last served the service; zero: never.
	LastSeen time.Time
}

// ErrTokenInvalid marks an authenticator's refusal of the token itself (see
// TokenAuthenticator).
var ErrTokenInvalid = errors.New("invalid client token")

// ErrTCPPortTaken is what SetServiceTCPPort returns when another service
// holds the port (and it was not one that could be taken over).
var ErrTCPPortTaken = errors.New("tcp port is held by another service")

// OptionsResolver is a ServiceResolver that can create a service with a
// chosen slug and access mode. The options apply only when the service is
// created; an existing service is returned unchanged, with Ignored saying
// what was left out. It validates what it is given: the control loop passes
// the client's values on as they came. A refusal the client can act on is a
// *RefusalError.
type OptionsResolver interface {
	ResolveWithOptions(ctx context.Context, userID, name, typ string, o ResolveOptions) (Resolved, error)
}

// TCPServiceResolver binds tcp tunnels to durable services and keeps the
// public port of each. When Options.Services is one, a tcp registration
// resolves (or creates) the service of its name, and a reconnect that asks
// for no port gets the port the service held; without it a tcp tunnel has no
// service, as before.
type TCPServiceResolver interface {
	// ResolveTCP returns the tcp service of the name, creating it when there
	// is none, in mode o.Access (open when empty). A name that belongs to a
	// service of another type is a *RefusalError with proto.CodeNameInUse; an
	// access that is no tcp mode one with proto.CodeAccessInvalid. An
	// existing service keeps its mode: Resolved.AccessMode is the mode it
	// has. Resolved.TCPPort is the port the service holds.
	ResolveTCP(ctx context.Context, userID, name string, o ResolveOptions) (Resolved, error)
	// TCPPortReservations maps every port a service holds to the hold.
	TCPPortReservations(ctx context.Context) (map[int]TCPPortHold, error)
	// SetServiceTCPPort makes port the one the service holds, pinned or
	// not. takeFrom, when set, is the service whose automatic hold on the
	// port is taken over in the same step. ErrTCPPortTaken when another
	// service holds the port.
	SetServiceTCPPort(ctx context.Context, serviceID string, port int, pinned bool, takeFrom string) error
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
	// ServiceID is the stable durable-service identity of the tunnel (empty
	// for a tcp tunnel of a relay without tcp services). Tunnels are
	// auto-promoted to a persisted services row; the UI needs that ID — not the per-session tunnel.ID —
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

// ConnLogEntry is one connection-log row of a connection the control server
// carries itself: a visitor of a tcp tunnel. Kind and Status are the
// observe.Traffic… and observe.Outcome… values, which are the kinds and
// statuses of the connection log.
type ConnLogEntry struct {
	Kind            string
	ServiceID       string
	TunnelID        string
	UserID          string
	ClientSessionID string
	SourceIP        string
	StartedAt       time.Time
	EndedAt         time.Time
	BytesIn         int64
	BytesOut        int64
	Status          string
	Reason          string
	// TokenName is the client token name of the user who forwards (kind
	// tcp_forward); "" for a visitor.
	TokenName string
}

// ConnLogReasonNoStream is the reason of a visitor whose client did not open
// the stream for it in time.
const ConnLogReasonNoStream = "no_stream"

// ConnLogSink receives one entry per finished connection. It is called on the
// goroutine of the connection and must not wait for the database; a
// cmd/server adapter hands entries to *connlog.SQLSink, which writes in the
// background. Declared here to keep the server package import-free of
// connlog.
type ConnLogSink interface {
	RecordConnection(ctx context.Context, e ConnLogEntry)
}

// SessionRecorder remembers the last session of every client token, so that
// a client that is offline can still be shown. *store.Store satisfies it.
// It is called inline on the serial control loop, like TunnelStore.
type SessionRecorder interface {
	RecordClientSession(ctx context.Context, tokenID string, rec db.ClientSessionRecord) error
}

// ServiceRecorder remembers when a client last served a durable service and
// which client token it logged in with, so that a service nobody serves now
// can be told from one nobody ever served. *store.Store satisfies it. It is
// called inline on the serial control loop, like TunnelStore.
type ServiceRecorder interface {
	TouchServiceLastSeen(ctx context.Context, serviceID, tokenName string) error
}

// sessionRecordTimeout bounds one write of a session record or of a service
// stamp.
const sessionRecordTimeout = 2 * time.Second

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
	// ServicesDomain is BURROW_SERVICES_DOMAIN: when set, the public address
	// of an http tunnel is https://<ServicesDomain>/svc/<slug>/ instead of
	// one on AuthDomain. The dashboard link stays on AuthDomain.
	ServicesDomain string
	// ConnLog, when non-nil, gets one connection-log entry for every visitor
	// of a tcp tunnel when its connection ends. nil: nothing is logged.
	ConnLog ConnLogSink
	// GetCertificate, when non-nil, supplies the control-channel certificate
	// (e.g. from the ACME manager) instead of loading TLSCert/TLSKey files.
	// Only GetCertificate is used (no TLS-ALPN NextProtos) since the control
	// channel speaks yamux, not an ACME challenge protocol.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// CertNames are the names GetCertificate manages (ACME domains). Discovery asks
	// for the certificate of one of them, never of a name from a request.
	CertNames []string
	// MinClientVersion is the oldest client version let in (MAJOR.MINOR.PATCH,
	// a leading v allowed). Empty lets every client in, and so does a client
	// that names no version or one that cannot be compared.
	MinClientVersion string
	// SessionRecorder, when non-nil, is told what a session looks like when
	// it connects, when its tunnels change and when it ends. nil: nothing is
	// recorded.
	SessionRecorder SessionRecorder
	// ServiceRecorder, when non-nil, is told that a durable service is being
	// served when its tunnel is registered and when that tunnel goes away
	// (unregistered, or still open when the session ends). nil: nothing is
	// stamped.
	ServiceRecorder ServiceRecorder
	// TCPAccess reads the ip policy of a tcp service in mode ip. nil: a tcp
	// registration in mode ip is refused, since nobody could be let in.
	TCPAccess TCPAccessLoader
	// Forwards authorizes forward streams to tcp services. nil: the relay
	// takes none (it closes such a stream, as an older relay does) and does
	// not announce proto.CapForward.
	Forwards ForwardAuthorizer
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
	// tcpServing maps the tcp services a live tunnel serves to the client
	// token of its session (under mu): one tunnel per service.
	tcpServing map[string]string
	// unnamedWarned: the warning about a tcp tunnel without a name was
	// logged.
	unnamedWarned atomic.Bool
	// accessEpoch counts the calls of RefreshTCPService. A registration that
	// read a service's access before a call and added its tunnel after it
	// reads the access again.
	accessEpoch atomic.Uint64
	// forwardEpoch counts the forward rechecks (RecheckForwards,
	// RecheckUserForwards). An open that was authorized before a recheck
	// and registered after it asks again (see handleForward).
	forwardEpoch atomic.Uint64
	// refusals samples the refused visitors of tcp tunnels; authFails counts
	// the failed auth_requests on the control port.
	refusals  *refusalSampler
	authFails *authFailures
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
		tlsC:      tlsCfg,
		refusals:  &refusalSampler{log: o.Logger},
		authFails: newAuthFailures(),
	}
	s.ports = newPortAllocator(o.PortMin, o.PortMax)
	return s, nil
}

// ControlCertSHA256 returns the SHA-256 of the leaf certificate the control
// port presents to a client that asks for serverName, as lower-case hex. A
// file certificate is fixed; a GetCertificate (ACME) is asked each time, so a
// renewed certificate shows up at once.
//
// serverName comes from a request's Host header, so it is not trusted: with
// CertNames set, a name outside them is answered with the first managed name,
// and nothing is ever requested for a name the relay does not manage. The
// hello carries a stub connection (certmagic reads hello.Conn), and a panic in
// the certificate source ends as an error.
func (s *Server) ControlCertSHA256(serverName string) (fp string, err error) {
	var cert *tls.Certificate
	switch {
	case s.tlsC.GetCertificate != nil:
		name := serverName
		if len(s.opts.CertNames) > 0 {
			name = s.opts.CertNames[0]
			for _, n := range s.opts.CertNames {
				if strings.EqualFold(n, serverName) {
					name = n
					break
				}
			}
		}
		defer func() {
			if r := recover(); r != nil {
				fp, err = "", fmt.Errorf("control certificate lookup failed: %v", r)
			}
		}()
		c, gerr := s.tlsC.GetCertificate(&tls.ClientHelloInfo{ServerName: name, Conn: stubConn{}})
		if gerr != nil {
			return "", gerr
		}
		cert = c
	case len(s.tlsC.Certificates) > 0:
		cert = &s.tlsC.Certificates[0]
	}
	if cert == nil || len(cert.Certificate) == 0 {
		return "", errors.New("no control certificate")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:]), nil
}

// stubConn stands in for the connection of a hello that no client sent.
type stubConn struct{ net.Conn }

func (stubConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }
func (stubConn) LocalAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4zero} }

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

// Capabilities are what this relay announces to a client it lets in
// (proto.Cap… for a relay): tcp_access when it keeps tcp services, whose
// access modes it then applies, and forward when it also takes forward
// streams to them.
func (s *Server) Capabilities() []string {
	var out []string
	if _, ok := s.opts.Services.(TCPServiceResolver); ok {
		out = append(out, proto.CapTCPAccess)
		if s.opts.Forwards != nil {
			out = append(out, proto.CapForward)
		}
	}
	return out
}

// forwarding reports whether the relay takes forward streams.
func (s *Server) forwarding() bool {
	_, ok := s.opts.Services.(TCPServiceResolver)
	return ok && s.opts.Forwards != nil
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	sid := uuid.NewString()
	// Failed sign-ins are counted per source; an invalid token from a source
	// that used them up is answered rate_limited until its bucket refills.
	source := sourceKey(visitorAddr(conn))
	cs, err := HandleHandshakeWith(conn, s.opts.Auth, sid, HandshakeOptions{
		MinClientVersion: s.opts.MinClientVersion, Capabilities: s.Capabilities(),
		RateLimited: s.authFails.limited(source),
	})
	if err != nil {
		if errors.Is(err, errAuthFailed) {
			s.authFails.failed(source)
		}
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
	s.recordSession(s.reg, cs)

	defer func() {
		tunnels := s.reg.snapshotTunnels(cs)
		for _, tn := range tunnels {
			s.endTCPTunnel(tn)
			_ = s.opts.Tunnels.MarkTunnelSeen(context.Background(), tn.ID)
		}
		// After the ports are free, so that a slow write keeps none taken, and
		// before RemoveSession: the record names what the session served last.
		s.recordSession(s.reg, cs)
		for _, tn := range tunnels {
			s.stampService(cs, tn)
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
			// Bounded before a goroutine is started for it.
			if cs.waitingFirst.Add(1) > maxStreamsWaiting {
				cs.waitingFirst.Add(-1)
				_ = st.Close()
				continue
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
	// TokenID is the id of the client token of the session ("" when the
	// authenticator named none); ConnectedAt is when it logged in.
	TokenID     string
	ConnectedAt time.Time
	Tunnels     []TunnelView
	// Forwards are the forward streams the session has open; a session that
	// only forwards holds no tunnel.
	Forwards []ForwardView
}

// recordSession writes what the session looks like now on the client token it
// logged in with: at connect (nothing served yet), after every change of its
// tunnels and when it ends, so that a relay that crashes loses no more than
// the last change. A failure is logged and never fails the session. Without a
// recorder or a token id nothing is written.
func (s *Server) recordSession(reg *Registry, cs *ClientSession) {
	if s.opts.SessionRecorder == nil || cs.TokenID == "" {
		return
	}
	served := []string{}
	for _, tn := range reg.snapshotTunnels(cs) {
		if tn.Name != "" {
			served = append(served, tn.Name)
		}
	}
	slices.Sort(served)
	ctx, cancel := context.WithTimeout(context.Background(), sessionRecordTimeout)
	defer cancel()
	if err := s.opts.SessionRecorder.RecordClientSession(ctx, cs.TokenID, db.ClientSessionRecord{
		OS: cs.OS, Arch: cs.Arch, ClientVersion: cs.ClientVersion, RemoteAddr: cs.RemoteAddr,
		Served: slices.Compact(served),
	}); err != nil {
		s.logRecordFailure("record client session failed", err, "session_id", cs.SessionID)
	}
}

// stampService notes on the durable service of tn that the client of cs is
// serving it, or just stopped: when the tunnel is registered, when it is
// unregistered and for every tunnel still open when the session ends. A
// failure is logged and never fails the tunnel. Nothing is written without a
// recorder or for a tunnel that has no durable service.
func (s *Server) stampService(cs *ClientSession, tn *Tunnel) {
	if s.opts.ServiceRecorder == nil || tn == nil || tn.ServiceID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionRecordTimeout)
	defer cancel()
	if err := s.opts.ServiceRecorder.TouchServiceLastSeen(ctx, tn.ServiceID, cs.TokenName); err != nil {
		s.logRecordFailure("record service last seen failed", err, "session_id", cs.SessionID, "service_id", tn.ServiceID)
	}
}

// logRecordFailure logs a write of a recorder that failed. A row that is gone
// (the token was revoked or the service deleted while the client was
// connected) is an expected state and logged at Debug; anything else is a
// warning.
func (s *Server) logRecordFailure(msg string, err error, args ...any) {
	level := slog.LevelWarn
	if errors.Is(err, db.ErrNotFound) {
		level = slog.LevelDebug
	}
	s.log.Log(context.Background(), level, msg, append(args, "err", err)...)
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
			TokenID: cs.TokenID, ConnectedAt: cs.ConnectedAt,
			Forwards: cs.forwardViews(),
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

// Tunnels returns a snapshot slice of the live tunnels of every type across
// all sessions.
func (s *Server) Tunnels() []*Tunnel {
	var out []*Tunnel
	for _, cs := range s.reg.Sessions() {
		out = append(out, s.reg.snapshotTunnels(cs)...)
	}
	return out
}

// LookupTunnelByServiceID finds a live tunnel of any type that serves the
// service. The proxy must not use it: only LookupHTTPTunnelByServiceID finds
// what may be reached over HTTP.
func (s *Server) LookupTunnelByServiceID(serviceID string) (*Tunnel, bool) {
	if serviceID == "" {
		return nil, false
	}
	for _, cs := range s.reg.Sessions() {
		for _, tn := range s.reg.snapshotTunnels(cs) {
			if tn.ServiceID == serviceID {
				return tn, true
			}
		}
	}
	return nil, false
}
