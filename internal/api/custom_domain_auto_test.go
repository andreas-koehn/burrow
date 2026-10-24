package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/relaynames"
)

// fakeDomainCerts records what the handlers ask of the ACME side (H10).
type fakeDomainCerts struct {
	mu        sync.Mutex
	managed   []string // "svc|host"
	unmanaged []string
	ready     map[string]bool
	manageErr error
}

func (f *fakeDomainCerts) Manage(serviceID, hostname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.managed = append(f.managed, serviceID+"|"+hostname)
	return f.manageErr
}

func (f *fakeDomainCerts) Unmanage(hostname string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmanaged = append(f.unmanaged, hostname)
}

func (f *fakeDomainCerts) Ready(hostname string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready[hostname]
}

type autoAPI struct {
	ds    *stubCustomDomainStore
	certs *fakeDomainCerts
	aud   *stubAuditAppender
	c     *authClient
}

// newAutoAPI serves the custom-domain routes. acme: the relay obtains
// certificates (Deps.CustomDomainCerts set); fileTLS: the dashboard serves a
// file certificate without ACME; neither: TLS ends at the proxy in front.
func newAutoAPI(t *testing.T, acme, fileTLS bool) *autoAPI {
	t.Helper()
	a := &autoAPI{ds: newStubDomainStore(), certs: &fakeDomainCerts{ready: map[string]bool{}}, aud: &stubAuditAppender{}}
	d := newDomainDeps(a.ds)
	d.AuditAppender = a.aud
	d.AuthDomain = "burrow.example.com"
	d.RelayNames = relaynames.New([]string{"burrow.example.com"})
	if acme {
		d.CustomDomainCerts = a.certs
	}
	d.CustomDomainFileTLS = fileTLS
	d.CustomDomainAutoCaps = &db.AutoDomainCaps{Max: 7, PerUser: 3}
	srv := httptest.NewServer(NewRouter(d))
	t.Cleanup(srv.Close)
	a.c = authedClient(t, srv)
	return a
}

func autoBody(host string) map[string]any {
	return map[string]any{"hostname": host, "cert_source": "auto"}
}

func decodeDomain(t *testing.T, r *http.Response) map[string]any {
	t.Helper()
	defer r.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func (a *autoAPI) auditPayload(t *testing.T, action string) map[string]string {
	t.Helper()
	for _, e := range a.aud.events {
		if e.Action == action {
			var p map[string]string
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("payload %s: %v", e.Payload, err)
			}
			return p
		}
	}
	t.Fatalf("no %s audit event in %+v", action, a.aud.events)
	return nil
}

// Under ACME, an auto custom domain is created without a certificate, the
// relay starts obtaining one for it, and the answer says pending; ready
// once certmagic has it.
func TestCustomDomainAuto_CreateUnderACME(t *testing.T) {
	a := newAutoAPI(t, true, false)
	r := a.c.post(t, "/api/v1/services/svc1/domains", autoBody("Grpc.Example.com."))
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", r.StatusCode, readBody(t, r))
	}
	out := decodeDomain(t, r)
	if out["cert_source"] != "auto" || out["cert_status"] != "pending" || out["hostname"] != "grpc.example.com" || out["cert_sha256"] != "" {
		t.Fatalf("answer %v", out)
	}
	row := a.ds.lastInsert
	if row.CertSource != db.CertSourceAuto || row.CertPEM != "" || row.KeyPEM != "" || row.CertSHA256 != "" || !row.NotAfter.IsZero() {
		t.Fatalf("stored %+v", row)
	}
	if !slices.Equal(a.certs.managed, []string{"svc1|grpc.example.com"}) {
		t.Fatalf("managed %v", a.certs.managed)
	}
	if p := a.auditPayload(t, audit.ActionServiceCustomDomainAdd); p["cert_source"] != "auto" || p["hostname"] != "grpc.example.com" {
		t.Fatalf("audit %v", p)
	}

	a.certs.ready["grpc.example.com"] = true
	out = decodeDomain(t, a.c.get(t, "/api/v1/services/svc1/domains/did-1"))
	if out["cert_status"] != "ready" {
		t.Fatalf("GET %v", out)
	}
	r = a.c.get(t, "/api/v1/services/svc1/domains")
	var list []map[string]any
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil || len(list) != 1 || list[0]["cert_status"] != "ready" {
		t.Fatalf("list %v %v", list, err)
	}
	_ = r.Body.Close()
}

