package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

type fakeClients struct {
	list   []ClientView
	detail ClientDetail
	ok     bool
}

func (f fakeClients) ListClients() []ClientView { return f.list }
func (f fakeClients) ListKnownClients(context.Context) ([]KnownClient, error) {
	return nil, nil
}
func (f fakeClients) GetClient(id string) (ClientDetail, bool) {
	if id == "miss" {
		return ClientDetail{}, false
	}
	return f.detail, f.ok
}

type fakeAccessSetter struct{ mode, lastID string }

func (f *fakeAccessSetter) SetTunnelAccessMode(_ context.Context, id, uid, mode string) error {
	if mode == "bad" {
		return errors.New("store: access_mode must be 'open', 'api_key', or 'burrow_login'")
	}
	if id == "miss" {
		return db.ErrNotFound
	}
	f.lastID, f.mode = id, mode
	return nil
}

func TestClientsOverviewAndAccessMode(t *testing.T) {
	fc := fakeClients{
		list: []ClientView{{SessionID: "c1", UserID: "u1", RemoteAddr: "1.1.1.1:5", OS: "linux", ServiceCount: 2, TotalBytesIn: 100}},
		detail: ClientDetail{
			ClientView: ClientView{SessionID: "c1", UserID: "u1"},
			Services:   []ClientServiceView{{ID: "tn1", Name: "web", AccessMode: "open", TotalBytesIn: 100, BytesIn: 5}},
		},
		ok: true,
	}
	as := &fakeAccessSetter{}
	d := Deps{Log: discardLog(), Clients: fc, AccessModes: as, Users: &fakeUserStore{role: "admin"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/clients")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("clients status=%d", r.StatusCode)
	}
	var cs []ClientView
	json.NewDecoder(r.Body).Decode(&cs)
	r.Body.Close()
	if len(cs) != 1 || cs[0].ServiceCount != 2 {
		t.Fatalf("clients: %+v", cs)
	}

	r = c.get(t, "/api/v1/clients/c1")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("client detail status=%d", r.StatusCode)
	}
	var cd ClientDetail
	json.NewDecoder(r.Body).Decode(&cd)
	r.Body.Close()
	if len(cd.Services) != 1 || cd.Services[0].AccessMode != "open" {
		t.Fatalf("detail: %+v", cd)
	}

	if r := c.get(t, "/api/v1/clients/miss"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing client status=%d want 404", r.StatusCode)
	}

	// set access mode (open works; api_key persisted-but-inert also 204)
	r = c.put(t, "/api/v1/tunnels/tn1/access-mode", map[string]string{"access_mode": "open"})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("set access mode status=%d", r.StatusCode)
	}
	r.Body.Close()
	if as.mode != "open" {
		t.Fatalf("mode not applied: %q", as.mode)
	}

	// invalid enum -> 400
	r = c.put(t, "/api/v1/tunnels/tn1/access-mode", map[string]string{"access_mode": "bad"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad mode status=%d want 400", r.StatusCode)
	}
	r.Body.Close()

	// missing tunnel -> 404
	r = c.put(t, "/api/v1/tunnels/miss/access-mode", map[string]string{"access_mode": "open"})
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing tunnel status=%d want 404", r.StatusCode)
	}
	r.Body.Close()
}
