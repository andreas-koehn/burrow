package api

// H12: one owner per host name across http and tcp, seen from the API.

import (
	"context"
	"errors"
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
	err    error
}

func (c coveringStore) CustomDomainCovers(_ context.Context, name string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
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

// Rows from before the checks: http wins, but only where the listener
// consults the custom domains: the 443 mux. The dedicated listener
// (BURROW_TLS_PASSTHROUGH_LISTEN) does not, so a name is reachable on it
// whatever a custom domain says, and shadowed_by stays absent while the mux
// is off.
func TestHostOwner_ShadowedBy(t *testing.T) {
	covers := func(n string) bool { return n == "rdp.example.com" || strings.HasSuffix(n, ".app.example.com") }
	setup := func(mux bool, listen string, lookupErr error) *tcpAPI {
		return newTCPAPI(t, func(d *Deps) {
			d.TCPDomain, d.TLSPassthrough, d.TLSPassthroughMux, d.TLSPassthroughListen = "tcp.example.com", mux || listen != "", mux, listen
			d.CustomDomains = coveringStore{newStubDomainStore(), covers, lookupErr}
		})
	}
	get := func(a *tcpAPI, h store.TCPHostname) tcpHostnameResp {
		a.svcs.tcpHostname = h
		return decodeTCPHostname(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname"))
	}
	only := store.TCPHostname{Hostname: "rdp.example.com", Slug: "has.dot", AccessMode: "open"} // no slug name
	two := store.TCPHostname{Hostname: "x.app.example.com", Slug: "t0a1b2c3d4e", AccessMode: "open"}
	free := store.TCPHostname{Hostname: "ok.example.com", Slug: "t0a1b2c3d4e", AccessMode: "open"}

	// Mux only: the custom domain wins its name.
	a := setup(true, "", nil)
	if got := get(a, only); got.ShadowedBy != "custom_domain" || got.Reachable || len(got.ReachableOn) != 0 {
		t.Fatalf("mux, shadowed: %+v", got)
	}
	if got := get(a, two); got.ShadowedBy != "custom_domain" || !got.Reachable || !slices.Equal(got.ReachableOn, []string{"https"}) {
		t.Fatalf("mux, one of two: %+v", got)
	}
	if got := get(a, free); got.ShadowedBy != "" || !got.Reachable {
		t.Fatalf("mux, free: %+v", got)
	}
	if body := readBody(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname")); strings.Contains(body, "shadowed_by") {
		t.Fatalf("shadowed_by on a free name: %s", body)
	}
	// A private service is reached by no listener.
	priv := two
	priv.AccessMode = "private"
	if got := get(a, priv); got.Reachable || len(got.ReachableOn) != 0 {
		t.Fatalf("private: %+v", got)
	}

	// Dedicated listener only: nothing is shadowed, every name is reachable.
	b := setup(false, ":8444", nil)
	if got := get(b, only); got.ShadowedBy != "" || !got.Reachable || !slices.Equal(got.ReachableOn, []string{":8444"}) {
		t.Fatalf("listener only: %+v", got)
	}
	// Both: the shadowed name is reachable on the listener, and told.
	c := setup(true, ":8444", nil)
	if got := get(c, only); got.ShadowedBy != "custom_domain" || !got.Reachable || !slices.Equal(got.ReachableOn, []string{":8444"}) {
		t.Fatalf("both, shadowed: %+v", got)
	}
	if got := get(c, free); !slices.Equal(got.ReachableOn, []string{"https", ":8444"}) {
		t.Fatalf("both, free: %+v", got)
	}

	// A failed lookup: the mux terminates, so the mux does not count; the
	// listener still does, and nothing is claimed shadowed.
	d := setup(true, "", errors.New("db down"))
	if got := get(d, free); got.Reachable || got.ShadowedBy != "" || len(got.ReachableOn) != 0 {
		t.Fatalf("lookup error, mux: %+v", got)
	}
	e := setup(true, ":8444", errors.New("db down"))
	if got := get(e, free); !got.Reachable || !slices.Equal(got.ReachableOn, []string{":8444"}) {
		t.Fatalf("lookup error, both: %+v", got)
	}
	// The legacy setup (TLSPassthrough alone) is the dedicated listener.
	f := newTCPAPI(t, func(d *Deps) { d.TCPDomain, d.TLSPassthrough = "tcp.example.com", true })
	if got := get(f, free); !got.Reachable || len(got.ReachableOn) != 1 {
		t.Fatalf("legacy: %+v", got)
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

// Custom domain names are normalised and validated before any check: a
// trailing dot, upper case or a wildcard cannot slip past the refusals, and
// a name that is no DNS name is 400.
func TestHostOwner_CustomDomainNamesAreNormalised(t *testing.T) {
	ds := newStubDomainStore()
	d := newDomainDeps(ds)
	d.TCPDomain = "tcp.example.com"
	d.RelayNames = relaynames.New([]string{"burrow.example.com"})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)
	for _, h := range []string{"a.tcp.example.com.", "A.TCP.example.com.", " tcp.example.com. ", "*.tcp.example.com.", "burrow.example.com.", "x.burrow.example.com."} {
		r := ac.post(t, "/api/v1/services/svc1/domains", domainBody(h))
		if body := readBody(t, r); r.StatusCode != http.StatusConflict {
			t.Errorf("POST %q: %d %s, want 409", h, r.StatusCode, body)
		}
		r = ac.put(t, "/api/v1/services/svc1/domains/did-1", domainBody(h))
		if body := readBody(t, r); r.StatusCode != http.StatusConflict {
			t.Errorf("PUT %q: %d %s, want 409", h, r.StatusCode, body)
		}
	}
	for _, h := range []string{"a..example.com", "-a.example.com", "a-.example.com", "a_b.example.com", "*.*.example.com", "a.*.example.com",
		"*example.com", "*.", "*", "example", "a.example.com..", strings.Repeat("a", 64) + ".example.com",
		"b\u00fccher.example.com", "xn--.example.com", "a b.example.com", "1.2.3.4"} {
		r := ac.post(t, "/api/v1/services/svc1/domains", domainBody(h))
		if body := readBody(t, r); r.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %q: %d %s, want 400", h, r.StatusCode, body)
		}
		r = ac.put(t, "/api/v1/services/svc1/domains/did-1", domainBody(h))
		if body := readBody(t, r); r.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %q: %d %s, want 400", h, r.StatusCode, body)
		}
	}
	if ds.lastInsert.Hostname != "" || ds.lastUpdate.Hostname != "" {
		t.Fatalf("stored %q %q", ds.lastInsert.Hostname, ds.lastUpdate.Hostname)
	}
	// Punycode is an ordinary name, and a good one is stored normalised.
	ca, caKey, pool, _ := genCA(t)
	certPEM, keyPEM := genCert(t, ca, caKey, []string{"app.example.com"})
	d2 := newDomainDepsWithRoots(ds, pool)
	srv2 := httptest.NewServer(NewRouter(d2))
	defer srv2.Close()
	ac2 := authedClient(t, srv2)
	r := ac2.post(t, "/api/v1/services/svc1/domains", map[string]any{"hostname": " App.Example.com. ", "cert_pem": certPEM, "key_pem": keyPEM})
	if body := readBody(t, r); r.StatusCode != http.StatusCreated || ds.lastInsert.Hostname != "app.example.com" {
		t.Fatalf("good name: %d %s stored %q", r.StatusCode, body, ds.lastInsert.Hostname)
	}
}