// Behind a proxy that terminates TLS (a plain dashboard listener), auto
// needs no certificate: not_needed, and nothing is ordered.
func TestCustomDomainAuto_BehindAProxy(t *testing.T) {
	a := newAutoAPI(t, false, false)
	r := a.c.post(t, "/api/v1/services/svc1/domains", map[string]any{
		"hostname": "grpc.example.com", "cert_source": "auto", "cert_pem": "", "key_pem": "",
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", r.StatusCode, readBody(t, r))
	}
	if out := decodeDomain(t, r); out["cert_status"] != "not_needed" || out["cert_source"] != "auto" {
		t.Fatalf("answer %v", out)
	}
	if len(a.certs.managed) != 0 {
		t.Fatalf("managed %v", a.certs.managed)
	}
}

// The refusals of auto, each 400 and nothing stored: a file certificate
// without ACME (nobody could serve the name), a wildcard, a certificate
// sent along, an unknown source.
func TestCustomDomainAuto_Refusals(t *testing.T) {
	for _, c := range []struct {
		name          string
		acme, fileTLS bool
		body          map[string]any
		want          string
	}{
		{"file certificate", false, true, autoBody("grpc.example.com"), "ACME"},
		{"wildcard", true, false, autoBody("*.example.com"), "wildcard"},
		{"certificate along", true, false, map[string]any{"hostname": "grpc.example.com", "cert_source": "auto", "cert_pem": "x", "key_pem": "y"}, "cert_pem"},
		{"unknown source", true, false, map[string]any{"hostname": "grpc.example.com", "cert_source": "letsencrypt"}, "cert_source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAutoAPI(t, c.acme, c.fileTLS)
			for _, r := range []*http.Response{
				a.c.post(t, "/api/v1/services/svc1/domains", c.body),
				a.c.put(t, "/api/v1/services/svc1/domains/did-1", c.body),
			} {
				if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, c.want) {
					t.Errorf("%s %s: %d %s, want 400 %q", r.Request.Method, c.name, r.StatusCode, body, c.want)
				}
			}
			if a.ds.lastInsert.Hostname != "" || a.ds.lastUpdate.Hostname != "" || len(a.certs.managed) != 0 {
				t.Fatalf("stored or managed: %+v %+v %v", a.ds.lastInsert, a.ds.lastUpdate, a.certs.managed)
			}
		})
	}
}

// uploaded, named or left out (an older dashboard), still needs the
// certificate and key.
func TestCustomDomainAuto_UploadedStillNeedsACertificate(t *testing.T) {
	a := newAutoAPI(t, true, false)
	for _, body := range []map[string]any{
		{"hostname": "app.example.com"},
		{"hostname": "app.example.com", "cert_source": "uploaded"},
		{"hostname": "app.example.com", "cert_source": "uploaded", "cert_pem": "x"},
	} {
		for _, r := range []*http.Response{
			a.c.post(t, "/api/v1/services/svc1/domains", body),
			a.c.put(t, "/api/v1/services/svc1/domains/did-1", body),
		} {
			if b := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(b, "cert_pem and key_pem are required") {
				t.Errorf("%s %v: %d %s", r.Request.Method, body, r.StatusCode, b)
			}
		}
	}
	if len(a.certs.managed) != 0 {
		t.Fatalf("managed %v", a.certs.managed)
	}
}

