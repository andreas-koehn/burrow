package api

// The access of tcp services through the API (native tcp, P02): grants,
// access modes, the cap on visitors, and the relay being told of each.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// typedServices is a ServiceOwnerLookup whose services all belong to
// u-self, with the type of each id.
type typedServices map[string]string

func (s typedServices) GetServiceByID(_ context.Context, id string) (db.Service, error) {
	typ, ok := s[id]
	if !ok {
		return db.Service{}, db.ErrNotFound
	}
	return db.Service{ID: id, UserID: "u-self", Name: "name-" + id, Type: typ}, nil
}

// memGrants is an in-memory IPGrantStore.
type memGrants struct {
	mu   sync.Mutex
	rows []db.IPGrant
}

func (m *memGrants) CreateIPGrantCapped(_ context.Context, g db.IPGrant, max int, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.ServiceID == g.ServiceID && r.ExpiresAt.After(now) {
			n++
		}
	}
	if n >= max {
		return db.ErrTooManyIPGrants
	}
	m.rows = append(m.rows, g)
	return nil
}

func (m *memGrants) ListIPGrants(_ context.Context, serviceID string, now time.Time) ([]db.IPGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []db.IPGrant{}
	for _, g := range m.rows {
		if g.ServiceID == serviceID && g.ExpiresAt.After(now) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (m *memGrants) DeleteIPGrant(_ context.Context, serviceID, id string) (db.IPGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, g := range m.rows {
		if g.ServiceID == serviceID && g.ID == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return g, nil
		}
	}
	return db.IPGrant{}, db.ErrNotFound
}

func (m *memGrants) PruneIPGrants(_ context.Context, serviceID string, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keep []db.IPGrant
	var n int64
	for _, g := range m.rows {
		if g.ServiceID == serviceID && !g.ExpiresAt.After(now) {
			n++
			continue
		}
		keep = append(keep, g)
	}
	m.rows = keep
	return n, nil
}

// relayTold records what the relay was told: refreshed services and closed
// sessions.
type relayTold struct {
	mu        sync.Mutex
	refreshed []string
	closed    []string
}

func (r *relayTold) RefreshTCPService(_ context.Context, id string) error {
	r.mu.Lock()
	r.refreshed = append(r.refreshed, id)
	r.mu.Unlock()
	return nil
}

func (r *relayTold) CloseByService(id string) int {
	r.mu.Lock()
	r.closed = append(r.closed, id)
	r.mu.Unlock()
	return 1
}

func (r *relayTold) snapshot() (refreshed, closed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refreshed...), append([]string(nil), r.closed...)
}

type tcpAPI struct {
	srv    *httptest.Server
	c      *authClient
	grants *memGrants
	relay  *relayTold
	audits *stubAuditAppender
	svcs   *fakeServiceStore
	ipgeo  *fakeIPGeoStore
}

func newTCPAPI(t *testing.T, mut func(*Deps)) *tcpAPI {
	t.Helper()
	a := &tcpAPI{grants: &memGrants{}, relay: &relayTold{}, audits: &stubAuditAppender{},
		svcs: &fakeServiceStore{}, ipgeo: newFakeIPGeoStore()}
	d := Deps{
		Log: discardLog(), Users: &fakeUserStore{role: "user", selfID: "u-self"},
		IPGeo: a.ipgeo, IPGeoServices: typedServices{"rdp": "tcp", "web": "http"},
		IPGrants: a.grants, TCPServices: a.relay, ServiceSessions: a.relay, AuditAppender: a.audits,
		Services: a.svcs,
	}
	if mut != nil {
		mut(&d)
	}
	a.srv = httptest.NewServer(NewRouter(d))
	t.Cleanup(a.srv.Close)
	a.c = authedClient(t, a.srv)
	return a
}

