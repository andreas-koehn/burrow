package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
)

// closedServices records which services were asked to drop their sessions.
type closedServices struct{ ids []string }

func (c *closedServices) CloseByService(id string) int {
	c.ids = append(c.ids, id)
	return 1
}

// forgotten records which services the in-memory state was dropped for.
type forgotten struct{ ids []string }

func (f *forgotten) forget(id string) { f.ids = append(f.ids, id) }

// deleteLive answers that the listed services have a live tunnel.
type deleteLive map[string]bool

func (l deleteLive) LookupByServiceID(id string) (LiveTunnelSnapshot, bool) {
	return LiveTunnelSnapshot{Connected: l[id], SessionID: "s1"}, l[id]
}

func (deleteLive) LookupByTunnelID(string) (TunnelLocator, bool) { return TunnelLocator{}, false }

// invalidated records the hostnames evicted from the certificate cache.
type invalidated struct{ hosts []string }

func (i *invalidated) InvalidateHostname(h string) { i.hosts = append(i.hosts, h) }

type deleteFixture struct {
	*authzHarness
	aud    *stubAuditAppender
	closed *closedServices
	forgot *forgotten
	live   deleteLive
	certs  *invalidated
}

func newDeleteFixture(t *testing.T, mods ...func(*Deps)) *deleteFixture {
	t.Helper()
	f := &deleteFixture{aud: &stubAuditAppender{}, closed: &closedServices{}, forgot: &forgotten{},
		live: deleteLive{}, certs: &invalidated{}}
	f.authzHarness = newAuthzHarness(t, func(d *Deps) {
		d.AuditAppender = f.aud
		d.ServiceSessions = f.closed
		d.OnServiceDeleted = f.forgot.forget
		d.LiveTunnels = f.live
		d.CustomDomainCache = f.certs
		for _, m := range mods {
			m(d)
		}
	})
	return f
}

func (f *deleteFixture) exists(t *testing.T, id string) bool {
	t.Helper()
	_, err := db.Wrap(f.sqldb).GetServiceByID(context.Background(), id)
	return err == nil
}

// otherUser is a second plain user, who owns nothing.
func (f *deleteFixture) otherUser(t *testing.T) func(*http.Request) {
	t.Helper()
	if _, err := f.sqldb.Exec(`INSERT INTO users(id, email, password_hash, role) VALUES('u-other','other@x','x','user')`); err != nil {
		t.Fatal(err)
	}
	sid, err := f.st.CreateSession(context.Background(), "u-other", "authz-test", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	return func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf"})
		req.Header.Set("X-CSRF-Token", "csrf")
	}
}

func TestDeleteService_OwnerDeletes(t *testing.T) {
	f := newDeleteFixture(t)
	rec := f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, f.session(t, "user"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if f.exists(t, authzServiceID) {
		t.Fatal("the service is still there")
	}
	if got := f.do(t, http.MethodGet, "/api/v1/services/"+authzServiceID, nil, f.session(t, "user")); got.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", got.Code)
	}
	if len(f.aud.events) != 1 {
		t.Fatalf("audit events: %+v", f.aud.events)
	}
	ev := f.aud.events[0]
	if ev.Action != audit.ActionServiceDelete || ev.SubjectID != authzServiceID || ev.SubjectLabel != "web" || ev.ActorID != "u-user" {
		t.Fatalf("audit event: %+v", ev)
	}
	var payload map[string]string
	if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload["slug"] != "web" || payload["owner_id"] != "u-user" {
		t.Fatalf("audit payload: %s (%v)", ev.Payload, err)
	}
	if len(f.closed.ids) != 1 || f.closed.ids[0] != authzServiceID {
		t.Fatalf("sessions closed for %v", f.closed.ids)
	}
	if len(f.forgot.ids) != 1 || f.forgot.ids[0] != authzServiceID {
		t.Fatalf("state forgotten for %v", f.forgot.ids)
	}
}

func TestDeleteService_OtherUserGets404(t *testing.T) {
	f := newDeleteFixture(t)
	rec := f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, f.otherUser(t))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete by another user: %d %s", rec.Code, rec.Body)
	}
	if !f.exists(t, authzServiceID) {
		t.Fatal("the service was deleted")
	}
	if len(f.aud.events) != 0 || len(f.closed.ids) != 0 || len(f.forgot.ids) != 0 {
		t.Fatalf("side effects of a refused delete: %v %v %v", f.aud.events, f.closed.ids, f.forgot.ids)
	}
}

func TestDeleteService_AnyHolderDeletes(t *testing.T) {
	f := newDeleteFixture(t)
	for _, role := range []string{"admin", "cr-legacy"} {
		id := "svc-" + role
		if err := f.st.CreateService(context.Background(), db.Service{
			ID: id, UserID: "u-user", Name: "n-" + role, Type: "http", Subdomain: "s-" + role, AccessMode: "open",
		}); err != nil {
			t.Fatal(err)
		}
		if rec := f.do(t, http.MethodDelete, "/api/v1/services/"+id, nil, f.session(t, role)); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: delete: %d %s", role, rec.Code, rec.Body)
		}
		if f.exists(t, id) {
			t.Fatalf("%s: the service is still there", role)
		}
	}
}