// An uploaded domain answers cert_source uploaded, cert_status ready, and
// its audit row says so; nothing is ordered.
func TestCustomDomainAuto_UploadedIsReady(t *testing.T) {
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"app.example.com"})
	a := &autoAPI{ds: newStubDomainStore(), certs: &fakeDomainCerts{}, aud: &stubAuditAppender{}}
	d := newDomainDepsWithRoots(a.ds, pool)
	d.AuditAppender = a.aud
	d.CustomDomainCerts = a.certs
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	a.c = authedClient(t, srv)

	r := a.c.post(t, "/api/v1/services/svc1/domains", map[string]any{"hostname": "app.example.com", "cert_pem": certPEM, "key_pem": keyPEM})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", r.StatusCode, readBody(t, r))
	}
	if out := decodeDomain(t, r); out["cert_source"] != "uploaded" || out["cert_status"] != "ready" {
		t.Fatalf("answer %v", out)
	}
	if a.ds.lastInsert.CertSource != db.CertSourceUploaded {
		t.Fatalf("stored %q", a.ds.lastInsert.CertSource)
	}
	if p := a.auditPayload(t, audit.ActionServiceCustomDomainAdd); p["cert_source"] != "uploaded" || p["cert_sha256"] == "" {
		t.Fatalf("audit %v", p)
	}
	if len(a.certs.managed) != 0 {
		t.Fatalf("managed %v", a.certs.managed)
	}

	// Switching it to auto starts the order and audits the source.
	r = a.c.put(t, "/api/v1/services/svc1/domains/did-1", autoBody("app.example.com"))
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	if !slices.Equal(a.certs.managed, []string{"svc1|app.example.com"}) || len(a.certs.unmanaged) != 0 {
		t.Fatalf("managed %v unmanaged %v", a.certs.managed, a.certs.unmanaged)
	}
	if p := a.auditPayload(t, audit.ActionServiceCustomDomainUpdate); p["cert_source"] != "auto" || p["cert_sha256"] != "" {
		t.Fatalf("audit %v", p)
	}
	if row := a.ds.rows["did-1"]; row.CertSource != db.CertSourceAuto || row.CertPEM != "" || row.KeyPEM != "" {
		t.Fatalf("stored %+v", row)
	}

	// Back to uploaded: the order stops.
	r = a.c.put(t, "/api/v1/services/svc1/domains/did-1", map[string]any{"hostname": "app.example.com", "cert_source": "uploaded", "cert_pem": certPEM, "key_pem": keyPEM})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	if !slices.Equal(a.certs.unmanaged, []string{"app.example.com"}) {
		t.Fatalf("unmanaged %v", a.certs.unmanaged)
	}
}

// The stored status follows an uploaded certificate's expiry; an auto row
// switched from an expired upload says active.
func TestCustomDomainAuto_StatusOfAnAutoRow(t *testing.T) {
	a := newAutoAPI(t, true, false)
	a.ds.rows["did-1"] = db.ServiceCustomDomain{ID: "did-1", ServiceID: "svc1", Hostname: "auto.example.com", CertSource: db.CertSourceAuto, Status: "cert_expired"}
	if out := decodeDomain(t, a.c.get(t, "/api/v1/services/svc1/domains/did-1")); out["status"] != "active" {
		t.Fatalf("status %v", out["status"])
	}
}

// Renaming an auto domain moves the order; keeping the name keeps it.
func TestCustomDomainAuto_RenameMovesTheOrder(t *testing.T) {
	a := newAutoAPI(t, true, false)
	a.ds.rows["did-1"] = db.ServiceCustomDomain{ID: "did-1", ServiceID: "svc1", Hostname: "old.example.com", CertSource: db.CertSourceAuto}

	if r := a.c.put(t, "/api/v1/services/svc1/domains/did-1", autoBody("old.example.com")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	if len(a.certs.unmanaged) != 0 {
		t.Fatalf("unmanaged %v", a.certs.unmanaged)
	}
	if r := a.c.put(t, "/api/v1/services/svc1/domains/did-1", autoBody("new.example.com")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %d %s", r.StatusCode, readBody(t, r))
	}
	if !slices.Equal(a.certs.unmanaged, []string{"old.example.com"}) || a.certs.managed[len(a.certs.managed)-1] != "svc1|new.example.com" {
		t.Fatalf("managed %v unmanaged %v", a.certs.managed, a.certs.unmanaged)
	}
}

// Deleting an auto domain stops its order; an uploaded one has none.
func TestCustomDomainAuto_DeleteUnmanages(t *testing.T) {
	a := newAutoAPI(t, true, false)
	a.ds.rows["did-1"] = db.ServiceCustomDomain{ID: "did-1", ServiceID: "svc1", Hostname: "auto.example.com", CertSource: db.CertSourceAuto}
	a.ds.rows["did-2"] = db.ServiceCustomDomain{ID: "did-2", ServiceID: "svc1", Hostname: "up.example.com", CertSource: db.CertSourceUploaded, NotAfter: time.Now().Add(time.Hour)}
	for _, id := range []string{"did-1", "did-2"} {
		if r := a.c.delete(t, "/api/v1/services/svc1/domains/"+id); r.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE %s %d", id, r.StatusCode)
		}
	}
	if !slices.Equal(a.certs.unmanaged, []string{"auto.example.com"}) {
		t.Fatalf("unmanaged %v", a.certs.unmanaged)
	}
	if p := a.auditPayload(t, audit.ActionServiceCustomDomainDelete); p["cert_source"] != "auto" {
		t.Fatalf("audit %v", p)
	}
}

