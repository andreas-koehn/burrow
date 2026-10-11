package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// liveServices is a LiveTunnelLookup over the services a client serves now.
type liveServices map[string]LiveTunnelSnapshot

func (l liveServices) LookupByServiceID(serviceID string) (LiveTunnelSnapshot, bool) {
	snap, ok := l[serviceID]
	return snap, ok
}

func (liveServices) LookupByTunnelID(string) (TunnelLocator, bool) { return TunnelLocator{}, false }

// countingProviders counts the provider lists a request asks for.
type countingProviders struct {
	*fakeProviderStore
	lists   int
	listErr error
}

func (c *countingProviders) ListProviders(ctx context.Context) ([]db.AIProvider, error) {
	c.lists++
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.fakeProviderStore.ListProviders(ctx)
}

// ownServices is a service store that scopes the list like the real one: a
// caller who is not an admin gets the services of their own user id.
type ownServices struct {
	*fakeServiceStore
	calls []string // "callerID/role" of every ListServices
}

func (o *ownServices) ListServices(_ context.Context, callerID, role string) ([]store.ServiceView, error) {
	o.calls = append(o.calls, callerID+"/"+role)
	var out []store.ServiceView
	for _, sv := range o.listSvcs {
		if role == "admin" || sv.UserID == callerID {
			out = append(out, sv)
		}
	}
	return out, nil
}

var servedAt = time.Date(2026, 10, 10, 8, 30, 0, 0, time.UTC)

const servedAtJSON = "2026-10-10T08:30:00Z"

// statusServices holds an http and a tcp service in each of the three states,
// and the backing row of a direct provider. "s-off" backs the tunnel
// provider "ollama".
func statusServices() []store.ServiceView {
	return []store.ServiceView{
		{ID: "s-live", Name: "web", Type: "http", Subdomain: "web", AccessMode: "open", LastSeen: &servedAt, LastClient: "laptop"},
		{ID: "s-off", Name: "ollama", Type: "http", Subdomain: "ollama", AccessMode: "api_key", LastSeen: &servedAt, LastClient: "desktop"},
		{ID: "s-new", Name: "new", Type: "http", AccessMode: "open"},
		{ID: "t-live", Name: "db", Type: "tcp", LastSeen: &servedAt, LastClient: "laptop"},
		{ID: "t-off", Name: "ssh", Type: "tcp", LastSeen: &servedAt, LastClient: "desktop"},
		{ID: "t-new", Name: "redis", Type: "tcp"},
		{ID: "prov-openrouter", Name: "OpenRouter", Type: "direct", AccessMode: "api_key"},
	}
}

func statusLive() liveServices {
	return liveServices{
		"s-live": {Connected: true, LocalAddr: "127.0.0.1:3000", SessionID: "sess-1"},
		"t-live": {Connected: true, LocalAddr: "127.0.0.1:5432", RemotePort: 9001, SessionID: "sess-2"},
	}
}

func statusProviders() *countingProviders {
	return &countingProviders{fakeProviderStore: &fakeProviderStore{rows: []db.AIProvider{
		tunnelProvider("ollama", "Ollama", "s-off"),
		{Slug: "openrouter", Name: "OpenRouter", Kind: "direct", ServiceID: "prov-openrouter", APIFormat: "openai", CredentialSlot: "OPENROUTER"},
	}}}
}

// wantServiceStatus checks the status fields of one service response.
func wantServiceStatus(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	for _, k := range []string{"status", "last_seen", "last_client", "client_session_id", "provider_slug", "connected"} {
		v, present := got[k]
		if !present {
			t.Errorf("%v: field %q is missing", got["id"], k)
			continue
		}
		if v != want[k] {
			t.Errorf("%v: %s = %#v, want %#v", got["id"], k, v, want[k])
		}
	}
}

var wantStatusByService = map[string]map[string]any{
	"s-live": {"status": "online", "last_seen": servedAtJSON, "last_client": "laptop", "client_session_id": "sess-1", "provider_slug": "", "connected": true},
	"s-off":  {"status": "offline", "last_seen": servedAtJSON, "last_client": "desktop", "client_session_id": "", "provider_slug": "ollama", "connected": false},
	"s-new":  {"status": "never_connected", "last_seen": nil, "last_client": "", "client_session_id": "", "provider_slug": "", "connected": false},
	// A tcp service follows the same three states.
	"t-live": {"status": "online", "last_seen": servedAtJSON, "last_client": "laptop", "client_session_id": "sess-2", "provider_slug": "", "connected": true},
	"t-off":  {"status": "offline", "last_seen": servedAtJSON, "last_client": "desktop", "client_session_id": "", "provider_slug": "", "connected": false},
	"t-new":  {"status": "never_connected", "last_seen": nil, "last_client": "", "client_session_id": "", "provider_slug": "", "connected": false},
}

func TestServedState(t *testing.T) {
	for _, tc := range []struct {
		live     bool
		lastSeen *time.Time
		want     string
	}{
		{true, &servedAt, "online"},
		{true, nil, "online"},
		{false, &servedAt, "offline"},
		{false, nil, "never_connected"},
	} {
		if got := servedState(tc.live, tc.lastSeen); got != tc.want {
			t.Errorf("servedState(%v, %v) = %q, want %q", tc.live, tc.lastSeen, got, tc.want)
		}
	}
}

