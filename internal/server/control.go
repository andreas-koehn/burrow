package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/version"
)

const authReadTimeout = 10 * time.Second

// HandshakeOptions are the parts of a handshake that depend on the relay's
// configuration.
type HandshakeOptions struct {
	// MinClientVersion: see Options.MinClientVersion.
	MinClientVersion string
}

// HandleHandshake is HandleHandshakeWith without a minimum client version.
func HandleHandshake(conn net.Conn, auth TokenAuthenticator, sessionID string) (*ClientSession, error) {
	return HandleHandshakeWith(conn, auth, sessionID, HandshakeOptions{})
}

// HandleHandshakeWith reads the auth frame from a raw conn, validates the token
// via the supplied TokenAuthenticator, replies auth_response, and returns a new
// ClientSession on success. On failure it writes an error/auth_response and
// returns nil.
func HandleHandshakeWith(conn net.Conn, auth TokenAuthenticator, sessionID string, o HandshakeOptions) (*ClientSession, error) {
	_ = conn.SetReadDeadline(time.Now().Add(authReadTimeout))
	var env proto.Envelope
	if err := proto.ReadFrame(conn, &env); err != nil {
		return nil, fmt.Errorf("read auth frame: %w", err)
	}
	if env.Type != proto.MsgAuthRequest {
		_ = proto.WriteMessage(conn, proto.MsgError, proto.Error{Message: "expected auth_request", Code: proto.CodeBadRequest})
		return nil, fmt.Errorf("first message was %s", env.Type)
	}
	var ar proto.AuthRequest
	if err := proto.DecodePayload(env, &ar); err != nil {
		_ = proto.WriteMessage(conn, proto.MsgError, proto.Error{Message: "bad auth payload", Code: proto.CodeBadRequest})
		return nil, err
	}
	var userID, tokenID, tokenName string
	var err error
	if ta, ok := auth.(interface {
		AuthenticateToken(ctx context.Context, token string) (string, string, string, error)
	}); ok {
		userID, tokenID, tokenName, err = ta.AuthenticateToken(context.Background(), ar.Token)
	} else if na, ok := auth.(interface {
		AuthenticateNamed(ctx context.Context, token string) (string, string, error)
	}); ok {
		userID, tokenName, err = na.AuthenticateNamed(context.Background(), ar.Token)
	} else {
		userID, err = auth.Authenticate(context.Background(), ar.Token)
	}
	if err != nil {
		_ = proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{OK: false, Error: "invalid token", Code: proto.CodeInvalidToken})
		return nil, fmt.Errorf("token auth: %w", err)
	}
	// After the token: only a client that may connect is told it is too old.
	if olderThan(ar.ClientVersion, o.MinClientVersion) {
		min := strings.TrimPrefix(strings.TrimSpace(o.MinClientVersion), "v")
		_ = proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{
			OK: false, Code: proto.CodeClientTooOld,
			Error: "client too old: this relay needs burrow " + min + " or newer",
		})
		return nil, fmt.Errorf("client version %q is older than the minimum %s", ar.ClientVersion, min)
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear deadline
	if err := proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{OK: true, SessionID: sessionID, RelayVersion: version.Version}); err != nil {
		return nil, err
	}
	cs := &ClientSession{
		SessionID: sessionID, UserID: userID, RemoteAddr: conn.RemoteAddr().String(),
		OS: ar.OS, Arch: ar.Arch, ClientVersion: ar.ClientVersion, TokenName: tokenName,
		TokenID: tokenID, ConnectedAt: time.Now(),
		Tunnels: map[string]*Tunnel{},
	}
	// Only a client that says it reads request summaries is sent any. A name
	// this relay does not know turns nothing on.
	if slices.Contains(ar.Capabilities, proto.CapRequestSummaries) {
		cs.summaries = make(chan proto.RequestSummary, summaryQueue)
	}
	return cs, nil
}

