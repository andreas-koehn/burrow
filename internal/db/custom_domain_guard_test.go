package db

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// R09 review C1(b): a custom domain may not lie under another service's
// wildcard custom domain, nor a wildcard cover another service's custom
// domain, whatever its cert_source; the service's own names may overlap.
func TestCustomDomainWildcardOverlap(t *testing.T) {
	checkCustomDomainWildcardOverlap(t, testDB(t), "u-wildover")
}

func checkCustomDomainWildcardOverlap(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	a, b := "wa"+userID, "wb"+userID
	base := userID + ".example"
	for _, id := range []string{a, b} {
		if err := x.CreateService(ctx, Service{ID: id, UserID: userID, Name: id, Type: "http", AccessMode: "open"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.InsertCustomDomain(ctx, hostOwnDomain(a, "*.wild."+base)); err != nil {
		t.Fatal(err)
	}
	if _, err := x.InsertCustomDomain(ctx, hostOwnDomain(a, "plain."+base)); err != nil {
		t.Fatal(err)
	}
	auto := func(svc, h string) ServiceCustomDomain {
		return ServiceCustomDomain{ServiceID: svc, Hostname: h, CertSource: CertSourceAuto}
	}
	// Under another service's wildcard: refused, uploaded or auto, at any depth.
	for _, d := range []ServiceCustomDomain{
		hostOwnDomain(b, "x.wild."+base),
		auto(b, "y.wild."+base),
		auto(b, "z.q.wild."+base),
		hostOwnDomain(b, "*.q.wild."+base),
		// A wildcard over another service's name, or over its wildcard.
		hostOwnDomain(b, "*."+base),
	} {
		if _, err := x.InsertCustomDomain(ctx, d); !errors.Is(err, ErrHostOverlapsWildcard) {
			t.Errorf("insert %s for b: %v, want ErrHostOverlapsWildcard", d.Hostname, err)
		}
	}
	// The wildcard's own service, and names beside it, are free.
	for _, d := range []ServiceCustomDomain{
		hostOwnDomain(a, "x.wild."+base),
		hostOwnDomain(b, "wild."+base), // the parent itself: *. does not cover it
		hostOwnDomain(b, "other."+base),
	} {
		if _, err := x.InsertCustomDomain(ctx, d); err != nil {
			t.Errorf("insert %s for %s: %v", d.Hostname, d.ServiceID, err)
		}
	}
	// An update into the overlap is refused and leaves the row as it was.
	row, err := x.LookupCustomDomainByHostname(ctx, "other."+base)
	if err != nil {
		t.Fatal(err)
	}
	row.Hostname = "w.wild." + base
	if err := x.UpdateCustomDomain(ctx, row); !errors.Is(err, ErrHostOverlapsWildcard) {
		t.Errorf("update: %v", err)
	}
	// A wildcard row updated to itself is no overlap with itself.
	own, err := x.LookupCustomDomainByHostname(ctx, "*.wild."+base)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.UpdateCustomDomain(ctx, own); err != nil {
		t.Errorf("self update: %v", err)
	}
}

// R09 review I2: the auto rows are capped, in all and per user, in the
// write's transaction; an uploaded row is not counted, and an update of an
// auto row does not count itself.
func TestCustomDomainAutoCaps(t *testing.T) { checkCustomDomainAutoCaps(t, testDB(t), "u-autocap") }

func countAuto(t *testing.T, x *DB) int {
	t.Helper()
	var n int
	if err := x.sqlDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM service_custom_domains WHERE cert_source = 'auto'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func checkCustomDomainAutoCaps(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	u1, u2 := userID+"-1", userID+"-2"
	freshServiceUser(t, x, u1)
	freshServiceUser(t, x, u2)
	s1, s2 := "c1"+userID, "c2"+userID
	if err := x.CreateService(ctx, Service{ID: s1, UserID: u1, Name: s1, Type: "http", AccessMode: "open"}); err != nil {
		t.Fatal(err)
	}
	if err := x.CreateService(ctx, Service{ID: s2, UserID: u2, Name: s2, Type: "http", AccessMode: "open"}); err != nil {
		t.Fatal(err)
	}
	auto := func(svc, h string) ServiceCustomDomain {
		return ServiceCustomDomain{ServiceID: svc, Hostname: h + "." + userID + ".example", CertSource: CertSourceAuto}
	}
	caps := AutoDomainCaps{Max: countAuto(t, x) + 3, PerUser: 2}
	for _, h := range []string{"a", "b"} {
		if _, err := x.InsertCustomDomainCapped(ctx, auto(s1, h), caps); err != nil {
			t.Fatalf("insert %s: %v", h, err)
		}
	}
	if _, err := x.InsertCustomDomainCapped(ctx, auto(s1, "c"), caps); !errors.Is(err, ErrAutoDomainUserCap) {
		t.Fatalf("third of u1: %v, want ErrAutoDomainUserCap", err)
	}
	// Uploaded rows are not counted.
	if _, err := x.InsertCustomDomainCapped(ctx, hostOwnDomain(s1, "up."+userID+".example"), caps); err != nil {
		t.Fatalf("uploaded: %v", err)
	}
	if _, err := x.InsertCustomDomainCapped(ctx, auto(s2, "d"), caps); err != nil {
		t.Fatalf("u2: %v", err)
	}
	if _, err := x.InsertCustomDomainCapped(ctx, auto(s2, "e"), caps); !errors.Is(err, ErrAutoDomainCap) {
		t.Fatalf("over all: %v, want ErrAutoDomainCap", err)
	}
	// Renaming an auto row does not count it twice; switching the uploaded
	// row to auto does count.
	row, err := x.LookupCustomDomainByHostname(ctx, "a."+userID+".example")
	if err != nil {
		t.Fatal(err)
	}
	row.Hostname = "a2." + userID + ".example"
	if err := x.UpdateCustomDomainCapped(ctx, row, caps); err != nil {
		t.Fatalf("rename: %v", err)
	}
	up, err := x.LookupCustomDomainByHostname(ctx, "up."+userID+".example")
	if err != nil {
		t.Fatal(err)
	}
	up.CertSource, up.CertPEM, up.KeyPEM, up.CertSHA256 = CertSourceAuto, "", "", ""
	if err := x.UpdateCustomDomainCapped(ctx, up, caps); !errors.Is(err, ErrAutoDomainCap) {
		t.Fatalf("switch: %v, want ErrAutoDomainCap (the relay's cap is full)", err)
	}
	caps.Max++
	if err := x.UpdateCustomDomainCapped(ctx, up, caps); !errors.Is(err, ErrAutoDomainUserCap) {
		t.Fatalf("switch: %v, want ErrAutoDomainUserCap", err)
	}
	// The domains of a user's services, for its delete.
	got, err := x.ListCustomDomainsOfUser(ctx, u1)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range got {
		names = append(names, d.Hostname)
	}
	slices.Sort(names)
	want := []string{"a2." + userID + ".example", "b." + userID + ".example", "up." + userID + ".example"}
	if !slices.Equal(names, want) {
		t.Fatalf("domains of u1: %v, want %v", names, want)
	}
}
