package db

import (
	"context"
	"testing"
	"time"
)

// CustomDomainCovers: a custom domain's own name, a name under it, and a
// name under a wildcard; never a parent or a sibling.
func TestCustomDomainCovers(t *testing.T) { checkCustomDomainCovers(t, testDB(t), "u-covers") }

func checkCustomDomainCovers(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	svc := Service{ID: "svc-covers-" + userID, UserID: userID, Name: "web", Type: "http", AccessMode: "open"}
	if err := x.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"app.example.com", "*.wild.example.com"} {
		if _, err := x.InsertCustomDomain(ctx, ServiceCustomDomain{ServiceID: svc.ID, Hostname: h, CertPEM: "c", KeyPEM: "k",
			CertSHA256: h, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]bool{
		"app.example.com": true, "x.app.example.com": true, "a.b.app.example.com": true,
		"a.wild.example.com": true, "wild.example.com": false, "example.com": false,
		"other.example.com": false, "myapp.example.com": false,
	} {
		got, err := x.CustomDomainCovers(ctx, name)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
}
