package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/acme"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/relaynames"
)

type listDomains struct {
	rows []db.ServiceCustomDomain
	err  error
}

func (l listDomains) ListAllCustomDomains(context.Context) ([]db.ServiceCustomDomain, error) {
	return l.rows, l.err
}

// hangingIssuer is an ACME that never answers.
type hangingIssuer struct {
	mu      sync.Mutex
	managed []string
	hook    func(event, name string, err error)
	release chan struct{}
}

func (h *hangingIssuer) ManageAsync(_ context.Context, names []string) error {
	<-h.release
	h.mu.Lock()
	defer h.mu.Unlock()
	h.managed = append(h.managed, names...)
	return errors.New("acme: directory unreachable")
}
func (h *hangingIssuer) Unmanage([]string)                                  {}
func (h *hangingIssuer) HasCertificate(string) bool                         { return false }
func (h *hangingIssuer) OnCertEvent(fn func(event, name string, err error)) { h.hook = fn }

type certAuditRows struct {
	mu   sync.Mutex
	rows []audit.Event
}

func (a *certAuditRows) Append(_ context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, e)
	return nil
}

func (a *certAuditRows) list() []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rows)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Startup hands the existing auto names to certmagic without waiting on
// it, skips a name the relay does not own (never ordered, audited), and
// audits the failures of the CA; uploaded rows are not certmagic's.
func TestStartCustomDomainCerts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	iss := &hangingIssuer{release: make(chan struct{})}
	aud := &certAuditRows{}
	rows := listDomains{rows: []db.ServiceCustomDomain{
		{ServiceID: "s1", Hostname: "a.example.com", CertSource: db.CertSourceAuto},
		{ServiceID: "s2", Hostname: "up.example.com", CertSource: db.CertSourceUploaded},
		{ServiceID: "s3", Hostname: "x.burrow.example.com", CertSource: db.CertSourceAuto},
	}}
	allow := func(h string) string {
		if relaynames.New([]string{"burrow.example.com"}).Owns(h) {
			return "a name the relay serves itself"
		}
		return ""
	}

	start := time.Now()
	cd := startCustomDomainCerts(ctx, iss, rows, allow, aud, quietLog())
	if d := time.Since(start); d > time.Second {
		t.Fatalf("startup waited %v on ACME", d)
	}
	if iss.hook == nil {
		t.Fatal("certmagic's events do not reach the custom domains")
	}
	close(iss.release)
	cd.Wait()

	iss.mu.Lock()
	managed := slices.Clone(iss.managed)
	iss.mu.Unlock()
	if !slices.Equal(managed, []string{"a.example.com"}) {
		t.Fatalf("managed %v", managed)
	}
	iss.hook("cert_failed", "a.example.com", errors.New("429 too many orders"))

	var got []string
	for _, e := range aud.list() {
		if e.Action != audit.ActionServiceCustomDomainCertFailed || e.Result != "error" || e.AggregationKey != e.SubjectLabel {
			t.Fatalf("audit %+v", e)
		}
		var p map[string]string
		_ = json.Unmarshal(e.Payload, &p)
		got = append(got, e.SubjectID+"|"+p["hostname"]+"|"+p["error"])
	}
	want := []string{
		"s1|a.example.com|acme: directory unreachable",
		"s3|x.burrow.example.com|a name the relay serves itself",
		"s1|a.example.com|429 too many orders",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("audit rows\n got %v\nwant %v", got, want)
	}
}

// A failed listing does not stop the relay.
func TestStartCustomDomainCerts_ListingFails(t *testing.T) {
	iss := &hangingIssuer{release: make(chan struct{})}
	cd := startCustomDomainCerts(context.Background(), iss, listDomains{err: errors.New("db down")}, func(string) string { return "" }, nil, quietLog())
	if cd == nil {
		t.Fatal("nil")
	}
	cd.Wait()
}

func TestCustomDomainGetCertificate(t *testing.T) {
	uploaded := &tls.Certificate{}
	fromNext := &tls.Certificate{}
	var lookups []string
	custom := func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		lookups = append(lookups, h.ServerName)
		if h.ServerName == "up.example.com" {
			return uploaded, nil
		}
		return nil, nil // a miss, or an auto row (no certificate)
	}
	next := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return fromNext, nil }
	owns := relaynames.New([]string{"burrow.example.com"}).Owns
	get := customDomainGetCertificate(owns, custom, next)

	for _, c := range []struct {
		hello *tls.ClientHelloInfo
		want  *tls.Certificate
	}{
		{&tls.ClientHelloInfo{ServerName: "up.example.com"}, uploaded},
		{&tls.ClientHelloInfo{ServerName: "auto.example.com"}, fromNext},
		{&tls.ClientHelloInfo{ServerName: "burrow.example.com"}, fromNext},
		{&tls.ClientHelloInfo{ServerName: "up.example.com", SupportedProtos: []string{acme.ACMETLSProto}}, fromNext},
		{&tls.ClientHelloInfo{}, fromNext},
	} {
		if got, err := get(c.hello); err != nil || got != c.want {
			t.Errorf("%q %v: got %p, want %p", c.hello.ServerName, c.hello.SupportedProtos, got, c.want)
		}
	}
	if !slices.Equal(lookups, []string{"up.example.com", "auto.example.com"}) {
		t.Fatalf("looked up %v", lookups)
	}
	// Without a source of its own the listener's file certificate answers.
	if got, err := customDomainGetCertificate(owns, custom, nil)(&tls.ClientHelloInfo{ServerName: "burrow.example.com"}); got != nil || err != nil {
		t.Fatalf("%v %v", got, err)
	}
}

func TestDispatchCustomDomains(t *testing.T) {
	for _, c := range []struct {
		auth          string
		fileTLS, acme bool
		want          bool
	}{
		{"burrow.example.com", false, false, true},
		{"", true, false, true},
		{"", false, true, true},
		{"", false, false, false},
	} {
		if got := dispatchCustomDomains(c.auth, c.fileTLS, c.acme); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}
