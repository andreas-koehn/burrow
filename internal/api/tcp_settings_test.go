package api

// The tcp settings of a service through the API (tcp complete, E2–E4): the
// PROXY protocol and the idle timeout are read and changed on one route and
// reach the relay; the connection log and the live connections carry the
// protocol and the TLS server name.

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// liveTCP is the relay as the settings and connections routes read it.
type liveTCP struct {
	conns       []server.ConnView
	clientProxy map[string]string
	supported   map[string]bool // service id → its client can write the header; absent: offline
}

func (l *liveTCP) ServiceConnections(string) []server.ConnView { return l.conns }
func (l *liveTCP) CloseServiceConnection(string, string) bool  { return false }
func (l *liveTCP) ClientProxyProtocol(serviceID string) string { return l.clientProxy[serviceID] }
func (l *liveTCP) ClientProxySupported(serviceID string) (bool, bool) {
	s, ok := l.supported[serviceID]
	return s, ok
}

func decodeTCPSettings(t *testing.T, r *http.Response) tcpSettingsResp {
	t.Helper()
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	var s tcpSettingsResp
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTCPSettings_GetAndPut(t *testing.T) {
	live := &liveTCP{clientProxy: map[string]string{"rdp": "v2"}, supported: map[string]bool{"rdp": true}}
	a := newTCPAPI(t, func(d *Deps) { d.Connections = live })
	a.svcs.getSvc = store.ServiceDetail{ServiceView: store.ServiceView{ID: "rdp", Type: "tcp", AccessMode: "open"}}
	a.svcs.tcpSettings = store.TCPSettings{IdleTimeoutS: 600}

	got := decodeTCPSettings(t, a.c.get(t, "/api/v1/services/rdp/tcp-settings"))
	if got.ProxyProtocol != "" || got.IdleTimeoutS != 600 || got.ClientProxyProtocol != "v2" ||
		got.ClientProxySupported == nil || !*got.ClientProxySupported {
		t.Fatalf("GET %+v", got)
	}
	// An old client cannot write the header: false; offline: null.
	live.supported["rdp"] = false
	if got := decodeTCPSettings(t, a.c.get(t, "/api/v1/services/rdp/tcp-settings")); got.ClientProxySupported == nil || *got.ClientProxySupported {
		t.Fatalf("old client: %+v", got)
	}
	delete(live.supported, "rdp")
	if body := readBody(t, a.c.get(t, "/api/v1/services/rdp/tcp-settings")); !strings.Contains(body, `"client_proxy_supported":null`) {
		t.Fatalf("offline: %s", body)
	}
	live.supported["rdp"] = true

	// One field changes it alone; the answer is the settings after.
	got = decodeTCPSettings(t, a.c.put(t, "/api/v1/services/rdp/tcp-settings", map[string]string{"proxy_protocol": "v2"}))
	if got.ProxyProtocol != "v2" || got.IdleTimeoutS != 600 || got.ClientProxyProtocol != "v2" {
		t.Fatalf("PUT %+v", got)
	}
	u := a.svcs.lastTCPUpdate
	if u.ProxyProtocol == nil || *u.ProxyProtocol != "v2" || u.IdleTimeoutS != nil {
		t.Fatalf("update %+v", u)
	}
	if refreshed, _ := a.relay.snapshot(); !slices.Equal(refreshed, []string{"rdp"}) {
		t.Fatalf("relay told %v", refreshed)
	}
	got = decodeTCPSettings(t, a.c.put(t, "/api/v1/services/rdp/tcp-settings", map[string]any{"proxy_protocol": "", "idle_timeout_s": 0}))
	if got.ProxyProtocol != "" || got.IdleTimeoutS != 0 {
		t.Fatalf("both off: %+v", got)
	}

	for _, c := range []struct {
		name string
		body string
		err  error
		want int
		text string
	}{
		{"nothing to change", `{}`, nil, http.StatusBadRequest, "proxy_protocol or idle_timeout_s"},
		{"a field it does not know", `{"rate_limit_kbps": 5}`, nil, http.StatusBadRequest, "unknown"},
		{"not json", `nope`, nil, http.StatusBadRequest, ""},
		{"idle out of range", `{"idle_timeout_s": 30}`, store.ErrInvalidIdleTimeout, http.StatusBadRequest, "60 to 86400"},
		{"unknown proxy protocol", `{"proxy_protocol": "v1"}`, store.ErrInvalidProxyProtocol, http.StatusBadRequest, "v2"},
		{"http service", `{"proxy_protocol": "v2"}`, store.ErrServiceNotTCP, http.StatusConflict, "tcp services only"},
		{"another user's", `{"proxy_protocol": "v2"}`, store.ErrForbidden, http.StatusForbidden, ""},
		{"missing", `{"proxy_protocol": "v2"}`, db.ErrNotFound, http.StatusNotFound, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a.svcs.tcpSettingsErr = c.err
			defer func() { a.svcs.tcpSettingsErr = nil }()
			r := a.c.putRaw(t, "/api/v1/services/rdp/tcp-settings", c.body)
			body := readBody(t, r)
			if r.StatusCode != c.want || !strings.Contains(body, c.text) {
				t.Fatalf("status %d %s, want %d with %q", r.StatusCode, body, c.want, c.text)
			}
		})
	}
	a.svcs.tcpSettingsErr = store.ErrServiceNotTCP
	if r := a.c.get(t, "/api/v1/services/web/tcp-settings"); r.StatusCode != http.StatusConflict {
		t.Fatalf("GET http: %d", r.StatusCode)
	}
	if refreshed, _ := a.relay.snapshot(); len(refreshed) != 2 {
		t.Fatalf("a refused write told the relay: %v", refreshed)
	}
}

