package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestIPGrants(t *testing.T) { checkIPGrants(t, testDB(t), "u-grants") }

// checkIPGrants: grants are listed until they expire, the first to expire
// first; one is deleted by its service only; expired ones are pruned; the
// cap of a tcp service is set and cleared.
func checkIPGrants(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-g-" + userID, UserID: userID, Name: "rdp", Type: "tcp", AccessMode: "ip"}
	other := Service{ID: "svc-o-" + userID, UserID: userID, Name: "ssh", Type: "tcp", AccessMode: "ip"}
	for _, s := range []Service{svc, other} {
		if err := x.CreateService(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	grants := []IPGrant{
		{ID: "g8-" + userID, ServiceID: svc.ID, CIDR: "203.0.113.7/32", ExpiresAt: now.Add(8 * time.Hour), Note: "laptop", CreatedBy: userID},
		{ID: "g1-" + userID, ServiceID: svc.ID, CIDR: "2001:db8::/64", ExpiresAt: now.Add(time.Hour)},
		{ID: "gx-" + userID, ServiceID: svc.ID, CIDR: "198.51.100.0/24", ExpiresAt: now.Add(-time.Minute)},
		{ID: "go-" + userID, ServiceID: other.ID, CIDR: "192.0.2.1/32", ExpiresAt: now.Add(time.Hour)},
	}
	for _, g := range grants {
		if err := x.CreateIPGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	list, err := x.ListIPGrants(ctx, svc.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "g1-"+userID || list[1].ID != "g8-"+userID {
		t.Fatalf("listed %+v", list)
	}
	g := list[1]
	if g.CIDR != "203.0.113.7/32" || g.Note != "laptop" || g.CreatedBy != userID || !g.ExpiresAt.Equal(now.Add(8*time.Hour)) || g.CreatedAt.IsZero() {
		t.Fatalf("grant %+v", g)
	}
	if list[0].CreatedBy != "" {
		t.Fatalf("created by %q", list[0].CreatedBy)
	}
	if _, err := x.DeleteIPGrant(ctx, svc.ID, "go-"+userID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete another service's grant: %v", err)
	}
	if d, err := x.DeleteIPGrant(ctx, svc.ID, "g8-"+userID); err != nil || d.CIDR != "203.0.113.7/32" {
		t.Fatalf("delete: %+v %v", d, err)
	}
	if n, err := x.PruneIPGrants(ctx, svc.ID, now); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if list, _ := x.ListIPGrants(ctx, svc.ID, now.Add(2*time.Hour)); len(list) != 0 {
		t.Fatalf("after expiry: %+v", list)
	}

	if err := x.SetServiceMaxConnections(ctx, svc.ID, 7); err != nil {
		t.Fatal(err)
	}
	if s, _ := x.GetServiceByID(ctx, svc.ID); s.MaxConnections != 7 {
		t.Fatalf("max connections %d", s.MaxConnections)
	}
	if err := x.SetServiceMaxConnections(ctx, svc.ID, 0); err != nil {
		t.Fatal(err)
	}
	if s, _ := x.GetServiceByID(ctx, svc.ID); s.MaxConnections != 0 {
		t.Fatalf("max connections after clear %d", s.MaxConnections)
	}
	if err := x.SetServiceMaxConnections(ctx, "nope", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service: %v", err)
	}
}

func TestIPGrantsCapped(t *testing.T) { checkIPGrantsCapped(t, testDB(t), "u-grantcap") }

// checkIPGrantsCapped: a capped insert never leaves a service with more
// active grants than the cap, also when many run at once; expired grants do
// not count.
func checkIPGrantsCapped(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-cap-" + userID, UserID: userID, Name: "rdp", Type: "tcp", AccessMode: "ip"}
	if err := x.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := x.CreateIPGrant(ctx, IPGrant{ID: "old-" + userID, ServiceID: svc.ID, CIDR: "192.0.2.1/32", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	const limit = 3
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- x.CreateIPGrantCapped(ctx, IPGrant{ID: fmt.Sprintf("g%d-%s", i, userID), ServiceID: svc.ID,
				CIDR: "192.0.2.1/32", ExpiresAt: now.Add(time.Hour)}, limit, now)
		}(i)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrTooManyIPGrants):
			t.Errorf("capped insert: %v", err)
		}
	}
	list, err := x.ListIPGrants(ctx, svc.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > limit || len(list) != ok || ok == 0 {
		t.Fatalf("%d active grants, %d inserts reported ok, cap %d", len(list), ok, limit)
	}
}
