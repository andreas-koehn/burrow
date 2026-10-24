package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
)

// A tcp service's TLS passthrough host name: validated and stored lower
// case, unique across services, owner or :any only, and every write is
// audited with the name before and after.
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
	db := mustGetOrCreateService(t, st, other.ID, "db", "tcp")

	got, err := st.ServiceTCPHostname(ctx, owner.ID, "user", rdp.ID)
	if err != nil || got.Hostname != "" || got.AccessMode != "open" {
		t.Fatalf("new service: %+v %v", got, err)
	}
	before, after, err := st.SetServiceTCPHostname(ctx, owner.ID, "user", rdp.ID, "RDP.Example.COM.")
	if err != nil || before.Hostname != "" || after.Hostname != "rdp.example.com" {
		t.Fatalf("set: %+v %+v %v", before, after, err)
	}
	if got, _ := st.ServiceTCPHostname(ctx, owner.ID, "user", rdp.ID); got.Hostname != "rdp.example.com" {
		t.Fatalf("read back: %+v", got)
	}
	// A collision: another service, another user, the same name in any case.
	if _, _, err := st.SetServiceTCPHostname(ctx, other.ID, "user", db.ID, "rdp.EXAMPLE.com"); !errors.Is(err, ErrTCPHostnameTaken) {
		t.Fatalf("collision: %v", err)
	}
	for _, bad := range []string{
		"", " ", "localhost", "*.example.com", "rdp_x.example.com", "203.0.113.7", "[::1]", "rdp.example.com:443",
		"https://rdp.example.com", "-a.example.com", "a..example.com", strings.Repeat("a", 64) + ".example.com",
		"rdp.example.123",
	} {
		if bad == "" {
			continue // "" clears, below
		}
		if _, _, err := st.SetServiceTCPHostname(ctx, owner.ID, "user", rdp.ID, bad); !errors.Is(err, ErrInvalidTCPHostname) {
			t.Errorf("%q: %v, want ErrInvalidTCPHostname", bad, err)
		}
	}
	if _, _, err := st.SetServiceTCPHostname(ctx, owner.ID, "user", web.ID, "web.example.com"); !errors.Is(err, ErrServiceNotTCP) {
		t.Fatalf("http: %v", err)
	}
	if _, err := st.ServiceTCPHostname(ctx, owner.ID, "user", web.ID); !errors.Is(err, ErrServiceNotTCP) {
		t.Fatalf("http read: %v", err)
	}
	if _, _, err := st.SetServiceTCPHostname(ctx, other.ID, "user", rdp.ID, "mine.example.com"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user: %v", err)
	}
	if _, err := st.ServiceTCPHostname(ctx, other.ID, "user", rdp.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user reads: %v", err)
	}
	if _, after, err := st.SetServiceTCPHostname(ctx, admin.ID, "admin", rdp.ID, ""); err != nil || after.Hostname != "" {
		t.Fatalf("clear: %+v %v", after, err)
	}
	// Free again: the other service takes it.
	if _, after, err := st.SetServiceTCPHostname(ctx, other.ID, "user", db.ID, "rdp.example.com"); err != nil || after.Hostname != "rdp.example.com" {
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
		rdp.ID + ` {"from":"rdp.example.com","to":""}`,
		db.ID + ` {"from":"","to":"rdp.example.com"}`,
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit:\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Contains(audit.AllActions, audit.ActionServiceTCPHostnameUpdate) || audit.ActionServiceTCPHostnameUpdate != "service.tcp_hostname.update" {
		t.Fatal("service.tcp_hostname.update is not an audit action")
	}
}