// The H12 409s come before anything of cert_source: a relay-own name is a
// conflict even where auto itself would be refused.
func TestCustomDomainAuto_OwnershipConflictsComeFirst(t *testing.T) {
	a := newAutoAPI(t, false, true)
	for _, h := range []string{"burrow.example.com", "x.burrow.example.com"} {
		body := map[string]any{"hostname": h, "cert_source": "nonsense", "cert_pem": "x"}
		if r := a.c.post(t, "/api/v1/services/svc1/domains", body); r.StatusCode != http.StatusConflict {
			t.Errorf("POST %s: %d %s", h, r.StatusCode, readBody(t, r))
		}
		if r := a.c.put(t, "/api/v1/services/svc1/domains/did-1", body); r.StatusCode != http.StatusConflict {
			t.Errorf("PUT %s: %d %s", h, r.StatusCode, readBody(t, r))
		}
	}
}

// A failure to start the order does not fail the write: the domain is
// bound, its status stays pending, and the failure is logged and audited
// by the ACME side.
func TestCustomDomainAuto_AManageErrorKeepsTheDomain(t *testing.T) {
	a := newAutoAPI(t, true, false)
	a.certs.manageErr = errors.New("storage broken")
	r := a.c.post(t, "/api/v1/services/svc1/domains", autoBody("grpc.example.com"))
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", r.StatusCode, readBody(t, r))
	}
	if out := decodeDomain(t, r); out["cert_status"] != "pending" {
		t.Fatalf("answer %v", out)
	}
}

// ─── Host dispatch on the dashboard listener (H10) ──────────────────────────

func TestCustomDomainSwitch_Precedence(t *testing.T) {
	owns := relaynames.New([]string{"burrow.example.com"}, "apps.example.com", "acme.example.org").Owns
	bound := map[string]bool{
		"grpc.example.com": true,
		// rows from before the H12 checks: the relay's names still win
		"burrow.example.com": true, "apps.example.com": true, "x.burrow.example.com": true, "acme.example.org": true,
	}
	var lookups []string
	lookup := func(_ context.Context, host string) (bool, error) {
		lookups = append(lookups, host)
		if host == "broken.example.com" {
			return false, errors.New("db down")
		}
		return bound[host], nil
	}
	var siteHosts []string
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		siteHosts = append(siteHosts, r.Host)
		_, _ = io.WriteString(w, "site")
	})
	router := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "router") })
	h := CustomDomainSwitch(owns, lookup, site, router, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for host, want := range map[string]string{
		"grpc.example.com":      "site",
		"GRPC.example.com.:443": "site",
		"grpc.example.com:8080": "site",
		"burrow.example.com":    "router",
		"x.burrow.example.com":  "router",
		"apps.example.com":      "router",
		"acme.example.org":      "router",
		"localhost:8080":        "router",
		"127.0.0.1":             "router",
		"[::1]:443":             "router",
		"":                      "router",
		"unknown.example.com":   "router",
		"broken.example.com":    "router",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Body.String() != want {
			t.Errorf("Host %q went to the %s, want the %s", host, rec.Body.String(), want)
		}
	}
	for _, l := range lookups {
		if owns(l) {
			t.Errorf("the relay's own name %q was looked up", l)
		}
	}
	// The proxy behind routes on Host as well: it gets the name it was
	// matched by, with the port.
	slices.Sort(siteHosts)
	if want := []string{"grpc.example.com", "grpc.example.com:443", "grpc.example.com:8080"}; !slices.Equal(siteHosts, want) {
		t.Errorf("site saw Host %v, want %v", siteHosts, want)
	}
}
