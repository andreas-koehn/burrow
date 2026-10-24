package api

// POST /services with type "tcp" (tcp complete, E9): the row exists before
// any client connects, with the mode, the fixed port and the first settings
// the caller gave.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

func newTCPCreate(t *testing.T) (*fakeServiceStore, *stubAuditAppender, *authClient) {
	t.Helper()
	ss := &fakeServiceStore{}
	aud := &stubAuditAppender{}
	d := newServiceDeps(ss, fakeLiveTunnels{}, "")
	d.AuditAppender = aud
	d.TCPPortMin, d.TCPPortMax = 9000, 9100
	srv, c := newServiceServer(t, d)
	t.Cleanup(srv.Close)
	return ss, aud, c
}

func TestPostService_TCPCreatesPrivateRowByDefault(t *testing.T) {
	ss, aud, c := newTCPCreate(t)
	r := c.post(t, "/api/v1/services", map[string]any{"type": "tcp", "name": "rdp"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", r.StatusCode, readBody(t, r))
	}
	var got struct{ ID, Slug, CreatedAt string }
	var raw map[string]any
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	got.ID, _ = raw["id"].(string)
	got.Slug, _ = raw["slug"].(string)
	if got.ID == "" || got.Slug == "" || raw["created_at"] == "" {
		t.Fatalf("answer %v", raw)
	}
	if len(ss.createdSvcs) != 1 {
		t.Fatalf("created %d", len(ss.createdSvcs))
	}
	row := ss.createdSvcs[0]
	if row.Type != "tcp" || row.Name != "rdp" || row.AccessMode != "private" || row.ID != got.ID ||
		row.Subdomain != got.Slug || row.TCPPort != 0 || row.TCPPortPinned || row.UserID == "" {
		t.Fatalf("row %+v", row)
	}
	if len(aud.events) != 1 || aud.events[0].Action != audit.ActionServiceCreate ||
		!strings.Contains(string(aud.events[0].Payload), `"type":"tcp"`) ||
		!strings.Contains(string(aud.events[0].Payload), `"access_mode":"private"`) {
		t.Fatalf("audit %+v", aud.events)
	}
}

func TestPostService_TCPKeepsPortModeAndSettings(t *testing.T) {
	ss, aud, c := newTCPCreate(t)
	r := c.post(t, "/api/v1/services", map[string]any{
		"type": "tcp", "name": "ssh-box", "access_mode": "ip", "tcp_port": 9042,
		"proxy_protocol": "v2", "idle_timeout_s": 600,
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	row := ss.createdSvcs[0]
	if row.AccessMode != "ip" || row.TCPPort != 9042 || !row.TCPPortPinned || row.ProxyProtocol != "v2" || row.IdleTimeoutS != 600 {
		t.Fatalf("row %+v", row)
	}
	p := string(aud.events[0].Payload)
	for _, want := range []string{`"tcp_port":9042`, `"proxy_protocol":"v2"`, `"idle_timeout_s":600`} {
		if !strings.Contains(p, want) {
			t.Errorf("audit payload %s lacks %s", p, want)
		}
	}
}

func TestPostService_TCPRefusals(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		err  error
		want int
		text string
	}{
		{"no name", map[string]any{"type": "tcp"}, nil, 400, "name"},
		{"name with space", map[string]any{"type": "tcp", "name": "a b"}, nil, 400, "name"},
		{"long name", map[string]any{"type": "tcp", "name": strings.Repeat("a", 65)}, nil, 400, "name"},
		{"http mode", map[string]any{"type": "tcp", "name": "x1", "access_mode": "api_key"}, nil, 400, "access_mode"},
		{"public alias", map[string]any{"type": "tcp", "name": "x1", "access_mode": "public"}, nil, 400, "access_mode"},
		{"port below range", map[string]any{"type": "tcp", "name": "x1", "tcp_port": 8999}, nil, 400, "9000"},
		{"port above range", map[string]any{"type": "tcp", "name": "x1", "tcp_port": 9101}, nil, 400, "9100"},
		{"bad proxy", map[string]any{"type": "tcp", "name": "x1", "proxy_protocol": "v1"}, nil, 400, "proxy_protocol"},
		{"bad idle", map[string]any{"type": "tcp", "name": "x1", "idle_timeout_s": 5}, nil, 400, "idle_timeout_s"},
		{"unknown type", map[string]any{"type": "udp", "name": "x1"}, nil, 400, "type"},
		{"http with a port", map[string]any{"service_id": "abc", "tcp_port": 9001}, nil, 400, "tcp"},
		{"name taken", map[string]any{"type": "tcp", "name": "rdp"}, db.ErrDuplicateService, 409, "already"},
		{"port taken", map[string]any{"type": "tcp", "name": "rdp", "tcp_port": 9001}, db.ErrTCPPortReserved, 409, "port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss, aud, c := newTCPCreate(t)
			ss.createSvcErr = tc.err
			r := c.post(t, "/api/v1/services", tc.body)
			body := readBody(t, r)
			if r.StatusCode != tc.want || !strings.Contains(body, tc.text) {
				t.Fatalf("status %d: %s; want %d with %q", r.StatusCode, body, tc.want, tc.text)
			}
			if len(ss.createdSvcs) != 0 || len(aud.events) != 0 {
				t.Fatalf("a refused create left a row or an audit row: %+v %+v", ss.createdSvcs, aud.events)
			}
		})
	}
}

func TestPostService_HTTPStillNeedsServiceID(t *testing.T) {
	_, _, c := newTCPCreate(t)
	r := c.post(t, "/api/v1/services", map[string]any{"type": "http"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", r.StatusCode)
	}
	r.Body.Close()
	r = c.post(t, "/api/v1/services", map[string]any{"type": "http", "service_id": "web-one"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", r.StatusCode)
	}
	r.Body.Close()
}
