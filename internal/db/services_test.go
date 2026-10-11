package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// mustUser inserts a users row for the given ID (e-mail derived from id).
// Fatals the test on error. Mirrors the inline user-seed pattern in migrate_test.go.
func mustUser(t *testing.T, x *DB, id string) {
	t.Helper()
	if err := x.CreateUser(context.Background(), User{
		ID: id, Email: id + "@test.invalid", PasswordHash: "h", Role: "user",
	}); err != nil {
		t.Fatalf("mustUser(%q): %v", id, err)
	}
}

func TestServiceLifecycle(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")

	// GetOrCreateService: defaults
	s, err := x.GetOrCreateService(ctx, "u1", "web", "http")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" || s.AccessMode != "open" || s.APIKeyHeader != "Authorization" {
		t.Fatalf("bad default service: %+v", s)
	}

	// Identity stability
	s2, err := x.GetOrCreateService(ctx, "u1", "web", "http")
	if err != nil {
		t.Fatal(err)
	}
	if s2.ID != s.ID {
		t.Fatalf("identity not stable: %s vs %s", s2.ID, s.ID)
	}

	// SetServiceSubdomain + GetServiceBySubdomain
	if err := x.SetServiceSubdomain(ctx, s.ID, "k7p2qx"); err != nil {
		t.Fatal(err)
	}

	// SetServiceAccessMode
	if err := x.SetServiceAccessMode(ctx, s.ID, "api_key", "X-Api-Key"); err != nil {
		t.Fatal(err)
	}

	got, err := x.GetServiceBySubdomain(ctx, "k7p2qx")
	if err != nil || got.ID != s.ID || got.AccessMode != "api_key" {
		t.Fatalf("by subdomain: %+v err=%v", got, err)
	}

	// GetServiceBySubdomain missing → ErrNotFound
	_, err = x.GetServiceBySubdomain(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// CreateServiceAPIKey + GetServiceAPIKeyByHash
	if err := x.CreateServiceAPIKey(ctx, ServiceAPIKey{
		ID: "k1", ServiceID: s.ID, Name: "ci", KeyHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	k, err := x.GetServiceAPIKeyByHash(ctx, s.ID, "abc")
	if err != nil || k.ID != "k1" {
		t.Fatalf("by hash: %+v err=%v", k, err)
	}

	// SetAccessPolicy replaces
	if err := x.SetAccessPolicy(ctx, s.ID, []string{"user", "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := x.SetAccessPolicy(ctx, s.ID, []string{"user"}); err != nil {
		t.Fatal(err)
	}
	roles, err := x.GetAccessPolicy(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0] != "user" {
		t.Fatalf("policy not replaced: %v", roles)
	}

	// GetAccessPolicy always returns non-nil slice
	rolesEmpty, err := x.GetAccessPolicy(ctx, "no-such-service")
	if err != nil {
		t.Fatal(err)
	}
	if rolesEmpty == nil {
		t.Fatal("GetAccessPolicy must return non-nil slice for unknown service")
	}
	if len(rolesEmpty) != 0 {
		t.Fatalf("want empty slice for unknown service, got %v", rolesEmpty)
	}

	// DeleteService cascades
	if err := x.DeleteService(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetServiceByID(ctx, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}

	// API key should cascade-delete too
	if _, err := x.GetServiceAPIKeyByHash(ctx, s.ID, "abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("api key should cascade after service delete, got %v", err)
	}
}

func TestServiceListAndLookup(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")
	mustUser(t, x, "u2")

	s1, _ := x.GetOrCreateService(ctx, "u1", "web", "http")
	s2, _ := x.GetOrCreateService(ctx, "u1", "api", "tcp")
	_, _ = x.GetOrCreateService(ctx, "u2", "web", "http")

	list, err := x.ListServicesByUser(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 services for u1, got %d", len(list))
	}

	all, err := x.ListAllServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 total services, got %d", len(all))
	}

	// GetServiceByID
	got, err := x.GetServiceByID(ctx, s1.ID)
	if err != nil || got.ID != s1.ID {
		t.Fatalf("GetServiceByID: %+v err=%v", got, err)
	}
	if _, err := x.GetServiceByID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetServiceByID missing: want ErrNotFound, got %v", err)
	}

	// SetServiceSubdomain ErrNotFound for unknown id
	if err := x.SetServiceSubdomain(ctx, "nope", "sub1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetServiceSubdomain missing: want ErrNotFound, got %v", err)
	}

	// SetServiceAccessMode ErrNotFound for unknown id
	if err := x.SetServiceAccessMode(ctx, "nope", "open", "Authorization"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetServiceAccessMode missing: want ErrNotFound, got %v", err)
	}

	// DeleteService ErrNotFound
	if err := x.DeleteService(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteService missing: want ErrNotFound, got %v", err)
	}

	_ = s2
}

func TestServiceAPIKeyOperations(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")

	s, _ := x.GetOrCreateService(ctx, "u1", "web", "http")

	// Create two keys
	_ = x.CreateServiceAPIKey(ctx, ServiceAPIKey{ID: "k1", ServiceID: s.ID, Name: "ci", KeyHash: "hash1"})
	_ = x.CreateServiceAPIKey(ctx, ServiceAPIKey{ID: "k2", ServiceID: s.ID, Name: "prod", KeyHash: "hash2"})

	// ListServiceAPIKeys
	keys, err := x.ListServiceAPIKeys(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(keys))
	}

	// TouchServiceAPIKey sets last_used
	if err := x.TouchServiceAPIKey(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	keys2, _ := x.ListServiceAPIKeys(ctx, s.ID)
	var found bool
	for _, k := range keys2 {
		if k.ID == "k1" {
			if k.LastUsed == nil {
				t.Fatal("TouchServiceAPIKey: LastUsed should be non-nil after touch")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("k1 not found in list after touch")
	}

	// GetServiceAPIKeyByHash missing → ErrNotFound
	if _, err := x.GetServiceAPIKeyByHash(ctx, s.ID, "nohash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetServiceAPIKeyByHash missing: want ErrNotFound, got %v", err)
	}

	// DeleteServiceAPIKey (scoped)
	if err := x.DeleteServiceAPIKey(ctx, "k1", s.ID); err != nil {
		t.Fatal(err)
	}
	keys3, _ := x.ListServiceAPIKeys(ctx, s.ID)
	if len(keys3) != 1 || keys3[0].ID != "k2" {
		t.Fatalf("after delete: %+v", keys3)
	}

	// DeleteServiceAPIKey ErrNotFound for wrong serviceID
	if err := x.DeleteServiceAPIKey(ctx, "k2", "wrong-service"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteServiceAPIKey wrong scope: want ErrNotFound, got %v", err)
	}

	// DeleteServiceAPIKey ErrNotFound for missing key
	if err := x.DeleteServiceAPIKey(ctx, "nope", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteServiceAPIKey missing: want ErrNotFound, got %v", err)
	}
}

func TestSetAccessPolicyEmptyIsDenyAll(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")

	s, _ := x.GetOrCreateService(ctx, "u1", "web", "http")

	// Set some roles then clear with empty slice
	_ = x.SetAccessPolicy(ctx, s.ID, []string{"admin", "user"})
	_ = x.SetAccessPolicy(ctx, s.ID, []string{})
	roles, err := x.GetAccessPolicy(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 0 {
		t.Fatalf("SetAccessPolicy empty: want 0 roles, got %v", roles)
	}
	if roles == nil {
		t.Fatal("GetAccessPolicy must return non-nil empty slice")
	}
}

func TestSetServiceSubdomainUniqueCollision(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "u1")

	s1, err := x.GetOrCreateService(ctx, "u1", "svc1", "http")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := x.GetOrCreateService(ctx, "u1", "svc2", "http")
	if err != nil {
		t.Fatal(err)
	}

	if err := x.SetServiceSubdomain(ctx, s1.ID, "collideme"); err != nil {
		t.Fatalf("first SetServiceSubdomain: %v", err)
	}
	err = x.SetServiceSubdomain(ctx, s2.ID, "collideme")
	if err == nil {
		t.Fatal("expected UNIQUE collision error, got nil")
	}
	// Load-bearing: a collision must NOT masquerade as ErrNotFound (Task 6 retry depends on this distinction).
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("collision must not surface as ErrNotFound, got: %v", err)
	}
	// Advisory: the wrapped driver error should mention the UNIQUE constraint so Task 6 can detect it.
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Logf("warning: collision error lacks 'UNIQUE'; Task 6 detection may need adjustment: %v", err)
	}
}

