package db

import (
	"context"
	"errors"
	"testing"
)

func TestMigration0044_DownAndUp(t *testing.T) {
	checkMigration0029DownAndUp(t, testDB(t), "0044_v0.13.0_tcp_hostname_only.sql")
}

func TestServiceTCPHostnameOnly(t *testing.T) {
	checkServiceTCPHostnameOnly(t, testDB(t), "u-hostonly")
}

// A service has its public port until it is set to host name only; every
// reader of service rows sees the setting, and it can be turned back.
func checkServiceTCPHostnameOnly(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-hostonly-" + userID, UserID: userID, Name: "db", Type: "tcp", AccessMode: "open"}
	if err := x.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if got, err := x.GetServiceByID(ctx, svc.ID); err != nil || got.TCPHostnameOnly {
		t.Fatalf("new service: %+v %v", got, err)
	}
	if err := x.SetServiceTCPHostnameOnly(ctx, svc.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, svc.ID); !got.TCPHostnameOnly {
		t.Fatalf("after set: %+v", got)
	}
	if list, err := x.ListServicesByUser(ctx, userID); err != nil || len(list) != 1 || !list[0].TCPHostnameOnly {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := x.SetServiceTCPHostnameOnly(ctx, svc.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, svc.ID); got.TCPHostnameOnly {
		t.Fatalf("after clear: %+v", got)
	}
	if err := x.SetServiceTCPHostnameOnly(ctx, "svc-missing-"+userID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service: %v", err)
	}
}
