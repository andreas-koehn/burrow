package main

// Native tcp, P04: forward streams. A signed-in client reaches a private tcp
// service through the relay; only the owner and the roles the service allows
// get through; every stream is a live connection, a connection-log row and
// (once a minute per user and service) an audit row.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

var _ server.ForwardAuthorizer = forwardAuthorizer{}

// The authorizer finds a tcp service by id, by the caller's own name and by
// slug; the owner gets through, another user only with a role in the
// policy; a suspended user, an http service and an unknown target are not
// found, and so is another user's name that is not the caller's.
func TestForwardAuthorizer(t *testing.T) {
	x, admin := optionsDB(t)
	ctx := context.Background()
	for _, u := range []db.User{
		{ID: "u-bob", Email: "bob@x.com", PasswordHash: "x", Role: "user", Status: "active"},
		{ID: "u-eve", Email: "eve@x.com", PasswordHash: "x", Role: "user", Status: "suspended"},
	} {
		if err := x.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := x.UpdateUserStatus(ctx, u.ID, u.Status); err != nil {
			t.Fatal(err)
		}
	}
	res := serviceResolverAdapter{db: x}
	rdp, err := res.ResolveTCP(ctx, admin, "rdp", server.ResolveOptions{Access: "private"})
	if err != nil || rdp.Slug == "" {
		t.Fatalf("resolve: %+v %v", rdp, err)
	}
	if _, err := x.GetOrCreateService(ctx, admin, "web", "http"); err != nil {
		t.Fatal(err)
	}
	a := forwardAuthorizer{db: x}
	for _, target := range []string{"rdp", rdp.Slug, rdp.ServiceID} {
		got, err := a.AuthorizeForward(ctx, admin, target)
		if err != nil || got.ServiceID != rdp.ServiceID || got.Name != "rdp" {
			t.Fatalf("owner by %q: %+v %v", target, got, err)
		}
	}
	for _, target := range []string{"web", "nope", "00000000-0000-0000-0000-000000000000"} {
		if _, err := a.AuthorizeForward(ctx, admin, target); !errors.Is(err, server.ErrForwardNotFound) {
			t.Fatalf("owner, %q: %v", target, err)
		}
	}
	// Bob has no service named rdp, and may not reach the admin's.
	if _, err := a.AuthorizeForward(ctx, "u-bob", "rdp"); !errors.Is(err, server.ErrForwardNotFound) {
		t.Fatalf("bob by name: %v", err)
	}
	for _, target := range []string{rdp.Slug, rdp.ServiceID} {
		got, err := a.AuthorizeForward(ctx, "u-bob", target)
		if !errors.Is(err, server.ErrForwardDenied) || got.ServiceID != rdp.ServiceID {
			t.Fatalf("bob by %q: %+v %v", target, got, err)
		}
	}
	if err := x.SetAccessPolicy(ctx, rdp.ServiceID, []string{"user"}); err != nil {
		t.Fatal(err)
	}
	if got, err := a.AuthorizeForward(ctx, "u-bob", rdp.Slug); err != nil || got.ServiceID != rdp.ServiceID {
		t.Fatalf("bob with his role allowed: %+v %v", got, err)
	}
	if _, err := a.AuthorizeForward(ctx, "u-eve", rdp.Slug); !errors.Is(err, server.ErrForwardNotFound) {
		t.Fatalf("suspended: %v", err)
	}
	if _, err := a.AuthorizeForward(ctx, "u-gone", rdp.Slug); !errors.Is(err, server.ErrForwardNotFound) {
		t.Fatalf("no such user: %v", err)
	}
}

