package server

import (
	"context"
	"net"
	"testing"

	"github.com/ankoehn/burrow/internal/proto"
)

// namedFakeAuth also reports a token name (AuthenticateNamed).
type namedFakeAuth struct{ uid, name string }

func (n namedFakeAuth) Authenticate(context.Context, string) (string, error) {
	return n.uid, nil
}
func (n namedFakeAuth) AuthenticateNamed(_ context.Context, _ string) (string, string, error) {
	return n.uid, n.name, nil
}

func TestHandshakeCapturesMetadata(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() {
		_ = proto.WriteMessage(c2, proto.MsgAuthRequest, proto.AuthRequest{
			ProtocolVersion: 1, Token: "t", ClientVersion: "9.9",
			OS: "linux", Arch: "amd64", Hostname: "box-1",
		})
		var resp proto.Envelope
		_ = proto.ReadFrame(c2, &resp)
	}()
	cs, err := HandleHandshake(c1, namedFakeAuth{uid: "u1", name: "laptop"}, "sid-1")
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if cs.OS != "linux" || cs.Arch != "amd64" || cs.ClientVersion != "9.9" {
		t.Fatalf("metadata not captured: %+v", cs)
	}
	if cs.TokenName != "laptop" {
		t.Fatalf("token name not captured: %q", cs.TokenName)
	}
}

// tokenFakeAuth also reports the id of the token (AuthenticateToken). Its
// AuthenticateNamed answers with another name, to show which one is asked.
type tokenFakeAuth struct{ uid, id, name string }

func (a tokenFakeAuth) Authenticate(context.Context, string) (string, error) {
	return a.uid, nil
}
func (a tokenFakeAuth) AuthenticateNamed(context.Context, string) (string, string, error) {
	return a.uid, "named-" + a.name, nil
}
func (a tokenFakeAuth) AuthenticateToken(context.Context, string) (string, string, string, error) {
	return a.uid, a.id, a.name, nil
}

func TestHandshakeCapturesTheTokenID(t *testing.T) {
	req := proto.AuthRequest{ProtocolVersion: 1, Token: "bur_test_0000"}
	_, cs := handshake(t, tokenFakeAuth{uid: "u1", id: "tok-1", name: "laptop"}, HandshakeOptions{}, req)
	if cs == nil || cs.UserID != "u1" || cs.TokenID != "tok-1" || cs.TokenName != "laptop" {
		t.Fatalf("session: %+v", cs)
	}
	// An authenticator without AuthenticateToken keeps working, with no id.
	_, cs = handshake(t, namedFakeAuth{uid: "u1", name: "laptop"}, HandshakeOptions{}, req)
	if cs == nil || cs.TokenID != "" || cs.TokenName != "laptop" {
		t.Fatalf("named authenticator: %+v", cs)
	}
	_, cs = handshake(t, fakeAuth{uid: "u1"}, HandshakeOptions{}, req)
	if cs == nil || cs.TokenID != "" || cs.TokenName != "" || cs.UserID != "u1" {
		t.Fatalf("plain authenticator: %+v", cs)
	}
}