// postFrom posts a grant as a request from the given forwarded address.
func (a *tcpAPI) postFrom(t *testing.T, xff string, body any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+"/api/v1/services/rdp/ip-grants", mustJSON(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", a.c.csrf)
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := a.c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeGrant(t *testing.T, r *http.Response) ipGrantCreatedResp {
	t.Helper()
	defer r.Body.Close()
	var g ipGrantCreatedResp
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

// "Allow my IP": without a cidr the grant is for the caller's address, as
// the dashboard sees it after the trusted proxies, with its family; the
// relay is told and the grant is audited. A private address is warned of.
func TestIPGrants_AllowMyIP(t *testing.T) {
	a := newTCPAPI(t, func(d *Deps) { d.TrustedProxies = []string{"127.0.0.1"} })
	r := a.postFrom(t, "2001:db8:5::7", ipGrantReq{TTLHours: 8, Note: " laptop "})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d %s", r.StatusCode, readBody(t, r))
	}
	g := decodeGrant(t, r)
	if g.CIDR != "2001:db8:5::7/128" || g.Family != "ipv6" || !g.FromCaller || g.Note != "laptop" || g.Warning != "" || g.CreatedBy != "u-self" {
		t.Fatalf("grant %+v", g)
	}
	if d := time.Until(g.ExpiresAt); d < 7*time.Hour+59*time.Minute || d > 8*time.Hour {
		t.Fatalf("expires in %v", d)
	}
	r = a.postFrom(t, "203.0.113.9", ipGrantReq{TTLHours: 1})
	if g := decodeGrant(t, r); g.CIDR != "203.0.113.9/32" || g.Family != "ipv4" {
		t.Fatalf("ipv4 grant %+v", g)
	}
	r = a.postFrom(t, "", ipGrantReq{TTLHours: 1}) // the loopback peer itself
	if g := decodeGrant(t, r); g.CIDR != "127.0.0.1/32" || g.Warning == "" {
		t.Fatalf("loopback grant %+v", g)
	}
	refreshed, closed := a.relay.snapshot()
	if !slices.Equal(refreshed, []string{"rdp", "rdp", "rdp"}) || len(closed) != 0 {
		t.Fatalf("relay told %v, closed %v", refreshed, closed)
	}
	if len(a.audits.events) != 3 || a.audits.events[0].Action != audit.ActionServiceIPGrantAdd || a.audits.events[0].SubjectID != "rdp" ||
		!strings.Contains(string(a.audits.events[0].Payload), `"cidr":"2001:db8:5::7/128"`) {
		t.Fatalf("audit %+v", a.audits.events)
	}

	r = a.c.get(t, "/api/v1/services/rdp/ip-grants")
	var list []ipGrantResp
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil || len(list) != 3 {
		t.Fatalf("list %v %+v", err, list)
	}
	r = a.c.delete(t, "/api/v1/services/rdp/ip-grants/"+list[0].ID)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke %d %s", r.StatusCode, readBody(t, r))
	}
	if r := a.c.delete(t, "/api/v1/services/rdp/ip-grants/"+list[0].ID); r.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke again %d", r.StatusCode)
	}
	if ev := a.audits.events[len(a.audits.events)-1]; ev.Action != audit.ActionServiceIPGrantRevoke {
		t.Fatalf("revoke audit %+v", ev)
	}
	if refreshed, _ := a.relay.snapshot(); len(refreshed) != 4 {
		t.Fatalf("relay told %v after the revoke", refreshed)
	}
}

