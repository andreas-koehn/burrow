package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// A tcp service's TLS passthrough host name: validated and stored lower
// case, unique across services, set by a holder of services:configure:any
// only (an owner may clear it), never a name of a custom domain or under
// one, and every write is audited with the name before and after.
func TestServiceTCPHostname(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	other := mustCreateUser(t, st, "other@x", "user")
	admin := mustCreateUser(t, st, "admin@x", "admin")
	sink := &auditSink{}
	st.SetAuditLogger(sink)
	web := mustGetOrCreateService(t, st, owner.ID, "web", "http")
	rdp := mustGetOrCreateService(t, st, owner.ID, "rdp", "tcp")
	dbs := mustGetOrCreateService(t, st, other.ID, "db", "tcp")

	got, err := st.ServiceTCPHostname(ctx, owner.ID, "user", rdp.ID)
	if err != nil || got.Hostname != "" || got.AccessMode != "open" {
		t.Fatalf("new service: %+v %v", got, err)
	}
	// The owner with :own alone may not give its service a name of its own.
	if _, _, err := st.SetServiceTCPHostname(ctx, owner.ID, "user", rdp.ID, "rdp.example.com"); !errors.Is(err, ErrTCPHostnameNeedsAny) {
		t.Fatalf("owner sets: %v, want ErrTCPHostnameNeedsAny", err)
	}
	before, after, err := st.SetServiceTCPHostname(ctx, admin.ID, "admin", rdp.ID, "RDP.Example.COM.")
	if err != nil || before.Hostname != "" || after.Hostname != "rdp.example.com" {
		t.Fatalf("set: %+v %+v %v", before, after, err)
	}
	if got, _ := st.ServiceTCPHostname(ctx, owner.ID, "user", rdp.ID); got.Hostname != "rdp.example.com" {
		t.Fatalf("read back: %+v", got)
	}
	// A collision: another service, another user, the same name in any case.
	if _, _, err := st.SetServiceTCPHostname(ctx, admin.ID, "admin", dbs.ID, "rdp.EXAMPLE.com"); !errors.Is(err, ErrTCPHostnameTaken) {
		t.Fatalf("collision: %v", err)
	}
	// A custom domain's name, a name under it, and one a wildcard covers.
	for _, h := range []string{"app.example.com", "*.wild.example.com"} {
		if _, err := st.q.InsertCustomDomain(ctx, db.ServiceCustomDomain{ServiceID: web.ID, Hostname: h, CertPEM: "c", KeyPEM: "k",
			CertSHA256: h, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"app.example.com", "x.app.example.com", "a.wild.example.com", "WILD.example.com"} {
		_, _, err := st.SetServiceTCPHostname(ctx, admin.ID, "admin", dbs.ID, h)
		if h == "WILD.example.com" {
			if err != nil { // the wildcard covers names under it, not itself
				t.Fatalf("%s: %v", h, err)
			}
			continue
		}
		if !errors.Is(err, ErrTCPHostnameReserved) {
			t.Errorf("%s: %v, want ErrTCPHostnameReserved", h, err)
		}
	}
	for _, bad := range []string{
		" ", "localhost", "*.example.com", "rdp_x.example.com", "203.0.113.7", "[::1]", "rdp.example.com:443",
		"https://rdp.example.com", "-a.example.com", "a..example.com", strings.Repeat("a", 64) + ".example.com",
		"rdp.example.123",
	} {
		if _, _, err := st.SetServiceTCPHostname(ctx, admin.ID, "admin", rdp.ID, bad); !errors.Is(err, ErrInvalidTCPHostname) {
			t.Errorf("%q: %v, want ErrInvalidTCPHostname", bad, err)
		}
	}
	if _, _, err := st.SetServiceTCPHostname(ctx, admin.ID, "admin", web.ID, "web.example.com"); !errors.Is(err, ErrServiceNotTCP) {
		t.Fatalf("http: %v", err)
	}
	if _, err := st.ServiceTCPHostname(ctx, owner.ID, "user", web.ID); !errors.Is(err, ErrServiceNotTCP) {
		t.Fatalf("http read: %v", err)
	}
	if _, _, err := st.SetServiceTCPHostname(ctx, other.ID, "user", rdp.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user: %v", err)
	}
	if _, err := st.ServiceTCPHostname(ctx, other.ID, "user", rdp.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user reads: %v", err)
	}
	// Clearing narrows access: the owner may.
	if _, after, err := st.SetServiceTCPHostname(ctx, owner.ID, "user", rdp.ID, ""); err != nil || after.Hostname != "" {
		t.Fatalf("clear: %+v %v", after, err)
	}
	// Free again: the other service takes it.
	if _, after, err := st.SetServiceTCPHostname(ctx, admin.ID, "admin", dbs.ID, "rdp.example.com"); err != nil || after.Hostname != "rdp.example.com" {
		t.Fatalf("free again: %+v %v", after, err)
	}
	var writes []string
	for _, e := range sink.ev {
		if e.Action == audit.ActionServiceTCPHostnameUpdate {
			writes = append(writes, e.SubjectID+" "+string(e.Payload))
		}
	}
	want := []string{
		rdp.ID + ` {"from":"","to":"rdp.example.com"}`,
		dbs.ID + ` {"from":"","to":"wild.example.com"}`,
		rdp.ID + ` {"from":"rdp.example.com","to":""}`,
		dbs.ID + ` {"from":"wild.example.com","to":"rdp.example.com"}`,
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit:\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Contains(audit.AllActions, audit.ActionServiceTCPHostnameUpdate) || audit.ActionServiceTCPHostnameUpdate != "service.tcp_hostname.update" {
		t.Fatal("service.tcp_hostname.update is not an audit action")
	}
}

// Host name only: the owner may turn the public port of a tcp service off
// and on again; the read says so, and every change is audited with the
// setting before and after. Not for http, not for another user's service.
func TestServiceTCPHostnameOnly(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	other := mustCreateUser(t, st, "other@x", "user")
	sink := &auditSink{}
	st.SetAuditLogger(sink)
	web := mustGetOrCreateService(t, st, owner.ID, "web", "http")
	rdp := mustGetOrCreateService(t, st, owner.ID, "rdp", "tcp")

	if got, err := st.ServiceTCPHostname(ctx, owner.ID, "user", rdp.ID); err != nil || got.HostnameOnly {
		t.Fatalf("new service: %+v %v", got, err)
	}
	before, after, err := st.SetServiceTCPHostnameOnly(ctx, owner.ID, "user", rdp.ID, true)
	if err != nil || before.HostnameOnly || !after.HostnameOnly {
		t.Fatalf("set: %+v %+v %v", before, after, err)
	}
	if got, _ := st.ServiceTCPHostname(ctx, owner.ID, "user", rdp.ID); !got.HostnameOnly {
		t.Fatalf("read back: %+v", got)
	}
	if _, after, err := st.SetServiceTCPHostnameOnly(ctx, owner.ID, "user", rdp.ID, false); err != nil || after.HostnameOnly {
		t.Fatalf("back: %+v %v", after, err)
	}
	if _, _, err := st.SetServiceTCPHostnameOnly(ctx, owner.ID, "user", web.ID, true); !errors.Is(err, ErrServiceNotTCP) {
		t.Fatalf("http: %v", err)
	}
	if _, _, err := st.SetServiceTCPHostnameOnly(ctx, other.ID, "user", rdp.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user: %v", err)
	}
	var writes []string
	for _, e := range sink.ev {
		if e.Action == audit.ActionServiceTCPHostnameOnlyUpdate {
			writes = append(writes, e.SubjectID+" "+string(e.Payload))
		}
	}
	want := []string{rdp.ID + ` {"from":false,"to":true}`, rdp.ID + ` {"from":true,"to":false}`}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit:\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Contains(audit.AllActions, audit.ActionServiceTCPHostnameOnlyUpdate) || audit.ActionServiceTCPHostnameOnlyUpdate != "service.tcp_hostname_only.update" {
		t.Fatal("service.tcp_hostname_only.update is not an audit action")
	}
}
