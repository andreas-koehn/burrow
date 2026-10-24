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

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/version"
)

const authReadTimeout = 10 * time.Second

// HandshakeOptions are the parts of a handshake that depend on the relay's
// configuration.
type HandshakeOptions struct {
	// MinClientVersion: see Options.MinClientVersion.
	MinClientVersion string
	// Capabilities are announced to a client that is let in.
	Capabilities []string
	// RateLimited: the source of the connection has failed too often. Its
	// token is still checked, so a valid client behind the same address gets
	// in; a token that is not valid is answered with proto.CodeRateLimited.
	RateLimited bool
}

// HandleHandshake is HandleHandshakeWith without a minimum client version.
func HandleHandshake(conn net.Conn, auth TokenAuthenticator, sessionID string) (*ClientSession, error) {
	return HandleHandshakeWith(conn, auth, sessionID, HandshakeOptions{})
}

// HandleHandshakeWith reads the auth frame from a raw conn, validates the token
// via the supplied TokenAuthenticator, replies auth_response, and returns a new
// ClientSession on success. On failure it writes an error/auth_response and
// returns nil. A failure for what the client sent (a wrong token, a first
// message that is no auth_request) matches errAuthFailed.
func HandleHandshakeWith(conn net.Conn, auth TokenAuthenticator, sessionID string, o HandshakeOptions) (*ClientSession, error) {
	_ = conn.SetReadDeadline(time.Now().Add(authReadTimeout))
	var env proto.Envelope
	if err := proto.ReadFrame(conn, &env); err != nil {
		return nil, fmt.Errorf("read auth frame: %w", err)
	}
	if env.Type != proto.MsgAuthRequest {
		_ = proto.WriteMessage(conn, proto.MsgError, proto.Error{Message: "expected auth_request", Code: proto.CodeBadRequest})
		return nil, authFailure{fmt.Errorf("first message was %s", env.Type)}
	}
	var ar proto.AuthRequest
	if err := proto.DecodePayload(env, &ar); err != nil {
		_ = proto.WriteMessage(conn, proto.MsgError, proto.Error{Message: "bad auth payload", Code: proto.CodeBadRequest})
		return nil, authFailure{err}
	}
	var userID, tokenID, tokenName string
	// A plain authenticator has no scopes: its tokens are scope all, said here
	// and not left empty (an empty scope allows nothing).
	scope := db.TokenScopeAll
	var err error
	if sa, ok := auth.(ScopedAuthenticator); ok {
		userID, tokenID, tokenName, scope, err = sa.AuthenticateTokenScope(context.Background(), ar.Token)
	} else if ta, ok := auth.(interface {
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
		// Only a token the authenticator rejected is a failed sign-in; a
		// failure of the store is not the client's: it is not counted, and
		// the client is told to try again rather than that its token is bad
		// (which would make it stop).
		if !errors.Is(err, ErrTokenInvalid) {
			_ = proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{
				OK: false, Code: proto.CodeInternal, Error: "the relay could not check the token; try again later",
			})
			return nil, fmt.Errorf("token auth: %w", err)
		}
		if o.RateLimited {
			_ = proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{
				OK: false, Code: proto.CodeRateLimited,
				Error: "too many failed sign-ins from this address; try again in a few minutes",
			})
			return nil, authFailure{fmt.Errorf("token auth, address limited: %w", err)}
		}
		_ = proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{OK: false, Error: "invalid token", Code: proto.CodeInvalidToken})
		return nil, authFailure{fmt.Errorf("token auth: %w", err)}
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
	if err := proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{
		OK: true, SessionID: sessionID, RelayVersion: version.Version, Capabilities: o.Capabilities,
	}); err != nil {
		return nil, err
	}
	cs := &ClientSession{
		SessionID: sessionID, UserID: userID, RemoteAddr: conn.RemoteAddr().String(), LocalAddr: conn.LocalAddr().String(),
		OS: ar.OS, Arch: ar.Arch, ClientVersion: ar.ClientVersion, TokenName: tokenName,
		TokenID: tokenID, Scope: scope, ConnectedAt: time.Now(),
		Tunnels: map[string]*Tunnel{},
	}
	// Only a client that says it reads request summaries is sent any. A name
	// this relay does not know turns nothing on.
	if slices.Contains(ar.Capabilities, proto.CapRequestSummaries) {
		cs.summaries = make(chan proto.RequestSummary, summaryQueue)
	}
	cs.proxyV2 = slices.Contains(ar.Capabilities, proto.CapProxyProtocolV2)
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
	held map[int]TCPPortHold // the ports services hold
	tcp  TCPServiceResolver
}

