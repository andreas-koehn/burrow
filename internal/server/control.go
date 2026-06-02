package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/proto"
)

const authReadTimeout = 10 * time.Second

// HandleHandshake reads the auth frame from a raw conn, validates the token via
// the supplied TokenAuthenticator, replies auth_response, and returns a new
// ClientSession on success. On failure it writes an error/auth_response and
// returns nil.
func HandleHandshake(conn net.Conn, auth TokenAuthenticator, sessionID string) (*ClientSession, error) {
	_ = conn.SetReadDeadline(time.Now().Add(authReadTimeout))
	var env proto.Envelope
	if err := proto.ReadFrame(conn, &env); err != nil {
		return nil, fmt.Errorf("read auth frame: %w", err)
	}
	if env.Type != proto.MsgAuthRequest {
		_ = proto.WriteMessage(conn, proto.MsgError, proto.Error{Message: "expected auth_request"})
		return nil, fmt.Errorf("first message was %s", env.Type)
	}
	var ar proto.AuthRequest
	if err := proto.DecodePayload(env, &ar); err != nil {
		_ = proto.WriteMessage(conn, proto.MsgError, proto.Error{Message: "bad auth payload"})
		return nil, err
	}
	var userID, tokenName string
	var err error
	if na, ok := auth.(interface {
		AuthenticateNamed(ctx context.Context, token string) (string, string, error)
	}); ok {
		userID, tokenName, err = na.AuthenticateNamed(context.Background(), ar.Token)
	} else {
		userID, err = auth.Authenticate(context.Background(), ar.Token)
	}
	if err != nil {
		_ = proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{OK: false, Error: "invalid token"})
		return nil, fmt.Errorf("token auth: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear deadline
	if err := proto.WriteMessage(conn, proto.MsgAuthResponse, proto.AuthResponse{OK: true, SessionID: sessionID}); err != nil {
		return nil, err
	}
	return &ClientSession{
		SessionID: sessionID, UserID: userID, RemoteAddr: conn.RemoteAddr().String(),
		OS: ar.OS, Arch: ar.Arch, ClientVersion: ar.ClientVersion, TokenName: tokenName,
		Tunnels: map[string]*Tunnel{},
	}, nil
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
				_ = cs.SendControl(proto.MsgError, proto.Error{Message: "bad tunnel_register"})
				continue
			}
			switch tr.Type {
			case "http":
				if s.opts.Services == nil {
					_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: "http tunnels not configured"})
					continue
				}
				serviceID, subdomain, rerr := s.opts.Services.Resolve(context.Background(), cs.UserID, tr.Name, "http")
				if rerr != nil {
					_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: "resolve service: " + rerr.Error()})
					continue
				}
				tn := &Tunnel{
					ID: uuid.NewString(), Name: tr.Name, Type: tr.Type, LocalAddr: tr.LocalAddr, sess: cs,
					IsHTTP: true, Subdomain: subdomain, ServiceID: serviceID,
				}
				reg.AddTunnel(cs, tn)
				if err := s.opts.Tunnels.SaveTunnel(context.Background(), cs.UserID, tn); err != nil {
					s.log.Warn("persist tunnel failed", "tunnel_id", tn.ID, "err", err)
				}
				s.opts.Events.PublishTunnelsChanged(cs.UserID)
				var hostname string
				if s.opts.AuthDomain != "" {
					hostname = subdomain + "." + s.opts.AuthDomain
				}
				s.log.Info("http tunnel registered", "tunnel_id", tn.ID, "subdomain", subdomain, "session_id", cs.SessionID)
				_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: true, TunnelID: tn.ID, RemotePort: 0, Hostname: hostname})
			case "", "tcp":
				port, perr := s.ports.Allocate(tr.RemotePort)
				if perr != nil {
					_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: perr.Error()})
					continue
				}
				tn := &Tunnel{ID: uuid.NewString(), Name: tr.Name, Type: tr.Type, RemotePort: port, LocalAddr: tr.LocalAddr, sess: cs}
				if lerr := s.startPublicListener(tn); lerr != nil {
					s.ports.Release(port)
					_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: lerr.Error()})
					continue
				}
				reg.AddTunnel(cs, tn)
				// Best-effort persist. RunControlLoop is serial (ping/pong/register/
				// unregister on one goroutine), so any TunnelStore wired here MUST be
				// fast and non-blocking — a slow store would stall heartbeat handling
				// for this client. (Task 8 wires local sqlite; offload if ever remote.)
				if err := s.opts.Tunnels.SaveTunnel(context.Background(), cs.UserID, tn); err != nil {
					s.log.Warn("persist tunnel failed", "tunnel_id", tn.ID, "err", err)
				}
				s.opts.Events.PublishTunnelsChanged(cs.UserID)
				s.log.Info("tunnel registered", "tunnel_id", tn.ID, "remote_port", port, "session_id", cs.SessionID)
				_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: true, TunnelID: tn.ID, RemotePort: port})
			default:
				_ = cs.SendControl(proto.MsgTunnelRegisterResp, proto.TunnelRegisterResponse{OK: false, Error: "unknown tunnel type \"" + tr.Type + "\""})
				continue
			}
		case proto.MsgTunnelUnregister:
			var tu proto.TunnelUnregister
			if err := proto.DecodePayload(env, &tu); err == nil {
				if tn := reg.Tunnel(cs, tu.TunnelID); tn != nil && tn.Listener != nil {
					_ = tn.Listener.Close()
					s.ports.Release(tn.RemotePort)
				}
				reg.RemoveTunnel(cs, tu.TunnelID)
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
			_ = cs.SendControl(proto.MsgError, proto.Error{Message: "unexpected: " + string(env.Type)})
		}
	}
}
