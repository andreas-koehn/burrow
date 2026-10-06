package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/version"
)

// optResolver is a ServiceResolver that also takes the create options. It
// records what it was asked and answers with res or err.
type optResolver struct {
	res  Resolved
	err  error
	seen []ResolveOptions
}

func (o *optResolver) Resolve(context.Context, string, string, string) (string, string, error) {
	return "", "", errors.New("Resolve must not be used when ResolveWithOptions exists")
}

func (o *optResolver) ResolveWithOptions(_ context.Context, _, _, _ string, opts ResolveOptions) (Resolved, error) {
	o.seen = append(o.seen, opts)
	return o.res, o.err
}

func TestRegisterHTTP_OptionsReachTheResolverAndTheAnswerSaysWhatHappened(t *testing.T) {
	r := &optResolver{res: Resolved{ServiceID: "svc-1", Slug: "my-app", AccessMode: "burrow_login", Created: true}}
	_, _, _, cli := newTestServerWithHTTP(t, r, "burrow.example.com")

	rr := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app", Access: "burrow_login"})
	if !rr.OK || rr.TunnelID == "" {
		t.Fatalf("register: %+v", rr)
	}
	if len(r.seen) != 1 || r.seen[0] != (ResolveOptions{Slug: "my-app", Access: "burrow_login"}) {
		t.Fatalf("resolver saw %+v", r.seen)
	}
	if rr.URL != "https://burrow.example.com/svc/my-app/" || rr.AccessMode != "burrow_login" || !rr.Created ||
		rr.DashboardURL != "https://burrow.example.com/services/svc-1" || len(rr.Ignored) != 0 || rr.Code != "" {
		t.Fatalf("answer: %+v", rr)
	}

	// The service exists with other values: nothing is applied, and the
	// answer says which wishes were left out.
	r.res = Resolved{ServiceID: "svc-1", Slug: "my-app", AccessMode: "burrow_login", Ignored: []string{"access", "slug"}}
	rr = doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "other", Access: "open"})
	if !rr.OK || rr.Created || rr.AccessMode != "burrow_login" || rr.URL != "https://burrow.example.com/svc/my-app/" ||
		!reflect.DeepEqual(rr.Ignored, []string{"access", "slug"}) {
		t.Fatalf("answer for an existing service: %+v", rr)
	}
}

func TestRegisterHTTP_NoAuthDomainGivesNoDashboardURL(t *testing.T) {
	r := &optResolver{res: Resolved{ServiceID: "svc-1", Slug: "abc234", AccessMode: "open", Created: true}}
	_, _, _, cli := newTestServerWithHTTP(t, r, "")
	rr := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
	if !rr.OK || rr.URL != "" || rr.DashboardURL != "" || rr.AccessMode != "open" {
		t.Fatalf("answer: %+v", rr)
	}
}

func TestRegisterHTTP_RefusalsCarryACodeNextToTheText(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		code     string
		wantText string
	}{
		{"slug invalid", &RefusalError{Code: proto.CodeSlugInvalid, Message: "slug must be 3-40 characters"}, proto.CodeSlugInvalid, "slug must be 3-40 characters"},
		{"slug taken", &RefusalError{Code: proto.CodeSlugTaken, Message: "slug already in use; try: abc234"}, proto.CodeSlugTaken, "slug already in use; try: abc234"},
		{"access invalid", fmt.Errorf("wrapped: %w", &RefusalError{Code: proto.CodeAccessInvalid, Message: "access must be one of"}), proto.CodeAccessInvalid, "access must be one of"},
		// Any other failure keeps the text it has always had.
		{"other", errors.New("db down"), proto.CodeInternal, "resolve service: db down"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, reg, cs, cli := newTestServerWithHTTP(t, &optResolver{err: c.err}, "burrow.example.com")
			rr := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "x"})
			if rr.OK || rr.Code != c.code || rr.Error != c.wantText {
				t.Fatalf("answer: %+v", rr)
			}
			if n := len(reg.snapshotTunnels(cs)); n != 0 {
				t.Fatalf("%d tunnels after a refusal", n)
			}
		})
	}
}

