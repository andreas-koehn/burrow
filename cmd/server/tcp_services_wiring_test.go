package main

// Native tcp, P01: a tcp tunnel is a service. The resolver creates and reuses
// tcp service rows and keeps their ports; a name has one type; the live
// lookups of the API see tcp tunnels; tcp visitors reach the connection log.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/server"
)

// The resolver is a TCPServiceResolver.
var _ server.TCPServiceResolver = serviceResolverAdapter{}

// A tcp registration creates a tcp service in mode open, with a generated
// slug (for forwards; TestResolveTCP_Slugs), and the next one finds it. The port it holds comes back with it, and Created
// stays true until a client served the service once.
func TestResolveTCP_CreatesAndReuses(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	ctx := context.Background()
	r, err := a.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Created || r.ServiceID == "" || r.AccessMode != "open" || r.Slug == "" || r.TCPPort != 0 {
		t.Fatalf("created: %+v", r)
	}
	row := servicesOf(t, x, uid)["rdp"]
	if row.ID != r.ServiceID || row.Type != "tcp" || row.AccessMode != "open" || row.Subdomain != r.Slug {
		t.Fatalf("row %+v", row)
	}
	if err := a.SetServiceTCPPort(ctx, r.ServiceID, 9005, true, ""); err != nil {
		t.Fatal(err)
	}
	r2, err := a.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{})
	if err != nil || !r2.Created || r2.ServiceID != r.ServiceID || r2.TCPPort != 9005 || !r2.TCPPortPinned || r2.AccessMode != "open" {
		t.Fatalf("again, never served: %+v %v", r2, err)
	}
	if err := x.TouchServiceLastSeen(ctx, r.ServiceID, "laptop"); err != nil {
		t.Fatal(err)
	}
	if r3, err := a.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{}); err != nil || r3.Created {
		t.Fatalf("again, served: %+v %v", r3, err)
	}
	held, err := a.TCPPortReservations(ctx)
	if h := held[9005]; err != nil || h.ServiceID != r.ServiceID || h.UserID != uid || !h.Pinned || h.LastSeen.IsZero() {
		t.Fatalf("reservations %v %v", held, err)
	}
	other, err := a.ResolveTCP(ctx, uid, "ssh", server.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetServiceTCPPort(ctx, other.ServiceID, 9005, false, ""); !errors.Is(err, server.ErrTCPPortTaken) {
		t.Fatalf("a held port: %v", err)
	}
}

// The ports per user: the configured limit for a user, none for an
// administrator, none when the limit is 0.
func TestResolveTCP_PortLimit(t *testing.T) {
	x, admin := optionsDB(t)
	ctx := context.Background()
	if err := x.CreateUser(ctx, db.User{ID: "u-plain", Email: "u@x.com", PasswordHash: "x", Role: "user", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	a := serviceResolverAdapter{db: x, tcpPortsPerUser: 10}
	if r, err := a.ResolveTCP(ctx, "u-plain", "rdp", server.ResolveOptions{}); err != nil || r.TCPPortLimit != 10 {
		t.Fatalf("user: %+v %v", r, err)
	}
	if r, err := a.ResolveTCP(ctx, admin, "rdp", server.ResolveOptions{}); err != nil || r.TCPPortLimit != 0 {
		t.Fatalf("admin: %+v %v", r, err)
	}
	a.tcpPortsPerUser = 0
	if r, err := a.ResolveTCP(ctx, "u-plain", "rdp", server.ResolveOptions{}); err != nil || r.TCPPortLimit != 0 {
		t.Fatalf("no limit: %+v %v", r, err)
	}
}

// Many registrations at the same moment create one service. (Each reports
// Created until a client served it; the control server lets only one of them
// register, so service.created is announced once.)
func TestResolveTCP_ConcurrentCreation(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x}
	type out struct {
		r   server.Resolved
		err error
	}
	res := make(chan out, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r, err := a.ResolveTCP(context.Background(), uid, "ssh", server.ResolveOptions{})
			res <- out{r, err}
		}()
	}
	id := ""
	for i := 0; i < 8; i++ {
		o := <-res
		if o.err != nil {
			t.Fatalf("registration %d: %v", i, o.err)
		}
		if id == "" {
			id = o.r.ServiceID
		}
		if o.r.ServiceID != id || !o.r.Created {
			t.Fatalf("registration %d: %+v, the others' service %q", i, o.r, id)
		}
	}
	if rows := servicesOf(t, x, uid); len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
}

