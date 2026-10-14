// cmd/server/e2e_local_provider_test.go
//
// A model server on a machine comes online: the two calls the dashboard's
// "New provider" dialog makes, then a real client on a real control server.
// Unlike e2e_gateway_tunnel_test.go nothing stands in for the tunnel
// registry, so the provider's status, the client list and the captured
// request are read from what the control server recorded.
package main

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// lateTunnels hands the gateway a control server that is built after it.
type lateTunnels struct{ srv atomic.Pointer[server.Server] }

func (l *lateTunnels) LookupHTTPTunnelByServiceID(serviceID string) (*server.Tunnel, bool) {
	if srv := l.srv.Load(); srv != nil {
		return srv.LookupHTTPTunnelByServiceID(serviceID)
	}
	return nil, false
}

func (l *lateTunnels) OpenTunnelStream(ctx context.Context, tn *server.Tunnel) (net.Conn, error) {
	return l.srv.Load().OpenTunnelStream(ctx, tn)
}

func (l *lateTunnels) LookupSessionByTunnelID(tunnelID string) (sessionID, userID string, ok bool) {
	if srv := l.srv.Load(); srv != nil {
		return srv.LookupSessionByTunnelID(tunnelID)
	}
	return "", "", false
}

func TestE2E_LocalProviderComesOnline(t *testing.T) {
	const (
		serviceID = "ollama" // the dialog makes id, title and slug the same word
		tokenName = "workstation"
	)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatalf("devcert: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "dev-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	var (
		tunnels lateTunnels
		srv     *server.Server
		token   string
	)
	e := bootGatewayE2EDeps(t, &tunnels, func(st *store.Store, v04 *v04Stack, deps *api.Deps) {
		ctx := context.Background()
		wrapped := db.Wrap(deps.DB.(*sql.DB))
		admin, err := st.GetUserByEmail(ctx, "admin-gw@test")
		if err != nil {
			t.Fatalf("get admin: %v", err)
		}
		if token, err = st.IssueClientToken(ctx, admin.ID, tokenName); err != nil {
			t.Fatalf("issue client token: %v", err)
		}
		// The control server as main builds it: both recorders are the store.
		srv, err = server.New(server.Options{
			Listen:          "127.0.0.1:0",
			TLSCert:         filepath.Join(dir, "dev-server.pem"),
			TLSKey:          filepath.Join(dir, "dev-server-key.pem"),
			Auth:            st,
			Tunnels:         tunnelStoreAdapter{st},
			SessionRecorder: st,
			ServiceRecorder: st,
			Services:        serviceResolverAdapter{db: wrapped, authDomain: deps.AuthDomain},
			AuthDomain:      deps.AuthDomain,
			PublicBind:      "127.0.0.1",
			Logger:          log,
		})
		if err != nil {
			t.Fatalf("server.New: %v", err)
		}
		tunnels.srv.Store(srv)
		srvCtx, stop := context.WithCancel(ctx)
		go func() { _ = srv.Serve(srvCtx) }()
		// Registered after the database's cleanup, so it runs before it: the
		// last session is written down while the database is still open.
		t.Cleanup(func() { stop(); srv.Wait() })

		deps.Clients = clientsAdapter{srv: srv, st: st, services: st, tokens: st}
		deps.LiveTunnels = liveTunnelLookupAdapter{srv: srv}
		deps.InspectorRings = v04.InspectorMgr
		deps.InspectorServices = cacheServiceLookupAdapter{db: wrapped}
	})
	for end := time.Now().Add(5 * time.Second); srv.Addr() == ""; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the control server never bound")
		}
	}

	// What the relay says about the provider and about the client.
	type providerView struct {
		Slug       string
		State      string
		LastSeen   *time.Time `json:"last_seen"`
		LastClient string     `json:"last_client"`
	}
	provider := func(t *testing.T, slug string) providerView {
		t.Helper()
		code, body := e.admin(t, "GET", "/api/v1/ai/providers/"+slug, nil)
		var p providerView
		if err := json.Unmarshal(body, &p); code != http.StatusOK || err != nil {
			t.Fatalf("get provider: %d %s", code, body)
		}
		return p
	}
	type knownClient struct {
		TokenName string `json:"token_name"`
		Status    string
		Served    []string
	}
	knownByName := func(t *testing.T) knownClient {
		t.Helper()
		code, body := e.admin(t, "GET", "/api/v1/clients/known", nil)
		var list []knownClient
		if err := json.Unmarshal(body, &list); code != http.StatusOK || err != nil {
			t.Fatalf("known clients: %d %s", code, body)
		}
		for _, c := range list {
			if c.TokenName == tokenName {
				return c
			}
		}
		t.Fatalf("no client named %q in %s", tokenName, body)
		return knownClient{}
	}
	// until polls what until it holds or five seconds passed.
	until := func(t *testing.T, what string, holds func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !holds(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("after 5 s still not: %s", what)
			}
		}
	}

	// The two calls of the dialog: a service, then a provider that hides it.
	if code, body := e.admin(t, "POST", "/api/v1/services", map[string]any{"service_id": serviceID, "title": serviceID, "access_mode": "api_key"}); code/100 != 2 {
		t.Fatalf("create service: %d %s", code, body)
	}
	code, body := e.admin(t, "POST", "/api/v1/ai/providers", map[string]any{"name": "Ollama", "kind": "tunnel", "service_id": serviceID, "gateway_only": true, "api_format": "openai"})
	var made providerView
	if err := json.Unmarshal(body, &made); code != http.StatusCreated || err != nil || made.Slug == "" {
		t.Fatalf("create provider: %d %s", code, body)
	}
	slug := made.Slug

	if p := provider(t, slug); p.State != "never_connected" || p.LastSeen != nil {
		t.Fatalf("before any client: state %q last_seen %v, want never_connected and none", p.State, p.LastSeen)
	}
	if c := knownByName(t); c.Status != "never_connected" {
		t.Fatalf("before it connects the client is %q", c.Status)
	}

	// What the operator sets up afterwards: a model and a gateway key, which
	// the dialog offers, and the request inspector of the provider's service,
	// which is off until its AI config switches it on.
	if code, body := e.admin(t, "POST", "/api/v1/ai/models", map[string]any{"name": "local", "targets": []map[string]string{
		{"dialect": "openai", "provider": slug, "model": "llama3"},
	}}); code != http.StatusCreated {
		t.Fatalf("create model: %d %s", code, body)
	}
	code, body = e.admin(t, "POST", "/api/v1/ai/keys", map[string]any{"name": "app"})
	var key struct{ ID, Key string }
	if err := json.Unmarshal(body, &key); code != http.StatusCreated || err != nil || key.Key == "" || key.ID == "" {
		t.Fatalf("create gateway key: %d %s", code, body)
	}
	if code, body := e.admin(t, "PUT", "/api/v1/services/"+serviceID+"/ai-config", map[string]any{"inspector": map[string]any{"enabled": true, "max_requests": 20}}); code/100 != 2 {
		t.Fatalf("switch the inspector on: %d %s", code, body)
	}

	// `burrow http <port> --name ollama` on the machine with the model server.
	modelServer := newGWTunnelApp(t, "ollama")
	clientCtx, stopClient := context.WithCancel(context.Background())
	done := make(chan struct{})
	c := client.New(client.Options{
		Server: srv.Addr(), Token: token, RootCAs: caPool, ServerName: "localhost",
		Tunnels: []client.TunnelSpec{{Name: serviceID, Type: "http", LocalAddr: modelServer.srv.Listener.Addr().String()}},
		Logger:  log,
	})
	go func() { defer close(done); _ = c.Run(clientCtx) }()
	t.Cleanup(func() { stopClient(); <-done })

	until(t, "the provider is online", func() bool { return provider(t, slug).State == "online" })
	if c := knownByName(t); c.Status != "online" || !slices.Equal(c.Served, []string{serviceID}) {
		t.Fatalf("while connected the client is %+v, want online serving [%s]", c, serviceID)
	}

	// One request of an app, through the gateway.
	r := e.call(t, "POST", "/openai/v1/chat/completions", `{"model":"local","messages":[{"role":"user","content":"hi"}]}`, bearer(key.Key)...)
	if r.Status != http.StatusOK || r.Header.Get("Burrow-Provider") != slug {
		t.Fatalf("request: status %d headers %v body %s", r.Status, r.Header, r.Body)
	}
	if n := len(modelServer.requests()); n != 1 {
		t.Fatalf("the model server received %d requests, want 1", n)
	}
	type captured struct {
		Provider     string
		Model        string
		GatewayKeyID string `json:"gateway_key_id"`
		Status       int
	}
	var reqs []captured
	until(t, "the request is listed for the provider", func() bool {
		code, body := e.admin(t, "GET", "/api/v1/ai/requests?provider="+slug, nil)
		reqs = nil
		if err := json.Unmarshal(body, &reqs); code != http.StatusOK || err != nil {
			t.Fatalf("requests: %d %s", code, body)
		}
		return len(reqs) > 0
	})
	if len(reqs) != 1 || reqs[0].Provider != slug || reqs[0].Model != "local" || reqs[0].GatewayKeyID != key.ID || reqs[0].Status != http.StatusOK {
		t.Fatalf("captured %+v, want one request of provider %s, model local, gateway key %s", reqs, slug, key.ID)
	}

	// The client leaves.
	stopClient()
	<-done
	until(t, "the provider is offline", func() bool { return provider(t, slug).State == "offline" })
	if p := provider(t, slug); p.LastSeen == nil || p.LastClient != tokenName {
		t.Fatalf("after the client left: last_seen %v last_client %q, want a time and %q", p.LastSeen, p.LastClient, tokenName)
	}
	until(t, "the client is offline", func() bool { return knownByName(t).Status == "offline" })
	if c := knownByName(t); !slices.Equal(c.Served, []string{serviceID}) {
		t.Fatalf("the offline client served %v, want [%s]", c.Served, serviceID)
	}
}
