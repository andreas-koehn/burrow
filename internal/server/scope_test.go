package server

// Client token scopes (tcp complete, Q02): a forward token never registers a
// tunnel, a publish token never opens a forward, and an all token does both.

import (
	"context"
	"fmt"
	"testing"

	"github.com/ankoehn/burrow/internal/proto"
)

// scopedUsers is tokenUsers with two more tokens of u1: "fwd" has the scope
// forward and "pub" the scope publish. Every other token is scope all.
type scopedUsers struct{ tokenUsers }

func (scopedUsers) AuthenticateTokenScope(ctx context.Context, tok string) (string, string, string, string, error) {
	switch tok {
	case "fwd":
		return "u1", "tok-fwd", "u1-forward", "forward", nil
	case "pub":
		return "u1", "tok-pub", "u1-publish", "publish", nil
	}
	u, id, name, err := tokenUsers{}.AuthenticateToken(ctx, tok)
	if err != nil {
		return "", "", "", "", fmt.Errorf("scoped: %w", err)
	}
	return u, id, name, "all", nil
}

func newScopeRelay(t *testing.T, port int) *forwardRelay {
	return newForwardRelay(t, port, func(o *Options) { o.Auth = scopedUsers{} })
}

func TestScope_ForwardTokenMayNotRegister(t *testing.T) {
	r := newScopeRelay(t, 19840)
	c := r.clientWith("fwd", nil)
	rr := c.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389"})
	if rr.OK || rr.Code != proto.CodeScopeDenied || rr.Error == "" {
		t.Fatalf("a forward token registered: %+v", rr)
	}
	if rr.RemotePort != 0 || rr.TunnelID != "" {
		t.Fatalf("a refused registration opened something: %+v", rr)
	}
	// The same refusal for an http tunnel.
	rr = c.register(t, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:8080"})
	if rr.OK || rr.Code != proto.CodeScopeDenied {
		t.Fatalf("a forward token registered http: %+v", rr)
	}
	// Only the action is refused: the same session keeps its control stream.
	rr = c.register(t, proto.TunnelRegister{Name: "again", Type: "tcp", LocalAddr: "127.0.0.1:3389"})
	if rr.OK || rr.Code != proto.CodeScopeDenied {
		t.Fatalf("second attempt: %+v", rr)
	}
	// And it forwards: the service of the home client is reached.
	if rr := r.home.register(t, proto.TunnelRegister{Name: "box", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"}); !rr.OK {
		t.Fatalf("publish token register: %+v", rr)
	}
	f := r.forwarder("fwd")
	_, resp, err := f.open(t, "box")
	if err != nil || !resp.OK {
		t.Fatalf("forward with a forward token: %+v %v", resp, err)
	}
}

func TestScope_PublishTokenMayNotForward(t *testing.T) {
	r := newScopeRelay(t, 19845)
	rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"})
	if !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	f := r.forwarder("pub")
	st, resp, err := f.open(t, "rdp")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = st.Close()
	if resp.OK || resp.Code != proto.CodeScopeDenied || resp.Error == "" {
		t.Fatalf("a publish token forwarded: %+v", resp)
	}
	// Not counted as a forward that was let through: no row of a forward.
	if rows := r.rows(TrafficTCPForward); len(rows) != 0 {
		t.Fatalf("rows: %+v", rows)
	}
	// The same session may register tunnels.
	c := r.clientWith("pub", nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "mine", Type: "tcp", LocalAddr: "127.0.0.1:22"}); !rr.OK {
		t.Fatalf("publish token register: %+v", rr)
	}
}

func TestScope_AllTokenDoesBoth(t *testing.T) {
	r := newScopeRelay(t, 19850)
	// "secret" is the token of the home client: scope all.
	rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"})
	if !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	f := r.forwarder("secret")
	st, resp, err := f.open(t, "rdp")
	if err != nil || !resp.OK {
		t.Fatalf("forward: %+v %v", resp, err)
	}
	_ = st.Close()
}

func TestScope_AnAuthenticatorWithoutScopesLimitsNothing(t *testing.T) {
	r := newForwardRelay(t, 19855, nil) // tokenUsers names no scope
	rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"})
	if !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if _, resp, err := r.forwarder("secret").open(t, "rdp"); err != nil || !resp.OK {
		t.Fatalf("forward: %+v %v", resp, err)
	}
}
