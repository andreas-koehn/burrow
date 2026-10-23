package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// v0.13.0 native tcp (migration 0040): a tcp service keeps its public port.
// Each check runs on SQLite here and on Postgres in
// tcp_services_postgres_test.go; basePort keeps the two apart on a Postgres
// database that outlives the run.

func TestServiceTCPPort(t *testing.T) { checkServiceTCPPort(t, testDB(t), "u-tcpport", 9501) }

func TestMigration0040_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0040_v0.13.0_tcp_services.sql")
}

func TestServiceIPGrantsGoWithService(t *testing.T) {
	checkServiceIPGrantsCascade(t, testDB(t), "u-grants")
}

// A tcp service starts without a port; the port it is given is read back,
// listed as reserved for it, and cannot be given to a second service. Giving
// it port 0 clears it, and a deleted service frees its port.
func checkServiceTCPPort(t *testing.T, x *DB, userID string, basePort int) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	a := Service{ID: "svc-a-" + userID, UserID: userID, Name: "rdp", Type: "tcp", AccessMode: "open"}
	b := Service{ID: "svc-b-" + userID, UserID: userID, Name: "ssh", Type: "tcp", AccessMode: "open"}
	for _, s := range []Service{a, b} {
		if err := x.CreateService(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := x.GetServiceByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TCPPort != 0 || got.MaxConnections != 0 {
		t.Fatalf("new tcp service: TCPPort %d, MaxConnections %d, want 0 and 0", got.TCPPort, got.MaxConnections)
	}

	if err := x.SetServiceTCPPort(ctx, a.ID, basePort); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, a.ID); got.TCPPort != basePort {
		t.Fatalf("TCPPort = %d, want %d", got.TCPPort, basePort)
	}
	res, err := x.ListTCPPortReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res[basePort] != a.ID {
		t.Fatalf("reservations[%d] = %q, want %q (%v)", basePort, res[basePort], a.ID, res)
	}
	// Setting the same port again is no change.
	if err := x.SetServiceTCPPort(ctx, a.ID, basePort); err != nil {
		t.Fatalf("same port again: %v", err)
	}
	if err := x.SetServiceTCPPort(ctx, b.ID, basePort); !errors.Is(err, ErrTCPPortReserved) {
		t.Fatalf("a second service on the port: err = %v, want ErrTCPPortReserved", err)
	}
	if err := x.SetServiceTCPPort(ctx, "svc-missing-"+userID, basePort+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service: err = %v, want ErrNotFound", err)
	}

	if err := x.SetServiceTCPPort(ctx, a.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, a.ID); got.TCPPort != 0 {
		t.Fatalf("cleared: TCPPort = %d, want 0", got.TCPPort)
	}
	if err := x.SetServiceTCPPort(ctx, b.ID, basePort); err != nil {
		t.Fatalf("a freed port: %v", err)
	}
	if err := x.DeleteService(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	res, err = x.ListTCPPortReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := res[basePort]; ok {
		t.Fatalf("a deleted service still holds port %d (%q)", basePort, id)
	}
}

// The grants of a service go with it.
func checkServiceIPGrantsCascade(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-" + userID, UserID: userID, Name: "rdp", Type: "tcp", AccessMode: "open"}
	if err := x.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if _, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO service_ip_grants(id, service_id, cidr, expires_at, note, created_by) VALUES(?,?,?,?,?,?)`,
		"g-"+userID, svc.ID, "203.0.113.7/32", time.Now().UTC().Add(time.Hour), "laptop", userID); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	if err := x.DeleteService(ctx, svc.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := x.sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM service_ip_grants WHERE service_id=?`, svc.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d grants outlived their service", n)
	}
}
