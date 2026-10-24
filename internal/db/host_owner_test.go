package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// H12: one owner per host name. A custom domain that is, is above, or is a
// wildcard over a tcp_hostname is refused (ErrHostIsTCPName); a tcp_hostname
// a custom domain covers is refused (ErrTCPHostnameCovered); both in one
// transaction, so two writers never both win.
func TestHostOwnership(t *testing.T) { checkHostOwnership(t, testDB(t), "u-hostown") }

func hostOwnDomain(svc, host string) ServiceCustomDomain {
	return ServiceCustomDomain{ServiceID: svc, Hostname: host, CertPEM: "c", KeyPEM: "k",
		CertSHA256: host, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
}

func checkHostOwnership(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	web, rdp := "web"+userID, "rdp"+userID
	for id, typ := range map[string]string{web: "http", rdp: "tcp"} {
		if err := x.CreateService(ctx, Service{ID: id, UserID: userID, Name: typ, Type: typ, AccessMode: "open"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.SetServiceTCPHostname(ctx, rdp, "rdp.hostown.example"); err != nil {
		t.Fatal(err)
	}
	// Custom domains the tcp name is in the way of.
	for _, h := range []string{"rdp.hostown.example", "RDP.hostown.example", "hostown.example", "*.hostown.example"} {
		if _, err := x.InsertCustomDomain(ctx, hostOwnDomain(web, h)); !errors.Is(err, ErrHostIsTCPName) {
			t.Errorf("insert %s: %v, want ErrHostIsTCPName", h, err)
		}
	}
	// Neighbours are free.
	for _, h := range []string{"xrdp.hostown.example", "web.hostown.example", "*.rdp.hostown.example", "other.example"} {
		if _, err := x.InsertCustomDomain(ctx, hostOwnDomain(web, h)); err != nil {
			t.Errorf("insert %s: %v", h, err)
		}
	}
	// An update to a tcp name is refused as well; the row stays as it was.
	row, err := x.LookupCustomDomainByHostname(ctx, "web.hostown.example")
	if err != nil {
		t.Fatal(err)
	}
	row.Hostname = "rdp.hostown.example"
	if err := x.UpdateCustomDomain(ctx, row); !errors.Is(err, ErrHostIsTCPName) {
		t.Errorf("update: %v, want ErrHostIsTCPName", err)
	}
	if _, err := x.LookupCustomDomainByHostname(ctx, "web.hostown.example"); err != nil {
		t.Errorf("row gone: %v", err)
	}
	// The other direction: a custom domain covers the name.
	if err := x.SetServiceTCPHostname(ctx, rdp, ""); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"web.hostown.example", "a.web.hostown.example"} {
		if err := x.SetServiceTCPHostname(ctx, rdp, h); !errors.Is(err, ErrTCPHostnameCovered) {
			t.Errorf("set %s: %v, want ErrTCPHostnameCovered", h, err)
		}
	}
	// A wildcard over rdp.hostown.example does not cover it, so its
	// children are covered and itself is not.
	if err := x.SetServiceTCPHostname(ctx, rdp, "rdp.hostown.example"); err != nil {
		t.Errorf("set rdp.hostown.example: %v", err)
	}
	// Clearing is never refused.
	if err := x.SetServiceTCPHostname(ctx, rdp, ""); err != nil {
		t.Errorf("clear: %v", err)
	}
}

// Two writers on one name: a custom domain and a tcp_hostname at the same
// time never both succeed, and one of them does.
func TestHostOwnershipRace(t *testing.T) { checkHostOwnershipRace(t, testDB(t), "u-hostrace") }

func checkHostOwnershipRace(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	web, rdp := "web"+userID, "rdp"+userID
	for id, typ := range map[string]string{web: "http", rdp: "tcp"} {
		if err := x.CreateService(ctx, Service{ID: id, UserID: userID, Name: typ, Type: typ, AccessMode: "open"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("r%d.race.example", i)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var domErr, tcpErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, domErr = x.InsertCustomDomain(ctx, hostOwnDomain(web, name))
		}()
		go func() {
			defer wg.Done()
			<-start
			tcpErr = x.SetServiceTCPHostname(ctx, rdp, name)
		}()
		close(start)
		wg.Wait()
		if (domErr == nil) == (tcpErr == nil) {
			t.Fatalf("%s: domain %v, tcp %v: want exactly one winner", name, domErr, tcpErr)
		}
		if err := x.SetServiceTCPHostname(ctx, rdp, ""); err != nil {
			t.Fatal(err)
		}
	}
}