// SummaryWanted reports whether the client that owns the tunnel asked for
// request summaries. The proxy asks before it builds one; the answer costs
// one read of the tunnel index.
func (s *Server) SummaryWanted(tunnelID string) bool {
	if tunnelID == "" {
		return false
	}
	cs, ok := s.reg.SessionByTunnelID(tunnelID)
	return ok && cs.WantsRequestSummaries()
}

// RequestSummary hands the summary of a request to the client whose tunnel
// served it, when that client asked for summaries. It never waits (see
// ClientSession.offerSummary), so the proxy can call it on the goroutine of
// the request: with SummaryWanted it is the proxy's SummarySink.
//
// The summary goes to the session that owns the tunnel it names, and only when
// that tunnel is an http tunnel of the service: never to another session, and
// never for a tcp tunnel. The proxy always names the tunnel it resolved for
// the request; a summary without one is for nobody.
func (s *Server) RequestSummary(serviceID string, sum proto.RequestSummary) {
	if serviceID == "" || sum.TunnelID == "" {
		return
	}
	cs, ok := s.reg.SessionByTunnelID(sum.TunnelID)
	if !ok || !cs.WantsRequestSummaries() {
		return
	}
	if tn := s.reg.Tunnel(cs, sum.TunnelID); tn == nil || !tn.IsHTTP || tn.ServiceID != serviceID {
		return
	}
	cs.offerSummary(sum)
}

// olderThan reports whether the client version is older than min. Both are
// MAJOR.MINOR.PATCH with an optional leading v; anything else (no version, a
// branch name, a pre-release) cannot be compared and is not older.
func olderThan(client, min string) bool {
	c, okc := releaseVersion(client)
	m, okm := releaseVersion(min)
	if !okc || !okm {
		return false
	}
	for i := range c {
		if c[i] != m[i] {
			return c[i] < m[i]
		}
	}
	return false
}

func releaseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" || len(p) > 9 {
			return out, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return out, false
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ignoredOptions lists, sorted, which of the two wishes msg carries.
func ignoredOptions(tr proto.TunnelRegister) []string {
	var out []string
	if tr.Access != "" {
		out = append(out, "access")
	}
	if tr.Slug != "" {
		out = append(out, "slug")
	}
	return out
}

// resolveHTTP binds an http tunnel to its service. A resolver that cannot
// take the client's wishes reports them as ignored.
func (s *Server) resolveHTTP(cs *ClientSession, tr proto.TunnelRegister) (Resolved, error) {
	if or, ok := s.opts.Services.(OptionsResolver); ok {
		return or.ResolveWithOptions(context.Background(), cs.UserID, tr.Name, "http", ResolveOptions{Slug: tr.Slug, Access: tr.Access})
	}
	serviceID, slug, err := s.opts.Services.Resolve(context.Background(), cs.UserID, tr.Name, "http")
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{ServiceID: serviceID, Slug: slug, Ignored: ignoredOptions(tr)}, nil
}

// refusalResponse is the answer to a registration that err refused: a
// *RefusalError is for the client to read; any other failure keeps the text
// it has always had, after prefix.
func refusalResponse(prefix string, err error) proto.TunnelRegisterResponse {
	refused := proto.TunnelRegisterResponse{OK: false, Error: prefix + err.Error(), Code: proto.CodeInternal}
	var re *RefusalError
	if errors.As(err, &re) {
		refused.Error, refused.Code = re.Message, re.Code
	}
	return refused
}

// tcpService is what a tcp registration knows about its service before it
// takes a port.
type tcpService struct {
	res  Resolved
	held map[int]string // the ports services hold
	tcp  TCPServiceResolver
}