func TestListServices_Status(t *testing.T) {
	ps := statusProviders()
	d := newServiceDeps(&fakeServiceStore{listSvcs: statusServices()}, statusLive(), "example.com")
	d.AIProviders = ps
	srv, c := newServiceServer(t, d)
	defer srv.Close()

	r := c.get(t, "/api/v1/services")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []map[string]any
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	// The backing row of the direct provider is not a service.
	if len(out) != len(wantStatusByService) {
		t.Fatalf("want %d services, got %d: %v", len(wantStatusByService), len(out), out)
	}
	for _, got := range out {
		id, _ := got["id"].(string)
		want, ok := wantStatusByService[id]
		if !ok {
			t.Fatalf("unexpected service %v", got)
		}
		wantServiceStatus(t, got, want)
	}
	// The map of provider slugs is built once, not once per row.
	if ps.lists != 1 {
		t.Errorf("providers were listed %d times for one service list, want 1", ps.lists)
	}
}

func TestGetService_Status(t *testing.T) {
	for _, sv := range statusServices() {
		want, ok := wantStatusByService[sv.ID]
		if !ok {
			continue
		}
		t.Run(sv.ID, func(t *testing.T) {
			ps := statusProviders()
			ss := &fakeServiceStore{getSvc: store.ServiceDetail{ServiceView: sv, APIKeyCount: 1, AccessPolicy: []string{"user"}}}
			d := newServiceDeps(ss, statusLive(), "example.com")
			d.AIProviders = ps
			srv, c := newServiceServer(t, d)
			defer srv.Close()

			r := c.get(t, "/api/v1/services/"+sv.ID)
			if r.StatusCode != http.StatusOK {
				t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
			}
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			wantServiceStatus(t, got, want)
			// The detail fields stay.
			if got["api_key_count"] != float64(1) || got["id"] != sv.ID {
				t.Errorf("detail fields: %v", got)
			}
			if ps.lists != 1 {
				t.Errorf("providers were listed %d times for one service, want 1", ps.lists)
			}
		})
	}
}

// Without a provider store, or when its list fails, the services still
// render; they name no provider.
func TestListServices_StatusWithoutProviders(t *testing.T) {
	for name, ps := range map[string]AIProviderStore{
		"no provider store": nil,
		"list fails":        &countingProviders{fakeProviderStore: &fakeProviderStore{}, listErr: errors.New("database is locked")},
	} {
		t.Run(name, func(t *testing.T) {
			d := newServiceDeps(&fakeServiceStore{listSvcs: statusServices()}, statusLive(), "example.com")
			d.AIProviders = ps
			srv, c := newServiceServer(t, d)
			defer srv.Close()

			r := c.get(t, "/api/v1/services")
			if r.StatusCode != http.StatusOK {
				t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
			}
			var out []serviceResp
			if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			if len(out) != len(wantStatusByService) {
				t.Fatalf("want %d services, got %+v", len(wantStatusByService), out)
			}
			for _, sv := range out {
				if sv.ProviderSlug != "" || sv.Status == "" {
					t.Errorf("%s: provider_slug %q status %q", sv.ID, sv.ProviderSlug, sv.Status)
				}
			}
		})
	}
}

// The new fields change nothing about who sees what: a caller who is not an
// admin still gets their own services only, and no provider of someone else.
func TestListServices_NonAdminSeesOnlyOwn(t *testing.T) {
	ss := &ownServices{fakeServiceStore: &fakeServiceStore{listSvcs: []store.ServiceView{
		{ID: "mine", UserID: "u-self", Name: "web", Type: "http", LastSeen: &servedAt, LastClient: "laptop"},
		{ID: "mine-ai", UserID: "u-self", Name: "llm", Type: "http"},
		{ID: "theirs", UserID: "u-other", Name: "secret", Type: "http", LastSeen: &servedAt, LastClient: "their-laptop"},
	}}}
	ps := &fakeProviderStore{rows: []db.AIProvider{
		tunnelProvider("my-llm", "Mine", "mine-ai"),
		tunnelProvider("their-llm", "Theirs", "theirs"),
	}}
	d := Deps{
		Users:       &fakeUserStore{role: "user"},
		Services:    ss,
		AIProviders: ps,
		LiveTunnels: liveServices{"theirs": {Connected: true, SessionID: "sess-theirs"}},
		Log:         discardLog(),
	}
	srv, c := newServiceServer(t, d)
	defer srv.Close()

	r := c.get(t, "/api/v1/services")
	body := wantStatus(t, r, http.StatusOK)
	var out []serviceResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].ID != "mine" || out[1].ID != "mine-ai" {
		t.Fatalf("want the caller's two services, got %s", body)
	}
	if out[0].Status != "offline" || out[0].LastClient != "laptop" || out[1].Status != "never_connected" || out[1].ProviderSlug != "my-llm" {
		t.Errorf("own services: %s", body)
	}
	for _, leak := range []string{"theirs", "their-llm", "their-laptop", "sess-theirs", "secret"} {
		if strings.Contains(body, leak) {
			t.Errorf("the list names %q of another user: %s", leak, body)
		}
	}
	if len(ss.calls) != 1 || ss.calls[0] != "u-self/user" {
		t.Errorf("ListServices was asked as %v, want once as u-self/user", ss.calls)
	}
}