// tcpResolveTimeout bounds what a tcp registration asks the store before it
// takes a port.
const tcpResolveTimeout = 5 * time.Second

// errTCPLookup is what the client is told when the store failed: the
// failure itself is logged, never sent.
const errTCPLookup = "the relay could not look up the service; try again later"

// resolveTCP binds a tcp tunnel to its service when the relay keeps tcp
// services (see TCPServiceResolver), and reads the holds on ports. Without a
// resolver the zero tcpService is returned; a tunnel without a name (an old
// client that was given none) gets the holds and no service. The client's
// access wish is passed on (it applies when the service is created); a tcp
// service has no slug.
func (s *Server) resolveTCP(cs *ClientSession, tr proto.TunnelRegister) (tcpService, error) {
	tcp, ok := s.opts.Services.(TCPServiceResolver)
	if !ok {
		return tcpService{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), tcpResolveTimeout)
	defer cancel()
	var res Resolved
	if tr.Name != "" {
		var err error
		if res, err = tcp.ResolveTCP(ctx, cs.UserID, tr.Name, ResolveOptions{Access: tr.Access, AccessDefault: tr.AccessDefault}); err != nil {
			return tcpService{}, err
		}
	} else if s.unnamedWarned.CompareAndSwap(false, true) {
		s.log.Warn("a client registered a tcp tunnel without a name: it gets no service and no sticky port; upgrade the client and give the tunnel a name",
			"session_id", cs.SessionID, "client_version", cs.ClientVersion)
	}
	// Without them a port of another service could be handed out.
	held, err := tcp.TCPPortReservations(ctx)
	if err != nil {
		return tcpService{}, fmt.Errorf("tcp port reservations: %w", err)
	}
	return tcpService{res: res, held: held, tcp: tcp}, nil
}

