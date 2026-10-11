package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

type fakeSnapshotter struct{ sessions []server.SessionSnapshot }

func (f fakeSnapshotter) SnapshotSessions() []server.SessionSnapshot { return f.sessions }

type fakeTunnelGetter struct{ rows map[string]db.Tunnel }

func (f fakeTunnelGetter) GetTunnel(_ context.Context, id string) (db.Tunnel, error) {
	r, ok := f.rows[id]
	if !ok {
		return db.Tunnel{}, db.ErrNotFound
	}
	return r, nil
}

func TestClientsAdapter(t *testing.T) {
	snap := fakeSnapshotter{sessions: []server.SessionSnapshot{{
		SessionID: "c1", UserID: "u1", RemoteAddr: "1.1.1.1:5",
		OS: "linux", Arch: "amd64", ClientVersion: "9.9", Token: "laptop",
		Tunnels: []server.TunnelView{
			{ID: "tn1", Name: "web", Type: "tcp", RemotePort: 9000, LocalAddr: "127.0.0.1:3000", BytesIn: 5, BytesOut: 7, Connected: true},
			{ID: "tn2", Name: "ssh", Type: "tcp", RemotePort: 9001, LocalAddr: "127.0.0.1:22", BytesIn: 1, BytesOut: 2, Connected: true},
		},
	}}}
	tg := fakeTunnelGetter{rows: map[string]db.Tunnel{
		"tn1": {ID: "tn1", TotalBytesIn: 100, TotalBytesOut: 40, AccessMode: "api_key"},
		"tn2": {ID: "tn2", TotalBytesIn: 10, TotalBytesOut: 3, AccessMode: "open"},
	}}
	a := clientsAdapter{srv: snap, st: tg}

	list := a.ListClients()
	if len(list) != 1 {
		t.Fatalf("want 1 client, got %d", len(list))
	}
	cv := list[0]
	if cv.SessionID != "c1" || cv.TokenName != "laptop" || cv.OS != "linux" || cv.ServiceCount != 2 {
		t.Fatalf("client view: %+v", cv)
	}
	if cv.TotalBytesIn != 110 || cv.TotalBytesOut != 43 {
		t.Fatalf("aggregate totals wrong: %+v", cv)
	}

	cd, ok := a.GetClient("c1")
	if !ok || len(cd.Services) != 2 {
		t.Fatalf("detail: ok=%v services=%d", ok, len(cd.Services))
	}
	var tn1 *struct {
		live  uint64
		total int64
		mode  string
	}
	for _, s := range cd.Services {
		if s.ID == "tn1" {
			tn1 = &struct {
				live  uint64
				total int64
				mode  string
			}{s.BytesIn, s.TotalBytesIn, s.AccessMode}
		}
	}
	if tn1 == nil || tn1.live != 5 || tn1.total != 100 || tn1.mode != "api_key" {
		t.Fatalf("tn1 service view wrong: %+v", tn1)
	}

	if _, ok := a.GetClient("nope"); ok {
		t.Fatal("GetClient(nope) must be !ok")
	}
}

func TestClientsAdapterUsesDurableServiceMode(t *testing.T) {
	snap := fakeSnapshotter{sessions: []server.SessionSnapshot{{
		SessionID: "c1", UserID: "u1", Token: "laptop",
		Tunnels: []server.TunnelView{
			{ID: "tn1", Name: "ollama", Type: "http", ServiceID: "svc-ollama", Connected: true},
			{ID: "tn2", Name: "ssh", Type: "tcp", RemotePort: 9001, Connected: true},
		},
	}}}
	tg := fakeTunnelGetter{rows: map[string]db.Tunnel{
		"tn1": {ID: "tn1", AccessMode: "open"},
		"tn2": {ID: "tn2", AccessMode: "open"},
	}}
	a := clientsAdapter{
		srv:      snap,
		st:       tg,
		services: fakeServiceModeGetter{modes: map[string]string{"svc-ollama": "api_key"}},
	}
	cd, ok := a.GetClient("c1")
	if !ok || len(cd.Services) != 2 {
		t.Fatalf("detail: ok=%v services=%d", ok, len(cd.Services))
	}
	if cd.Services[0].AccessMode != "api_key" || cd.Services[0].ServiceID != "svc-ollama" {
		t.Fatalf("http service: want api_key/svc-ollama, got %+v", cd.Services[0])
	}
	if cd.Services[1].AccessMode != "open" || cd.Services[1].ServiceID != "" {
		t.Fatalf("tcp service: want open/empty service id, got %+v", cd.Services[1])
	}
}

