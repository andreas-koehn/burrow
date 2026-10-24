package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Migration 0045 (http/2, gRPC and certificate source): the upstream
// protocol settings of a service, the certificate source of a custom
// domain and the gRPC columns of the connection log. Each check runs on
// SQLite here and on Postgres in tcp_services_postgres_test.go.

func TestMigration0045_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0045_v0.13.0_http2_grpc.sql")
}

func TestServiceHTTPSettings(t *testing.T) { checkServiceHTTPSettings(t, testDB(t), "u-httpset") }

func TestCustomDomainCertSource(t *testing.T) { checkCustomDomainCertSource(t, testDB(t), "u-certsrc") }

func TestConnectionLogGRPCColumns(t *testing.T) { checkConnectionLogGRPCColumns(t, testDB(t)) }

// A service starts on HTTP/1.1 with verification off and no cap; what is
// set is read back by every reader of service rows, a field left nil is
// kept, and "" clears the optional texts.
func checkServiceHTTPSettings(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-http-" + userID, UserID: userID, Name: "api", Type: "http", AccessMode: "open", Subdomain: "h" + userID}
	if err := x.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetServiceByID(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamProtocol != "http1" || got.UpstreamTLSVerify || got.UpstreamTLSServerName != "" ||
		got.UpstreamTLSCAPEM != "" || got.GRPCMaxDurationS != 0 {
		t.Fatalf("new service: %+v", got)
	}
	if err := x.SetServiceHTTPSettings(ctx, svc.ID, HTTPSettingsUpdate{
		UpstreamProtocol: ptr("h2"), UpstreamTLSVerify: ptr(true), UpstreamTLSServerName: ptr("app.internal"),
		UpstreamTLSCAPEM: ptr("PEM"), GRPCMaxDurationS: ptr(3600),
	}); err != nil {
		t.Fatal(err)
	}
	want := func(g Service, proto string, verify bool, sn, ca string, max int) {
		t.Helper()
		if g.UpstreamProtocol != proto || g.UpstreamTLSVerify != verify || g.UpstreamTLSServerName != sn ||
			g.UpstreamTLSCAPEM != ca || g.GRPCMaxDurationS != max {
			t.Fatalf("got %+v; want %s %v %q %q %d", g, proto, verify, sn, ca, max)
		}
	}
	got, _ = x.GetServiceByID(ctx, svc.ID)
	want(got, "h2", true, "app.internal", "PEM", 3600)
	list, err := x.ListServicesByUser(ctx, userID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	want(list[0], "h2", true, "app.internal", "PEM", 3600)

	// One field alone keeps the others.
	if err := x.SetServiceHTTPSettings(ctx, svc.ID, HTTPSettingsUpdate{UpstreamProtocol: ptr("h2c")}); err != nil {
		t.Fatal(err)
	}
	got, _ = x.GetServiceByID(ctx, svc.ID)
	want(got, "h2c", true, "app.internal", "PEM", 3600)
	// "" clears the optional texts (NULL), false and 0 turn the rest off.
	if err := x.SetServiceHTTPSettings(ctx, svc.ID, HTTPSettingsUpdate{
		UpstreamTLSVerify: ptr(false), UpstreamTLSServerName: ptr(""), UpstreamTLSCAPEM: ptr(""), GRPCMaxDurationS: ptr(0),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = x.GetServiceByID(ctx, svc.ID)
	want(got, "h2c", false, "", "", 0)
	var nulls int
	if err := x.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM services WHERE id=? AND upstream_tls_server_name IS NULL AND upstream_tls_ca_pem IS NULL`, svc.ID).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("cleared texts are NULL: %d %v", nulls, err)
	}
	// An empty update is a no-op that still reports a missing service.
	if err := x.SetServiceHTTPSettings(ctx, svc.ID, HTTPSettingsUpdate{}); err != nil {
		t.Fatal(err)
	}
	if err := x.SetServiceHTTPSettings(ctx, "svc-missing-"+userID, HTTPSettingsUpdate{UpstreamProtocol: ptr("h2")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service: %v", err)
	}
	if err := x.SetServiceHTTPSettings(ctx, "svc-missing-"+userID, HTTPSettingsUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service, empty update: %v", err)
	}
}

// A custom domain is "uploaded" unless it says otherwise; "auto" keeps the
// empty certificate columns, and every reader returns the source.
func checkCustomDomainCertSource(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-cd-" + userID, UserID: userID, Name: "api", Type: "http", AccessMode: "open", Subdomain: "c" + userID}
	if err := x.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	up, err := x.InsertCustomDomain(ctx, ServiceCustomDomain{ServiceID: svc.ID, Hostname: "up-" + userID + ".example.com",
		CertPEM: "c", KeyPEM: "k", CertSHA256: "s", NotBefore: time.Now().UTC(), NotAfter: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if up.CertSource != CertSourceUploaded {
		t.Fatalf("default source %q", up.CertSource)
	}
	auto, err := x.InsertCustomDomain(ctx, ServiceCustomDomain{ServiceID: svc.ID, Hostname: "auto-" + userID + ".example.com", CertSource: CertSourceAuto})
	if err != nil {
		t.Fatal(err)
	}
	got, err := x.GetCustomDomain(ctx, svc.ID, auto.ID)
	if err != nil || got.CertSource != CertSourceAuto || got.CertPEM != "" || got.KeyPEM != "" || got.CertSHA256 != "" {
		t.Fatalf("auto: %+v %v", got, err)
	}
	if got, err := x.LookupCustomDomainByHostname(ctx, auto.Hostname); err != nil || got.CertSource != CertSourceAuto {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	list, err := x.ListCustomDomains(ctx, svc.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	sources := map[string]string{}
	for _, d := range list {
		sources[d.Hostname] = d.CertSource
	}
	if sources[up.Hostname] != CertSourceUploaded || sources[auto.Hostname] != CertSourceAuto {
		t.Fatalf("list sources %v", sources)
	}
	all, err := x.ListAllCustomDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, d := range all {
		if (d.ID == up.ID && d.CertSource == CertSourceUploaded) || (d.ID == auto.ID && d.CertSource == CertSourceAuto) {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("list all: %+v", all)
	}
	// Switching a domain to an uploaded certificate keeps the source in step.
	auto.CertSource, auto.CertPEM, auto.KeyPEM, auto.CertSHA256 = CertSourceUploaded, "c2", "k2", "s2"
	auto.NotAfter = time.Now().UTC().Add(time.Hour)
	if err := x.UpdateCustomDomain(ctx, auto); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetCustomDomain(ctx, svc.ID, auto.ID); got.CertSource != CertSourceUploaded || got.CertPEM != "c2" {
		t.Fatalf("after update: %+v", got)
	}
	// An unknown source never reaches the table.
	if _, err := x.InsertCustomDomain(ctx, ServiceCustomDomain{ServiceID: svc.ID, Hostname: "bad-" + userID + ".example.com", CertSource: "magic"}); err == nil {
		t.Fatal("an unknown cert_source was stored")
	}
}

// The gRPC columns exist and are NULL for a row that sets none.
func checkConnectionLogGRPCColumns(t *testing.T, x *DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := x.DB().ExecContext(ctx, `INSERT INTO connection_logs(id, kind, source_ip, started_at, ended_at, duration_ms, bytes_in, bytes_out, status, grpc_method, grpc_status)
		VALUES('g1','http_proxy','10.0.0.1',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,0,0,0,'closed_clean','/a.B/C',14)`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.DB().ExecContext(ctx, `INSERT INTO connection_logs(id, kind, source_ip, started_at, ended_at, duration_ms, bytes_in, bytes_out, status)
		VALUES('g2','http_proxy','10.0.0.1',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,0,0,0,'closed_clean')`); err != nil {
		t.Fatal(err)
	}
	var m1 string
	var s1 int
	if err := x.DB().QueryRowContext(ctx, `SELECT grpc_method, grpc_status FROM connection_logs WHERE id='g1'`).Scan(&m1, &s1); err != nil || m1 != "/a.B/C" || s1 != 14 {
		t.Fatalf("g1: %q %d %v", m1, s1, err)
	}
	var n int
	if err := x.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM connection_logs WHERE id='g2' AND grpc_method IS NULL AND grpc_status IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("g2 NULL columns: %d %v", n, err)
	}
}
