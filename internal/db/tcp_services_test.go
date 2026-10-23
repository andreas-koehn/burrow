package db

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// v0.13.0 native tcp (migration 0040): a tcp service keeps its public port.
// Each check runs on SQLite here and on Postgres in
// tcp_services_postgres_test.go; basePort keeps the two apart on a Postgres
// database that outlives the run.

func TestServiceTCPPort(t *testing.T) { checkServiceTCPPort(t, testDB(t), "u-tcpport", 9501) }

func TestServiceTCPPortTakeOver(t *testing.T) {
	checkServiceTCPPortTakeOver(t, testDB(t), "u-tcptake", 9511)
}

func TestMigration0040_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0040_v0.13.0_tcp_services.sql")
}

func TestServiceIPGrantsGoWithService(t *testing.T) {
	checkServiceIPGrantsCascade(t, testDB(t), "u-grants")
}

// A tcp service starts without a port; the port it is given is read back
// with its kind, listed as held by it, and cannot be given to a second
// service. Giving it port 0 clears it, and a deleted service frees its port.
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
	if got.TCPPort != 0 || got.TCPPortPinned || got.MaxConnections != 0 {
		t.Fatalf("new tcp service: %+v", got)
	}

	if err := x.SetServiceTCPPort(ctx, a.ID, basePort, true, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, a.ID); got.TCPPort != basePort || !got.TCPPortPinned {
		t.Fatalf("TCPPort = %d pinned %v, want %d pinned", got.TCPPort, got.TCPPortPinned, basePort)
	}
	res, err := x.ListTCPPortReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h := res[basePort]; h.ServiceID != a.ID || h.UserID != userID || !h.Pinned || h.LastSeen != nil {
		t.Fatalf("reservations[%d] = %+v", basePort, h)
	}
	// Setting the same port again is no change; automatic now.
	if err := x.SetServiceTCPPort(ctx, a.ID, basePort, false, ""); err != nil {
		t.Fatalf("same port again: %v", err)
	}
	if err := x.SetServiceTCPPort(ctx, b.ID, basePort, false, ""); !errors.Is(err, ErrTCPPortReserved) {
		t.Fatalf("a second service on the port: err = %v, want ErrTCPPortReserved", err)
	}
	if err := x.SetServiceTCPPort(ctx, "svc-missing-"+userID, basePort+1, false, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service: err = %v, want ErrNotFound", err)
	}

	if err := x.SetServiceTCPPort(ctx, a.ID, 0, false, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, a.ID); got.TCPPort != 0 || got.TCPPortPinned {
		t.Fatalf("cleared: %+v", got)
	}
	if err := x.SetServiceTCPPort(ctx, b.ID, basePort, false, ""); err != nil {
		t.Fatalf("a freed port: %v", err)
	}
	if err := x.DeleteService(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	res, err = x.ListTCPPortReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := res[basePort]; ok {
		t.Fatalf("a deleted service still holds port %d (%+v)", basePort, h)
	}
}

// A port is taken over from the service named in takeFrom while that one
// holds it automatically, in one step: the other service loses it. A pinned
// hold, or a port the named service no longer holds, is not taken and
// nothing changes.
func checkServiceTCPPortTakeOver(t *testing.T, x *DB, userID string, basePort int) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	auto := Service{ID: "svc-auto-" + userID, UserID: userID, Name: "auto", Type: "tcp", AccessMode: "open"}
	fixed := Service{ID: "svc-fixed-" + userID, UserID: userID, Name: "fixed", Type: "tcp", AccessMode: "open"}
	for _, s := range []Service{auto, fixed} {
		if err := x.CreateService(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.SetServiceTCPPort(ctx, auto.ID, basePort, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := x.SetServiceTCPPort(ctx, fixed.ID, basePort, true, auto.ID); err != nil {
		t.Fatalf("take over an automatic hold: %v", err)
	}
	if got, _ := x.GetServiceByID(ctx, auto.ID); got.TCPPort != 0 {
		t.Fatalf("the displaced service still holds %d", got.TCPPort)
	}
	if got, _ := x.GetServiceByID(ctx, fixed.ID); got.TCPPort != basePort || !got.TCPPortPinned {
		t.Fatalf("taker: %+v", got)
	}
	// fixed's hold is pinned: auto cannot take it back.
	if err := x.SetServiceTCPPort(ctx, auto.ID, basePort, false, fixed.ID); !errors.Is(err, ErrTCPPortReserved) {
		t.Fatalf("take a pinned hold: err = %v", err)
	}
	// auto holds nothing now: naming it takes nothing.
	if err := x.SetServiceTCPPort(ctx, fixed.ID, basePort+1, true, auto.ID); !errors.Is(err, ErrTCPPortReserved) {
		t.Fatalf("take from a service that holds nothing there: err = %v", err)
	}
	if got, _ := x.GetServiceByID(ctx, fixed.ID); got.TCPPort != basePort {
		t.Fatalf("a refused take over changed the taker: %+v", got)
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

func TestMigration0041_TCPSlugsAndTokenName(t *testing.T) {
	checkMigration0041(t, testDB(t), "u-tcpslug", "0041_v0.13.0_tcp_forwards.sql")
}

// Migration 0041 gives every tcp service without a slug one (a valid slug,
// each its own) and leaves http services as they are; connection_logs gets
// token_name. Down takes the tcp slugs and the column away again.
func checkMigration0041(t *testing.T, x *DB, userID, file string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	raw, err := migrationFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	_, down, _ := strings.Cut(string(raw), "-- +goose Down")
	if _, err := x.sqlDB.ExecContext(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	rows := []Service{
		{ID: "svc-t1-" + userID, UserID: userID, Name: "rdp", Type: "tcp", AccessMode: "private"},
		{ID: "svc-t2-" + userID, UserID: userID, Name: "ssh", Type: "tcp", AccessMode: "open"},
		{ID: "svc-h1-" + userID, UserID: userID, Name: "web", Type: "http", AccessMode: "open", Subdomain: "web-" + userID},
		{ID: "svc-h2-" + userID, UserID: userID, Name: "api", Type: "http", AccessMode: "open"},
	}
	for _, s := range rows {
		if err := x.CreateService(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.sqlDB.ExecContext(ctx, upBlock(string(raw))); err != nil {
		t.Fatalf("up: %v", err)
	}
	got := map[string]string{}
	for _, s := range rows {
		svc, err := x.GetServiceByID(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		got[s.Name] = svc.Subdomain
	}
	for _, name := range []string{"rdp", "ssh"} {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`).MatchString(got[name]) {
			t.Errorf("tcp service %s got slug %q", name, got[name])
		}
	}
	if got["rdp"] == got["ssh"] {
		t.Errorf("two tcp services share the slug %q", got["rdp"])
	}
	if got["web"] != "web-"+userID || got["api"] != "" {
		t.Errorf("http services changed: %v", got)
	}
	// A database that outlives the run (Postgres) may hold the row of the
	// last one.
	if _, err := x.sqlDB.ExecContext(ctx, `DELETE FROM connection_logs WHERE id = ?`, "cl-"+userID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO connection_logs(id, kind, source_ip, started_at, ended_at, duration_ms, status, token_name)
		 VALUES(?,?,?,?,?,?,?,?)`, "cl-"+userID, "tcp_forward", "127.0.0.1", time.Now(), time.Now(), 0, "closed_clean", "laptop"); err != nil {
		t.Fatalf("token_name: %v", err)
	}
}