// A tcp service gets a generated slug when it is created, and a row from
// before without one gets one at its next registration. Its slug never makes
// an http address: not in the API's url, not in /tunnels, not through the
// login gate's lookup.
func TestResolveTCP_Slugs(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	a := serviceResolverAdapter{db: x}
	r, err := a.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{})
	if err != nil || r.Slug == "" {
		t.Fatalf("created: %+v %v", r, err)
	}
	if row := servicesOf(t, x, uid)["rdp"]; row.Subdomain != r.Slug {
		t.Fatalf("row slug %q, resolved %q", row.Subdomain, r.Slug)
	}
	if again, _ := a.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{}); again.Slug != r.Slug {
		t.Fatalf("the slug moved: %q then %q", r.Slug, again.Slug)
	}
	// A row from before (no slug).
	if err := x.CreateService(ctx, db.Service{ID: "svc-old", UserID: uid, Name: "ssh", Type: "tcp", AccessMode: "open"}); err != nil {
		t.Fatal(err)
	}
	old, err := a.ResolveTCP(ctx, uid, "ssh", server.ResolveOptions{})
	if err != nil || old.ServiceID != "svc-old" || old.Slug == "" || old.Slug == r.Slug {
		t.Fatalf("backfilled: %+v %v", old, err)
	}
	if row := servicesOf(t, x, uid)["ssh"]; row.Subdomain != old.Slug {
		t.Fatalf("row slug %q", row.Subdomain)
	}
	st := store.New(x.DB())
	if _, err := st.ServiceForSubdomain(ctx, r.Slug); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("ServiceForSubdomain of a tcp slug: %v", err)
	}
	lister := tunnelListerAdapter{slugs: st, authDomain: "burrow.example.com"}
	if u := lister.tunnelURL(r.ServiceID); u != "" {
		t.Fatalf("tunnel url of a tcp service %q", u)
	}
}

// fwdSession is a signed-in client that holds no tunnel and opens forward
// streams, as `burrow forward` does.
type fwdSession struct {
	conn net.Conn
	sess *yamux.Session
	caps []string
}

func dialForwarder(t *testing.T, r *obsRelay, token string) *fwdSession {
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
	if _, err := sess.OpenStream(); err != nil { // the control stream comes first
		t.Fatal(err)
	}
	f := &fwdSession{conn: conn, sess: sess, caps: ar.Capabilities}
	t.Cleanup(func() { _ = sess.Close(); _ = conn.Close() })
	return f
}

func (f *fwdSession) open(t *testing.T, target string) (*yamux.Stream, proto.ForwardOpenResponse) {
	t.Helper()
	st, err := f.sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteMessage(st, proto.MsgForwardOpen, proto.ForwardOpen{Target: target}); err != nil {
		t.Fatal(err)
	}
	_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
	var env proto.Envelope
	var resp proto.ForwardOpenResponse
	if err := proto.ReadFrame(st, &env); err != nil || env.Type != proto.MsgForwardOpenResp || proto.DecodePayload(env, &resp) != nil {
		t.Fatalf("answer to %q: %v %s", target, err, env.Type)
	}
	_ = st.SetReadDeadline(time.Time{})
	return st, resp
}

// signIn logs a user in to the dashboard and returns a client of theirs and
// its CSRF token.
func signIn(t *testing.T, base, email, password string) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := hc.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login %s: %v %v", email, resp, err)
	}
	_ = resp.Body.Close()
	u, _ := url.Parse(base)
	for _, ck := range jar.Cookies(u) {
		if ck.Name == "burrow_csrf" {
			return hc, ck.Value
		}
	}
	t.Fatal("no CSRF cookie")
	return nil, ""
}