// claimTCPService marks the service as served by one tunnel of a session
// that logged in with tokenID. ok is false when another tunnel serves it;
// sameToken then says that tunnel's session logged in with the same token
// (probably the same client, whose last session the relay has not seen end).
// A tunnel without a service claims nothing.
func (s *Server) claimTCPService(serviceID, tokenID string) (ok, sameToken bool) {
	if serviceID == "" {
		return true, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if holder, served := s.tcpServing[serviceID]; served {
		return false, holder == tokenID
	}
	if s.tcpServing == nil {
		s.tcpServing = map[string]string{}
	}
	s.tcpServing[serviceID] = tokenID
	return true, false
}

// releaseTCPService ends a claim of claimTCPService.
func (s *Server) releaseTCPService(serviceID string) {
	if serviceID == "" {
		return
	}
	s.mu.Lock()
	delete(s.tcpServing, serviceID)
	s.mu.Unlock()
}

// servingTCP reports whether a tunnel serves the tcp service now.
func (s *Server) servingTCP(serviceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, served := s.tcpServing[serviceID]
	return served
}

// endTCPTunnel closes what a tcp tunnel holds on the relay: its listener,
// its port and its claim on the service. The service keeps its hold on the
// port for the next registration.
func (s *Server) endTCPTunnel(tn *Tunnel) {
	if tn == nil || tn.IsHTTP {
		return
	}
	if tn.Listener != nil {
		_ = tn.Listener.Close()
		s.ports.Release(tn.RemotePort)
	}
	s.releaseTCPService(tn.ServiceID)
}

// registerTCP registers a tcp tunnel: its service, its access mode, its
// public port and its listener, and answers the client.
//
// A tunnel never gets a public port that its client did not ask for and
// that its service does not have: a client that asked for a mode the relay
// cannot apply (no tcp services, no name) or for another mode than the
// service has is refused with proto.CodeAccessInvalid, and nothing is
// opened. Mode private opens no port at all; mode ip opens one whose
// visitors pass the service's ip policy before the home client is told.
func (s *Server) registerTCP(reg *Registry, cs *ClientSession, tr proto.TunnelRegister) {
	refuseWith := func(code, msg string) {
		_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: msg, Code: code})
		s.refused(reg, cs, tr, code)
	}
	// The mode a new service would get: the wish, else the client's default.
	want := tr.Access
	if want == "" {
		want = tr.AccessDefault
	}
	if (tr.Access != "" && !ValidTCPAccessMode(tr.Access)) || (tr.AccessDefault != "" && !ValidTCPAccessMode(tr.AccessDefault)) {
		refuseWith(proto.CodeAccessInvalid, "access for a tcp service must be one of: open, ip, private")
		return
	}
	_, keepsServices := s.opts.Services.(TCPServiceResolver)
	if want != "" && want != TCPAccessOpen && (!keepsServices || tr.Name == "") {
		msg := "this relay cannot apply an access mode to a tcp tunnel; nothing was opened"
		if keepsServices {
			msg = "a tcp tunnel needs a name to have an access mode; nothing was opened"
		}
		refuseWith(proto.CodeAccessInvalid, msg)
		return
	}
	epoch := s.accessEpoch.Load()
	svc, rerr := s.resolveTCP(cs, tr)
	if rerr != nil {
		var re *RefusalError
		if errors.As(rerr, &re) {
			refuseWith(re.Code, re.Message)
			return
		}
		s.log.Warn("resolve tcp service failed", "session_id", cs.SessionID, "name", tr.Name, "err", rerr)
		refuseWith(proto.CodeInternal, errTCPLookup)
		return
	}
	serviceID := svc.res.ServiceID
	mode := TCPAccessOpen
	if serviceID != "" {
		if svc.res.AccessMode != "" {
			mode = svc.res.AccessMode
		}
		if !ValidTCPAccessMode(mode) {
			s.log.Warn("tcp service has no tcp access mode, registration refused", "service_id", serviceID, "access_mode", mode)
			refuseWith(proto.CodeAccessInvalid, fmt.Sprintf("the service %q has the access mode %q, which a tcp service cannot have; change it in the dashboard", tr.Name, mode))
			return
		}
		// The mode in the dashboard wins over the client's wish when it is
		// stricter (open < ip < private): a service tightened in the
		// dashboard is served so, and the answer names the mode. A wish
		// stricter than the stored mode is refused: serving it would open
		// more than the client asked for.
		if tr.Access != "" && tcpAccessRank(tr.Access) > tcpAccessRank(mode) {
			refuseWith(proto.CodeAccessInvalid, "service is "+mode+" in the dashboard; restart with --access "+mode+" to serve it")
			return
		}
	}
	maxConns := svc.res.MaxConnections
	var policy *tcpPolicy
	if mode == TCPAccessIP || (mode == TCPAccessOpen && serviceID != "" && s.opts.TCPAccess != nil) {
		var perr error
		var loadedMax int
		if policy, loadedMax, perr = s.loadTCPPolicy(serviceID, mode); perr != nil {
			var re *RefusalError
			if errors.As(perr, &re) {
				refuseWith(re.Code, re.Message)
				return
			}
			s.log.Warn("load tcp ip policy failed", "session_id", cs.SessionID, "service_id", serviceID, "err", perr)
			refuseWith(proto.CodeInternal, errTCPLookup)
			return
		}
		if mode == TCPAccessIP {
			maxConns = loadedMax
		}
	}
	if ok, same := s.claimTCPService(serviceID, cs.TokenID); !ok {
		// The same token: likely the client's own last session, not reaped
		// yet; it tries again. Another token: another machine, which must
		// not keep trying.
		code := proto.CodeNameInUse
		if same {
			code = proto.CodeServiceBusy
		}
		refuseWith(code, fmt.Sprintf("the service %q is already served by another client", tr.Name))
		return
	}
	tn := &Tunnel{
		ID: uuid.NewString(), Name: tr.Name, Type: tr.Type, LocalAddr: tr.LocalAddr, sess: cs,
		ServiceID: serviceID, accessMode: mode, created: svc.res.Created,
	}
	tn.setTCPSettings(svc.res.ProxyProtocol, svc.res.IdleTimeout)
	if serviceID != "" {
		// The service's bans and the usage of its day, read when the relay
		// does not hold them yet: a ban that could not be read would let its
		// source in, so the registration is refused and the client retries.
		g, gerr := s.guardFor(serviceID)
		if gerr != nil {
			s.log.Warn("read tcp bans and usage failed", "session_id", cs.SessionID, "service_id", serviceID, "err", gerr)
			s.releaseTCPService(serviceID)
			refuseWith(proto.CodeInternal, errTCPLookup)
			return
		}
		g.apply(svc.res.Limits, s.now())
		tn.guard = g
	}
	tn.slug = svc.res.Slug
	tn.setHostname(svc.res.TCPHostname)
	if tr.ProxyProtocol == proto.ProxyProtocolV2 {
		tn.clientProxy = proto.ProxyProtocolV2
	}
	if mode == TCPAccessPrivate {
		// Forward streams count against the cap and the pending pairings.
		tn.limits = newTCPLimits(maxConns)
		// No listener and no port. An automatic hold the service kept from
		// an earlier mode is given back to the pool. A pinned hold (the
		// client asked for that port with --remote) is kept on purpose: it
		// goes only with the service or a new request of its own (P01), so
		// that switching back to open or ip gets the same port. It opens
		// nothing while the service is private.
		if svc.tcp != nil && svc.res.TCPPort != 0 && !svc.res.TCPPortPinned {
			ctx, cancel := context.WithTimeout(context.Background(), sessionRecordTimeout)
			if err := svc.tcp.SetServiceTCPPort(ctx, serviceID, 0, false, ""); err != nil {
				s.log.Warn("release tcp port of a private service failed", "service_id", serviceID, "err", err)
			}
			cancel()
		}
	} else if !s.openTCPPort(reg, cs, tr, svc, tn, maxConns, policy, refuseWith) {
		return
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
	s.log.Info("tunnel registered", "tunnel_id", tn.ID, "remote_port", tn.RemotePort, "service_id", serviceID,
		"access_mode", mode, "session_id", cs.SessionID)
	resp := proto.TunnelRegisterResponse{OK: true, TunnelID: tn.ID, RemotePort: tn.RemotePort}
	if serviceID != "" {
		resp.AccessMode, resp.Created, resp.Slug = mode, svc.res.Created, svc.res.Slug
		if s.opts.AuthDomain != "" {
			resp.DashboardURL = "https://" + s.opts.AuthDomain + "/services/" + serviceID
		}
		if tr.Slug != "" {
			resp.Ignored = []string{"slug"} // a tcp service has no slug
		}
	} else {
		// No service: a slug cannot be applied. An unnamed tunnel on a relay
		// that keeps tcp services can only have asked for open (any other
		// access was refused above), which it is: that is not ignored. A
		// relay without tcp services applies no access at all and says so,
		// as an older relay does.
		if keepsServices {
			tr.Access = ""
		}
		resp.Ignored = ignoredOptions(tr)
	}
	_ = cs.SendControl(proto.MsgTunnelRegisterResp, resp)
	// The service changed while this registration read it: read it again,
	// now that RefreshTCPService can find the tunnel. A new mode ends the
	// session the client was just answered on, and it registers again.
	if serviceID != "" && s.accessEpoch.Load() != epoch {
		ctx, cancel := context.WithTimeout(context.Background(), tcpResolveTimeout)
		_ = s.reconcileTCP(ctx, tn)
		cancel()
	}
}

