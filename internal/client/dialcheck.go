package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"runtime"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/version"
)

// AuthResult is the relay's answer to a sign-in check.
type AuthResult struct {
	OK           bool
	Error        string // the relay's reason when !OK
	RelayVersion string // "" until the relay reports it
}

// dialControl opens the TLS connection to the control endpoint.
func dialControl(ctx context.Context, o Options) (net.Conn, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: o.Insecure, //nolint:gosec // dev-only opt-in (spec D4)
		RootCAs:            o.RootCAs,
		ServerName:         o.ServerName,
		MinVersion:         tls.VersionTLS12,
	}
	d := &tls.Dialer{Config: tlsCfg}
	conn, err := d.DialContext(ctx, "tcp", o.Server)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	return conn, nil
}

// authenticate sends the auth request and reads the answer. A refusal is the
// returned response with OK false, not an error.
func authenticate(conn net.Conn, token string) (proto.AuthResponse, error) {
	if err := proto.WriteMessage(conn, proto.MsgAuthRequest, proto.AuthRequest{
		ProtocolVersion: proto.ProtocolVersion, Token: token,
		ClientVersion: version.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
	}); err != nil {
		return proto.AuthResponse{}, err
	}
	var env proto.Envelope
	if err := proto.ReadFrame(conn, &env); err != nil {
		return proto.AuthResponse{}, err
	}
	var ar proto.AuthResponse
	if env.Type != proto.MsgAuthResponse || proto.DecodePayload(env, &ar) != nil {
		return proto.AuthResponse{}, fmt.Errorf("auth failed: %s", ar.Error)
	}
	return ar, nil
}

// CheckAuth connects to the control endpoint, authenticates and closes. It
// registers no tunnel. A rejected token is an AuthResult with OK false; an
// error means the relay could not be asked.
func CheckAuth(ctx context.Context, o Options) (AuthResult, error) {
	conn, err := dialControl(ctx, o)
	if err != nil {
		return AuthResult{}, err
	}
	defer conn.Close()
	// Reads on the connection do not see ctx; closing it ends them.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	ar, err := authenticate(conn, o.Token)
	if err != nil {
		if ctx.Err() != nil {
			return AuthResult{}, ctx.Err()
		}
		return AuthResult{}, err
	}
	return AuthResult{OK: ar.OK, Error: ar.Error}, nil
}
