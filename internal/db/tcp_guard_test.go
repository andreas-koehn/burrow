package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// tcp complete (E6, E7): the limits of a tcp service, its bans and its daily
// usage. Each check runs on SQLite here and on Postgres in
// tcp_services_postgres_test.go.

func TestServiceTCPLimits(t *testing.T) { checkServiceTCPLimits(t, testDB(t), "u-tcplim") }

func TestTCPBansAndUsage(t *testing.T) { checkTCPBansAndUsage(t, testDB(t), "u-tcpban") }

// A tcp service starts without limits; what is set is read back by every
// reader of service rows, a nil field is kept, and 0 / "" turn them off.
func checkServiceTCPLimits(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-lim-" + userID, UserID: userID, Name: "web", Type: "tcp", AccessMode: "open"}
	if err := x.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetServiceByID(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RateLimitKbps != 0 || got.DailyBytes != 0 || got.TCPAutoBan != "" {
		t.Fatalf("new service: %+v", got)
	}
	const ab = `{"enabled":true,"short_s":3,"count":10,"window_min":10,"ban_min":60}`
	big := int64(1) << 40 // more than 32 bits
	if err := x.SetServiceTCPLimits(ctx, svc.ID, ptr(1024), &big, ptr(ab)); err != nil {
		t.Fatal(err)
	}
	got, _ = x.GetServiceByID(ctx, svc.ID)
	if got.RateLimitKbps != 1024 || got.DailyBytes != big || got.TCPAutoBan != ab {
		t.Fatalf("after set: %+v", got)
	}
	list, err := x.ListServicesByUser(ctx, userID)
	if err != nil || len(list) != 1 || list[0].DailyBytes != big || list[0].TCPAutoBan != ab {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := x.SetServiceTCPLimits(ctx, svc.ID, ptr(0), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, svc.ID); got.RateLimitKbps != 0 || got.DailyBytes != big || got.TCPAutoBan != ab {
		t.Fatalf("rate alone: %+v", got)
	}
	zero := int64(0)
	if err := x.SetServiceTCPLimits(ctx, svc.ID, nil, &zero, ptr("")); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetServiceByID(ctx, svc.ID); got.DailyBytes != 0 || got.TCPAutoBan != "" {
		t.Fatalf("after clear: %+v", got)
	}
	if err := x.SetServiceTCPLimits(ctx, "svc-missing-"+userID, ptr(64), nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service: %v", err)
	}
}

// Bans: a ban is listed until it expires, a second ban of the same source
// replaces the first, a removal returns the row, expired rows are pruned.
// Usage: what is added to a day adds up, other days and services are apart.
func checkTCPBansAndUsage(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	id := "svc-ban-" + userID
	if err := x.CreateService(ctx, Service{ID: id, UserID: userID, Name: "rdp", Type: "tcp", AccessMode: "open"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, b := range []TCPBan{
		{ServiceID: id, Source: "203.0.113.7", Until: now.Add(time.Hour), Reason: "auto: 10 short", CreatedAt: now},
		{ServiceID: id, Source: "2001:db8:1:2::/64", Until: now.Add(30 * time.Minute), Reason: "manual", CreatedAt: now},
		{ServiceID: id, Source: "198.51.100.1", Until: now.Add(-time.Minute), Reason: "old", CreatedAt: now.Add(-time.Hour)},
	} {
		if err := x.PutTCPBan(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	bans, err := x.ListTCPBans(ctx, id, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(bans) != 2 || bans[0].Source != "2001:db8:1:2::/64" || bans[1].Source != "203.0.113.7" ||
		!bans[1].Until.Equal(now.Add(time.Hour)) || bans[1].Reason != "auto: 10 short" || !bans[1].CreatedAt.Equal(now) {
		t.Fatalf("bans: %+v", bans)
	}
	if n, err := x.CountTCPBans(ctx, id, now); err != nil || n != 2 {
		t.Fatalf("count %d %v", n, err)
	}
	// The same source again: replaced.
	if err := x.PutTCPBan(ctx, TCPBan{ServiceID: id, Source: "203.0.113.7", Until: now.Add(2 * time.Hour), Reason: "manual: again", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	bans, _ = x.ListTCPBans(ctx, id, now)
	if len(bans) != 2 || bans[1].Reason != "manual: again" || !bans[1].Until.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("after replace: %+v", bans)
	}
	got, err := x.DeleteTCPBan(ctx, id, "203.0.113.7")
	if err != nil || got.Reason != "manual: again" {
		t.Fatalf("delete: %+v %v", got, err)
	}
	if _, err := x.DeleteTCPBan(ctx, id, "203.0.113.7"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if n, err := x.PruneTCPBans(ctx, id, now); err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}

	day, other := "2026-10-24", "2026-10-25"
	if in, out, err := x.TCPDailyUsage(ctx, id, day); err != nil || in != 0 || out != 0 {
		t.Fatalf("empty day: %d %d %v", in, out, err)
	}
	big := int64(3) << 32
	for _, add := range [][2]int64{{100, 200}, {big, 5}, {0, 1}} {
		if err := x.AddTCPDailyUsage(ctx, id, day, add[0], add[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.AddTCPDailyUsage(ctx, id, other, 7, 7); err != nil {
		t.Fatal(err)
	}
	if in, out, err := x.TCPDailyUsage(ctx, id, day); err != nil || in != 100+big || out != 206 {
		t.Fatalf("day: %d %d %v", in, out, err)
	}
	if in, out, _ := x.TCPDailyUsage(ctx, id, other); in != 7 || out != 7 {
		t.Fatalf("other day: %d %d", in, out)
	}
	// They go with the service.
	if _, err := x.sqlDB.ExecContext(ctx, `DELETE FROM services WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if bans, _ := x.ListTCPBans(ctx, id, now); len(bans) != 0 {
		t.Fatalf("bans after delete: %+v", bans)
	}
	if in, _, _ := x.TCPDailyUsage(ctx, id, day); in != 0 {
		t.Fatalf("usage after delete: %d", in)
	}
}
