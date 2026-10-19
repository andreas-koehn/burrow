package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ankoehn/burrow/internal/store"
)

type fakeSettings struct {
	saved   map[string]string
	testErr error
}

func (f *fakeSettings) GetSettings(context.Context) (map[string]string, error) {
	return map[string]string{"smtp.host": "mx", "smtp.port": "587", "smtp.tls": "starttls"}, nil
}
func (f *fakeSettings) SaveSettings(_ context.Context, kv map[string]string) error {
	f.saved = kv
	return nil
}
func (f *fakeSettings) SendTestEmail(_ context.Context, to string) error { return f.testErr }

func TestSettingsEndpoints(t *testing.T) {
	fs := &fakeSettings{}
	d := Deps{Log: discardLog(), Settings: fs, Users: &fakeUserStore{role: "admin"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	// GET never includes a password key
	r := c.get(t, "/api/v1/settings")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("get settings status=%d", r.StatusCode)
	}
	var got map[string]string
	json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if _, bad := got["smtp.password"]; bad {
		t.Fatal("settings GET must never expose smtp.password")
	}

	// PUT saves only whitelisted keys
	r = c.put(t, "/api/v1/settings", map[string]string{
		"smtp.host": "mail", "smtp.port": "25", "smtp.tls": "none",
		"smtp.password": "leak", "evil": "x",
	})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("put settings status=%d", r.StatusCode)
	}
	r.Body.Close()
	if _, leaked := fs.saved["smtp.password"]; leaked {
		t.Fatal("PUT must reject smtp.password into the DB")
	}
	if _, junk := fs.saved["evil"]; junk {
		t.Fatal("PUT must drop non-whitelisted keys")
	}
	if fs.saved["smtp.host"] != "mail" {
		t.Fatalf("saved: %+v", fs.saved)
	}

	// test-email success
	r = c.post(t, "/api/v1/settings/test-email", map[string]string{"to": "ops@x.io"})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("test-email ok status=%d", r.StatusCode)
	}
	r.Body.Close()

	// test-email unconfigured -> 409
	fs.testErr = store.ErrSMTPUnconfigured
	r = c.post(t, "/api/v1/settings/test-email", map[string]string{"to": "ops@x.io"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("test-email unconfigured status=%d want 409", r.StatusCode)
	}
	r.Body.Close()

	// test-email send failure -> 502
	fs.testErr = errors.New("dial tcp: refused")
	r = c.post(t, "/api/v1/settings/test-email", map[string]string{"to": "ops@x.io"})
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("test-email fail status=%d want 502", r.StatusCode)
	}
	r.Body.Close()
}

// services_domain is read-only on the relay: GET /settings answers it from
// BURROW_SERVICES_DOMAIN ("" when unset) and PUT neither stores nor needs it.
func TestSettingsGetServicesDomain(t *testing.T) {
	for _, domain := range []string{"", "apps.example.com"} {
		fs := &fakeSettings{}
		d := Deps{Log: discardLog(), Settings: fs, Users: &fakeUserStore{role: "admin"}, ServicesDomain: domain}
		srv := httptest.NewServer(NewRouter(d))
		c := authedClient(t, srv)
		r := c.get(t, "/api/v1/settings")
		var got map[string]string
		json.NewDecoder(r.Body).Decode(&got)
		r.Body.Close()
		v, ok := got["services_domain"]
		if !ok || v != domain {
			t.Errorf("domain %q: services_domain = %q (present %v)", domain, v, ok)
		}
		r = c.put(t, "/api/v1/settings", map[string]string{"smtp.host": "m", "services_domain": "evil.example"})
		r.Body.Close()
		if _, stored := fs.saved["services_domain"]; stored {
			t.Errorf("PUT stored services_domain")
		}
		srv.Close()
	}
}

// Every signed-in caller learns the services domain from /me, not only a
// holder of settings:manage: the Services pages print tunnel addresses and
// warn about the shared origin for anyone (T14).
func TestMeServicesDomain(t *testing.T) {
	for _, domain := range []string{"", "apps.example.com"} {
		d := Deps{Log: discardLog(), Settings: &fakeSettings{}, Users: &fakeUserStore{role: "user"}, ServicesDomain: domain}
		srv := httptest.NewServer(NewRouter(d))
		c := authedClient(t, srv)
		r := c.get(t, "/api/v1/me")
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		r.Body.Close()
		v, ok := got["services_domain"]
		if !ok || v != domain {
			t.Errorf("domain %q: /me services_domain = %v (present %v)", domain, v, ok)
		}
		srv.Close()
	}
}
