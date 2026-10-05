package store

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/db"
)

// genTestCAPEM produces a self-signed CA PEM block useful only for store-layer
// PEM-parse validation (the cert chain is never actually used to verify).
func genTestCAPEM(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "burrow-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// mustCreateUser is a test helper that creates a user with the given email and
// role, fatally failing the test if creation fails.
func mustCreateUser(t *testing.T, st *Store, email, role string) db.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), email, "password1", role)
	if err != nil {
		t.Fatalf("mustCreateUser(%q, %q): %v", email, role, err)
	}
	return u
}

// mustGetOrCreateService is a test helper that calls GetOrCreateService on the
// underlying db layer via the store, fatally failing the test if it errors.
func mustGetOrCreateService(t *testing.T, st *Store, userID, name, typ string) db.Service {
	t.Helper()
	svc, err := st.q.GetOrCreateService(context.Background(), userID, name, typ)
	if err != nil {
		t.Fatalf("mustGetOrCreateService(%q, %q, %q): %v", userID, name, typ, err)
	}
	return svc
}

// TestServiceConfigPermissionGate verifies that SetServiceAccessMode enforces
// the services:configure:own and services:configure:any permission gates,
// and that non-open modes on a tcp service return ErrServiceNotHTTP.
func TestServiceConfigPermissionGate(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	other := mustCreateUser(t, st, "other@x", "user")
	admin := mustCreateUser(t, st, "admin@x", "admin")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	// owner (services:configure:own) may set mode
	if err := st.SetServiceAccessMode(ctx, owner.ID, "user", svc.ID, "api_key", "", nil); err != nil {
		t.Fatalf("owner should be allowed: %v", err)
	}
	// a different non-admin user → forbidden
	err := st.SetServiceAccessMode(ctx, other.ID, "user", svc.ID, "open", "", nil)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	// admin (services:configure:any) may configure anyone's
	if err := st.SetServiceAccessMode(ctx, admin.ID, "admin", svc.ID, "open", "", nil); err != nil {
		t.Fatalf("admin should be allowed: %v", err)
	}
	// non-open on a tcp service → ErrServiceNotHTTP
	tcp := mustGetOrCreateService(t, st, owner.ID, "db", "tcp")
	if err := st.SetServiceAccessMode(ctx, owner.ID, "user", tcp.ID, "api_key", "", nil); !errors.Is(err, ErrServiceNotHTTP) {
		t.Fatalf("want ErrServiceNotHTTP, got %v", err)
	}
}

// TestSetServiceAccessModeInvalidMode verifies ErrInvalidAccessMode for bad mode values.
func TestSetServiceAccessModeInvalidMode(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	err := st.SetServiceAccessMode(ctx, owner.ID, "user", svc.ID, "invalid_mode", "", nil)
	if !errors.Is(err, ErrInvalidAccessMode) {
		t.Fatalf("want ErrInvalidAccessMode, got %v", err)
	}
}

// TestSetServiceAccessModeNotFound verifies db.ErrNotFound propagates for unknown service.
func TestSetServiceAccessModeNotFound(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	admin := mustCreateUser(t, st, "admin@x", "admin")

	err := st.SetServiceAccessMode(ctx, admin.ID, "admin", "does-not-exist", "open", "", nil)
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("want db.ErrNotFound, got %v", err)
	}
}

func TestServiceAccessModeNotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.ServiceAccessMode(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("want an error for an unknown service id")
	}
}

// TestSetServiceAccessModeHeaderDefault verifies that an empty header defaults
// to "Authorization" for api_key mode.
func TestSetServiceAccessModeHeaderDefault(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	if err := st.SetServiceAccessMode(ctx, owner.ID, "user", svc.ID, "api_key", "", nil); err != nil {
		t.Fatalf("SetServiceAccessMode: %v", err)
	}
	// The db row should have "Authorization" stored.
	updated, err := st.q.GetServiceByID(ctx, svc.ID)
	if err != nil {
		t.Fatalf("GetServiceByID: %v", err)
	}
	if updated.APIKeyHeader != "Authorization" {
		t.Fatalf("want APIKeyHeader=Authorization, got %q", updated.APIKeyHeader)
	}
}