func TestRegister_ExistingRefusalsKeepTheirTextAndGainACode(t *testing.T) {
	_, _, _, cli := newTestServerWithHTTP(t, nil, "burrow.example.com")
	rr := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
	if rr.OK || rr.Error != "http tunnels not configured" || rr.Code != proto.CodeInternal {
		t.Fatalf("http without a resolver: %+v", rr)
	}
	rr = doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "udp", LocalAddr: "127.0.0.1:3000"})
	if rr.OK || rr.Error != `unknown tunnel type "udp"` || rr.Code != proto.CodeUnknownTunnelType {
		t.Fatalf("unknown type: %+v", rr)
	}
	// A port outside the range.
	rr = doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "tcp", RemotePort: 1, LocalAddr: "127.0.0.1:3000"})
	if rr.OK || rr.Error == "" || rr.Code != proto.CodePortUnavailable {
		t.Fatalf("port out of range: %+v", rr)
	}
}

// A tcp service has neither a slug nor an access mode.
func TestRegisterTCP_SlugAndAccessAreReportedAsIgnored(t *testing.T) {
	_, _, _, cli := newTestServerWithHTTP(t, &optResolver{}, "burrow.example.com")
	rr := doRegister(t, cli, proto.TunnelRegister{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", Slug: "my-app", Access: "open"})
	if !rr.OK || rr.RemotePort == 0 || !reflect.DeepEqual(rr.Ignored, []string{"access", "slug"}) || rr.AccessMode != "" || rr.Created {
		t.Fatalf("answer: %+v", rr)
	}
	rr = doRegister(t, cli, proto.TunnelRegister{Name: "pg2", Type: "tcp", LocalAddr: "127.0.0.1:5432", Slug: "my-app"})
	if !rr.OK || !reflect.DeepEqual(rr.Ignored, []string{"slug"}) {
		t.Fatalf("answer: %+v", rr)
	}
	rr = doRegister(t, cli, proto.TunnelRegister{Name: "pg3", Type: "tcp", LocalAddr: "127.0.0.1:5432"})
	if !rr.OK || rr.Ignored != nil {
		t.Fatalf("answer without wishes: %+v", rr)
	}
}

// A resolver that knows nothing of the options cannot apply them; the answer
// says so instead of dropping them without a word.
func TestRegisterHTTP_ResolverWithoutOptions(t *testing.T) {
	_, _, _, cli := newTestServerWithHTTP(t, fakeResolver{sub: "abc234", id: "svc-9"}, "burrow.example.com")
	rr := doRegister(t, cli, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Access: "api_key"})
	if !rr.OK || rr.URL != "https://burrow.example.com/svc/abc234/" || !reflect.DeepEqual(rr.Ignored, []string{"access"}) {
		t.Fatalf("answer: %+v", rr)
	}
}

// handshake runs one handshake with the given request and returns the answer
// and the session.
func handshake(t *testing.T, auth TokenAuthenticator, o HandshakeOptions, req proto.AuthRequest) (proto.AuthResponse, *ClientSession) {
	t.Helper()
	cli, srv := dialPair()
	defer cli.Close()
	defer srv.Close()
	done := make(chan *ClientSession, 1)
	go func() { cs, _ := HandleHandshakeWith(srv, auth, "sid-1", o); done <- cs }()
	if err := proto.WriteMessage(cli, proto.MsgAuthRequest, req); err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := proto.ReadFrame(cli, &env); err != nil || env.Type != proto.MsgAuthResponse {
		t.Fatalf("want auth_response, got %v err=%v", env.Type, err)
	}
	var ar proto.AuthResponse
	if err := proto.DecodePayload(env, &ar); err != nil {
		t.Fatal(err)
	}
	return ar, <-done
}

func TestHandshake_TellsTheRelayVersion(t *testing.T) {
	ar, cs := handshake(t, fakeAuth{uid: "u1"}, HandshakeOptions{}, proto.AuthRequest{ProtocolVersion: 1, Token: "bur_test_0000"})
	if !ar.OK || ar.SessionID != "sid-1" || ar.RelayVersion != version.Version || ar.Code != "" || cs == nil {
		t.Fatalf("answer: ok %v session %q relay %q code %q", ar.OK, ar.SessionID, ar.RelayVersion, ar.Code)
	}
}

func TestHandshake_BadTokenKeepsItsTextAndGainsACode(t *testing.T) {
	ar, cs := handshake(t, fakeAuth{err: errors.New("nope")}, HandshakeOptions{}, proto.AuthRequest{ProtocolVersion: 1, Token: "bur_test_bad0"})
	if ar.OK || ar.Error != "invalid token" || ar.Code != proto.CodeInvalidToken || cs != nil {
		t.Fatalf("answer: ok %v error %q code %q", ar.OK, ar.Error, ar.Code)
	}
	if ar.RelayVersion != "" {
		t.Fatal("a refused token learns nothing about the relay")
	}
}

func TestHandshake_MinimumClientVersion(t *testing.T) {
	cases := []struct {
		min, client string
		ok          bool
	}{
		{"0.7.0", "0.6.9", false},
		{"v0.7.0", "v0.6.0", false},
		{"0.7.0", "0.7.0", true},
		{"0.7.0", "v0.7.1", true},
		{"0.7.0", "1.0.0", true},
		{"0.10.0", "0.9.0", false},
		// A client that names no version, or one that cannot be compared,
		// is let in.
		{"0.7.0", "", true},
		{"0.7.0", "develop", true},
		{"0.7.0", "0.6.0-rc1", true},
		{"0.7.0", "9.9", true},
		{"", "0.1.0", true},
	}
	for _, c := range cases {
		t.Run(c.min+"/"+c.client, func(t *testing.T) {
			ar, cs := handshake(t, fakeAuth{uid: "u1"}, HandshakeOptions{MinClientVersion: c.min},
				proto.AuthRequest{ProtocolVersion: 1, Token: "bur_test_0000", ClientVersion: c.client})
			if ar.OK != c.ok || (cs != nil) != c.ok {
				t.Fatalf("ok = %v, session = %v, want %v", ar.OK, cs != nil, c.ok)
			}
			if c.ok {
				return
			}
			want := strings.TrimPrefix(c.min, "v")
			if ar.Code != proto.CodeClientTooOld || !strings.Contains(ar.Error, want) || ar.SessionID != "" {
				t.Fatalf("refusal: code %q error %q", ar.Code, ar.Error)
			}
		})
	}
}

// The version is checked after the token: only a client that may connect
// learns that it is too old.
func TestHandshake_BadTokenComesBeforeTheVersion(t *testing.T) {
	ar, _ := handshake(t, fakeAuth{err: errors.New("nope")}, HandshakeOptions{MinClientVersion: "0.7.0"},
		proto.AuthRequest{ProtocolVersion: 1, Token: "bur_test_bad0", ClientVersion: "0.1.0"})
	if ar.Code != proto.CodeInvalidToken {
		t.Fatalf("code %q", ar.Code)
	}
}

// What is wrong on the control stream is answered with an error that has a code.
func TestControlErrorsCarryACode(t *testing.T) {
	cli, srv := dialPair()
	defer cli.Close()
	go func() { _, _ = HandleHandshake(srv, fakeAuth{uid: "u1"}, "sid") }()
	_ = proto.WriteMessage(cli, proto.MsgPing, proto.Ping{})
	var env proto.Envelope
	_ = cli.SetReadDeadline(time.Now().Add(time.Second))
	if err := proto.ReadFrame(cli, &env); err != nil || env.Type != proto.MsgError {
		t.Fatalf("want error message, got %v err=%v", env.Type, err)
	}
	var e proto.Error
	_ = proto.DecodePayload(env, &e)
	if e.Message != "expected auth_request" || e.Code != proto.CodeBadRequest {
		t.Fatalf("error: %+v", e)
	}
}