// A manual grant is masked; too wide a range, a bad ttl, an http service and
// a full service are refused, and nothing is stored or told.
func TestIPGrants_Validation(t *testing.T) {
	a := newTCPAPI(t, nil)
	r := a.c.post(t, "/api/v1/services/rdp/ip-grants", ipGrantReq{CIDR: "198.51.100.77/24", TTLHours: 24})
	if g := decodeGrant(t, r); r.StatusCode != http.StatusCreated || g.CIDR != "198.51.100.0/24" || g.FromCaller || g.Warning != "" {
		t.Fatalf("manual %d %+v", r.StatusCode, g)
	}
	for name, c := range map[string]struct {
		path string
		body ipGrantReq
		want int
	}{
		"wide v4":   {"rdp", ipGrantReq{CIDR: "10.0.0.0/8", TTLHours: 1}, http.StatusBadRequest},
		"wide v6":   {"rdp", ipGrantReq{CIDR: "2001:db8::/32", TTLHours: 1}, http.StatusBadRequest},
		"all":       {"rdp", ipGrantReq{CIDR: "0.0.0.0/0", TTLHours: 1}, http.StatusBadRequest},
		"nonsense":  {"rdp", ipGrantReq{CIDR: "my laptop", TTLHours: 1}, http.StatusBadRequest},
		"no ttl":    {"rdp", ipGrantReq{CIDR: "192.0.2.1"}, http.StatusBadRequest},
		"long ttl":  {"rdp", ipGrantReq{CIDR: "192.0.2.1", TTLHours: 169}, http.StatusBadRequest},
		"long note": {"rdp", ipGrantReq{CIDR: "192.0.2.1", TTLHours: 1, Note: strings.Repeat("x", 201)}, http.StatusBadRequest},
		"http":      {"web", ipGrantReq{CIDR: "192.0.2.1", TTLHours: 1}, http.StatusConflict},
		"missing":   {"nope", ipGrantReq{CIDR: "192.0.2.1", TTLHours: 1}, http.StatusNotFound},
	} {
		r := a.c.post(t, "/api/v1/services/"+c.path+"/ip-grants", c.body)
		if r.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", name, r.StatusCode, readBody(t, r), c.want)
		}
	}
	if len(a.grants.rows) != 1 {
		t.Fatalf("%d grants stored", len(a.grants.rows))
	}
	for i := 1; i < MaxActiveIPGrants; i++ {
		if r := a.c.post(t, "/api/v1/services/rdp/ip-grants", ipGrantReq{CIDR: "192.0.2.1", TTLHours: 1}); r.StatusCode != http.StatusCreated {
			t.Fatalf("grant %d: %d", i, r.StatusCode)
		}
	}
	if r := a.c.post(t, "/api/v1/services/rdp/ip-grants", ipGrantReq{CIDR: "192.0.2.1", TTLHours: 1}); r.StatusCode != http.StatusConflict {
		t.Fatalf("grant over the cap: %d", r.StatusCode)
	}
	// Expired grants do not count and are pruned.
	a.grants.mu.Lock()
	for i := range a.grants.rows {
		a.grants.rows[i].ExpiresAt = time.Now().Add(-time.Second)
	}
	a.grants.mu.Unlock()
	if r := a.c.post(t, "/api/v1/services/rdp/ip-grants", ipGrantReq{CIDR: "192.0.2.1", TTLHours: 1}); r.StatusCode != http.StatusCreated || len(a.grants.rows) != 1 {
		t.Fatalf("after expiry: %d, %d rows", r.StatusCode, len(a.grants.rows))
	}
}

// A new mode of a tcp service closes the session that serves it and tells
// the relay; the same mode, an http service and a refused change do not.
func TestTCPAccessMode_ChangeClosesTheTunnel(t *testing.T) {
	a := newTCPAPI(t, nil)
	a.svcs.getSvc = store.ServiceDetail{ServiceView: store.ServiceView{ID: "rdp", Type: "tcp", AccessMode: "open"}}
	if r := a.c.put(t, "/api/v1/services/rdp/access-mode", setAccessModeReq{AccessMode: "private"}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d %s", r.StatusCode, readBody(t, r))
	}
	refreshed, closed := a.relay.snapshot()
	if !slices.Equal(closed, []string{"rdp"}) || !slices.Equal(refreshed, []string{"rdp"}) {
		t.Fatalf("closed %v refreshed %v", closed, refreshed)
	}
	if r := a.c.put(t, "/api/v1/services/rdp/access-mode", setAccessModeReq{AccessMode: "open"}); r.StatusCode != http.StatusNoContent {
		t.Fatal(r.StatusCode)
	}
	if _, closed := a.relay.snapshot(); len(closed) != 1 {
		t.Fatalf("the same mode closed the tunnel: %v", closed)
	}
	a.svcs.getSvc = store.ServiceDetail{ServiceView: store.ServiceView{ID: "web", Type: "http", AccessMode: "open"}}
	_ = a.c.put(t, "/api/v1/services/web/access-mode", setAccessModeReq{AccessMode: "api_key"})
	a.svcs.getSvc = store.ServiceDetail{ServiceView: store.ServiceView{ID: "rdp", Type: "tcp", AccessMode: "open"}}
	a.svcs.setModeErr = store.ErrTCPAccessMode
	if r := a.c.put(t, "/api/v1/services/rdp/access-mode", setAccessModeReq{AccessMode: "api_key"}); r.StatusCode != http.StatusConflict {
		t.Fatalf("http mode on tcp: %d", r.StatusCode)
	}
	if _, closed := a.relay.snapshot(); len(closed) != 1 {
		t.Fatalf("closed %v", closed)
	}
}