// The live connections and the connection log name what each connection
// spoke, and the exports carry it too.
func TestTCPSettings_ProtocolAndSNIAreShown(t *testing.T) {
	live := &liveTCP{conns: []server.ConnView{{ID: "c1", Kind: "visitor", SourceIP: "203.0.113.7", Since: time.Now(),
		Protocol: "tls", SNI: "rdp.example.com"}}}
	a := newTCPAPI(t, func(d *Deps) { d.Connections = live })
	a.svcs.getSvc = store.ServiceDetail{ServiceView: store.ServiceView{ID: "rdp", Type: "tcp", AccessMode: "open"}}
	r := a.c.get(t, "/api/v1/services/rdp/connections")
	var conns []serviceConnectionResp
	if err := json.NewDecoder(r.Body).Decode(&conns); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(conns) != 1 || conns[0].Protocol != "tls" || conns[0].SNI != "rdp.example.com" {
		t.Fatalf("connections %+v", conns)
	}

	logs := &fakeConnLogStore{logs: []db.ConnectionLog{{ID: "x1", Kind: "tcp_proxy", SourceIP: "203.0.113.7", Status: "closed_clean",
		StartedAt: time.Now(), EndedAt: time.Now(), Protocol: "tls", SNI: "rdp.example.com"}}}
	srv := httptest.NewServer(NewRouter(Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, ConnLogDB: logs}))
	defer srv.Close()
	c := authedClient(t, srv)
	var rows []connectionLogResp
	lr := c.get(t, "/api/v1/connection-logs")
	if err := json.NewDecoder(lr.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	lr.Body.Close()
	if len(rows) != 1 || rows[0].Protocol != "tls" || rows[0].SNI != "rdp.example.com" {
		t.Fatalf("rows %+v", rows)
	}
	recs, err := csv.NewReader(strings.NewReader(readBody(t, c.get(t, "/api/v1/connection-logs/export?format=csv")))).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("csv %v %v", recs, err)
	}
	head, row := recs[0], recs[1]
	if head[len(head)-2] != "protocol" || head[len(head)-1] != "sni" || row[len(row)-2] != "tls" || row[len(row)-1] != "rdp.example.com" {
		t.Fatalf("csv %v / %v", head, row)
	}
	if nd := readBody(t, c.get(t, "/api/v1/connection-logs/export")); !strings.Contains(nd, `"protocol":"tls"`) || !strings.Contains(nd, `"sni":"rdp.example.com"`) {
		t.Fatalf("ndjson %s", nd)
	}
}

// putRaw sends body as it is.
func (c *authClient) putRaw(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, c.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