// TestCreateServiceAdminPreProvisioning exercises the v0.5.2 P3.6 admin
// pre-provisioning surface. The handler hands a fully-formed db.Service
// (with the operator-supplied service_id as the PK) to CreateService and
// expects a UNIQUE-constraint violation on the second insert to surface
// as the typed ErrDuplicateService sentinel.
func TestCreateServiceAdminPreProvisioning(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()
	mustUser(t, x, "admin1")

	s := Service{
		ID:           "svc_pre001",
		UserID:       "admin1",
		Name:         "Pre-provisioned",
		Type:         "http",
		AccessMode:   "open",
		APIKeyHeader: "Authorization",
	}
	if err := x.CreateService(ctx, s); err != nil {
		t.Fatalf("first CreateService: %v", err)
	}

	// Round-trip via GetServiceByID — the row should be visible exactly as inserted.
	got, err := x.GetServiceByID(ctx, "svc_pre001")
	if err != nil {
		t.Fatalf("GetServiceByID: %v", err)
	}
	if got.Name != "Pre-provisioned" || got.AccessMode != "open" || got.UserID != "admin1" {
		t.Errorf("unexpected stored row: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt was not populated")
	}

	// Second insert with the same ID must surface ErrDuplicateService.
	dupErr := x.CreateService(ctx, s)
	if !errors.Is(dupErr, ErrDuplicateService) {
		t.Fatalf("duplicate insert: got %v; want ErrDuplicateService", dupErr)
	}

	// A genuinely different ID under the same user (different name) must succeed.
	s2 := s
	s2.ID = "svc_pre002"
	s2.Name = "Another"
	if err := x.CreateService(ctx, s2); err != nil {
		t.Fatalf("distinct-ID insert: %v", err)
	}
}