// A name has one type: a tcp registration of an http service's name is
// refused with name_in_use, and so is an http registration of a tcp
// service's name, through every path of the resolver. Nothing is created.
func TestResolve_NameInUseAcrossTypes(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x, authDomain: "burrow.example.com"}
	ctx := context.Background()
	if _, err := a.ResolveWithOptions(ctx, uid, "web", "http", server.ResolveOptions{}); err != nil {
		t.Fatal(err)
	}
	rdp, err := a.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.CreateService(ctx, db.Service{ID: "direct-1", UserID: uid, Name: "llm", Type: "direct", AccessMode: "api_key"}); err != nil {
		t.Fatal(err)
	}

	_, err = a.ResolveTCP(ctx, uid, "web", server.ResolveOptions{})
	if msg := refusal(t, err, proto.CodeNameInUse); msg != `the name "web" belongs to an http service; choose another name` {
		t.Fatalf("tcp on an http name: %q", msg)
	}
	_, err = a.ResolveTCP(ctx, uid, "llm", server.ResolveOptions{})
	if msg := refusal(t, err, proto.CodeNameInUse); msg != `the name "llm" belongs to an AI provider; choose another name` {
		t.Fatalf("tcp on a provider name: %q", msg)
	}
	for _, o := range []server.ResolveOptions{{}, {Slug: "my-rdp", Access: "api_key"}} {
		_, err = a.ResolveWithOptions(ctx, uid, "rdp", "http", o)
		if msg := refusal(t, err, proto.CodeNameInUse); msg != `the name "rdp" belongs to a tcp service; choose another name` {
			t.Fatalf("http on a tcp name (%+v): %q", o, msg)
		}
	}
	_, _, err = a.Resolve(ctx, uid, "rdp", "http")
	refusal(t, err, proto.CodeNameInUse)

	rows := servicesOf(t, x, uid)
	if len(rows) != 3 || rows["web"].Type != "http" || rows["rdp"].Type != "tcp" || rows["rdp"].Subdomain != rdp.Slug {
		t.Fatalf("rows %+v", rows)
	}
}

// fakeTunnelSource is a registryTunnelSource over a fixed set of tunnels.
type fakeTunnelSource struct {
	tunnels  []*server.Tunnel
	sessions []server.SessionSnapshot
}

func (f *fakeTunnelSource) Tunnels() []*server.Tunnel                  { return f.tunnels }
func (f *fakeTunnelSource) SnapshotSessions() []server.SessionSnapshot { return f.sessions }
func (f *fakeTunnelSource) LookupSessionByTunnelID(id string) (string, string, bool) {
	for _, ss := range f.sessions {
		for _, tv := range ss.Tunnels {
			if tv.ID == id {
				return ss.SessionID, ss.UserID, true
			}
		}
	}
	return "", "", false
}

// The API's live lookups see a tcp tunnel: by service (with its port, which
// is what makes a delete of a connected tcp service a 409) and by tunnel.
func TestLiveTunnelLookupAdapter_TCP(t *testing.T) {
	tn := &server.Tunnel{ID: "tn-tcp", ServiceID: "svc-rdp", LocalAddr: "127.0.0.1:3389", RemotePort: 9007}
	a := liveTunnelLookupAdapter{srv: &fakeTunnelSource{
		tunnels:  []*server.Tunnel{tn},
		sessions: []server.SessionSnapshot{{SessionID: "sess-1", UserID: "u-1", Tunnels: []server.TunnelView{{ID: "tn-tcp"}}}},
	}}
	snap, ok := a.LookupByServiceID("svc-rdp")
	if !ok || !snap.Connected || snap.RemotePort != 9007 || snap.LocalAddr != "127.0.0.1:3389" || snap.SessionID != "sess-1" {
		t.Fatalf("by service: %+v %v", snap, ok)
	}
	loc, ok := a.LookupByTunnelID("tn-tcp")
	if !ok || loc.ServiceID != "svc-rdp" || loc.UserID != "u-1" {
		t.Fatalf("by tunnel: %+v %v", loc, ok)
	}
}