func TestDeleteService_UnknownIs404(t *testing.T) {
	f := newDeleteFixture(t)
	rec := f.do(t, http.MethodDelete, "/api/v1/services/nope", nil, f.session(t, "admin"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: %d", rec.Code)
	}
	if len(f.aud.events) != 0 {
		t.Fatalf("audit for an unknown service: %+v", f.aud.events)
	}
}

func TestDeleteService_ProviderBlocks(t *testing.T) {
	f := newDeleteFixture(t)
	ctx := context.Background()
	if err := f.st.CreateService(ctx, db.Service{
		ID: "svc-llm", UserID: "u-user", Name: "llm", Type: "http", Subdomain: "llm", AccessMode: "api_key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateTunnelProvider(ctx, "ollama", "Ollama", "svc-llm", ""); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		rec := f.do(t, http.MethodDelete, "/api/v1/services/svc-llm", nil, f.session(t, "user"))
		if rec.Code != http.StatusConflict {
			t.Fatalf("delete: %d %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("message %q lacks %q", rec.Body, want)
		}
		if !f.exists(t, "svc-llm") {
			t.Fatal("the service is gone")
		}
		if _, err := f.st.ProviderBySlug(ctx, "ollama"); err != nil {
			t.Fatalf("the provider went with it: %v", err)
		}
	}
	// A tunnel provider alone blocks.
	check("ollama")
	// So does a model that targets it; the message names the model.
	if _, err := f.st.CreateModel(ctx, db.AIModel{Name: "burrow-simple", Enabled: true,
		Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "m"}}}); err != nil {
		t.Fatal(err)
	}
	check("burrow-simple")
	if len(f.aud.events) != 0 || len(f.closed.ids) != 0 {
		t.Fatal("side effects of a refused delete")
	}
}

func TestDeleteService_ConnectedIs409(t *testing.T) {
	f := newDeleteFixture(t)
	f.live[authzServiceID] = true
	rec := f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, f.session(t, "user"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete of a connected service: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "the service is connected; stop the client or remove the tunnel from its config, then delete") {
		t.Fatalf("message: %s", rec.Body)
	}
	if !f.exists(t, authzServiceID) || len(f.aud.events) != 0 || len(f.forgot.ids) != 0 {
		t.Fatal("a refused delete changed something")
	}
	// Another user is not told that it is connected.
	if rec := f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, f.otherUser(t)); rec.Code != http.StatusNotFound {
		t.Fatalf("another user: %d", rec.Code)
	}
	// Once the client is gone the delete goes through.
	delete(f.live, authzServiceID)
	if rec := f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, f.session(t, "user")); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
}

func TestDeleteService_EvictsCertificatesOfItsDomains(t *testing.T) {
	f := newDeleteFixture(t)
	x := db.Wrap(f.sqldb)
	for _, h := range []string{"a.example.com", "b.example.com"} {
		if _, err := x.InsertCustomDomain(context.Background(), db.ServiceCustomDomain{
			ServiceID: authzServiceID, Hostname: h, CertPEM: "c", KeyPEM: "k", CertSHA256: "x",
			NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A refused delete leaves the cache alone.
	f.live[authzServiceID] = true
	f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, f.session(t, "user"))
	if len(f.certs.hosts) != 0 {
		t.Fatalf("evicted %v for a refused delete", f.certs.hosts)
	}
	delete(f.live, authzServiceID)
	if rec := f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, f.session(t, "user")); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	got := slices.Clone(f.certs.hosts)
	sort.Strings(got)
	if !slices.Equal(got, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("evicted %v", got)
	}
}

// Usage rows keep the id of a deleted service; the cost views read them
// without a lookup of the service, so they answer and carry the id.
func TestDeleteService_UsageHistoryStillReads(t *testing.T) {
	f := &deleteFixture{aud: &stubAuditAppender{}, closed: &closedServices{}, forgot: &forgotten{}, live: deleteLive{}, certs: &invalidated{}}
	f.authzHarness = newAuthzHarness(t, func(d *Deps) {
		if x, ok := d.Budgets.(*db.DB); ok {
			d.CostEngine = cost.New(x, cost.Pricing{})
		}
	})
	if _, err := f.sqldb.Exec(`INSERT INTO usage_events(id, service_id, ts, kind, tokens_in, tokens_out, cost_usd) VALUES('ue1', ?, ?, 'openai', 10, 5, 0.25)`,
		authzServiceID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	admin := f.session(t, "admin")
	if rec := f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec := f.do(t, http.MethodGet, "/api/v1/cost/export?window=today", nil, admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), authzServiceID) {
		t.Fatalf("export: %d %s", rec.Code, rec.Body)
	}
	rec = f.do(t, http.MethodGet, "/api/v1/cost/summary?window=today", nil, admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total_usd":0.25`) {
		t.Fatalf("summary: %d %s", rec.Code, rec.Body)
	}
}
