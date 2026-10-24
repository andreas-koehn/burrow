package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// R09 review C1(a): auto proves no control of the name, so only a holder
// of services:configure:any may use it; an :own user still uploads, since
// the chain to a system root is the proof.
func TestCustomDomainAuto_NeedsAnAdministrator(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"app.example.com"})
	ds := newStubDomainStore()
	certs := &fakeDomainCerts{ready: map[string]bool{}}
	d := newDomainDepsWithRoots(ds, pool)
	d.Users = &fakeUserStore{role: "user"}
	d.CustomDomainCerts = certs
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	for _, r := range []*http.Response{
		c.post(t, "/api/v1/services/svc1/domains", autoBody("grpc.example.com")),
		c.put(t, "/api/v1/services/svc1/domains/did-1", autoBody("grpc.example.com")),
	} {
		if body := readBody(t, r); r.StatusCode != http.StatusForbidden || !strings.Contains(body, "automatic certificates need an administrator; upload a certificate instead") {
			t.Errorf("%s: %d %s", r.Request.Method, r.StatusCode, body)
		}
	}
	if len(certs.managed) != 0 || ds.lastInsert.Hostname != "" {
		t.Fatalf("managed %v stored %+v", certs.managed, ds.lastInsert)
	}
	r := c.post(t, "/api/v1/services/svc1/domains", map[string]any{"hostname": "app.example.com", "cert_pem": certPEM, "key_pem": keyPEM})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("upload by an :own user: %d %s", r.StatusCode, readBody(t, r))
	}
	_ = r.Body.Close()
	// The ownership 409 still comes first.
	d2 := newDomainDeps(newStubDomainStore())
	d2.Users = &fakeUserStore{role: "user"}
	d2.CustomDomainCerts = certs
	d2.AuthDomain = "burrow.example.com"
	srv2 := httptest.NewServer(NewRouter(d2))
	defer srv2.Close()
	if r := authedClient(t, srv2).post(t, "/api/v1/services/svc1/domains", autoBody("burrow.example.com")); r.StatusCode != http.StatusConflict {
		t.Fatalf("relay name for an :own user: %d %s", r.StatusCode, readBody(t, r))
	}
}

// R09 review C1(b) and I2 through the handlers: the store's overlap is
// 409, its caps are 429, and the relay's caps reach the store.
func TestCustomDomainAuto_OverlapAndCaps(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		want   string
	}{
		{db.ErrHostOverlapsWildcard, http.StatusConflict, "wildcard custom domain of another service"},
		{db.ErrAutoDomainCap, http.StatusTooManyRequests, "BURROW_ACME_CUSTOM_MAX"},
		{db.ErrAutoDomainUserCap, http.StatusTooManyRequests, "BURROW_ACME_CUSTOM_PER_USER"},
	} {
		a := newAutoAPI(t, true, false)
		a.ds.insertErr, a.ds.updateErr = c.err, c.err
		for _, r := range []*http.Response{
			a.c.post(t, "/api/v1/services/svc1/domains", autoBody("grpc.example.com")),
			a.c.put(t, "/api/v1/services/svc1/domains/did-1", autoBody("grpc.example.com")),
		} {
			if body := readBody(t, r); r.StatusCode != c.status || !strings.Contains(body, c.want) {
				t.Errorf("%v %s: %d %s", c.err, r.Request.Method, r.StatusCode, body)
			}
		}
		if len(a.certs.managed) != 0 {
			t.Fatalf("managed %v", a.certs.managed)
		}
	}
	a := newAutoAPI(t, true, false)
	_ = a.c.post(t, "/api/v1/services/svc1/domains", autoBody("grpc.example.com"))
	if a.ds.lastCaps == nil || *a.ds.lastCaps != (db.AutoDomainCaps{Max: 7, PerUser: 3}) {
		t.Fatalf("caps %+v", a.ds.lastCaps)
	}
}

// R09 review M2: a PUT that cannot read the old row still stops the order
// of the name when it is switched to uploaded.
func TestCustomDomainAuto_PutWithoutTheOldRow(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"app.example.com"})
	ds := newStubDomainStore()
	ds.getErr = context.DeadlineExceeded
	certs := &fakeDomainCerts{ready: map[string]bool{}}
	d := newDomainDepsWithRoots(ds, pool)
	d.CustomDomainCerts = certs
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	r := authedClient(t, srv).put(t, "/api/v1/services/svc1/domains/did-1", map[string]any{"hostname": "app.example.com", "cert_pem": certPEM, "key_pem": keyPEM})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("%d %s", r.StatusCode, readBody(t, r))
	}
	if !slices.Equal(certs.unmanaged, []string{"app.example.com"}) {
		t.Fatalf("unmanaged %v", certs.unmanaged)
	}
}

func insertDomains(t *testing.T, x *db.DB) {
	t.Helper()
	for _, d := range []db.ServiceCustomDomain{
		{ServiceID: authzServiceID, Hostname: "auto.example.com", CertSource: db.CertSourceAuto},
		{ServiceID: authzServiceID, Hostname: "up.example.com", CertPEM: "c", KeyPEM: "k", CertSHA256: "x",
			NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)},
	} {
		if _, err := x.InsertCustomDomain(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
}

// R09 review I1: deleting a service stops the orders of its auto domains.
func TestDeleteService_UnmanagesItsAutoDomains(t *testing.T) {
	certs := &fakeDomainCerts{ready: map[string]bool{}}
	f := newDeleteFixture(t, func(d *Deps) { d.CustomDomainCerts = certs })
	insertDomains(t, db.Wrap(f.sqldb))
	if rec := f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, f.session(t, "user")); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if !slices.Equal(certs.unmanaged, []string{"auto.example.com"}) {
		t.Fatalf("unmanaged %v", certs.unmanaged)
	}
}

// R09 review I1 and M5: deleting a user takes the domains of their
// services: the cache drops them and the auto ones stop being ordered.
func TestDeleteUser_EndsTheDomainsOfTheirServices(t *testing.T) {
	certs := &fakeDomainCerts{ready: map[string]bool{}}
	f := newDeleteFixture(t, func(d *Deps) { d.CustomDomainCerts = certs })
	insertDomains(t, db.Wrap(f.sqldb))
	if rec := f.do(t, http.MethodDelete, "/api/v1/users/u-user", nil, f.session(t, "admin")); rec.Code != http.StatusNoContent {
		t.Fatalf("delete user: %d %s", rec.Code, rec.Body)
	}
	got := slices.Clone(f.certs.hosts)
	sort.Strings(got)
	if !slices.Equal(got, []string{"auto.example.com", "up.example.com"}) {
		t.Fatalf("evicted %v", got)
	}
	if !slices.Equal(certs.unmanaged, []string{"auto.example.com"}) {
		t.Fatalf("unmanaged %v", certs.unmanaged)
	}
}
