package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
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

type deleteFixture struct {
	*authzHarness
	aud    *stubAuditAppender
	closed *closedServices
	forgot *forgotten
}

func newDeleteFixture(t *testing.T) *deleteFixture {
	t.Helper()
	f := &deleteFixture{aud: &stubAuditAppender{}, closed: &closedServices{}, forgot: &forgotten{}}
	f.authzHarness = newAuthzHarness(t, func(d *Deps) {
		d.AuditAppender = f.aud
		d.ServiceSessions = f.closed
		d.OnServiceDeleted = f.forgot.forget
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
	// So does a model that targets it; the message names the provider still.
	if _, err := f.st.CreateModel(ctx, db.AIModel{Name: "burrow-simple", Enabled: true,
		Targets: []db.AIModelTarget{{Dialect: "openai", ProviderSlug: "ollama", TargetModel: "m"}}}); err != nil {
		t.Fatal(err)
	}
	check("ollama")
	if len(f.aud.events) != 0 || len(f.closed.ids) != 0 {
		t.Fatal("side effects of a refused delete")
	}
}