// resolveTCP binds a tcp tunnel to its service when the relay keeps tcp
// services (see TCPServiceResolver). Without one, the tunnel has no service
// and the zero tcpService is returned. The client's access and slug wishes
// are not passed on: a tcp service has neither yet.
func (s *Server) resolveTCP(cs *ClientSession, tr proto.TunnelRegister) (tcpService, error) {
	tcp, ok := s.opts.Services.(TCPServiceResolver)
	if !ok {
		return tcpService{}, nil
	}
	ctx := context.Background()
	res, err := tcp.ResolveTCP(ctx, cs.UserID, tr.Name, ResolveOptions{})
	if err != nil {
		return tcpService{}, err
	}
	// Without them a port of another service could be handed out.
	held, err := tcp.TCPPortReservations(ctx)
	if err != nil {
		return tcpService{}, fmt.Errorf("tcp port reservations: %w", err)
	}
	return tcpService{res: res, held: held, tcp: tcp}, nil
}

// registerTCP registers a tcp tunnel: its service, its public port and its
// listener, and answers the client.
func (s *Server) registerTCP(reg *Registry, cs *ClientSession, tr proto.TunnelRegister) {
	svc, rerr := s.resolveTCP(cs, tr)
	if rerr != nil {
		refused := refusalResponse("resolve service: ", rerr)
		_ = cs.SendControl(proto.MsgTunnelRegisterResp, refused)
		s.refused(reg, cs, tr, refused.Code)
		return
	}
	port, perr := s.ports.allocateTCP(tr.RemotePort, svc.res.ServiceID, svc.res.TCPPort, svc.held)
	if perr != nil {
		_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: perr.Error(), Code: proto.CodePortUnavailable})
		s.refused(reg, cs, tr, proto.CodePortUnavailable)
		return
	}
	tn := &Tunnel{
		ID: uuid.NewString(), Name: tr.Name, Type: tr.Type, RemotePort: port, LocalAddr: tr.LocalAddr, sess: cs,
		ServiceID: svc.res.ServiceID, accessMode: svc.res.AccessMode, created: svc.res.Created,
	}
	if lerr := s.startPublicListener(tn); lerr != nil {
		s.ports.Release(port)
		_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: lerr.Error(), Code: proto.CodePortUnavailable})
		s.refused(reg, cs, tr, proto.CodePortUnavailable)
		return
	}
	// The service keeps the port for its next registration. A failure costs
	// only that: the tunnel is up.
	if svc.tcp != nil && port != svc.res.TCPPort {
		ctx, cancel := context.WithTimeout(context.Background(), sessionRecordTimeout)
		if err := svc.tcp.SetServiceTCPPort(ctx, tn.ServiceID, port); err != nil {
			s.log.Warn("keep tcp port failed", "service_id", tn.ServiceID, "remote_port", port, "err", err)
		}
		cancel()
	}
	reg.AddTunnel(cs, tn)
	// Best-effort persist. RunControlLoop is serial (ping/pong/register/
	// unregister on one goroutine), so any TunnelStore wired here MUST be
	// fast and non-blocking — a slow store would stall heartbeat handling
	// for this client. (Task 8 wires local sqlite; offload if ever remote.)
	if err := s.opts.Tunnels.SaveTunnel(context.Background(), cs.UserID, tn); err != nil {
		s.log.Warn("persist tunnel failed", "tunnel_id", tn.ID, "err", err)
	}
	s.recordSession(reg, cs)
	s.stampService(cs, tn)
	s.opts.Events.PublishTunnelsChanged(cs.UserID)
	s.log.Info("tunnel registered", "tunnel_id", tn.ID, "remote_port", port, "service_id", tn.ServiceID, "session_id", cs.SessionID)
	_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{
		OK: true, TunnelID: tn.ID, RemotePort: port,
		// A tcp service has neither a slug nor an access mode yet.
		Ignored: ignoredOptions(tr),
	})
}