// TestSetServiceAccessModeMTLS verifies that the mtls path requires a
// non-empty + parseable CA PEM and that the CA is persisted via
// db.SetServiceMTLSCAPEM.
func TestSetServiceAccessModeMTLS(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	// 1. Empty CA PEM → ErrMTLSCARequired.
	err := st.SetServiceAccessMode(ctx, owner.ID, "user", svc.ID, "mtls", "", nil)
	if !errors.Is(err, ErrMTLSCARequired) {
		t.Fatalf("want ErrMTLSCARequired, got %v", err)
	}

	// 2. Junk PEM → ErrInvalidMTLSCAPEM.
	err = st.SetServiceAccessMode(ctx, owner.ID, "user", svc.ID, "mtls", "", []byte("not a pem"))
	if !errors.Is(err, ErrInvalidMTLSCAPEM) {
		t.Fatalf("want ErrInvalidMTLSCAPEM, got %v", err)
	}

	// 3. Valid CA PEM → success + CA persisted.
	caPEM := genTestCAPEM(t)
	if err := st.SetServiceAccessMode(ctx, owner.ID, "user", svc.ID, "mtls", "", caPEM); err != nil {
		t.Fatalf("mtls happy-path PEM did not parse: %v", err)
	}
	gotPEM, err := st.q.GetServiceMTLSCAPEM(ctx, svc.ID)
	if err != nil {
		t.Fatalf("GetServiceMTLSCAPEM: %v", err)
	}
	if !strings.Contains(gotPEM, "BEGIN CERTIFICATE") {
		t.Errorf("CA PEM not persisted: got %q", gotPEM)
	}
}

// TestCreateAPIKey verifies plaintext has buk_ prefix, only sha256 is stored,
// and plaintext can validate.
func TestCreateAPIKey(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	id, plaintext, err := st.CreateAPIKey(ctx, owner.ID, "user", svc.ID, "my-key")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if id == "" {
		t.Fatal("CreateAPIKey: id must not be empty")
	}
	if !strings.HasPrefix(plaintext, "buk_") {
		t.Fatalf("plaintext must have buk_ prefix, got %q", plaintext)
	}

	// Validate the returned plaintext works.
	keyID, ok, err := st.ValidateAPIKey(ctx, svc.ID, plaintext)
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if !ok {
		t.Fatal("ValidateAPIKey: freshly created key must validate")
	}
	if keyID != id {
		t.Fatalf("ValidateAPIKey: key id = %q, want %q", keyID, id)
	}

	// Wrong key must not validate.
	keyID, ok, err = st.ValidateAPIKey(ctx, svc.ID, "buk_bogus")
	if err != nil {
		t.Fatalf("ValidateAPIKey bogus: %v", err)
	}
	if ok || keyID != "" {
		t.Fatalf("ValidateAPIKey: bogus key must not validate (ok=%v id=%q)", ok, keyID)
	}
}

// TestCreateAPIKeyEmptyNameReturnsError verifies ErrNameRequired on empty name.
func TestCreateAPIKeyEmptyNameReturnsError(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	_, _, err := st.CreateAPIKey(ctx, owner.ID, "user", svc.ID, "")
	if !errors.Is(err, ErrNameRequired) {
		t.Fatalf("want ErrNameRequired, got %v", err)
	}
}

// TestCreateAPIKeyForbidden verifies that a non-owner, non-admin cannot create keys.
func TestCreateAPIKeyForbidden(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	other := mustCreateUser(t, st, "other@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	_, _, err := st.CreateAPIKey(ctx, other.ID, "user", svc.ID, "steal")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

// TestListAndDeleteAPIKey verifies the gated list/delete flow.
func TestListAndDeleteAPIKey(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	id1, _, err := st.CreateAPIKey(ctx, owner.ID, "user", svc.ID, "key1")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	_, _, err = st.CreateAPIKey(ctx, owner.ID, "user", svc.ID, "key2")
	if err != nil {
		t.Fatalf("CreateAPIKey 2: %v", err)
	}

	keys, err := st.ListAPIKeys(ctx, owner.ID, "user", svc.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(keys))
	}

	if err := st.DeleteAPIKey(ctx, owner.ID, "user", svc.ID, id1); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	keys, _ = st.ListAPIKeys(ctx, owner.ID, "user", svc.ID)
	if len(keys) != 1 {
		t.Fatalf("after delete: want 1 key, got %d", len(keys))
	}

	// delete non-existent → db.ErrNotFound
	err = st.DeleteAPIKey(ctx, owner.ID, "user", svc.ID, "does-not-exist")
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("delete non-existent: want db.ErrNotFound, got %v", err)
	}
}

