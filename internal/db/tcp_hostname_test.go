package db

import (
	"context"
	"errors"
	"testing"
)

func TestServiceTCPHostname(t *testing.T) { checkServiceTCPHostname(t, testDB(t), "u-tcphost") }

// A tcp service's TLS passthrough host name is read by every reader of
// service rows, cleared with "", and unique: a second service cannot take
// one that is held, and gets it once it is free.
func checkServiceTCPHostname(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	a := Service{ID: "svc-host-a-" + userID, UserID: userID, Name: "a", Type: "tcp", AccessMode: "open"}
	b := Service{ID: "svc-host-b-" + userID, UserID: userID, Name: "b", Type: "tcp", AccessMode: "open"}
	for _, s := range []Service{a, b} {
		if err := x.CreateService(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := x.GetServiceByID(ctx, a.ID); got.TCPHostname != "" {
		t.Fatalf("new service: %q", got.TCPHostname)
	}
	host := "rdp-" + userID + ".example.com"
	if err := x.SetServiceTCPHostname(ctx, a.ID, host); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, a.ID); got.TCPHostname != host {
		t.Fatalf("after set: %q", got.TCPHostname)
	}
	if list, err := x.ListServicesByUser(ctx, userID); err != nil || len(list) != 2 {
		t.Fatalf("list: %v %v", list, err)
	} else {
		for _, s := range list {
			if s.ID == a.ID && s.TCPHostname != host {
				t.Fatalf("list: %+v", s)
			}
		}
	}
	if err := x.SetServiceTCPHostname(ctx, b.ID, host); !errors.Is(err, ErrTCPHostnameTaken) {
		t.Fatalf("second service: %v, want ErrTCPHostnameTaken", err)
	}
	if got, _ := x.GetServiceByID(ctx, b.ID); got.TCPHostname != "" {
		t.Fatalf("the refused write was kept: %q", got.TCPHostname)
	}
	// Setting it again on its own service is no conflict.
	if err := x.SetServiceTCPHostname(ctx, a.ID, host); err != nil {
		t.Fatal(err)
	}
	if err := x.SetServiceTCPHostname(ctx, a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, a.ID); got.TCPHostname != "" {
		t.Fatalf("after clear: %q", got.TCPHostname)
	}
	// Two services without one are no conflict (NULL, not '').
	if err := x.SetServiceTCPHostname(ctx, b.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := x.SetServiceTCPHostname(ctx, b.ID, host); err != nil {
		t.Fatalf("free again: %v", err)
	}
	if err := x.SetServiceTCPHostname(ctx, "no-such-service", "x.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown service: %v", err)
	}
}