// The whole relay, with a home client (an echo target) and forwarding
// sessions: bytes go both ways through a private service by name, slug and
// id; the stream is a live connection of the service and a forward of the
// session; it ends as a tcp_forward row with the user, the token name and
// the address; three streams write one audit row. Another user is told
// not_found until the service's policy allows their role. A connection
// closed in the dashboard ends; a service nobody serves is offline.
func TestServe_TCPForward(t *testing.T) {
	r := bootObsRelay(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	token := func(name string) string {
		var tok struct {
			Token string `json:"token"`
		}
		r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": name}, http.StatusCreated, &tok)
		return tok.Token
	}
	homeTok, laptopTok := token("home"), token("laptop")
	stop := r.connectWith(homeTok, client.TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: echo.Addr().String()})
	defer stop()

	type svc struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Slug       string `json:"slug"`
		URL        string `json:"url"`
		AccessMode string `json:"access_mode"`
		Connected  bool   `json:"connected"`
		RemotePort int    `json:"remote_port"`
	}
	find := func() svc {
		var rows []svc
		r.mustAdmin(http.MethodGet, "/api/v1/services", nil, http.StatusOK, &rows)
		for _, s := range rows {
			if s.Name == "rdp" {
				return s
			}
		}
		return svc{}
	}
	first := r.tunnels()["rdp"].ID
	s := find()
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+s.ID+"/access-mode", map[string]string{"access_mode": "private"}, http.StatusNoContent, nil)
	r.eventually("the client registers again in mode private", func() bool {
		tn, ok := r.tunnels()["rdp"]
		return ok && tn.ID != first
	})
	if s = find(); s.AccessMode != "private" || !s.Connected || s.RemotePort != 0 || s.Slug == "" || s.URL != "" {
		t.Fatalf("private service: %+v", s)
	}
	if tn := r.tunnels()["rdp"]; tn.ServiceID != s.ID || tn.URL != "" {
		t.Fatalf("GET /tunnels: %+v", tn)
	}

	laptop := dialForwarder(t, r, laptopTok)
	if !slices.Contains(laptop.caps, proto.CapForward) || !slices.Contains(laptop.caps, proto.CapTCPAccess) {
		t.Fatalf("capabilities %v", laptop.caps)
	}
	var streams []*yamux.Stream
	for _, target := range []string{"rdp", s.Slug, s.ID} {
		st, resp := laptop.open(t, target)
		if !resp.OK || resp.ServiceID != s.ID || resp.Name != "rdp" {
			t.Fatalf("forward to %q: %+v", target, resp)
		}
		streams = append(streams, st)
	}
	// Bytes both ways, more than one window of yamux.
	big := make([]byte, 600<<10)
	_, _ = rand.Read(big)
	go func() { _, _ = streams[0].Write(big) }()
	back := make([]byte, len(big))
	_ = streams[0].SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(streams[0], back); err != nil || !bytes.Equal(back, big) {
		t.Fatalf("echo through the forward: %v", err)
	}

	type conn struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		SourceIP  string `json:"source_ip"`
		UserEmail string `json:"user_email"`
		TokenName string `json:"token_name"`
		SessionID string `json:"session_id"`
		BytesIn   int64  `json:"bytes_in"`
		BytesOut  int64  `json:"bytes_out"`
	}
	var conns []conn
	r.mustAdmin(http.MethodGet, "/api/v1/services/"+s.ID+"/connections", nil, http.StatusOK, &conns)
	if len(conns) != 3 {
		t.Fatalf("connections %+v", conns)
	}
	var bigConn conn
	for _, c := range conns {
		if c.Kind != "forward" || c.UserEmail != obsAdminEmail || c.TokenName != "laptop" || c.SourceIP != "127.0.0.1" || c.SessionID == "" {
			t.Fatalf("connection %+v", c)
		}
		if c.BytesIn == int64(len(big)) {
			bigConn = c
		}
	}
	if bigConn.ID == "" || bigConn.BytesOut != int64(len(big)) {
		t.Fatalf("live bytes: %+v", conns)
	}
	type fwd struct {
		Service   string `json:"service"`
		ServiceID string `json:"service_id"`
	}
	var clients []struct {
		SessionID    string `json:"session_id"`
		TokenName    string `json:"token_name"`
		ServiceCount int    `json:"service_count"`
		ForwardCount int    `json:"forward_count"`
		Forwards     []fwd  `json:"forwards"`
	}
	r.mustAdmin(http.MethodGet, "/api/v1/clients", nil, http.StatusOK, &clients)
	seen := false
	for _, c := range clients {
		if c.Forwards == nil {
			t.Fatalf("client without a forwards list: %+v", c)
		}
		if c.SessionID == bigConn.SessionID {
			seen = true
			if c.TokenName != "laptop" || c.ServiceCount != 0 || c.ForwardCount != 3 || c.Forwards[0].Service != "rdp" || c.Forwards[0].ServiceID != s.ID {
				t.Fatalf("forwarding client %+v", c)
			}
		}
	}
	if !seen {
		t.Fatalf("the forwarding session is not a client: %+v", clients)
	}

	// Closed in the dashboard: the stream ends.
	r.mustAdmin(http.MethodDelete, "/api/v1/services/"+s.ID+"/connections/"+bigConn.ID, nil, http.StatusNoContent, nil)
	_ = streams[0].SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := streams[0].Read(make([]byte, 1)); err == nil {
		t.Fatal("the closed connection still reads")
	}
	for _, st := range streams[1:] {
		_ = st.Close()
	}

	type logRow struct {
		Kind            string `json:"kind"`
		UserID          string `json:"user_id"`
		TokenName       string `json:"token_name"`
		SourceIP        string `json:"source_ip"`
		ClientSessionID string `json:"client_session_id"`
		BytesIn         int64  `json:"bytes_in"`
		Status          string `json:"status"`
		Reason          string `json:"reason"`
	}
	var rows []logRow
	r.eventually("three tcp_forward rows", func() bool {
		r.mustAdmin(http.MethodGet, "/api/v1/connection-logs?kind=tcp_forward&service_id="+s.ID, nil, http.StatusOK, &rows)
		return len(rows) == 3
	})
	var adminID string
	for _, row := range rows {
		adminID = row.UserID
		if row.TokenName != "laptop" || row.SourceIP != "127.0.0.1" || row.ClientSessionID != bigConn.SessionID || row.Status != "closed_clean" || row.UserID == "" {
			t.Fatalf("row %+v", row)
		}
	}
	type auditRow struct {
		Action    string          `json:"action"`
		ActorID   string          `json:"actor_id"`
		SubjectID string          `json:"subject_id"`
		Payload   json.RawMessage `json:"payload"`
	}
	var audits []auditRow
	r.eventually("the forward's audit row", func() bool {
		r.mustAdmin(http.MethodGet, "/api/v1/audit/events?action="+audit.ActionTCPForwardOpen, nil, http.StatusOK, &audits)
		return len(audits) >= 1
	})
	if len(audits) != 1 || audits[0].ActorID != adminID || audits[0].SubjectID != s.ID || !strings.Contains(string(audits[0].Payload), `"token_name":"laptop"`) {
		t.Fatalf("audit (one a minute per user and service): %+v", audits)
	}

	// Another user: not found, until the policy allows their role.
	r.mustAdmin(http.MethodPost, "/api/v1/users", map[string]string{"email": "bob@x.com", "password": "password-bob-1", "role": "user"}, http.StatusCreated, nil)
	bob, bobCSRF := signIn(t, r.base, "bob@x.com", "password-bob-1")
	confirmPassword(t, bob, r.base, bobCSRF, "password-bob-1")
	req, _ := http.NewRequest(http.MethodPost, r.base+"/api/v1/tokens", strings.NewReader(`{"name":"bob-laptop"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", bobCSRF)
	resp, err := bob.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("bob's token: %v %v", resp, err)
	}
	var bobTok struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&bobTok)
	_ = resp.Body.Close()
	bobFwd := dialForwarder(t, r, bobTok.Token)
	_, denied := bobFwd.open(t, s.Slug)
	_, absent := bobFwd.open(t, "no-such-slug")
	if denied.OK || denied.Code != proto.CodeNotFound || absent.Code != proto.CodeNotFound ||
		strings.ReplaceAll(absent.Error, "no-such-slug", s.Slug) != denied.Error {
		t.Fatalf("another user: %+v / %+v", denied, absent)
	}
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+s.ID+"/access-policy", map[string]any{"roles": []string{"user"}}, http.StatusNoContent, nil)
	st, ok := bobFwd.open(t, s.Slug)
	if !ok.OK {
		t.Fatalf("bob with his role allowed: %+v", ok)
	}
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := st.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if got := make([]byte, 4); func() error { _, err := io.ReadFull(st, got); return err }() != nil || string(got) != "ping" {
		t.Fatalf("bob's echo %q", got)
	}
	_ = st.Close()

	// Nobody serves it: offline.
	stop()
	r.eventually("the relay lists no tunnel", func() bool { return len(r.tunnels()) == 0 })
	if _, off := laptop.open(t, "rdp"); off.OK || off.Code != proto.CodeOffline {
		t.Fatalf("offline: %+v", off)
	}
}

// One audit row per user and service a minute, whatever the number of
// streams; another user or service is a row of its own.
func TestObserver_ForwardOpenedIsAuditedOncePerMinute(t *testing.T) {
	o, _, _, au := newTestObserver(t)
	ev := observe.Session{Kind: observe.ForwardOpened, UserID: "u-1", SessionID: "s-1", TokenID: "tok-1",
		TokenName: "laptop", SourceIP: "198.51.100.4", ServiceID: "svc-rdp", OwnerID: "u-0", TunnelID: "tn-1"}
	for i := 0; i < 5; i++ {
		o.ObserveSession(ev)
	}
	other := ev
	other.UserID = "u-2"
	o.ObserveSession(other)
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.got) != 2 {
		t.Fatalf("audit %+v", au.got)
	}
	e := au.got[0]
	var p map[string]any
	_ = json.Unmarshal(e.Payload, &p)
	if e.Action != audit.ActionTCPForwardOpen || e.ActorID != "u-1" || e.SubjectID != "svc-rdp" || e.SourceIP != "198.51.100.4" ||
		p["token_name"] != "laptop" || p["owner_id"] != "u-0" || p["session_id"] != "s-1" {
		t.Fatalf("audit %+v payload %v", e, p)
	}
	if !slices.Contains(audit.AllActions, audit.ActionTCPForwardOpen) || !slices.Contains(audit.AllActions, audit.ActionServiceConnectionClose) {
		t.Fatal("the forward actions are not listed")
	}
}

// policyCounter is a forwardDB that counts the policy reads.
type policyCounter struct {
	forwardDB
	reads int
}

func (p *policyCounter) GetAccessPolicy(ctx context.Context, id string) ([]string, error) {
	p.reads++
	return p.forwardDB.GetAccessPolicy(ctx, id)
}

// A refusal costs the same whether the service is absent or denied: both
// read a policy before not_found. An administrator who does not own the
// service is a user like another: denied unless the policy names its role.
// The owner is told it owns it.
func TestForwardAuthorizer_TimingOwnerAndAdmins(t *testing.T) {
	x, owner := optionsDB(t)
	ctx := context.Background()
	if err := x.CreateUser(ctx, db.User{ID: "u-admin2", Email: "a2@x.com", PasswordHash: "x", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := x.CreateUser(ctx, db.User{ID: "u-bob", Email: "bob@x.com", PasswordHash: "x", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	rdp, err := serviceResolverAdapter{db: x}.ResolveTCP(ctx, owner, "rdp", server.ResolveOptions{Access: "private"})
	if err != nil {
		t.Fatal(err)
	}
	pc := &policyCounter{forwardDB: x}
	a := forwardAuthorizer{db: pc}
	if _, err := a.AuthorizeForward(ctx, "u-bob", "no-such-slug"); !errors.Is(err, server.ErrForwardNotFound) || pc.reads != 1 {
		t.Fatalf("absent: %v, %d policy reads", err, pc.reads)
	}
	if _, err := a.AuthorizeForward(ctx, "u-bob", rdp.Slug); !errors.Is(err, server.ErrForwardDenied) || pc.reads != 2 {
		t.Fatalf("denied: %v, %d policy reads", err, pc.reads)
	}
	if _, err := a.AuthorizeForward(ctx, "u-admin2", rdp.Slug); !errors.Is(err, server.ErrForwardDenied) {
		t.Fatalf("a non-owner admin: %v", err)
	}
	if got, err := a.AuthorizeForward(ctx, owner, "rdp"); err != nil || !got.Owner {
		t.Fatalf("owner: %+v %v", got, err)
	}
	if err := x.SetAccessPolicy(ctx, rdp.ServiceID, []string{"user"}); err != nil {
		t.Fatal(err)
	}
	if got, err := a.AuthorizeForward(ctx, "u-bob", rdp.Slug); err != nil || got.Owner {
		t.Fatalf("allowed user: %+v %v", got, err)
	}
}

// A forward stream lives no longer than its credentials: revoking the token
// of the session ends it, and so does suspending the user.
func TestServe_TCPForwardEndsWithItsCredentials(t *testing.T) {
	r := bootObsRelay(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	mint := func(name string) string {
		var tok struct {
			Token string `json:"token"`
		}
		r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": name}, http.StatusCreated, &tok)
		return tok.Token
	}
	homeTok, laptopTok := mint("home"), mint("laptop")
	stop := r.connectWith(homeTok, client.TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: echo.Addr().String()})
	defer stop()
	alive := func(st *yamux.Stream) bool {
		_ = st.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := st.Write([]byte("x")); err != nil {
			return false
		}
		_, err := io.ReadFull(st, make([]byte, 1))
		return err == nil
	}
	ended := func(st *yamux.Stream) bool {
		_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err := st.Read(make([]byte, 1))
		return err != nil
	}

	laptop := dialForwarder(t, r, laptopTok)
	st, resp := laptop.open(t, "rdp")
	if !resp.OK || !alive(st) {
		t.Fatalf("open: %+v", resp)
	}
	var tokens []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	r.mustAdmin(http.MethodGet, "/api/v1/tokens", nil, http.StatusOK, &tokens)
	for _, tk := range tokens {
		if tk.Name == "laptop" {
			r.mustAdmin(http.MethodDelete, "/api/v1/tokens/"+tk.ID, nil, http.StatusNoContent, nil)
		}
	}
	if !ended(st) {
		t.Fatal("the forward outlived its revoked token")
	}

	var slug, serviceID string
	var rows []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	r.mustAdmin(http.MethodGet, "/api/v1/services", nil, http.StatusOK, &rows)
	for _, s := range rows {
		if s.Name == "rdp" {
			slug, serviceID = s.Slug, s.ID
		}
	}
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+serviceID+"/access-policy", map[string]any{"roles": []string{"user"}}, http.StatusNoContent, nil)
	var bob struct {
		ID string `json:"id"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/users", map[string]string{"email": "bob@x.com", "password": "password-bob-1", "role": "user"}, http.StatusCreated, &bob)
	hc, csrf := signIn(t, r.base, "bob@x.com", "password-bob-1")
	confirmPassword(t, hc, r.base, csrf, "password-bob-1")
	req, _ := http.NewRequest(http.MethodPost, r.base+"/api/v1/tokens", strings.NewReader(`{"name":"bob-laptop"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	hr, err := hc.Do(req)
	if err != nil || hr.StatusCode != http.StatusCreated {
		t.Fatalf("bob's token: %v %v", hr, err)
	}
	var bobTok struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(hr.Body).Decode(&bobTok)
	_ = hr.Body.Close()
	bst, bresp := dialForwarder(t, r, bobTok.Token).open(t, slug)
	if !bresp.OK || !alive(bst) {
		t.Fatalf("bob: %+v", bresp)
	}
	r.mustAdmin(http.MethodPatch, "/api/v1/users/"+bob.ID, map[string]string{"status": "suspended"}, http.StatusNoContent, nil)
	if !ended(bst) {
		t.Fatal("the forward outlived its suspended user")
	}
}