func TestServiceGatewayOnly(t *testing.T) { checkServiceGatewayOnly(t, testDB(t), "u1") }

// checkServiceGatewayOnly: the gateway_only flag is written and read back by
// every service query. It runs against SQLite here and against a live
// Postgres in the postgres-tagged test.
func checkServiceGatewayOnly(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	sub := "gw-" + userID
	s, err := x.GetOrCreateService(ctx, userID, "svc", "http")
	if err == nil {
		err = x.SetServiceSubdomain(ctx, s.ID, sub)
	}
	if err != nil || s.GatewayOnly {
		t.Fatalf("default: %v %+v", err, s)
	}
	if err := x.SetServiceGatewayOnly(ctx, s.ID, true); err != nil {
		t.Fatal(err)
	}
	byID, _ := x.GetServiceByID(ctx, s.ID)
	if !byID.GatewayOnly {
		t.Fatal("GetServiceByID lost GatewayOnly")
	}
	bySub, err := x.GetServiceBySubdomain(ctx, sub)
	if err != nil || !bySub.GatewayOnly {
		t.Fatalf("GetServiceBySubdomain: %v %+v", err, bySub)
	}
	list, _ := x.ListServicesByUser(ctx, userID)
	if len(list) != 1 || !list[0].GatewayOnly {
		t.Fatalf("list by user: %+v", list)
	}
	all, _ := x.ListAllServices(ctx)
	found := false
	for _, a := range all {
		if a.ID == s.ID {
			found = a.GatewayOnly
		}
	}
	if !found {
		t.Fatalf("list all: %+v", all)
	}
	if err := x.SetServiceGatewayOnly(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	byID, _ = x.GetServiceByID(ctx, s.ID)
	if byID.GatewayOnly {
		t.Fatal("not cleared")
	}
	if err := x.SetServiceGatewayOnly(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

// freshServiceUser creates the user anew: deleting a user deletes its
// services. A database that outlives the test run (Postgres) keeps them.
func freshServiceUser(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
}

func TestServiceNeverSeen(t *testing.T) { checkServiceNeverSeen(t, testDB(t), "u-never-seen") }

func checkServiceNeverSeen(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	s, err := x.GetOrCreateService(ctx, userID, "web", "http")
	if err != nil {
		t.Fatal(err)
	}
	if s.LastSeen != nil || s.LastClient != "" {
		t.Fatalf("a new service: LastSeen = %v, LastClient = %q, want nil and \"\"", s.LastSeen, s.LastClient)
	}
	// A row inserted by the admin API reads the same.
	if err := x.CreateService(ctx, Service{ID: "svc-" + userID, UserID: userID, Name: "api", Type: "http", AccessMode: "open"}); err != nil {
		t.Fatal(err)
	}
	created, err := x.GetServiceByID(ctx, "svc-"+userID)
	if err != nil || created.LastSeen != nil || created.LastClient != "" {
		t.Fatalf("a created service: %v %+v", err, created)
	}
}

func TestTouchServiceLastSeen(t *testing.T) { checkTouchServiceLastSeen(t, testDB(t), "u-seen") }

func checkTouchServiceLastSeen(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	s, err := x.GetOrCreateService(ctx, userID, "web", "http")
	if err != nil {
		t.Fatal(err)
	}
	other, err := x.GetOrCreateService(ctx, userID, "other", "http")
	if err != nil {
		t.Fatal(err)
	}
	sub := "sub-" + userID
	if err := x.SetServiceSubdomain(ctx, s.ID, sub); err != nil {
		t.Fatal(err)
	}

	if err := x.TouchServiceLastSeen(ctx, s.ID, "laptop"); err != nil {
		t.Fatal(err)
	}
	seen := func(what string, got Service, err error, client string) {
		t.Helper()
		if err != nil || got.ID != s.ID || got.LastClient != client {
			t.Fatalf("%s: %v %+v, want LastClient %q", what, err, got, client)
		}
		if got.LastSeen == nil || time.Since(*got.LastSeen) > time.Second || time.Until(*got.LastSeen) > time.Second {
			t.Fatalf("%s: LastSeen = %v, want within a second of now", what, got.LastSeen)
		}
	}
	// Every read of a service holds the two columns.
	byID, err := x.GetServiceByID(ctx, s.ID)
	seen("by id", byID, err, "laptop")
	bySub, err := x.GetServiceBySubdomain(ctx, sub)
	seen("by subdomain", bySub, err, "laptop")
	again, err := x.GetOrCreateService(ctx, userID, "web", "http")
	seen("get or create", again, err, "laptop")
	mine, err := x.ListServicesByUser(ctx, userID)
	if err != nil || len(mine) != 2 {
		t.Fatalf("list by user: %v %+v", err, mine)
	}
	all, err := x.ListAllServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]Service{mine, all} {
		found := false
		for _, sv := range list {
			switch sv.ID {
			case s.ID:
				found = true
				seen("list", sv, nil, "laptop")
			case other.ID:
				// The service beside it was not touched.
				if sv.LastSeen != nil || sv.LastClient != "" {
					t.Fatalf("the other service was stamped: %+v", sv)
				}
			}
		}
		if !found {
			t.Fatalf("the service is missing from a list: %+v", list)
		}
	}

	// A second touch by another client replaces last_client and moves last_seen on.
	first := *byID.LastSeen
	time.Sleep(5 * time.Millisecond)
	if err := x.TouchServiceLastSeen(ctx, s.ID, "desktop"); err != nil {
		t.Fatal(err)
	}
	byID, err = x.GetServiceByID(ctx, s.ID)
	seen("after the second touch", byID, err, "desktop")
	if !byID.LastSeen.After(first) {
		t.Fatalf("LastSeen did not move on: %v then %v", first, byID.LastSeen)
	}
}

func TestTouchServiceLastSeenUnknown(t *testing.T) {
	checkTouchServiceLastSeenUnknown(t, testDB(t), "u-seen-unknown")
}

func checkTouchServiceLastSeenUnknown(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshServiceUser(t, x, userID)
	s, err := x.GetOrCreateService(ctx, userID, "web", "http")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.TouchServiceLastSeen(ctx, s.ID+"-missing", "laptop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown service err = %v, want ErrNotFound", err)
	}
	got, err := x.GetServiceByID(ctx, s.ID)
	if err != nil || got.LastSeen != nil || got.LastClient != "" {
		t.Fatalf("a touch of an unknown service must write nothing: %v %+v", err, got)
	}
}

// What migration 0027 adds, checked on both engines.
var migration0027Columns = []string{"last_seen", "last_client"}

func TestMigrate0027Schema(t *testing.T) {
	x := testDB(t)
	for _, c := range migration0027Columns {
		var n int
		if err := x.DB().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('services') WHERE name=?`, c).Scan(&n); err != nil || n != 1 {
			t.Errorf("column services.%s missing (n=%d, err=%v)", c, n, err)
		}
	}
}

// The SQLite and the Postgres file of migration 0027 hold the same
// statements; only the path in the first comment line and the type of
// last_seen differ.
func TestMigration0027_PairIsIdentical(t *testing.T) {
	body := func(name string) string {
		raw, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_, rest, _ := strings.Cut(string(raw), "\n")
		return rest
	}
	a := body("0027_v0.10.0_service_last_seen.sql")
	b := strings.Replace(body("0027_v0.10.0_service_last_seen.postgres.sql"), "TIMESTAMPTZ;", "TIMESTAMP;", 1)
	if a != b || !strings.Contains(a, "-- +goose Down") {
		t.Fatalf("the pair differs or has no Down block:\n%s\n---\n%s", a, b)
	}
}

func TestMigration0027_DownAndUp(t *testing.T) {
	checkMigration0027DownAndUp(t, testDB(t), "0027_v0.10.0_service_last_seen.sql", "u-mig27")
}

// checkMigration0027DownAndUp runs the Down block and the Up block again: Up
// only succeeds when Down removed both columns, and a service that was there
// before reads as never seen. Everything happens in one transaction that is
// rolled back, so a database shared with other tests never shows the schema
// without the columns.
func checkMigration0027DownAndUp(t *testing.T, x *DB, file, userID string) {
	t.Helper()
	ctx := context.Background()
	raw, err := migrationFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	_, down, found := strings.Cut(string(raw), "-- +goose Down")
	if !found || strings.TrimSpace(down) == "" {
		t.Fatalf("%s has no Down block", file)
	}
	freshServiceUser(t, x, userID)

	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO services(id, user_id, name, type) VALUES(?,?,?,?)`,
		"svc-"+userID, userID, "old", "http"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, upBlock(string(raw))); err != nil {
		t.Fatalf("up after down (a column the Down block left behind fails here): %v", err)
	}
	s, err := scanService(tx.QueryRowContext(ctx, `SELECT `+selectServiceCols+` FROM services WHERE id=?`, "svc-"+userID))
	if err != nil || s.LastSeen != nil || s.LastClient != "" {
		t.Fatalf("old service: %v %+v, want never seen", err, s)
	}
}