// TestListAPIKeysForbidden verifies non-owner non-admin cannot list.
func TestListAPIKeysForbidden(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	other := mustCreateUser(t, st, "other@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	_, err := st.ListAPIKeys(ctx, other.ID, "user", svc.ID)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

// TestAccessPolicy verifies GetAccessPolicy and SetAccessPolicy, including
// unknown role rejection.
func TestAccessPolicy(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	// Initially empty.
	policy, err := st.GetAccessPolicy(ctx, owner.ID, "user", svc.ID)
	if err != nil {
		t.Fatalf("GetAccessPolicy initial: %v", err)
	}
	if len(policy) != 0 {
		t.Fatalf("initial policy must be empty, got %v", policy)
	}

	// Set valid roles.
	if err := st.SetAccessPolicy(ctx, owner.ID, "user", svc.ID, []string{"user"}); err != nil {
		t.Fatalf("SetAccessPolicy: %v", err)
	}
	policy, err = st.GetAccessPolicy(ctx, owner.ID, "user", svc.ID)
	if err != nil {
		t.Fatalf("GetAccessPolicy after set: %v", err)
	}
	if len(policy) != 1 || policy[0] != "user" {
		t.Fatalf("policy after set: want [user], got %v", policy)
	}

	// Unknown role → ErrUnknownRole.
	if err := st.SetAccessPolicy(ctx, owner.ID, "user", svc.ID, []string{"user", "superadmin"}); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("want ErrUnknownRole, got %v", err)
	}

	// SetAccessPolicy forbidden for other user.
	other := mustCreateUser(t, st, "other@x", "user")
	if err := st.SetAccessPolicy(ctx, other.ID, "user", svc.ID, []string{"user"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

// TestRoleAllowed verifies the hot-path RoleAllowed helper (no permission gate).
func TestRoleAllowed(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	// Empty policy → deny all.
	ok, err := st.RoleAllowed(ctx, svc.ID, "user")
	if err != nil || ok {
		t.Fatalf("empty policy must deny: ok=%v err=%v", ok, err)
	}

	// Set policy to [user].
	if err := st.q.SetAccessPolicy(ctx, svc.ID, []string{"user"}); err != nil {
		t.Fatalf("SetAccessPolicy direct: %v", err)
	}
	ok, err = st.RoleAllowed(ctx, svc.ID, "user")
	if err != nil || !ok {
		t.Fatalf("user in policy must be allowed: ok=%v err=%v", ok, err)
	}
	ok, err = st.RoleAllowed(ctx, svc.ID, "admin")
	if err != nil || ok {
		t.Fatalf("admin not in policy must be denied: ok=%v err=%v", ok, err)
	}
}

// TestServiceForSubdomain verifies the hot-path subdomain lookup.
func TestServiceForSubdomain(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	// Set subdomain directly via db.
	if err := st.q.SetServiceSubdomain(ctx, svc.ID, "myapp"); err != nil {
		t.Fatalf("SetServiceSubdomain: %v", err)
	}

	found, err := st.ServiceForSubdomain(ctx, "myapp")
	if err != nil {
		t.Fatalf("ServiceForSubdomain: %v", err)
	}
	if found.ID != svc.ID {
		t.Fatalf("ServiceForSubdomain: want ID=%s, got %s", svc.ID, found.ID)
	}

	// Missing subdomain → db.ErrNotFound.
	_, err = st.ServiceForSubdomain(ctx, "missing")
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("missing subdomain: want db.ErrNotFound, got %v", err)
	}
}

// TestListServicesScoping verifies that a regular user only sees their own
// services while an admin sees all.
func TestListServicesScoping(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	u1 := mustCreateUser(t, st, "u1@x", "user")
	u2 := mustCreateUser(t, st, "u2@x", "user")
	admin := mustCreateUser(t, st, "admin@x", "admin")

	mustGetOrCreateService(t, st, u1.ID, "svc1", "http")
	mustGetOrCreateService(t, st, u2.ID, "svc2", "http")

	// u1 sees only their own service.
	svcs, err := st.ListServices(ctx, u1.ID, "user")
	if err != nil {
		t.Fatalf("ListServices u1: %v", err)
	}
	if len(svcs) != 1 {
		t.Fatalf("u1 should see 1 service, got %d", len(svcs))
	}
	if svcs[0].ID == "" {
		t.Fatal("ServiceView ID must be populated")
	}

	// admin sees all services.
	svcs, err = st.ListServices(ctx, admin.ID, "admin")
	if err != nil {
		t.Fatalf("ListServices admin: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("admin should see 2 services, got %d", len(svcs))
	}
}

// TestGetService verifies ServiceDetail is returned with api_key_count and
// access_policy populated.
func TestGetService(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svc := mustGetOrCreateService(t, st, owner.ID, "web", "http")

	_, _, err := st.CreateAPIKey(ctx, owner.ID, "user", svc.ID, "k1")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := st.q.SetAccessPolicy(ctx, svc.ID, []string{"user", "admin"}); err != nil {
		t.Fatalf("SetAccessPolicy direct: %v", err)
	}

	detail, err := st.GetService(ctx, owner.ID, "user", svc.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if detail.ID != svc.ID {
		t.Fatalf("GetService: want ID=%s, got %s", svc.ID, detail.ID)
	}
	if detail.APIKeyCount != 1 {
		t.Fatalf("GetService: want APIKeyCount=1, got %d", detail.APIKeyCount)
	}
	if len(detail.AccessPolicy) != 2 {
		t.Fatalf("GetService: want 2 access policy roles, got %d", len(detail.AccessPolicy))
	}

	// other user → forbidden
	other := mustCreateUser(t, st, "other@x", "user")
	_, err = st.GetService(ctx, other.ID, "user", svc.ID)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetService other: want ErrForbidden, got %v", err)
	}

	// not found → db.ErrNotFound
	_, err = st.GetService(ctx, owner.ID, "user", "does-not-exist")
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("GetService missing: want db.ErrNotFound, got %v", err)
	}
}

// TestValidateAPIKeyWrongService verifies that a key valid for service A does
// not validate for service B (scoped lookup).
func TestValidateAPIKeyWrongService(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	svcA := mustGetOrCreateService(t, st, owner.ID, "svcA", "http")
	svcB := mustGetOrCreateService(t, st, owner.ID, "svcB", "http")

	_, pt, err := st.CreateAPIKey(ctx, owner.ID, "user", svcA.ID, "k1")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	keyID, ok, err := st.ValidateAPIKey(ctx, svcB.ID, pt)
	if err != nil || ok || keyID != "" {
		t.Fatalf("key for svcA must not validate against svcB: id=%q ok=%v err=%v", keyID, ok, err)
	}
}

func TestSetServiceSlug(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, s, "owner@x", "user")
	svcID := mustGetOrCreateService(t, s, owner.ID, "web", "http").ID

	old, err := s.SetServiceSlug(ctx, owner.ID, "user", svcID, "my-app")
	if err != nil {
		t.Fatalf("SetServiceSlug: %v", err)
	}
	if old != "" {
		t.Fatalf("old slug = %q, want empty", old)
	}
	got, err := s.ServiceForSubdomain(ctx, "my-app")
	if err != nil || got.ID != svcID {
		t.Fatalf("lookup by new slug: %v %+v", err, got)
	}
	if old, err = s.SetServiceSlug(ctx, owner.ID, "user", svcID, "my-app2"); err != nil || old != "my-app" {
		t.Fatalf("second change: old=%q err=%v, want old=my-app", old, err)
	}

	if _, err := s.SetServiceSlug(ctx, owner.ID, "user", svcID, "Bad_Slug"); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("invalid slug err = %v, want ErrInvalidSlug", err)
	}
	if _, err := s.SetServiceSlug(ctx, "someone-else", "user", svcID, "other"); err == nil {
		t.Fatal("a non-owner, non-admin caller changed the slug")
	}
}

func TestSetServiceSlug_Taken(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, s, "owner@x", "user")
	a := mustGetOrCreateService(t, s, owner.ID, "web", "http").ID
	b := mustGetOrCreateService(t, s, owner.ID, "api", "http").ID
	if _, err := s.SetServiceSlug(ctx, owner.ID, "user", a, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetServiceSlug(ctx, owner.ID, "user", b, "shared"); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("err = %v, want ErrSlugTaken", err)
	}
}

func TestSuggestSlug_IsValidAndFree(t *testing.T) {
	s := newStore(t)
	slug, err := s.SuggestSlug(context.Background())
	if err != nil || !auth.ValidSlug(slug) {
		t.Fatalf("SuggestSlug = %q, %v", slug, err)
	}
	if _, err := s.ServiceForSubdomain(context.Background(), slug); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("suggested slug is already in use: %v", err)
	}
}