// openTCPPort gives a tcp tunnel in mode open or ip its public port, its
// limits and policy, and its listener, and keeps the port on the service. On
// a refusal it has answered the client, freed what it took, and returns
// false.
func (s *Server) openTCPPort(reg *Registry, cs *ClientSession, tr proto.TunnelRegister, svc tcpService, tn *Tunnel,
	maxConns int, policy *tcpPolicy, refuseWith func(code, msg string)) bool {
	serviceID := tn.ServiceID
	plan, perr := s.ports.allocateTCP(tcpPortRequest{
		requested: tr.RemotePort, serviceID: serviceID, userID: cs.UserID,
		own: svc.res.TCPPort, ownPinned: svc.res.TCPPortPinned, limit: svc.res.TCPPortLimit,
		held: svc.held, serving: s.servingTCP,
	})
	if perr != nil {
		s.releaseTCPService(serviceID)
		refuseWith(proto.CodePortUnavailable, perr.Error())
		return false
	}
	port := plan.port
	tn.RemotePort = port
	tn.limits = newTCPLimits(maxConns)
	if policy != nil {
		tn.policy.Store(policy)
	}
	if lerr := s.startPublicListener(tn); lerr != nil {
		s.ports.Release(port)
		s.releaseTCPService(serviceID)
		refuseWith(proto.CodePortUnavailable, lerr.Error())
		return false
	}
	// The service keeps the port for its next registration. A hold that
	// another service has meanwhile (a take-over that lost a race) refuses
	// the registration; any other failure costs only the stickiness. A
	// tunnel without a service holds nothing: the hold it took over or
	// reclaimed is cleared.
	if svc.tcp != nil && (plan.persist || plan.takeFrom != "") {
		ctx, cancel := context.WithTimeout(context.Background(), sessionRecordTimeout)
		var err error
		if serviceID != "" {
			err = svc.tcp.SetServiceTCPPort(ctx, serviceID, port, plan.pinned, plan.takeFrom)
		} else {
			err = svc.tcp.SetServiceTCPPort(ctx, plan.takeFrom, 0, false, "")
		}
		cancel()
		if err != nil {
			if errors.Is(err, ErrTCPPortTaken) || plan.takeFrom != "" {
				s.log.Warn("keep tcp port failed, registration refused", "service_id", serviceID, "remote_port", port, "err", err)
				s.endTCPTunnel(tn)
				refuseWith(proto.CodePortUnavailable, portNotAvailable(port).Error())
				return false
			}
			s.log.Warn("keep tcp port failed", "service_id", serviceID, "remote_port", port, "err", err)
		} else if plan.takeFrom != "" {
			s.portReclaimed(reg, cs, serviceID, plan.takeFrom, port, plan.reason)
		}
	}
	return true
}

