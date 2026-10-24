package api

// The TLS passthrough host name of a tcp service (tcp complete, E1): read
// with the names it is reached by, changed on a route of its own that asks
// for a recent confirmation, and told to the relay.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

func decodeTCPHostname(t *testing.T, r *http.Response) tcpHostnameResp {
	t.Helper()
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	var s tcpHostnameResp
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTCPHostname_GetAndPut(t *testing.T) {
	a := newTCPAPI(t, func(d *Deps) {
		d.TCPDomain, d.TLSPassthrough = "tcp.example.com", true
		d.AuthDomain, d.ServicesDomain = "burrow.example.com", "apps.example.com"
	})
	a.svcs.tcpHostname = store.TCPHostname{Slug: "t0a1b2c3d4e", AccessMode: "open"}

	got := decodeTCPHostname(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname"))
	if got.TCPHostname != "" || !slices.Equal(got.Hostnames, []string{"t0a1b2c3d4e.tcp.example.com"}) || !got.Passthrough || !got.Reachable {
		t.Fatalf("GET %+v", got)
	}
	got = decodeTCPHostname(t, a.c.put(t, "/api/v1/services/rdp/tcp-hostname", map[string]string{"tcp_hostname": "RDP.Example.com"}))
	if got.TCPHostname != "rdp.example.com" ||
		!slices.Equal(got.Hostnames, []string{"t0a1b2c3d4e.tcp.example.com", "rdp.example.com"}) || !got.Reachable {
		t.Fatalf("PUT %+v", got)
	}
	if a.svcs.lastTCPHostname != "RDP.Example.com" {
		t.Fatalf("store got %q", a.svcs.lastTCPHostname)
	}
	if refreshed, _ := a.relay.snapshot(); !slices.Equal(refreshed, []string{"rdp"}) {
		t.Fatalf("relay told %v", refreshed)
	}
	// A private service has its names and is not reachable by them.
	a.svcs.tcpHostname.AccessMode = "private"
	if got := decodeTCPHostname(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname")); got.Reachable || len(got.Hostnames) != 2 {
		t.Fatalf("private: %+v", got)
	}
	a.svcs.tcpHostname.AccessMode = "ip"
	got = decodeTCPHostname(t, a.c.put(t, "/api/v1/services/rdp/tcp-hostname", map[string]string{"tcp_hostname": ""}))
	if got.TCPHostname != "" || len(got.Hostnames) != 1 || !got.Reachable {
		t.Fatalf("cleared: %+v", got)
	}

	for _, c := range []struct {
		name string
		body string
		err  error
		want int
		text string
	}{
		{"no field", `{}`, nil, http.StatusBadRequest, "tcp_hostname or hostname_only is required"},
		{"a field it does not know", `{"tcp_hostname": "a.example.com", "x": 1}`, nil, http.StatusBadRequest, "unknown"},
		{"not json", `nope`, nil, http.StatusBadRequest, ""},
		{"no host name", `{"tcp_hostname": "not a host"}`, store.ErrInvalidTCPHostname, http.StatusBadRequest, "host name"},
		{"under the tcp domain", `{"tcp_hostname": "x.tcp.example.com"}`, nil, http.StatusBadRequest, "tcp.example.com"},
		{"the tcp domain", `{"tcp_hostname": "TCP.example.com"}`, nil, http.StatusBadRequest, "tcp.example.com"},
		{"the dashboard", `{"tcp_hostname": "burrow.example.com"}`, nil, http.StatusBadRequest, "burrow.example.com"},
		{"under the dashboard", `{"tcp_hostname": "abc.burrow.example.com"}`, nil, http.StatusBadRequest, "burrow.example.com"},
		{"the services host", `{"tcp_hostname": "apps.example.com"}`, nil, http.StatusBadRequest, "apps.example.com"},
		{"taken", `{"tcp_hostname": "db.example.com"}`, store.ErrTCPHostnameTaken, http.StatusConflict, "another service"},
		{"under the services host", `{"tcp_hostname": "x.apps.example.com"}`, nil, http.StatusBadRequest, "apps.example.com"},
		{"a custom domain", `{"tcp_hostname": "app.example.com"}`, store.ErrTCPHostnameReserved, http.StatusConflict, "custom domain"},
		{"an owner with :own", `{"tcp_hostname": "a.example.com"}`, store.ErrTCPHostnameNeedsAny, http.StatusForbidden, "services:configure:any"},
		{"http service", `{"tcp_hostname": "a.example.com"}`, store.ErrServiceNotTCP, http.StatusConflict, "tcp services only"},
		{"another user's", `{"tcp_hostname": "a.example.com"}`, store.ErrForbidden, http.StatusForbidden, ""},
		{"missing", `{"tcp_hostname": "a.example.com"}`, db.ErrNotFound, http.StatusNotFound, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a.svcs.tcpHostnameErr = c.err
			defer func() { a.svcs.tcpHostnameErr = nil }()
			r := a.c.putRaw(t, "/api/v1/services/rdp/tcp-hostname", c.body)
			body := readBody(t, r)
			if r.StatusCode != c.want || !strings.Contains(body, c.text) {
				t.Fatalf("status %d %s, want %d with %q", r.StatusCode, body, c.want, c.text)
			}
		})
	}
	if refreshed, _ := a.relay.snapshot(); len(refreshed) != 2 {
		t.Fatalf("a refused write told the relay: %v", refreshed)
	}
	a.svcs.tcpHostnameErr = store.ErrServiceNotTCP
	if r := a.c.get(t, "/api/v1/services/web/tcp-hostname"); r.StatusCode != http.StatusConflict {
		t.Fatalf("GET http: %d", r.StatusCode)
	}
}

// Without the passthrough listener a name reaches nothing, and without a
// tcp domain the slug has no name.
func TestTCPHostname_WithoutPassthrough(t *testing.T) {
	a := newTCPAPI(t, nil)
	a.svcs.tcpHostname = store.TCPHostname{Hostname: "rdp.example.com", Slug: "t0a1b2c3d4e", AccessMode: "open"}
	got := decodeTCPHostname(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname"))
	if got.Passthrough || got.Reachable || !slices.Equal(got.Hostnames, []string{"rdp.example.com"}) {
		t.Fatalf("GET %+v", got)
	}
	if body := readBody(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname")); strings.Contains(body, "null") {
		t.Fatalf("a null in %s", body)
	}
}

// Host name only (E1: by port, by host name, or both): the public port may
// be turned off only while the relay has the passthrough listener and the
// service a name there; otherwise 400 with the reason. A name that is the
// only one is not cleared while the port is off. Each write tells the relay.
func TestTCPHostname_HostnameOnly(t *testing.T) {
	a := newTCPAPI(t, func(d *Deps) { d.TCPDomain, d.TLSPassthrough = "tcp.example.com", true })
	a.svcs.tcpHostname = store.TCPHostname{Slug: "t0a1b2c3d4e", AccessMode: "open"}
	if got := decodeTCPHostname(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname")); got.HostnameOnly {
		t.Fatalf("GET %+v", got)
	}
	got := decodeTCPHostname(t, a.c.put(t, "/api/v1/services/rdp/tcp-hostname", map[string]any{"hostname_only": true}))
	if !got.HostnameOnly || !a.svcs.tcpHostname.HostnameOnly || a.svcs.lastTCPHostname != "" {
		t.Fatalf("PUT on: %+v (store %+v, name written %q)", got, a.svcs.tcpHostname, a.svcs.lastTCPHostname)
	}
	if refreshed, _ := a.relay.snapshot(); len(refreshed) != 1 {
		t.Fatalf("relay told %v", refreshed)
	}
	got = decodeTCPHostname(t, a.c.put(t, "/api/v1/services/rdp/tcp-hostname", map[string]any{"hostname_only": false}))
	if got.HostnameOnly {
		t.Fatalf("PUT off: %+v", got)
	}

	// Without a tcp domain the slug has no name: a name of its own is
	// needed, and it may come in the same request.
	b := newTCPAPI(t, func(d *Deps) { d.TLSPassthrough = true })
	b.svcs.tcpHostname = store.TCPHostname{Slug: "t0a1b2c3d4e", AccessMode: "ip"}
	r := b.c.putRaw(t, "/api/v1/services/rdp/tcp-hostname", `{"hostname_only": true}`)
	if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "host name") {
		t.Fatalf("no name: %d %s", r.StatusCode, body)
	}
	if b.svcs.tcpHostname.HostnameOnly {
		t.Fatal("a refused write was stored")
	}
	got = decodeTCPHostname(t, b.c.put(t, "/api/v1/services/rdp/tcp-hostname", map[string]any{"tcp_hostname": "db.example.com", "hostname_only": true}))
	if !got.HostnameOnly || got.TCPHostname != "db.example.com" {
		t.Fatalf("name and only: %+v", got)
	}
	// The only name is not cleared while the port is off.
	r = b.c.putRaw(t, "/api/v1/services/rdp/tcp-hostname", `{"tcp_hostname": ""}`)
	if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "hostname_only") {
		t.Fatalf("clear the only name: %d %s", r.StatusCode, body)
	}
	if b.svcs.tcpHostname.Hostname != "db.example.com" {
		t.Fatal("the only name was cleared")
	}
	// Both at once: the port back on, the name cleared.
	got = decodeTCPHostname(t, b.c.put(t, "/api/v1/services/rdp/tcp-hostname", map[string]any{"tcp_hostname": "", "hostname_only": false}))
	if got.HostnameOnly || got.TCPHostname != "" {
		t.Fatalf("both off: %+v", got)
	}

	// Without the passthrough listener no name reaches it.
	c := newTCPAPI(t, func(d *Deps) { d.TCPDomain = "tcp.example.com" })
	c.svcs.tcpHostname = store.TCPHostname{Hostname: "db.example.com", Slug: "t0a1b2c3d4e", AccessMode: "open"}
	r = c.c.putRaw(t, "/api/v1/services/rdp/tcp-hostname", `{"hostname_only": true}`)
	if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "BURROW_TLS_PASSTHROUGH_LISTEN") {
		t.Fatalf("no listener: %d %s", r.StatusCode, body)
	}
	// Turning the port back on needs nothing.
	c.svcs.tcpHostname.HostnameOnly = true
	if got := decodeTCPHostname(t, c.c.put(t, "/api/v1/services/rdp/tcp-hostname", map[string]any{"hostname_only": false})); got.HostnameOnly {
		t.Fatalf("back on: %+v", got)
	}
	if refreshed, _ := c.relay.snapshot(); len(refreshed) != 1 {
		t.Fatalf("relay told %v", refreshed)
	}
}

// A custom name stored under BURROW_TCP_DOMAIN before the domain was set
// never routes (those names are the slugs'): it is no name of the service,
// as the relay sees it, so it neither makes it reachable nor allows host
// name only.
func TestTCPHostname_ALegacyNameUnderTheTCPDomainDoesNotCount(t *testing.T) {
	a := newTCPAPI(t, func(d *Deps) { d.TCPDomain, d.TLSPassthrough = "tcp.example.com", true })
	a.svcs.tcpHostname = store.TCPHostname{Hostname: "db.tcp.example.com", AccessMode: "open"}
	got := decodeTCPHostname(t, a.c.get(t, "/api/v1/services/rdp/tcp-hostname"))
	if len(got.Hostnames) != 0 || got.Reachable || got.TCPHostname != "db.tcp.example.com" {
		t.Fatalf("GET %+v", got)
	}
	r := a.c.putRaw(t, "/api/v1/services/rdp/tcp-hostname", `{"hostname_only": true}`)
	if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "host name") {
		t.Fatalf("hostname_only: %d %s", r.StatusCode, body)
	}
}