// The control server's connection-log entries become connection_logs rows.
func TestServerConnLogAdapter_WritesRows(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	if err := x.CreateService(ctx, db.Service{ID: "svc-rdp", UserID: uid, Name: "rdp", Type: "tcp", AccessMode: "open"}); err != nil {
		t.Fatal(err)
	}
	a := serverConnLogAdapter{sink: connlog.NewSQLSink(x, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	start := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Millisecond)
	a.RecordConnection(ctx, server.ConnLogEntry{
		Kind: "tcp_proxy", ServiceID: "svc-rdp", TunnelID: "tn-1", UserID: uid, ClientSessionID: "sess-1",
		SourceIP: "203.0.113.9", StartedAt: start, EndedAt: start.Add(1500 * time.Millisecond),
		BytesIn: 11, BytesOut: 22, Status: "closed_clean", Protocol: "tls", SNI: "rdp.example.com",
	})
	var rows []db.ConnectionLog
	deadline := time.Now().Add(5 * time.Second)
	for len(rows) == 0 && time.Now().Before(deadline) {
		var err error
		rows, err = connlog.ListConnectionLogs(ctx, x, connlog.ConnLogQuery{Kind: "tcp_proxy"})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[0]
	if r.ServiceID != "svc-rdp" || r.TunnelID != "tn-1" || r.UserID != uid || r.ClientSessionID != "sess-1" ||
		r.SourceIP != "203.0.113.9" || r.BytesIn != 11 || r.BytesOut != 22 || r.Status != "closed_clean" || r.DurationMs != 1500 ||
		r.Protocol != "tls" || r.SNI != "rdp.example.com" {
		t.Fatalf("row %+v", r)
	}
}

// The whole relay: a tcp tunnel is listed as a service with its port, a
// delete is refused while it is served, its visitor's bytes are on the tunnel
// while the connection is open and in a connection-log row afterwards, and
// the service can be deleted once the client is gone.
func TestServe_TCPTunnelIsAService(t *testing.T) {
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
	var token struct {
		Token string `json:"token"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "tcp-client"}, http.StatusCreated, &token)
	// No port asked for, as an old client sends it.
	stop := r.connectWith(token.Token, client.TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: echo.Addr().String()})

	type svc struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Type       string `json:"type"`
		URL        string `json:"url"`
		AccessMode string `json:"access_mode"`
		Connected  bool   `json:"connected"`
		RemotePort int    `json:"remote_port"`
		Status     string `json:"status"`
	}
	find := func() (svc, bool) {
		var rows []svc
		r.mustAdmin(http.MethodGet, "/api/v1/services", nil, http.StatusOK, &rows)
		for _, s := range rows {
			if s.Name == "rdp" {
				return s, true
			}
		}
		return svc{}, false
	}
	s, ok := find()
	if !ok || s.Type != "tcp" || s.AccessMode != "open" || !s.Connected || s.RemotePort != r.tcpPort || s.URL != "" || s.Status != "online" {
		t.Fatalf("GET /services: %+v %v", s, ok)
	}
	if tn := r.tunnels()["rdp"]; tn.ServiceID != s.ID {
		t.Fatalf("GET /tunnels names service %q, want %q", tn.ServiceID, s.ID)
	}
	if code, body := r.admin(http.MethodDelete, "/api/v1/services/"+s.ID, nil); code != http.StatusConflict {
		t.Fatalf("delete while served: %d %s", code, body)
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.tcpPort)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo %q %v", line, err)
	}
	// Live: on the tunnel before the connection ends.
	r.eventually("live bytes on the tunnel", func() bool {
		tn := r.tunnels()["rdp"]
		return tn.BytesIn == 5 && tn.BytesOut == 5
	})
	_ = conn.Close()

	type logRow struct {
		Kind      string `json:"kind"`
		ServiceID string `json:"service_id"`
		SourceIP  string `json:"source_ip"`
		BytesIn   int64  `json:"bytes_in"`
		BytesOut  int64  `json:"bytes_out"`
		Status    string `json:"status"`
	}
	var rows []logRow
	r.eventually("a tcp_proxy row for the service", func() bool {
		r.mustAdmin(http.MethodGet, fmt.Sprintf("/api/v1/connection-logs?kind=tcp_proxy&service_id=%s", s.ID), nil, http.StatusOK, &rows)
		return len(rows) == 1
	})
	if g := rows[0]; g.SourceIP != "127.0.0.1" || g.BytesIn != 5 || g.BytesOut != 5 || g.Status != "closed_clean" {
		t.Fatalf("row %+v", g)
	}

	stop()
	r.eventually("the relay lists no tunnel", func() bool { return len(r.tunnels()) == 0 })
	if s, ok := find(); !ok || s.Connected || s.Status != "offline" {
		t.Fatalf("after the client left: %+v %v", s, ok)
	}
	r.mustAdmin(http.MethodDelete, "/api/v1/services/"+s.ID, nil, http.StatusNoContent, nil)
	if _, ok := find(); ok {
		t.Fatal("the service is still listed")
	}
}

// No door of the proxy reaches a tcp service, even one that has a slug: not
// its host, not /svc/<slug>, not a lookup by service id (custom domains, the
// gateway). The tcp tunnel fronts an http app here, so a leak would answer.
func TestProxy_NeverResolvesATCPService(t *testing.T) {
	s := bootE2EStack(t)
	ctx := context.Background()
	tok, err := s.store.IssueClientToken(ctx, s.userID, "tcp-guard")
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(client.Options{
		Server: s.server.Addr(), Token: tok, RootCAs: s.caPool, ServerName: "localhost",
		Tunnels: []client.TunnelSpec{{Name: "rdpguard", Type: "tcp", LocalAddr: s.upstreamAddr}},
		Logger:  s.log,
	})
	go func() { _ = c.Run(s.ctx) }()
	x := db.Wrap(s.db)
	var id string
	deadline := time.Now().Add(10 * time.Second)
	for id == "" && time.Now().Before(deadline) {
		if svc, ok := servicesOf(t, x, s.userID)["rdpguard"]; ok {
			if _, live := s.server.LookupTunnelByServiceID(svc.ID); live {
				id = svc.ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("the tcp tunnel never registered")
	}
	// The slug it was given (P04: every tcp service has one, for forwards),
	// and one set by hand.
	generated := servicesOf(t, x, s.userID)["rdpguard"].Subdomain
	if generated == "" {
		t.Fatal("the tcp service has no slug")
	}
	d := proxyDialerAdapter{st: s.store, srv: s.server}
	refused := func(slug string) {
		t.Helper()
		for name, get := range map[string]func() (*http.Response, error){
			"host": func() (*http.Response, error) {
				return s.visitorClient(t).Get("https://" + slug + "." + e2eAuthDomain + ":" + s.proxyPort + "/")
			},
			"/svc/": func() (*http.Response, error) {
				return s.pathClient(t).Get("https://" + e2eAuthDomain + "/svc/" + slug + "/")
			},
		} {
			resp, err := get()
			if err != nil {
				t.Fatalf("%s %s: %v", slug, name, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound || strings.Contains(string(body), "upstream got") {
				t.Fatalf("%s %s: %d %s", slug, name, resp.StatusCode, body)
			}
		}
		if _, err := d.Lookup(ctx, slug); !errors.Is(err, proxy.ErrNotFound) {
			t.Fatalf("Lookup %s: %v", slug, err)
		}
		// The login gate looks services up by slug too: a tcp one is none.
		if _, err := s.store.ServiceForSubdomain(ctx, slug); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("ServiceForSubdomain %s: %v", slug, err)
		}
	}
	refused(generated)
	if err := x.SetServiceSubdomain(ctx, id, "tcpguard"); err != nil {
		t.Fatal(err)
	}
	refused("tcpguard")
	if _, err := d.LookupByServiceID(ctx, id); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("LookupByServiceID: %v", err)
	}
	if _, err := (ingressDialerAdapter{d}).LookupByServiceID(ctx, id); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("ingress LookupByServiceID: %v", err)
	}
	if _, err := d.DialTunnelStreamByServiceID(ctx, id); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("DialTunnelStreamByServiceID: %v", err)
	}
	if _, err := d.DialTunnelStream(ctx, "tcpguard"); !errors.Is(err, proxy.ErrNotFound) {
		t.Fatalf("DialTunnelStream: %v", err)
	}
}

// A tcp port taken from an offline service is an audit row that names both
// services, the port and the reason; the subject is the service that lost it.
func TestObserver_TCPPortReclaimedIsAudited(t *testing.T) {
	o, _, _, au := newTestObserver(t)
	o.ObserveSession(observe.Session{
		Kind: observe.TCPPortReclaimed, UserID: "u-1", SessionID: "sess-1", TunnelType: "tcp",
		ServiceID: "svc-fixed", FromServiceID: "svc-away", Port: 9010, Reason: observe.TCPPortTakeover,
	})
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.got) != 1 {
		t.Fatalf("audit %+v", au.got)
	}
	e := au.got[0]
	var p map[string]any
	_ = json.Unmarshal(e.Payload, &p)
	if e.Action != audit.ActionServiceTCPPortReclaimed || e.ActorID != "u-1" || e.SubjectID != "svc-away" || e.Result != "ok" ||
		p["service_id"] != "svc-fixed" || p["from_service_id"] != "svc-away" || p["port"] != float64(9010) || p["reason"] != "takeover" {
		t.Fatalf("audit %+v payload %v", e, p)
	}
	if !slices.Contains(audit.AllActions, audit.ActionServiceTCPPortReclaimed) || audit.ActionServiceTCPPortReclaimed != "service.tcp_port.reclaimed" {
		t.Fatal("service.tcp_port.reclaimed is not a listed action")
	}
}
