package main

// Client token scopes (tcp complete, Q02) with the production wiring: the
// relay's authenticator names the scope of a token, and a real store, a real
// relay and real tokens refuse a forward token's registration and a publish
// token's forward.

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/server"
)

// The control port's authenticator must name scopes: a plain one would give
// every token the scope all.
var _ server.ScopedAuthenticator = controlAuth{}

func TestServe_TokenScopesAreEnforced(t *testing.T) {
	r := bootObsRelay(t)
	mint := func(name, scope string) string {
		var tok struct {
			Token string `json:"token"`
			Scope string `json:"scope"`
		}
		r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": name, "scope": scope}, http.StatusCreated, &tok)
		if tok.Scope != scope {
			t.Fatalf("token %s scope %q", name, tok.Scope)
		}
		return tok.Token
	}
	fwdTok, pubTok, allTok := mint("fwd", "forward"), mint("pub", "publish"), mint("all", "all")

	// register sends a tunnel_register on a fresh session and reads the answer.
	register := func(token, name string) proto.TunnelRegisterResponse {
		t.Helper()
		conn, err := tls.Dial("tcp", r.control, &tls.Config{RootCAs: r.caPool, ServerName: "localhost"})
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.WriteMessage(conn, proto.MsgAuthRequest, proto.AuthRequest{ProtocolVersion: 1, Token: token, ClientVersion: "0.13.0"}); err != nil {
			t.Fatal(err)
		}
		var env proto.Envelope
		var ar proto.AuthResponse
		if err := proto.ReadFrame(conn, &env); err != nil || proto.DecodePayload(env, &ar) != nil || !ar.OK {
			t.Fatalf("auth: %+v %v", ar, err)
		}
		sess, err := yamux.Client(conn, yamux.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sess.Close(); _ = conn.Close() })
		ctrl, err := sess.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.WriteMessage(ctrl, proto.MsgTunnelRegister, proto.TunnelRegister{Name: name, Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"}); err != nil {
			t.Fatal(err)
		}
		_ = ctrl.SetReadDeadline(time.Now().Add(15 * time.Second))
		for {
			var e proto.Envelope
			if err := proto.ReadFrame(ctrl, &e); err != nil {
				t.Fatalf("register answer: %v", err)
			}
			if e.Type != proto.MsgTunnelRegisterResp {
				continue
			}
			var rr proto.TunnelRegisterResponse
			if err := proto.DecodePayload(e, &rr); err != nil {
				t.Fatal(err)
			}
			return rr
		}
	}

	// A forward token registers nothing, on any number of sessions.
	for i := 0; i < 2; i++ {
		if rr := register(fwdTok, "rdp"); rr.OK || rr.Code != proto.CodeScopeDenied {
			t.Fatalf("forward token registered (attempt %d): %+v", i, rr)
		}
	}
	// A publish token and an all token register.
	if rr := register(pubTok, "rdp"); !rr.OK {
		t.Fatalf("publish token register: %+v", rr)
	}
	if rr := register(allTok, "ssh"); !rr.OK {
		t.Fatalf("all token register: %+v", rr)
	}

	// A publish token opens no forward, whatever it asks for; the other two do.
	for _, tc := range []struct {
		name, token string
		want        string
	}{{"publish", pubTok, proto.CodeScopeDenied}, {"forward", fwdTok, ""}, {"all", allTok, ""}} {
		f := dialForwarder(t, r, tc.token)
		_, resp := f.open(t, "rdp")
		if tc.want != "" {
			if resp.OK || resp.Code != tc.want || resp.Error == "" {
				t.Fatalf("%s token forwarded: %+v", tc.name, resp)
			}
			continue
		}
		// Not refused for its scope: the answer is about the service (the
		// registering sessions above are closed, so it is offline).
		if resp.Code == proto.CodeScopeDenied {
			t.Fatalf("%s token refused for its scope: %+v", tc.name, resp)
		}
	}
}