// loadTCPPolicy reads and compiles the ip policy of a service in mode ip (or
// the country rules of one in mode open),
// with its cap on visitors. A relay without a TCPAccessLoader cannot let
// anybody in, and a service whose mode changed meanwhile is retried by the
// client.
func (s *Server) loadTCPPolicy(serviceID, mode string) (*tcpPolicy, int, error) {
	if s.opts.TCPAccess == nil {
		return nil, 0, &RefusalError{Code: proto.CodeAccessInvalid, Message: "this relay cannot apply the access mode ip; nothing was opened"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), tcpResolveTimeout)
	defer cancel()
	acc, err := s.opts.TCPAccess.TCPAccess(ctx, serviceID)
	if err != nil {
		return nil, 0, err
	}
	if mode == TCPAccessOpen {
		// A mode that changed meanwhile is dealt with by the check after
		// the registration (accessEpoch); here only the country rules count.
		if acc.Mode != "" && acc.Mode != TCPAccessOpen {
			return nil, 0, nil
		}
		acc.Mode = TCPAccessOpen
	} else if acc.Mode != mode {
		return nil, 0, &RefusalError{Code: proto.CodeServiceBusy, Message: "the access mode of the service changed while registering; trying again"}
	}
	p, err := compileTCPPolicy(acc)
	if err != nil {
		return nil, 0, err
	}
	return p, acc.MaxConnections, nil
}

// RefreshTCPService tells the relay that the access of a tcp service changed
// (its mode, its ip lists, its grants or its cap on visitors). The live
// tunnel of the service reads it again: a new mode closes the session that
// serves it, so that its client registers again under the new mode (D14); a
// new policy or cap applies to the next visitor, and connected ones stay. A
// service the relay cannot read is closed too: it fails closed. Without a
// live tunnel nothing is read, but a registration under way reads the
// service again when it is done.
func (s *Server) RefreshTCPService(ctx context.Context, serviceID string) error {
	if serviceID == "" {
		return nil
	}
	s.accessEpoch.Add(1)
	tn, ok := s.LookupTunnelByServiceID(serviceID)
	if !ok || tn.IsHTTP {
		return nil
	}
	return s.reconcileTCP(ctx, tn)
}

// reconcileTCP applies the access of the service as it is now to its live
// tcp tunnel tn (see RefreshTCPService).
func (s *Server) reconcileTCP(ctx context.Context, tn *Tunnel) error {
	if s.opts.TCPAccess == nil {
		return nil
	}
	closeIt := func(why string, err error) {
		n := s.reg.CloseByService(tn.ServiceID)
		s.log.Info("tcp tunnel closed: "+why, "service_id", tn.ServiceID, "tunnel_id", tn.ID, "sessions", n, "err", err)
	}
	acc, err := s.opts.TCPAccess.TCPAccess(ctx, tn.ServiceID)
	if err != nil {
		closeIt("its access could not be read", err)
		return err
	}
	mode := acc.Mode
	if mode == "" {
		mode = TCPAccessOpen
	}
	if mode != tn.accessMode {
		closeIt("its access mode changed from "+tn.accessMode+" to "+mode, nil)
		return nil
	}
	if tn.limits != nil {
		tn.limits.setMax(acc.MaxConnections)
	}
	tn.setTCPSettings(acc.ProxyProtocol, acc.IdleTimeout)
	tn.setHostname(acc.Hostname)
	if tn.guard != nil {
		tn.guard.apply(acc.Limits, s.now())
	}
	if mode == TCPAccessIP || mode == TCPAccessOpen {
		acc.Mode = mode
		p, err := compileTCPPolicy(acc)
		if err != nil {
			closeIt("its ip policy could not be compiled", err)
			return err
		}
		tn.policy.Store(p)
	}
	return nil
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
			// A token that may only forward publishes nothing, whatever the
			// type; only this registration is refused, not the session.
			if !cs.MayPublish() {
				_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: errScopeNoPublish, Code: proto.CodeScopeDenied})
				s.refused(reg, cs, tr, proto.CodeScopeDenied)
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
				s.endTCPTunnel(tn)
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
