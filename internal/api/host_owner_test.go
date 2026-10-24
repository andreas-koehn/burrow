package api

// H12: one owner per host name across http and tcp, seen from the API.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/relaynames"
	"github.com/ankoehn/burrow/internal/store"
)

// coveringStore is a custom domain store that also answers
// CustomDomainCovers, as *db.DB does.
type coveringStore struct {
	*stubCustomDomainStore
	covers func(name string) bool
}

func (c coveringStore) CustomDomainCovers(_ context.Context, name string) (bool, error) {
	return c.covers(name), nil
}

// A tcp_hostname that is, or lies under, an ACME name is refused with 409,
// and nothing is stored.
func TestHostOwner_TCPHostnameUnderACMENameIsRefused(t *testing.T) {
	a := newTCPAPI(t, func(d *Deps) {
		d.TLSPassthrough = true
		d.ACMENames = []string{"burrow.example.com", "apps.example.com"}
	})
	a.svcs.tcpHostname = store.TCPHostname{Slug: "t0a1b2c3d4e", AccessMode: "open"}
	for _, name := range []string{"burrow.example.com", "x.burrow.example.com", "a.b.APPS.example.com"} {
		r := a.c.putRaw(t, "/api/v1/services/rdp/tcp-hostname", `{"tcp_hostname": "`+name+`"}`)
		if body := readBody(t, r); r.StatusCode != http.StatusConflict || !strings.Contains(body, "ACME") {
			t.Fatalf("%s: %d %s", name, r.StatusCode, body)
		}
	}
	if a.svcs.lastTCPHostname != "" {
		t.Fatalf("stored %q", a.svcs.lastTCPHostname)
	}
	// A sibling, and a name that only ends alike, are free.
	for _, name := range []string{"rdp.example.com", "xburrow.example.com"} {
		if got := decodeTCPHostname(t, a.c.put(t, "/api/v1/services/rdp/tcp-hostname", map[string]string{"tcp_hostname": name})); got.TCPHostname != name {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

// The store's refusal (a custom domain covers the name) is a 409.
func TestHostOwner_TCPHostnameCoveredByCustomDomain(t *testing.T) {
	a := newTCPAPI(t, func(d *Deps) { d.TLSPassthrough = true })
	a.svcs.tcpHostname = store.TCPHostname{Slug: "t0a1b2c3d4e", AccessMode: "open"}
	a.svcs.tcpHostnameErr = store.ErrTCPHostnameReserved
	r := a.c.putRaw(t, "/api/v1/services/rdp/tcp-hostname", `{"tcp_hostname": "app.example.com"}`)
	if body := readBody(t, r); r.StatusCode != http.StatusConflict || !strings.Contains(body, "custom domain") {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
}

func domainBody(host string) map[string]any {
	return map[string]any{"hostname": host, "cert_pem": "c", "key_pem": "k"}
}

// A custom domain that is, or lies under, the tcp domain, that covers its
// slug names, or that is a relay-own name or under one, is refused with 409
// before its certificate is looked at.
func TestHostOwner_CustomDomainRefusals(t *testing.T) {
	ds := newStubDomainStore()
	d := newDomainDeps(ds)
	d.TCPDomain = "tcp.example.com"
	d.AuthDomain = "burrow.example.com"
	d.ServicesDomain = "apps.example.com"
	d.ACMENames = []string{"burrow.example.com", "apps.example.com", "acme.example.org"}
	d.RelayNames = relaynames.New([]string{"burrow.example.com"}, "apps.example.com", "acme.example.org", "*.cert.example.com", "dash.example.net")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	type tc struct{ host, want string }
	for _, c := range []tc{
		{"tcp.example.com", "tcp host name"},
		{"a.tcp.example.com", "tcp host name"},
		{"*.tcp.example.com", "tcp host name"},
		{"EXAMPLE.com", "tcp host name"}, // a parent: covers every slug name
		{"*.example.com", "tcp host name"},
		{"burrow.example.com", "relay"},
		{"x.burrow.example.com", "relay"},
		{"apps.example.com", "relay"},
		{"y.apps.example.com", "relay"},
		{"acme.example.org", "relay"},
		{"z.acme.example.org", "relay"},
		{"dash.example.net", "relay"},
		{"q.cert.example.com", "relay"},
		{"*.burrow.example.com", "relay"},
	} {
		r := ac.post(t, "/api/v1/services/svc1/domains", domainBody(c.host))
		if body := readBody(t, r); r.StatusCode != http.StatusConflict || !strings.Contains(body, c.want) {
			t.Errorf("POST %s: %d %s, want 409 %q", c.host, r.StatusCode, body, c.want)
		}
		r = ac.put(t, "/api/v1/services/svc1/domains/did-1", domainBody(c.host))
		if body := readBody(t, r); r.StatusCode != http.StatusConflict || !strings.Contains(body, c.want) {
			t.Errorf("PUT %s: %d %s, want 409 %q", c.host, r.StatusCode, body, c.want)
		}
	}
	if ds.lastInsert.Hostname != "" || ds.lastUpdate.Hostname != "" {
		t.Fatalf("stored %q %q", ds.lastInsert.Hostname, ds.lastUpdate.Hostname)
	}
}

// What the store says (a tcp_hostname is in the way) is 409 "hostname is a
// tcp host name" on POST and PUT.
func TestHostOwner_CustomDomainOverATCPHostname(t *testing.T) {
	ds := newStubDomainStore()
	ds.insertErr, ds.updateErr = db.ErrHostIsTCPName, db.ErrHostIsTCPName
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"rdp.example.com"})
	srv := httptest.NewServer(NewRouter(newDomainDepsWithRoots(ds, pool)))
	defer srv.Close()
	ac := authedClient(t, srv)
	body := map[string]any{"hostname": "rdp.example.com", "cert_pem": certPEM, "key_pem": keyPEM}
	for _, r := range []*http.Response{
		ac.post(t, "/api/v1/services/svc1/domains", body),
		ac.put(t, "/api/v1/services/svc1/domains/did-1", body),
	} {
		if b := readBody(t, r); r.StatusCode != http.StatusConflict || !strings.Contains(b, "hostname is a tcp host name") {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
	}
}

// Rows from before the checks: http wins. The GET says which owner shadows
// a name, and a shadowed name is not reachable.
func TestHostOwner_ShadowedBy(t *testing.T) {
	covers := func(n string) bool { return n == "rdp.example.com" || strings.HasSuffix(n, ".app.example.com") }
	a := newTCPAPI(t, func(d *Deps) {
		d.TCPDomain, d.TLSPassthrough = "tcp.example.com", true
		d.CustomDomains = coveringStore{newStubDomainStore(), covers}
	})
	get := func() tcpHostnameResp {
		return decodeTCPHostname(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname"))
	}
	// Not shadowed: no field.
	a.svcs.tcpHostname = store.TCPHostname{Hostname: "ok.example.com", Slug: "t0a1b2c3d4e", AccessMode: "open"}
	if got := get(); got.ShadowedBy != "" || !got.Reachable {
		t.Fatalf("free: %+v", got)
	}
	if body := readBody(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname")); strings.Contains(body, "shadowed_by") {
		t.Fatalf("shadowed_by on a free name: %s", body)
	}
	// The only name is covered by a custom domain (the slug name is not
	// there without a tcp domain slug... it is, so use a service without).
	a.svcs.tcpHostname = store.TCPHostname{Hostname: "rdp.example.com", AccessMode: "open"}
	a.svcs.tcpHostname.Slug = "has.dot" // no slug name
	got := get()
	if got.ShadowedBy != "custom_domain" || got.Reachable || !slices.Equal(got.Hostnames, []string{"rdp.example.com"}) {
		t.Fatalf("shadowed: %+v", got)
	}
	// One name shadowed and the slug name free: still reachable, and told.
	a.svcs.tcpHostname = store.TCPHostname{Hostname: "x.app.example.com", Slug: "t0a1b2c3d4e", AccessMode: "open"}
	if got := get(); got.ShadowedBy != "custom_domain" || !got.Reachable {
		t.Fatalf("one of two: %+v", got)
	}
}

// With the 443 mux on, a name the relay serves itself (here every slug name,
// the tcp domain being under the dashboard's) is shadowed by "relay". Without
// the mux the dedicated listener serves it and nothing is shadowed.
func TestHostOwner_ShadowedByRelay(t *testing.T) {
	mut := func(mux bool) func(*Deps) {
		return func(d *Deps) {
			d.TCPDomain, d.TLSPassthrough, d.TLSPassthroughMux = "tcp.burrow.example.com", true, mux
			d.AuthDomain = "burrow.example.com"
			d.RelayNames = relaynames.New([]string{"burrow.example.com"})
		}
	}
	a := newTCPAPI(t, mut(true))
	a.svcs.tcpHostname = store.TCPHostname{Slug: "t0a1b2c3d4e", AccessMode: "open"}
	if got := decodeTCPHostname(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname")); got.ShadowedBy != "relay" || got.Reachable {
		t.Fatalf("mux: %+v", got)
	}
	b := newTCPAPI(t, mut(false))
	b.svcs.tcpHostname = store.TCPHostname{Slug: "t0a1b2c3d4e", AccessMode: "open"}
	if got := decodeTCPHostname(t, b.c.get(t, "/api/v1/services/rdp/tcp-hostname")); got.ShadowedBy != "" || !got.Reachable {
		t.Fatalf("no mux: %+v", got)
	}
}