type fakeTokenLister struct {
	tokens []db.ClientToken
	err    error
}

func (f fakeTokenLister) ListAllClientTokens(context.Context) ([]db.ClientToken, error) {
	return f.tokens, f.err
}

// The adapter hands the join every token and every live session with the id
// of its token, when it connected and the names of its tunnels.
func TestClientsAdapterListKnownClients(t *testing.T) {
	seen := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	snap := fakeSnapshotter{sessions: []server.SessionSnapshot{
		{
			SessionID: "c-old", UserID: "u1", Token: "laptop", TokenID: "tok-1", OS: "linux", ConnectedAt: seen.Add(time.Minute),
			Tunnels: []server.TunnelView{{ID: "tn1", Name: "web", Type: "http"}},
		},
		{
			SessionID: "c-new", UserID: "u1", Token: "laptop", TokenID: "tok-1", OS: "darwin", ConnectedAt: seen.Add(time.Hour),
			Tunnels: []server.TunnelView{{ID: "tn2", Name: "ollama", Type: "http"}, {ID: "tn3", Name: "web", Type: "http"}},
		},
		{SessionID: "c-gone", UserID: "u2", Token: "revoked", TokenID: "tok-gone", ConnectedAt: seen},
	}}
	tg := fakeTunnelGetter{rows: map[string]db.Tunnel{"tn1": {ID: "tn1", TotalBytesIn: 7}}}
	a := clientsAdapter{srv: snap, st: tg, tokens: fakeTokenLister{tokens: []db.ClientToken{
		{ID: "tok-1", UserID: "u1", Name: "laptop", LastSeen: &seen},
		{ID: "tok-2", UserID: "u1", Name: "nas", LastSeen: &seen, LastOS: "linux", LastServed: []string{"files"}},
		{ID: "tok-3", UserID: "u3", Name: "spare"},
	}}}
	list, err := a.ListKnownClients(context.Background())
	if err != nil || len(list) != 4 {
		t.Fatalf("known clients: %v %+v", err, list)
	}
	on := list[0]
	if on.TokenID != "tok-1" || on.Status != api.ClientOnline || on.OS != "darwin" || !on.LastSeen.Equal(seen) ||
		!reflect.DeepEqual(on.Served, []string{"ollama", "web"}) {
		t.Fatalf("online row: %+v", on)
	}
	if len(on.Sessions) != 2 || on.Sessions[0].SessionID != "c-new" || on.Sessions[0].ServiceCount != 2 ||
		on.Sessions[1].SessionID != "c-old" || on.Sessions[1].TotalBytesIn != 7 {
		t.Fatalf("sessions: %+v", on.Sessions)
	}
	if gone := list[1]; gone.TokenID != "" || gone.TokenName != "revoked" || gone.UserID != "u2" || gone.Status != api.ClientOnline {
		t.Fatalf("session of a revoked token: %+v", gone)
	}
	if off := list[2]; off.TokenID != "tok-2" || off.Status != api.ClientOffline || off.OS != "linux" || !reflect.DeepEqual(off.Served, []string{"files"}) {
		t.Fatalf("offline row: %+v", off)
	}
	if never := list[3]; never.TokenID != "tok-3" || never.Status != api.ClientNeverConnected || never.LastSeen != nil {
		t.Fatalf("never connected row: %+v", never)
	}

	a.tokens = fakeTokenLister{err: errors.New("database is locked")}
	if _, err := a.ListKnownClients(context.Background()); err == nil {
		t.Fatal("a failing token list must fail the answer, not show every session as revoked")
	}
}