// RunControlLoop processes control-stream messages until the stream closes.
func (s *Server) RunControlLoop(stream io.ReadWriteCloser, reg *Registry, cs *ClientSession) {
	defer stream.Close()
	for {
		var env proto.Envelope
		if err := proto.ReadFrame(stream, &env); err != nil {
			return
		}
		switch env.Type {
		case proto.MsgTunnelRegister:
			var tr proto.TunnelRegister
			if err := proto.DecodePayload(env, &tr); err != nil {
				_ = cs.SendControl(proto.MsgError, proto.Error{Message: "bad tunnel_register", Code: proto.CodeBadRequest})
				continue
			}
			switch tr.Type {
			case "http":
				if s.opts.Services == nil {
					_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: "http tunnels not configured", Code: proto.CodeInternal})
					s.refused(reg, cs, tr, proto.CodeInternal)
					continue
				}
				res, rerr := s.resolveHTTP(cs, tr)
				if rerr != nil {
					refused := refusalResponse("resolve service: ", rerr)
					_ = cs.SendControl(proto.MsgTunnelRegisterResp, refused)
					s.refused(reg, cs, tr, refused.Code)
					continue
				}
				serviceID, subdomain := res.ServiceID, res.Slug
				tn := &Tunnel{
					ID: uuid.NewString(), Name: tr.Name, Type: tr.Type, LocalAddr: tr.LocalAddr, sess: cs,
					IsHTTP: true, Subdomain: subdomain, ServiceID: serviceID,
					accessMode: res.AccessMode, created: res.Created,
				}
				reg.AddTunnel(cs, tn)
				if err := s.opts.Tunnels.SaveTunnel(context.Background(), cs.UserID, tn); err != nil {
					s.log.Warn("persist tunnel failed", "tunnel_id", tn.ID, "err", err)
				}
				s.recordSession(reg, cs)
				s.stampService(cs, tn)
				s.opts.Events.PublishTunnelsChanged(cs.UserID)
				var tunnelURL, dashboardURL string
				if s.opts.AuthDomain != "" {
					if !res.GatewayOnly {
						host := s.opts.AuthDomain
						if s.opts.ServicesDomain != "" {
							host = s.opts.ServicesDomain
						}
						tunnelURL = "https://" + host + "/svc/" + subdomain + "/"
					}
					dashboardURL = "https://" + s.opts.AuthDomain + "/services/" + serviceID
				}
				s.log.Info("http tunnel registered", "tunnel_id", tn.ID, "slug", subdomain, "url", tunnelURL, "session_id", cs.SessionID)
				_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{
					OK: true, TunnelID: tn.ID, RemotePort: 0, URL: tunnelURL,
					AccessMode: res.AccessMode, Created: res.Created, DashboardURL: dashboardURL, Ignored: res.Ignored,
					GatewayOnly: res.GatewayOnly,
				})
			case "", "tcp":
				s.registerTCP(reg, cs, tr)
			default:
				_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: "unknown tunnel type \"" + tr.Type + "\"", Code: proto.CodeUnknownTunnelType})
				s.refused(reg, cs, tr, proto.CodeUnknownTunnelType)
				continue
			}
		case proto.MsgTunnelUnregister:
			var tu proto.TunnelUnregister
			if err := proto.DecodePayload(env, &tu); err == nil {
				tn := reg.Tunnel(cs, tu.TunnelID)
				if tn != nil && tn.Listener != nil {
					_ = tn.Listener.Close()
					s.ports.Release(tn.RemotePort)
				}
				reg.RemoveTunnel(cs, tu.TunnelID)
				s.recordSession(reg, cs)
				s.stampService(cs, tn)
			}
		case proto.MsgPing:
			var p proto.Ping
			if err := proto.DecodePayload(env, &p); err != nil {
				s.log.Debug("decode ping payload", "err", err)
			}
			_ = cs.SendControl(proto.MsgPong, proto.Pong(p))
		case proto.MsgPong:
			// Pong is informational only. Dead-peer detection for the MVP is
			// provided entirely by yamux's built-in keepalive (EnableKeepAlive=true,
			// KeepAliveInterval=30s in yamuxConfig). The Ping/Pong messages are a
			// lightweight application-level liveness signal retained for future use;
			// yamux keepalive is the authoritative liveness mechanism.
		default:
			_ = cs.SendControl(proto.MsgError, proto.Error{Message: "unexpected: " + string(env.Type), Code: proto.CodeBadRequest})
		}
	}
}
