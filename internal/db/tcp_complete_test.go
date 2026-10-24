package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// v0.13.0 tcp complete (migration 0042): the per-service tcp settings, the
// tables for bans and daily usage, and the protocol columns of the
// connection log. Each check runs on SQLite here and on Postgres in
// tcp_services_postgres_test.go.

func TestMigration0042_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0042_v0.13.0_tcp_complete.sql")
}

func TestServiceTCPSettings(t *testing.T) { checkServiceTCPSettings(t, testDB(t), "u-tcpset") }

func TestTCPCompleteTables(t *testing.T) { checkTCPCompleteTables(t, testDB(t), "u-tcptables") }

// A tcp service starts with no PROXY protocol and no idle timeout; what is
// set is read back by every reader of service rows, and 0 / "" turn them off.
func checkServiceTCPSettings(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-set-" + userID, UserID: userID, Name: "web", Type: "tcp", AccessMode: "open"}
	if err := x.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetServiceByID(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyProtocol != "" || got.IdleTimeoutS != 0 {
		t.Fatalf("new service: %+v", got)
	}
	if err := x.SetServiceTCPSettings(ctx, svc.ID, ptr("v2"), ptr(3600)); err != nil {
		t.Fatal(err)
	}
	got, _ = x.GetServiceByID(ctx, svc.ID)
	if got.ProxyProtocol != "v2" || got.IdleTimeoutS != 3600 {
		t.Fatalf("after set: %+v", got)
	}
	list, err := x.ListServicesByUser(ctx, userID)
	if err != nil || len(list) != 1 || list[0].ProxyProtocol != "v2" || list[0].IdleTimeoutS != 3600 {
		t.Fatalf("list: %+v %v", list, err)
	}
	// One field alone leaves the other.
	if err := x.SetServiceTCPSettings(ctx, svc.ID, nil, ptr(60)); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, svc.ID); got.ProxyProtocol != "v2" || got.IdleTimeoutS != 60 {
		t.Fatalf("idle alone: %+v", got)
	}
	if err := x.SetServiceTCPSettings(ctx, svc.ID, ptr(""), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, svc.ID); got.ProxyProtocol != "" || got.IdleTimeoutS != 60 {
		t.Fatalf("proxy alone: %+v", got)
	}
	if err := x.SetServiceTCPSettings(ctx, svc.ID, nil, ptr(0)); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, svc.ID); got.ProxyProtocol != "" || got.IdleTimeoutS != 0 {
		t.Fatalf("after clear: %+v", got)
	}
	if err := x.SetServiceTCPSettings(ctx, "svc-missing-"+userID, ptr("v2"), ptr(60)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service: %v", err)
	}
}

// The tables and columns later tasks fill exist with their keys: a ban and
// a day of usage go with their service, a host name belongs to one service,
// and a connection-log row keeps sni and protocol.
func checkTCPCompleteTables(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	a := Service{ID: "svc-ta-" + userID, UserID: userID, Name: "a", Type: "tcp", AccessMode: "open"}
	b := Service{ID: "svc-tb-" + userID, UserID: userID, Name: "b", Type: "tcp", AccessMode: "open"}
	for _, s := range []Service{a, b} {
		if err := x.CreateService(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) error {
		_, err := x.sqlDB.ExecContext(ctx, q, args...)
		return err
	}
	host := "h-" + userID + ".tcp.example"
	if err := exec(`UPDATE services SET tcp_hostname=?, rate_limit_kbps=?, daily_bytes=?, tcp_autoban=? WHERE id=?`,
		host, 512, int64(5)<<40, `{"count":10}`, a.ID); err != nil {
		t.Fatalf("settings columns: %v", err)
	}
	if err := exec(`UPDATE services SET tcp_hostname=? WHERE id=?`, host, b.ID); err == nil {
		t.Fatal("two services share a tcp hostname")
	}
	if err := exec(`INSERT INTO tcp_bans(service_id, source, until, reason) VALUES(?,?,?,?)`,
		a.ID, "203.0.113.7", time.Now().Add(time.Hour).UTC(), "autoban"); err != nil {
		t.Fatalf("tcp_bans: %v", err)
	}
	if err := exec(`INSERT INTO tcp_bans(service_id, source, until, reason) VALUES(?,?,?,?)`,
		a.ID, "203.0.113.7", time.Now().UTC(), "again"); err == nil {
		t.Fatal("a source banned twice from one service")
	}
	if err := exec(`INSERT INTO tcp_daily_usage(service_id, day, bytes_in, bytes_out) VALUES(?,?,?,?)`,
		a.ID, "2026-10-24", int64(3)<<32, 7); err != nil {
		t.Fatalf("tcp_daily_usage: %v", err)
	}
	id := "cl-tc-" + userID
	_ = exec(`DELETE FROM connection_logs WHERE id = ?`, id)
	if err := exec(`INSERT INTO connection_logs(id, kind, source_ip, started_at, ended_at, duration_ms, status, sni, protocol)
		 VALUES(?,?,?,?,?,?,?,?,?)`, id, "tcp_proxy", "127.0.0.1", time.Now().UTC(), time.Now().UTC(), 0, "closed_clean",
		"rdp.example.com", "tls"); err != nil {
		t.Fatalf("connection_logs sni/protocol: %v", err)
	}
	if err := x.DeleteService(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := x.sqlDB.QueryRowContext(ctx, (`SELECT (SELECT COUNT(*) FROM tcp_bans WHERE service_id=?) + (SELECT COUNT(*) FROM tcp_daily_usage WHERE service_id=?)`),
		a.ID, a.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left after delete: %d %v", n, err)
	}
}

func ptr[T any](v T) *T { return &v }
