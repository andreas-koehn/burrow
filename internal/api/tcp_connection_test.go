package api

// The live connections of a tcp service through the API (native tcp, P04).

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// liveConns is a ServiceConnections over a list, recording what was closed.
type liveConns struct {
	mu     sync.Mutex
	conns  map[string][]server.ConnView
	closed []string
}

func (l *liveConns) ServiceConnections(id string) []server.ConnView {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]server.ConnView(nil), l.conns[id]...)
}

func (l *liveConns) CloseServiceConnection(serviceID, connID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, c := range l.conns[serviceID] {
		if c.ID == connID {
			l.conns[serviceID] = append(l.conns[serviceID][:i], l.conns[serviceID][i+1:]...)
			l.closed = append(l.closed, connID)
			return true
		}
	}
	return false
}

func tcpServiceDetail(id, typ string) store.ServiceDetail {
	return store.ServiceDetail{ServiceView: store.ServiceView{ID: id, Name: "name-" + id, Type: typ}}
}

// The list shows visitors and forwards, the oldest first, with the
// forwarder's email; a delete closes one, is audited, and a second delete
// of it is a 404.
func TestServiceConnections_ListAndClose(t *testing.T) {
	since := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	live := &liveConns{conns: map[string][]server.ConnView{"rdp": {
		{ID: "c2", Kind: server.ConnKindForward, SourceIP: "198.51.100.4", UserID: "u-2", TokenName: "laptop",
			SessionID: "s-9", Since: since.Add(time.Minute), BytesIn: 10, BytesOut: 20},
		{ID: "c1", Kind: server.ConnKindVisitor, SourceIP: "203.0.113.7", Since: since, BytesIn: 1, BytesOut: 2},
	}}}
	a := newTCPAPI(t, func(d *Deps) { d.Connections = live })
	a.svcs.getSvc = tcpServiceDetail("rdp", "tcp")

	r := a.c.get(t, "/api/v1/services/rdp/connections")
	var list []serviceConnectionResp
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil || r.StatusCode != http.StatusOK || len(list) != 2 {
		t.Fatalf("list %d %v %+v", r.StatusCode, err, list)
	}
	if list[0].ID != "c1" || list[0].Kind != "visitor" || list[0].UserID != "" || list[0].UserEmail != "" || list[0].BytesIn != 1 {
		t.Fatalf("visitor %+v", list[0])
	}
	if f := list[1]; f.ID != "c2" || f.Kind != "forward" || f.UserID != "u-2" || f.UserEmail != "self@x" || f.TokenName != "laptop" ||
		f.SessionID != "s-9" || f.SourceIP != "198.51.100.4" || f.BytesOut != 20 || !f.Since.Equal(since.Add(time.Minute)) {
		t.Fatalf("forward %+v", f)
	}

	if r := a.c.delete(t, "/api/v1/services/rdp/connections/c2"); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete %d %s", r.StatusCode, readBody(t, r))
	}
	if len(live.closed) != 1 || live.closed[0] != "c2" {
		t.Fatalf("closed %v", live.closed)
	}
	if n := len(a.audits.events); n != 1 {
		t.Fatalf("%d audit events", n)
	}
	ev := a.audits.events[0]
	if ev.Action != audit.ActionServiceConnectionClose || ev.SubjectID != "rdp" ||
		!strings.Contains(string(ev.Payload), `"connection_id":"c2"`) || !strings.Contains(string(ev.Payload), `"kind":"forward"`) {
		t.Fatalf("audit %+v %s", ev, ev.Payload)
	}
	if r := a.c.delete(t, "/api/v1/services/rdp/connections/c2"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("delete again %d", r.StatusCode)
	}
	if len(a.audits.events) != 1 {
		t.Fatal("a delete of nothing was audited")
	}
}

// An http service has no live connections to list (409); without a relay the
// list is empty; a service the store does not give the caller is 404.
func TestServiceConnections_Refusals(t *testing.T) {
	a := newTCPAPI(t, nil)
	a.svcs.getSvc = tcpServiceDetail("web", "http")
	if r := a.c.get(t, "/api/v1/services/web/connections"); r.StatusCode != http.StatusConflict {
		t.Fatalf("http service %d", r.StatusCode)
	}
	if r := a.c.delete(t, "/api/v1/services/web/connections/x"); r.StatusCode != http.StatusConflict {
		t.Fatalf("http service delete %d", r.StatusCode)
	}
	a.svcs.getSvc = tcpServiceDetail("rdp", "tcp")
	r := a.c.get(t, "/api/v1/services/rdp/connections")
	if body := strings.TrimSpace(readBody(t, r)); r.StatusCode != http.StatusOK || body != "[]" {
		t.Fatalf("no relay: %d %s", r.StatusCode, body)
	}
	a.svcs.getSvcErr = db.ErrNotFound
	if r := a.c.get(t, "/api/v1/services/rdp/connections"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("not the caller's %d", r.StatusCode)
	}
}