// The cap on visitors and the permanent lists reach the relay.
func TestTCPAccess_CapAndListsTellTheRelay(t *testing.T) {
	a := newTCPAPI(t, nil)
	if r := a.c.put(t, "/api/v1/services/rdp/max-connections", map[string]int{"max_connections": 8}); r.StatusCode != http.StatusNoContent || a.svcs.lastMaxConns != 8 {
		t.Fatalf("status %d, cap %d", r.StatusCode, a.svcs.lastMaxConns)
	}
	if r := a.c.put(t, "/api/v1/services/rdp/max-connections", map[string]string{}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("no cap: %d", r.StatusCode)
	}
	a.svcs.maxConnsErr = store.ErrInvalidMaxConnections
	if r := a.c.put(t, "/api/v1/services/rdp/max-connections", map[string]int{"max_connections": 999}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("out of range: %d", r.StatusCode)
	}
	a.svcs.maxConnsErr = store.ErrServiceNotTCP
	if r := a.c.put(t, "/api/v1/services/web/max-connections", map[string]int{"max_connections": 3}); r.StatusCode != http.StatusConflict {
		t.Fatalf("http: %d", r.StatusCode)
	}
	if r := a.c.put(t, "/api/v1/services/rdp/ip-geo", ipGeoReq{AllowCIDRs: []string{"192.0.2.0/24"}}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("ip-geo: %d %s", r.StatusCode, readBody(t, r))
	}
	if refreshed, _ := a.relay.snapshot(); !slices.Equal(refreshed, []string{"rdp", "rdp"}) {
		t.Fatalf("relay told %v", refreshed)
	}
}

// ip-geo of a tcp service: an allow entry wider than /8 (IPv4) or /32
// (IPv6) is refused with a 400 and nothing is stored; a change is audited as
// ipgeo.update with the lists before and after (for http too).
func TestIPGeo_TCPFloorAndAudit(t *testing.T) {
	a := newTCPAPI(t, nil)
	for _, wide := range []string{"0.0.0.0/0", "10.0.0.0/7", "::/0", "2001:db8::/31"} {
		r := a.c.put(t, "/api/v1/services/rdp/ip-geo", ipGeoReq{AllowCIDRs: []string{wide}})
		if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "/8") {
			t.Errorf("%s: %d %s", wide, r.StatusCode, body)
		}
	}
	if len(a.ipgeo.rows) != 0 || len(a.audits.events) != 0 {
		t.Fatalf("a refused list was stored or audited: %v %v", a.ipgeo.rows, a.audits.events)
	}
	// /8 and /32 are the widest allowed; an http service keeps its old rules.
	if r := a.c.put(t, "/api/v1/services/rdp/ip-geo", ipGeoReq{AllowCIDRs: []string{"10.0.0.0/8", "2001:db8::/32"}}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("at the floor: %d %s", r.StatusCode, readBody(t, r))
	}
	if r := a.c.put(t, "/api/v1/services/web/ip-geo", ipGeoReq{Enabled: true, AllowCIDRs: []string{"0.0.0.0/0"}}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("http: %d %s", r.StatusCode, readBody(t, r))
	}
	if r := a.c.put(t, "/api/v1/services/rdp/ip-geo", ipGeoReq{AllowCIDRs: []string{"192.0.2.0/24"}, BlockCIDRs: []string{"192.0.2.66"}}); r.StatusCode != http.StatusNoContent {
		t.Fatal(r.StatusCode)
	}
	if len(a.audits.events) != 3 {
		t.Fatalf("audit %+v", a.audits.events)
	}
	ev := a.audits.events[2]
	var p struct {
		From, To struct {
			Enabled    bool     `json:"enabled"`
			AllowCIDRs []string `json:"allow_cidrs"`
			BlockCIDRs []string `json:"block_cidrs"`
		}
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if ev.Action != audit.ActionIPGeoUpdate || ev.SubjectID != "rdp" || ev.SubjectLabel != "name-rdp" ||
		!slices.Equal(p.From.AllowCIDRs, []string{"10.0.0.0/8", "2001:db8::/32"}) ||
		!slices.Equal(p.To.AllowCIDRs, []string{"192.0.2.0/24"}) || !slices.Equal(p.To.BlockCIDRs, []string{"192.0.2.66"}) {
		t.Fatalf("audit %+v payload %s", ev, ev.Payload)
	}
}

// A failed read of the service before an access-mode write does not leave
// the relay untold: a change of a tcp mode is applied by the refresh.
func TestTCPAccessMode_RefreshEvenWhenTheBeforeReadFails(t *testing.T) {
	a := newTCPAPI(t, nil)
	a.svcs.getSvcErr = errors.New("database is busy")
	if r := a.c.put(t, "/api/v1/services/rdp/access-mode", setAccessModeReq{AccessMode: "open"}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d %s", r.StatusCode, readBody(t, r))
	}
	if refreshed, _ := a.relay.snapshot(); !slices.Equal(refreshed, []string{"rdp"}) {
		t.Fatalf("relay told %v", refreshed)
	}
}