// The wiring of main.go, with a real store and a real control server: the
// store names the token at the handshake, the server records the session on
// it, and the adapter reads it back while the client is online and after it
// has gone.
func TestKnownClientsThroughStoreAndServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "known.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	if err := st.SeedAdmin(ctx, "admin@x", "password1"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByEmail(ctx, "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.IssueClientToken(ctx, u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.IssueClientToken(ctx, u.ID, "spare"); err != nil {
		t.Fatal(err)
	}
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{
		Listen: "127.0.0.1:0", TLSCert: filepath.Join(dir, "dev-server.pem"), TLSKey: filepath.Join(dir, "dev-server-key.pem"),
		Auth: st, Tunnels: tunnelStoreAdapter{st}, Services: serviceResolverAdapter{db: db.Wrap(d)},
		PublicBind: "127.0.0.1", SessionRecorder: st, ServiceRecorder: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	go func() { _ = srv.Serve(sctx) }()
	// LIFO: the server stops before the database closes.
	t.Cleanup(func() { _ = d.Close() })
	t.Cleanup(func() { cancel(); srv.Wait() })
	for deadline := time.Now().Add(2 * time.Second); srv.Addr() == ""; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("control server never bound")
		}
	}
	a := clientsAdapter{srv: srv, st: st, services: st, tokens: st}
	known := func() (laptop, spare api.KnownClient) {
		t.Helper()
		list, err := a.ListKnownClients(ctx)
		if err != nil || len(list) != 2 {
			t.Fatalf("known clients: %v %+v", err, list)
		}
		for _, kc := range list {
			if kc.TokenName == "laptop" {
				laptop = kc
			} else {
				spare = kc
			}
		}
		return laptop, spare
	}
	if laptop, spare := known(); laptop.Status != api.ClientNeverConnected || spare.Status != api.ClientNeverConnected {
		t.Fatalf("before any client: %+v %+v", laptop, spare)
	}

	caPEM, _ := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := proto.WriteMessage(conn, proto.MsgAuthRequest, proto.AuthRequest{
		ProtocolVersion: 1, Token: token, OS: "linux", Arch: "amd64", ClientVersion: "0.9.0",
	}); err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	if err := proto.ReadFrame(conn, &env); err != nil || env.Type != proto.MsgAuthResponse {
		t.Fatalf("auth: %v %v", env.Type, err)
	}
	sess, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ctrl, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteMessage(ctrl, proto.MsgTunnelRegister, proto.TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"}); err != nil {
		t.Fatal(err)
	}
	var rr proto.TunnelRegisterResponse
	if err := proto.ReadFrame(ctrl, &env); err != nil || proto.DecodePayload(env, &rr) != nil || !rr.OK {
		t.Fatalf("register: %v %+v", err, rr)
	}

	laptop, spare := known()
	if laptop.Status != api.ClientOnline || laptop.TokenID == "" || laptop.UserID != u.ID || laptop.OS != "linux" ||
		laptop.ClientVersion != "0.9.0" || laptop.LastSeen == nil || len(laptop.Sessions) != 1 ||
		!reflect.DeepEqual(laptop.Served, []string{"web"}) {
		t.Fatalf("online: %+v", laptop)
	}
	if spare.Status != api.ClientNeverConnected || spare.LastSeen != nil {
		t.Fatalf("the other token: %+v", spare)
	}
	// The service the client serves is stamped with the name of its token.
	served := func() store.ServiceView {
		t.Helper()
		svcs, err := st.ListServices(ctx, u.ID, "admin")
		if err != nil || len(svcs) != 1 || svcs[0].Name != "web" || svcs[0].LastSeen == nil || svcs[0].LastClient != "laptop" {
			t.Fatalf("the served service: %v %+v", err, svcs)
		}
		return svcs[0]
	}
	atRegister := *served().LastSeen

	_ = conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if laptop, _ = known(); laptop.Status == api.ClientOffline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the client never went offline: %+v", laptop)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if laptop.OS != "linux" || laptop.Arch != "amd64" || laptop.ClientVersion != "0.9.0" || laptop.RemoteAddr != conn.LocalAddr().String() ||
		laptop.LastSeen == nil || time.Since(*laptop.LastSeen) > 5*time.Second ||
		len(laptop.Sessions) != 0 || !reflect.DeepEqual(laptop.Served, []string{"web"}) {
		t.Fatalf("offline: %+v", laptop)
	}
	// The stamp of the disconnect is written before the session is gone.
	if last := *served().LastSeen; last.Before(atRegister) || time.Since(last) > 5*time.Second {
		t.Fatalf("service last seen %v after the disconnect, %v at the register", last, atRegister)
	}
}
