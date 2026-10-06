package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// ---------------------------------------------------------------------------
// Fake implementations of ServiceStore and LiveTunnelLookup
// ---------------------------------------------------------------------------

// fakeServiceStore is the in-process stub for ServiceStore. Fields prefixed
// with "ret" are the values returned; fields prefixed with "err" are errors.
type fakeServiceStore struct {
	// ListServices
	listSvcs    []store.ServiceView
	listSvcsErr error

	// GetService
	getSvc    store.ServiceDetail
	getSvcErr error

	// SetServiceAccessMode
	setModeErr error

	// ListAPIKeys
	listKeys    []db.ServiceAPIKey
	listKeysErr error

	// CreateAPIKey
	createKeyID  string
	createKeyPT  string
	createKeyErr error

	// DeleteAPIKey
	deleteKeyErr error

	// GetAccessPolicy
	getPolicy    []string
	getPolicyErr error

	// SetAccessPolicy
	setPolicyErr error

	// CreateService (v0.5.2 P3.6)
	createSvcErr error
	createdSvcs  []db.Service

	// SetServiceGatewayOnly
	gatewayOnlyErr   error
	gatewayOnlyCalls []gatewayOnlyCall

	// SetServiceSlug / SuggestSlug
	setSlugOld         string
	setSlugGatewayOnly bool
	setSlugErr         error
	lastSlug           string
	suggestSlug        string
	suggestErr         error

	// last args captured for inspection
	lastMode   string
	lastHeader string
	lastCAPEM  []byte
	lastRoles  []string
}

func (f *fakeServiceStore) ListServices(_ context.Context, _, _ string) ([]store.ServiceView, error) {
	return f.listSvcs, f.listSvcsErr
}
func (f *fakeServiceStore) GetService(_ context.Context, _, _, _ string) (store.ServiceDetail, error) {
	return f.getSvc, f.getSvcErr
}
func (f *fakeServiceStore) SetServiceAccessMode(_ context.Context, _, _, _, mode, header string, caPEM []byte) error {
	f.lastMode = mode
	f.lastHeader = header
	f.lastCAPEM = append([]byte(nil), caPEM...)
	return f.setModeErr
}
func (f *fakeServiceStore) ListAPIKeys(_ context.Context, _, _, _ string) ([]db.ServiceAPIKey, error) {
	return f.listKeys, f.listKeysErr
}
func (f *fakeServiceStore) CreateAPIKey(_ context.Context, _, _, _, _ string) (string, string, error) {
	return f.createKeyID, f.createKeyPT, f.createKeyErr
}
func (f *fakeServiceStore) DeleteAPIKey(_ context.Context, _, _, _, _ string) error {
	return f.deleteKeyErr
}
func (f *fakeServiceStore) GetAccessPolicy(_ context.Context, _, _, _ string) ([]string, error) {
	return f.getPolicy, f.getPolicyErr
}
func (f *fakeServiceStore) SetAccessPolicy(_ context.Context, _, _, _ string, roles []string) error {
	f.lastRoles = roles
	return f.setPolicyErr
}

func (f *fakeServiceStore) SetServiceSlug(_ context.Context, _, _, _, slug string) (string, bool, error) {
	f.lastSlug = slug
	return f.setSlugOld, f.setSlugGatewayOnly, f.setSlugErr
}

type gatewayOnlyCall struct {
	callerID, role, serviceID string
	on                        bool
}

func (f *fakeServiceStore) SetServiceGatewayOnly(_ context.Context, callerID, role, serviceID string, on bool) error {
	f.gatewayOnlyCalls = append(f.gatewayOnlyCalls, gatewayOnlyCall{callerID, role, serviceID, on})
	return f.gatewayOnlyErr
}
func (f *fakeServiceStore) SuggestSlug(_ context.Context) (string, error) {
	return f.suggestSlug, f.suggestErr
}

// CreateService is the v0.5.2 admin pre-provisioning surface (P3.6).
// Records calls in createdSvcs so tests can assert what was inserted; on the
// SECOND call with the same ID, returns db.ErrDuplicateService so the 409
// test can simulate a duplicate without running the real DB.
func (f *fakeServiceStore) CreateService(_ context.Context, s db.Service) error {
	if f.createSvcErr != nil {
		return f.createSvcErr
	}
	for _, prior := range f.createdSvcs {
		if prior.ID == s.ID {
			return db.ErrDuplicateService
		}
	}
	f.createdSvcs = append(f.createdSvcs, s)
	return nil
}

// fakeLiveTunnels satisfies LiveTunnelLookup.
type fakeLiveTunnels struct {
	svcID      string
	localAddr  string
	connected  bool
	remotePort int
	exists     bool

	// tunnelID → locator mapping for LookupByTunnelID (Fix 3).
	tunnelID  string
	tunnelLoc TunnelLocator
}

func (f fakeLiveTunnels) LookupByServiceID(serviceID string) (LiveTunnelSnapshot, bool) {
	if !f.exists || serviceID != f.svcID {
		return LiveTunnelSnapshot{}, false
	}
	return LiveTunnelSnapshot{LocalAddr: f.localAddr, Connected: f.connected, RemotePort: f.remotePort}, true
}

func (f fakeLiveTunnels) LookupByTunnelID(tunnelID string) (TunnelLocator, bool) {
	if f.tunnelID == "" || tunnelID != f.tunnelID {
		return TunnelLocator{}, false
	}
	return f.tunnelLoc, true
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newServiceDeps builds a Deps for service-handler tests.
func newServiceDeps(ss *fakeServiceStore, lt LiveTunnelLookup, authDomain string) Deps {
	return Deps{
		Users:       &fakeUserStore{role: "admin"},
		Services:    ss,
		LiveTunnels: lt,
		AuthDomain:  authDomain,
		Log:         discardLog(),
	}
}

// newServiceServer builds a live httptest.Server with service deps and returns
// a logged-in authClient ready to call service endpoints.
func newServiceServer(t *testing.T, d Deps) (*httptest.Server, *authClient) {
	t.Helper()
	srv := httptest.NewServer(NewRouter(d))
	c := authedClient(t, srv)
	return srv, c
}

// ---------------------------------------------------------------------------
// GET /api/v1/services
// ---------------------------------------------------------------------------

func TestListServices_Empty(t *testing.T) {
	ss := &fakeServiceStore{listSvcs: []store.ServiceView{}}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []serviceResp
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if len(out) != 0 {
		t.Fatalf("want empty array, got %v", out)
	}
}

func TestListServices_WithItems(t *testing.T) {
	ss := &fakeServiceStore{
		listSvcs: []store.ServiceView{
			{ID: "s1", Name: "web", Type: "http", Subdomain: "k7p2qx",
				AccessMode: "open", APIKeyHeader: "Authorization"},
		},
	}
	lt := fakeLiveTunnels{svcID: "s1", localAddr: "127.0.0.1:3000", connected: true, exists: true}
	srv, c := newServiceServer(t, newServiceDeps(ss, lt, "tunnels.example.com"))
	defer srv.Close()

	r := c.get(t, "/api/v1/services")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []serviceResp
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if len(out) != 1 {
		t.Fatalf("want 1 service, got %d", len(out))
	}
	svc := out[0]
	if svc.ID != "s1" {
		t.Errorf("id: got %q want s1", svc.ID)
	}
	if svc.URL != "https://tunnels.example.com/svc/k7p2qx/" {
		t.Errorf("url: got %q want https://tunnels.example.com/svc/k7p2qx/", svc.URL)
	}
	if !svc.Connected {
		t.Error("want connected=true")
	}
	if svc.LocalAddr != "127.0.0.1:3000" {
		t.Errorf("local_addr: got %q want 127.0.0.1:3000", svc.LocalAddr)
	}
}

func TestListServices_NoAuthDomain_URLEmpty(t *testing.T) {
	ss := &fakeServiceStore{
		listSvcs: []store.ServiceView{
			{ID: "s1", Name: "web", Type: "http", Subdomain: "abc"},
		},
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", r.StatusCode)
	}
	var out []serviceResp
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if len(out) != 1 || out[0].URL != "" {
		t.Fatalf("want url empty when no auth_domain, got %q", out[0].URL)
	}
}

func TestListServices_Unauthenticated(t *testing.T) {
	ss := &fakeServiceStore{}
	d := newServiceDeps(ss, fakeLiveTunnels{}, "")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/services")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestListServices_StoreError(t *testing.T) {
	ss := &fakeServiceStore{listSvcsErr: errFake}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services")
	r.Body.Close()
	if r.StatusCode != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/services/{serviceID}
// ---------------------------------------------------------------------------

func TestGetService_Found(t *testing.T) {
	ss := &fakeServiceStore{
		getSvc: store.ServiceDetail{
			ServiceView: store.ServiceView{
				ID: "s1", Name: "web", Type: "http", Subdomain: "abc",
				AccessMode: "open", APIKeyHeader: "Authorization",
			},
			APIKeyCount:  2,
			AccessPolicy: []string{"user"},
		},
	}
	lt := fakeLiveTunnels{svcID: "s1", localAddr: "127.0.0.1:8080", connected: true, exists: true}
	srv, c := newServiceServer(t, newServiceDeps(ss, lt, "example.com"))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/s1")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out serviceDetailResp
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if out.ID != "s1" {
		t.Errorf("id: %q", out.ID)
	}
	if out.APIKeyCount != 2 {
		t.Errorf("api_key_count: %d", out.APIKeyCount)
	}
	if len(out.AccessPolicy) != 1 || out.AccessPolicy[0] != "user" {
		t.Errorf("access_policy: %v", out.AccessPolicy)
	}
	if out.URL != "https://example.com/svc/abc/" {
		t.Errorf("url: %q", out.URL)
	}
	if !out.Connected {
		t.Error("want connected=true")
	}
}

func TestGetService_NotFound(t *testing.T) {
	ss := &fakeServiceStore{getSvcErr: db.ErrNotFound}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/nope")
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", r.StatusCode)
	}
}

func TestGetService_Forbidden(t *testing.T) {
	ss := &fakeServiceStore{getSvcErr: store.ErrForbidden}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/s1")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// PUT /api/v1/services/{serviceID}/access-mode
// ---------------------------------------------------------------------------

func TestSetServiceAccessMode_Open(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode",
		map[string]string{"access_mode": "open"})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", r.StatusCode)
	}
	if ss.lastMode != "open" {
		t.Errorf("mode not applied: %q", ss.lastMode)
	}
}

func TestSetServiceAccessMode_APIKey_WithHeader(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode",
		map[string]string{"access_mode": "api_key", "api_key_header": "X-Api-Key"})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", r.StatusCode)
	}
	if ss.lastMode != "api_key" || ss.lastHeader != "X-Api-Key" {
		t.Errorf("mode=%q header=%q", ss.lastMode, ss.lastHeader)
	}
}

func TestSetServiceAccessMode_BurrowLogin_NoAuthDomain_409(t *testing.T) {
	ss := &fakeServiceStore{}
	// AuthDomain is empty — must reject burrow_login with 409 before calling store.
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode",
		map[string]string{"access_mode": "burrow_login"})
	body := readBody(t, r)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", r.StatusCode, body)
	}
	if !strings.Contains(body, "auth_domain") {
		t.Errorf("error must mention auth_domain, got: %s", body)
	}
	// Confirm store was NOT called.
	if ss.lastMode != "" {
		t.Errorf("store should not have been called, lastMode=%q", ss.lastMode)
	}
}

func TestSetServiceAccessMode_BurrowLogin_WithAuthDomain_204(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "example.com"))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode",
		map[string]string{"access_mode": "burrow_login"})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", r.StatusCode)
	}
}

func TestSetAccessMode_MTLSRefusedWithoutHostRouting(t *testing.T) {
	ss := &fakeServiceStore{}
	d := newServiceDeps(ss, fakeLiveTunnels{}, "example.com")
	d.HostRouting = false
	srv, c := newServiceServer(t, d)
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode", map[string]string{
		"access_mode": "mtls",
		"mtls_ca_pem": "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n",
	})
	body := readBody(t, r)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", r.StatusCode, body)
	}
	var obj map[string]string
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("body not JSON: %q", body)
	}
	if obj["error"] != "mTLS needs host routing, which is disabled on this relay" {
		t.Errorf("error = %q", obj["error"])
	}
	// Confirm store was NOT called.
	if ss.lastMode != "" {
		t.Errorf("store should not have been called, lastMode=%q", ss.lastMode)
	}
}

func TestSetAccessMode_MTLSAllowedWithHostRouting(t *testing.T) {
	ss := &fakeServiceStore{}
	d := newServiceDeps(ss, fakeLiveTunnels{}, "example.com")
	d.HostRouting = true
	srv, c := newServiceServer(t, d)
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode", map[string]string{
		"access_mode": "mtls",
		"mtls_ca_pem": "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n",
	})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", r.StatusCode)
	}
	if ss.lastMode != "mtls" || len(ss.lastCAPEM) == 0 {
		t.Errorf("store not called with mtls + CA: mode=%q ca=%d bytes", ss.lastMode, len(ss.lastCAPEM))
	}
}

// TestSetAccessMode_Legacy_MTLSRefusedWithoutHostRouting covers the same guard
// on the back-compat PUT /tunnels/{id}/access-mode route.
func TestSetAccessMode_Legacy_MTLSRefusedWithoutHostRouting(t *testing.T) {
	ss := &fakeServiceStore{}
	lt := fakeLiveTunnels{
		tunnelID:  "tn-live",
		tunnelLoc: TunnelLocator{ServiceID: "svc-abc", UserID: "u-self"},
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, lt, "example.com"))
	defer srv.Close()

	r := c.put(t, "/api/v1/tunnels/tn-live/access-mode", map[string]string{"access_mode": "mtls"})
	body := readBody(t, r)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", r.StatusCode, body)
	}
	if ss.lastMode != "" {
		t.Errorf("store should not have been called, lastMode=%q", ss.lastMode)
	}
}

func TestSetServiceAccessMode_InvalidMode_400(t *testing.T) {
	ss := &fakeServiceStore{setModeErr: store.ErrInvalidAccessMode}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode",
		map[string]string{"access_mode": "bad"})
	body := readBody(t, r)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", r.StatusCode, body)
	}
}

func TestSetServiceAccessMode_TCPService_409(t *testing.T) {
	ss := &fakeServiceStore{setModeErr: store.ErrServiceNotHTTP}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode",
		map[string]string{"access_mode": "api_key"})
	body := readBody(t, r)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", r.StatusCode, body)
	}
}

func TestSetServiceAccessMode_Forbidden(t *testing.T) {
	ss := &fakeServiceStore{setModeErr: store.ErrForbidden}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode",
		map[string]string{"access_mode": "open"})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
}

func TestSetServiceAccessMode_NotFound_404(t *testing.T) {
	ss := &fakeServiceStore{setModeErr: db.ErrNotFound}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/nope/access-mode",
		map[string]string{"access_mode": "open"})
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", r.StatusCode)
	}
}

func TestSetServiceAccessMode_MissingBody_400(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-mode",
		map[string]string{"access_mode": ""})
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for empty access_mode, got %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/services/{serviceID}/api-keys
// ---------------------------------------------------------------------------

func TestListAPIKeys_OK(t *testing.T) {
	now := time.Now().UTC()
	ss := &fakeServiceStore{
		listKeys: []db.ServiceAPIKey{
			{ID: "k1", ServiceID: "s1", Name: "prod", CreatedAt: now},
			{ID: "k2", ServiceID: "s1", Name: "dev", KeyHash: "shouldnotappear", CreatedAt: now},
		},
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/s1/api-keys")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if strings.Contains(body, "shouldnotappear") {
		t.Fatal("api-key hash must not appear in list response")
	}
	if strings.Contains(body, "key_hash") {
		t.Fatal("key_hash field must not be present")
	}
	var keys []apiKeyResp
	if err := json.Unmarshal([]byte(body), &keys); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(keys))
	}
	if keys[0].ID != "k1" || keys[0].Name != "prod" {
		t.Errorf("first key: %+v", keys[0])
	}
}

func TestListAPIKeys_Forbidden(t *testing.T) {
	ss := &fakeServiceStore{listKeysErr: store.ErrForbidden}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/s1/api-keys")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
}

func TestListAPIKeys_ServiceNotFound(t *testing.T) {
	ss := &fakeServiceStore{listKeysErr: db.ErrNotFound}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/nope/api-keys")
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/services/{serviceID}/api-keys
// ---------------------------------------------------------------------------

func TestCreateAPIKey_Success(t *testing.T) {
	ss := &fakeServiceStore{
		createKeyID: "k-new",
		createKeyPT: "buk_plaintext",
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services/s1/api-keys",
		map[string]string{"name": "ci-key"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out createAPIKeyResp
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if out.ID != "k-new" {
		t.Errorf("id: %q", out.ID)
	}
	if out.Key != "buk_plaintext" {
		t.Errorf("key: %q (must be plaintext once)", out.Key)
	}
}

func TestCreateAPIKey_EmptyName_400(t *testing.T) {
	ss := &fakeServiceStore{createKeyErr: store.ErrNameRequired}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services/s1/api-keys",
		map[string]string{"name": ""})
	body := readBody(t, r)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", r.StatusCode, body)
	}
	if !strings.Contains(body, "name is required") {
		t.Errorf("expected 'name is required' in body, got: %s", body)
	}
}

func TestCreateAPIKey_Forbidden(t *testing.T) {
	ss := &fakeServiceStore{createKeyErr: store.ErrForbidden}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services/s1/api-keys",
		map[string]string{"name": "x"})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
}

func TestCreateAPIKey_ServiceNotFound(t *testing.T) {
	ss := &fakeServiceStore{createKeyErr: db.ErrNotFound}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services/nope/api-keys",
		map[string]string{"name": "x"})
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", r.StatusCode)
	}
}

// TestCreateAPIKey_PlaintextOnce verifies the POST response carries the
// plaintext key. There is no GET-by-id to fetch it again (plaintext shown once).
func TestCreateAPIKey_PlaintextOnce(t *testing.T) {
	ss := &fakeServiceStore{
		createKeyID: "k1",
		createKeyPT: "buk_abc",
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services/s1/api-keys",
		map[string]string{"name": "once"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d", r.StatusCode)
	}
	var out createAPIKeyResp
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if out.Key == "" {
		t.Fatal("POST /api-keys must return plaintext key once")
	}
	// The list endpoint must never expose plaintext/hash.
	ss.listKeys = []db.ServiceAPIKey{{ID: "k1", Name: "once", KeyHash: "hashed", CreatedAt: time.Now()}}
	r2 := c.get(t, "/api/v1/services/s1/api-keys")
	body := readBody(t, r2)
	if strings.Contains(body, "buk_abc") || strings.Contains(body, "hashed") {
		t.Fatalf("list must not expose plaintext or hash, body: %s", body)
	}
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/services/{serviceID}/api-keys/{id}
// ---------------------------------------------------------------------------

func TestDeleteAPIKey_OK(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.delete(t, "/api/v1/services/s1/api-keys/k1")
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", r.StatusCode)
	}
}

func TestDeleteAPIKey_NotFound(t *testing.T) {
	ss := &fakeServiceStore{deleteKeyErr: db.ErrNotFound}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.delete(t, "/api/v1/services/s1/api-keys/missing")
	body := readBody(t, r)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", r.StatusCode, body)
	}
	if !strings.Contains(body, "api key not found") {
		t.Errorf("want 'api key not found', got: %s", body)
	}
}

func TestDeleteAPIKey_Forbidden(t *testing.T) {
	ss := &fakeServiceStore{deleteKeyErr: store.ErrForbidden}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.delete(t, "/api/v1/services/s1/api-keys/k1")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/services/{serviceID}/access-policy
// ---------------------------------------------------------------------------

func TestGetAccessPolicy_OK(t *testing.T) {
	ss := &fakeServiceStore{getPolicy: []string{"admin", "user"}}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/s1/access-policy")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out accessPolicyResp
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if len(out.Roles) != 2 {
		t.Fatalf("want 2 roles, got %v", out.Roles)
	}
}

func TestGetAccessPolicy_EmptyRoles(t *testing.T) {
	ss := &fakeServiceStore{getPolicy: []string{}}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/s1/access-policy")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", r.StatusCode)
	}
	body := readBody(t, r)
	// Must be an array (even empty), not null.
	if !strings.Contains(body, `"roles":[]`) {
		t.Errorf("want roles=[] not null, body: %s", body)
	}
}

func TestGetAccessPolicy_Forbidden(t *testing.T) {
	ss := &fakeServiceStore{getPolicyErr: store.ErrForbidden}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/s1/access-policy")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// PUT /api/v1/services/{serviceID}/access-policy
// ---------------------------------------------------------------------------

func TestSetAccessPolicy_OK(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-policy",
		map[string][]string{"roles": {"admin", "user"}})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", r.StatusCode)
	}
	if len(ss.lastRoles) != 2 {
		t.Errorf("roles not applied: %v", ss.lastRoles)
	}
}

func TestSetAccessPolicy_UnknownRole_400(t *testing.T) {
	// The store error is now a fallback; pre-validation fires first and must
	// include the offending role name in the body (spec Part D).
	ss := &fakeServiceStore{setPolicyErr: store.ErrUnknownRole}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-policy",
		map[string][]string{"roles": {"superuser"}})
	body := readBody(t, r)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", r.StatusCode, body)
	}
	if !strings.Contains(body, "unknown role") {
		t.Errorf("want 'unknown role' in body, got: %s", body)
	}
	// Spec Part D: body must include the offending role name.
	if !strings.Contains(body, "superuser") {
		t.Errorf("want offending role name 'superuser' in body, got: %s", body)
	}
}

func TestSetAccessPolicy_Forbidden(t *testing.T) {
	ss := &fakeServiceStore{setPolicyErr: store.ErrForbidden}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/access-policy",
		map[string][]string{"roles": {"user"}})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
}

func TestSetAccessPolicy_ServiceNotFound(t *testing.T) {
	ss := &fakeServiceStore{setPolicyErr: db.ErrNotFound}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/nope/access-policy",
		map[string][]string{"roles": {}})
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Fix 1: remote_port populated from live tunnel registry
// ---------------------------------------------------------------------------

// TestRemotePort_TCP_LiveTunnel verifies that a tcp service with a live tunnel
// having RemotePort=9000 returns remote_port=9000 in both list and detail.
func TestRemotePort_TCP_LiveTunnel(t *testing.T) {
	ss := &fakeServiceStore{
		listSvcs: []store.ServiceView{
			{ID: "tcp1", Name: "db", Type: "tcp"},
		},
		getSvc: store.ServiceDetail{
			ServiceView: store.ServiceView{ID: "tcp1", Name: "db", Type: "tcp"},
		},
	}
	lt := fakeLiveTunnels{
		svcID:      "tcp1",
		localAddr:  "127.0.0.1:5432",
		connected:  true,
		remotePort: 9000,
		exists:     true,
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, lt, ""))
	defer srv.Close()

	// List endpoint
	r := c.get(t, "/api/v1/services")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("list: want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var list []serviceResp
	json.NewDecoder(r.Body).Decode(&list)
	r.Body.Close()
	if len(list) != 1 {
		t.Fatalf("want 1 service, got %d", len(list))
	}
	if list[0].RemotePort != 9000 {
		t.Errorf("list remote_port: want 9000, got %d", list[0].RemotePort)
	}

	// Detail endpoint
	r2 := c.get(t, "/api/v1/services/tcp1")
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("detail: want 200, got %d body=%s", r2.StatusCode, readBody(t, r2))
	}
	var det serviceDetailResp
	json.NewDecoder(r2.Body).Decode(&det)
	r2.Body.Close()
	if det.RemotePort != 9000 {
		t.Errorf("detail remote_port: want 9000, got %d", det.RemotePort)
	}
}

// TestRemotePort_HTTP_LiveTunnel verifies that an http service with no
// RemotePort in its snapshot returns remote_port=0.
func TestRemotePort_HTTP_LiveTunnel(t *testing.T) {
	ss := &fakeServiceStore{
		listSvcs: []store.ServiceView{
			{ID: "h1", Name: "web", Type: "http"},
		},
		getSvc: store.ServiceDetail{
			ServiceView: store.ServiceView{ID: "h1", Name: "web", Type: "http"},
		},
	}
	lt := fakeLiveTunnels{
		svcID:     "h1",
		localAddr: "127.0.0.1:3000",
		connected: true,
		// remotePort is 0 (zero value) — http services don't have a remote port
		exists: true,
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, lt, ""))
	defer srv.Close()

	r := c.get(t, "/api/v1/services")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", r.StatusCode)
	}
	var list []serviceResp
	json.NewDecoder(r.Body).Decode(&list)
	r.Body.Close()
	if len(list) != 1 || list[0].RemotePort != 0 {
		t.Errorf("http service remote_port: want 0, got %d", list[0].RemotePort)
	}
}

// ---------------------------------------------------------------------------
// Fix 3: v0.2 PUT /tunnels/{id}/access-mode delegates via live registry
// ---------------------------------------------------------------------------

// fakeServiceStoreV3 captures SetServiceAccessMode calls for the v0.3 delegation test.
type fakeServiceStoreV3 struct {
	fakeServiceStore
	capturedServiceID string
	capturedMode      string
}

func (f *fakeServiceStoreV3) SetServiceAccessMode(_ context.Context, _, _, serviceID, mode, _ string, _ []byte) error {
	f.capturedServiceID = serviceID
	f.capturedMode = mode
	return nil
}

// TestSetAccessMode_V3_Delegation verifies that PUT /api/v1/tunnels/{tunnelID}/access-mode
// calls store.SetServiceAccessMode when d.LiveTunnels resolves the tunnel.
func TestSetAccessMode_V3_Delegation(t *testing.T) {
	ss := &fakeServiceStoreV3{}
	lt := fakeLiveTunnels{
		tunnelID:  "tn-live",
		tunnelLoc: TunnelLocator{ServiceID: "svc-abc", UserID: "u-self"},
	}
	d := Deps{
		Users:       &fakeUserStore{role: "user"},
		Services:    ss,
		LiveTunnels: lt,
		Log:         discardLog(),
		// AccessModes is intentionally nil — v0.3 path must not call it.
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/tunnels/tn-live/access-mode", map[string]string{"access_mode": "open"})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", r.StatusCode)
	}
	if ss.capturedServiceID != "svc-abc" {
		t.Errorf("serviceID: want svc-abc, got %q", ss.capturedServiceID)
	}
	if ss.capturedMode != "open" {
		t.Errorf("mode: want open, got %q", ss.capturedMode)
	}
}

// TestSetAccessMode_V3_Forbidden verifies that the v0.3 path surfaces 403
// when the store's permission gate returns ErrForbidden.
func TestSetAccessMode_V3_Forbidden(t *testing.T) {
	ss := &fakeServiceStore{setModeErr: store.ErrForbidden}
	lt := fakeLiveTunnels{
		tunnelID:  "tn-live",
		tunnelLoc: TunnelLocator{ServiceID: "svc-other", UserID: "u-other"},
	}
	d := Deps{
		Users:       &fakeUserStore{role: "user"},
		Services:    ss,
		LiveTunnels: lt,
		Log:         discardLog(),
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/tunnels/tn-live/access-mode", map[string]string{"access_mode": "open"})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
}

// TestSetAccessMode_V3_TunnelNotInRegistry verifies the legacy fallback when
// the tunnelID is not found in the live registry (LiveTunnels is non-nil but
// returns ok=false). Requires d.AccessModes to handle the call.
func TestSetAccessMode_V3_TunnelNotInRegistry(t *testing.T) {
	as := &fakeAccessSetter{}
	lt := fakeLiveTunnels{} // tunnelID empty → LookupByTunnelID always returns ok=false
	d := Deps{
		Users:       &fakeUserStore{role: "user"},
		AccessModes: as,
		LiveTunnels: lt,
		Log:         discardLog(),
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/tunnels/tn-legacy/access-mode", map[string]string{"access_mode": "open"})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204 (legacy fallback), got %d", r.StatusCode)
	}
	if as.mode != "open" {
		t.Errorf("legacy mode: want open, got %q", as.mode)
	}
}

// TestSetAccessMode_V3_LiveTunnelsNil verifies the legacy fallback when
// d.LiveTunnels is nil (v0.2 wiring, Task 12 not yet wired).
func TestSetAccessMode_V3_LiveTunnelsNil(t *testing.T) {
	as := &fakeAccessSetter{}
	d := Deps{
		Users:       &fakeUserStore{role: "user"},
		AccessModes: as,
		LiveTunnels: nil,
		Log:         discardLog(),
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.put(t, "/api/v1/tunnels/tn1/access-mode", map[string]string{"access_mode": "open"})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204 (nil LiveTunnels fallback), got %d", r.StatusCode)
	}
	if as.mode != "open" {
		t.Errorf("mode: want open, got %q", as.mode)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/services — admin pre-provisioning (v0.5.2 P3.6 / Task 9)
// ---------------------------------------------------------------------------

// TestPostServicesAdminCreatesService asserts the happy path: an admin caller
// POSTs a {service_id, title, access_mode} body and the handler inserts the
// row via ServiceStore.CreateService, returning 201 with {id, created_at}.
func TestPostServicesAdminCreatesService(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	body := map[string]any{
		"service_id":  "svc_pre001",
		"title":       "Pre-provisioned",
		"access_mode": "public",
	}
	r := c.post(t, "/api/v1/services", body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d body=%s; want 201", r.StatusCode, readBody(t, r))
	}
	var got struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if got.ID != "svc_pre001" {
		t.Errorf("id=%q; want svc_pre001", got.ID)
	}
	if got.CreatedAt == "" {
		t.Error("created_at empty")
	}

	// Confirm the fake store recorded the insert with the right shape.
	if len(ss.createdSvcs) != 1 {
		t.Fatalf("createdSvcs len=%d; want 1", len(ss.createdSvcs))
	}
	inserted := ss.createdSvcs[0]
	if inserted.ID != "svc_pre001" {
		t.Errorf("inserted.ID=%q", inserted.ID)
	}
	if inserted.Name != "Pre-provisioned" {
		t.Errorf("inserted.Name=%q; want Pre-provisioned", inserted.Name)
	}
	// access_mode "public" is aliased to the stored enum value "open" on the
	// services row — pin that mapping so the contract stays explicit.
	if inserted.AccessMode != "open" {
		t.Errorf("inserted.AccessMode=%q; want open (stored alias for public)", inserted.AccessMode)
	}
}

// TestPostServicesDefaultsAccessModeWhenOmitted asserts that an admin caller
// who omits access_mode gets the default — stored as "open" (the canonical
// row value the runtime enforces).
func TestPostServicesDefaultsAccessModeWhenOmitted(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services", map[string]any{"service_id": "svc_def"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d body=%s; want 201", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	if len(ss.createdSvcs) != 1 || ss.createdSvcs[0].AccessMode != "open" {
		t.Errorf("default access_mode not applied: %#v", ss.createdSvcs)
	}
}

// TestPostServicesNonAdminReturns403 asserts the non-admin caller is rejected
// at the RequireAdmin gate with 403.
func TestPostServicesNonAdminReturns403(t *testing.T) {
	ss := &fakeServiceStore{}
	d := Deps{
		Users:    &fakeUserStore{role: "user"},
		Services: ss,
		Log:      discardLog(),
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/services", map[string]any{"service_id": "svc_xyz"})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("status %d; want 403", r.StatusCode)
	}
	if len(ss.createdSvcs) != 0 {
		t.Errorf("store called from non-admin path: %#v", ss.createdSvcs)
	}
}

// TestPostServicesDuplicateReturns409 asserts that the SECOND POST for the
// same service_id maps the ErrDuplicateService sentinel to HTTP 409.
func TestPostServicesDuplicateReturns409(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	body := map[string]any{"service_id": "svc_dup"}
	r1 := c.post(t, "/api/v1/services", body)
	r1.Body.Close()
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("first POST status %d; want 201", r1.StatusCode)
	}
	r2 := c.post(t, "/api/v1/services", body)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Errorf("second POST status %d body=%s; want 409", r2.StatusCode, readBody(t, r2))
	}
}

// TestPostServicesBadServiceIDReturns400 covers each invalid shape the
// `^[a-z0-9_-]{3,64}$` regex must reject.
func TestPostServicesBadServiceIDReturns400(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	tooLong := strings.Repeat("a", 65) // 65 > 64-char max
	cases := []string{
		"",          // missing / empty
		"ab",        // shorter than 3
		tooLong,     // longer than 64
		"has space", // whitespace
		"UPPER",     // uppercase
		"bad/slash", // path separator
	}
	for _, bad := range cases {
		bad := bad
		t.Run("id="+bad, func(t *testing.T) {
			r := c.post(t, "/api/v1/services", map[string]any{"service_id": bad})
			defer r.Body.Close()
			if r.StatusCode != http.StatusBadRequest {
				t.Errorf("service_id=%q got %d; want 400", bad, r.StatusCode)
			}
		})
	}
	if len(ss.createdSvcs) != 0 {
		t.Errorf("store called for invalid service_id rows: %#v", ss.createdSvcs)
	}
}

// TestPostServicesBadAccessModeReturns400 asserts that an out-of-enum
// access_mode is rejected before the store is touched.
func TestPostServicesBadAccessModeReturns400(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services", map[string]any{
		"service_id":  "svc_ok1",
		"access_mode": "bogus",
	})
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d; want 400", r.StatusCode)
	}
	if len(ss.createdSvcs) != 0 {
		t.Errorf("store called for bad access_mode: %#v", ss.createdSvcs)
	}
}

// TestPostServicesBurrowLoginWithoutAuthDomainReturns409 mirrors the
// SetServiceAccessMode degraded-mode guard: an admin POST that asks for
// access_mode=burrow_login while AuthDomain is unconfigured must be rejected
// with 409 BEFORE the store is touched, since burrow_login auth cookies
// require a known auth_domain to scope cookies to.
func TestPostServicesBurrowLoginWithoutAuthDomainReturns409(t *testing.T) {
	ss := &fakeServiceStore{}
	// AuthDomain "" — degraded mode; burrow_login must be rejected.
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services", map[string]any{
		"service_id":  "svc_bl1",
		"access_mode": "burrow_login",
	})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Errorf("status %d body=%s; want 409", r.StatusCode, readBody(t, r))
	}
	if len(ss.createdSvcs) != 0 {
		t.Errorf("store called despite degraded-mode guard: %#v", ss.createdSvcs)
	}
}

// TestPostServicesUnauthenticatedReturns401 keeps the auth-ordering invariant:
// unauthenticated requests get 401 before RequireAdmin's 403.
func TestPostServicesUnauthenticatedReturns401(t *testing.T) {
	ss := &fakeServiceStore{}
	d := newServiceDeps(ss, fakeLiveTunnels{}, "")
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/services", "application/json",
		mustJSON(map[string]string{"service_id": "svc_x"}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d; want 401", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// errFake is a generic non-sentinel error for generic 500 tests.
// ---------------------------------------------------------------------------

var errFake = &fakeError{}

type fakeError struct{}

func (f *fakeError) Error() string { return "fake error" }

// ---------------------------------------------------------------------------
// Slug + URL (path routing)
// ---------------------------------------------------------------------------

func TestListServices_SlugAndURL(t *testing.T) {
	ss := &fakeServiceStore{
		listSvcs: []store.ServiceView{
			{ID: "s1", Name: "web", Type: "http", Subdomain: "p7baeh", AccessMode: "open"},
		},
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "burrow.example.com"))
	defer srv.Close()

	r := c.get(t, "/api/v1/services")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []map[string]any
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if len(out) != 1 {
		t.Fatalf("want 1 service, got %d", len(out))
	}
	got := out[0]
	if got["slug"] != "p7baeh" || got["url"] != "https://burrow.example.com/svc/p7baeh/" {
		t.Fatalf("got slug=%v url=%v", got["slug"], got["url"])
	}
	if _, ok := got["hostname"]; ok {
		t.Fatal("hostname must not be in the response")
	}
	if _, ok := got["subdomain"]; ok {
		t.Fatal("subdomain must not be in the response")
	}
}

func TestPutServiceSlug(t *testing.T) {
	ss := &fakeServiceStore{setSlugOld: "p7baeh"}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "burrow.example.com"))
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/slug", map[string]string{"slug": "my-app"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var ok map[string]string
	if err := json.NewDecoder(r.Body).Decode(&ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()
	if ok["slug"] != "my-app" || ok["url"] != "https://burrow.example.com/svc/my-app/" {
		t.Fatalf("body = %v", ok)
	}
	if ss.lastSlug != "my-app" {
		t.Fatalf("store got slug %q", ss.lastSlug)
	}

	ss.setSlugErr = store.ErrInvalidSlug
	r = c.put(t, "/api/v1/services/s1/slug", map[string]string{"slug": "Bad_Slug"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid slug: want 400, got %d", r.StatusCode)
	}
	var bad map[string]string
	json.NewDecoder(r.Body).Decode(&bad)
	r.Body.Close()
	if bad["error"] != auth.SlugRule {
		t.Fatalf("error text = %q, want auth.SlugRule", bad["error"])
	}

	ss.setSlugErr = store.ErrSlugTaken
	r = c.put(t, "/api/v1/services/s1/slug", map[string]string{"slug": "taken"})
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("taken slug: want 409, got %d", r.StatusCode)
	}

	ss.setSlugErr = db.ErrNotFound
	r = c.put(t, "/api/v1/services/nope/slug", map[string]string{"slug": "my-app"})
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown service: want 404, got %d", r.StatusCode)
	}
}

func TestGetSlugSuggestion(t *testing.T) {
	ss := &fakeServiceStore{suggestSlug: "k7p2qx"}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "burrow.example.com"))
	defer srv.Close()

	r := c.get(t, "/api/v1/services/slug-suggestion")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out map[string]string
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if out["slug"] != "k7p2qx" {
		t.Fatalf("slug = %q, want k7p2qx", out["slug"])
	}
}

func TestPostService_Slug(t *testing.T) {
	ss := &fakeServiceStore{}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, ""))
	defer srv.Close()

	r := c.post(t, "/api/v1/services", map[string]any{"service_id": "web-prod", "slug": "web"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status %d body=%s; want 201", r.StatusCode, readBody(t, r))
	}
	var created map[string]any
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	if created["slug"] != "web" {
		t.Fatalf("201 body slug = %v, want web", created["slug"])
	}
	if len(ss.createdSvcs) != 1 || ss.createdSvcs[0].Subdomain != "web" {
		t.Fatalf("stored row: %#v", ss.createdSvcs)
	}

	r = c.post(t, "/api/v1/services", map[string]any{"service_id": "web-bad", "slug": "Bad"})
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad slug: want 400, got %d", r.StatusCode)
	}

	r = c.post(t, "/api/v1/services", map[string]any{"service_id": "web-noslug"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("no slug: want 201, got %d", r.StatusCode)
	}
	if len(ss.createdSvcs) != 2 || ss.createdSvcs[1].Subdomain != "" {
		t.Fatalf("stored rows: %#v", ss.createdSvcs)
	}
}

// The backing row of a direct AI provider is not listed as a service.
func TestListServices_HidesDirectBackingRows(t *testing.T) {
	ss := &fakeServiceStore{
		listSvcs: []store.ServiceView{
			{ID: "prov-openrouter", Name: "OpenRouter", Type: "direct", AccessMode: "api_key"},
			{ID: "s1", Name: "web", Type: "http", Subdomain: "k7p2qx", AccessMode: "open"},
			{ID: "prov-zai", Name: "z.ai", Type: "direct", AccessMode: "api_key"},
		},
	}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "tunnels.example.com"))
	defer srv.Close()

	body := wantStatus(t, c.get(t, "/api/v1/services"), http.StatusOK)
	var out []serviceResp
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out) != 1 || out[0].ID != "s1" {
		t.Fatalf("services = %s (%v), want only s1", body, err)
	}

	// Only direct rows: an empty array, not null.
	ss.listSvcs = ss.listSvcs[:1]
	if body := wantStatus(t, c.get(t, "/api/v1/services"), http.StatusOK); strings.TrimSpace(body) != "[]" {
		t.Fatalf("services = %s, want []", body)
	}
}

// Slug and access mode of a direct provider's backing row cannot be changed
// through the service endpoints.
func TestServiceEndpoints_RefuseDirectBackingRow(t *testing.T) {
	ss := &fakeServiceStore{setModeErr: store.ErrDirectService, setSlugErr: store.ErrDirectService}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "tunnels.example.com"))
	defer srv.Close()

	for _, resp := range []*http.Response{
		c.put(t, "/api/v1/services/prov-openrouter/access-mode", map[string]string{"access_mode": "open"}),
		c.put(t, "/api/v1/services/prov-openrouter/slug", map[string]string{"slug": "my-slug"}),
	} {
		if body := wantStatus(t, resp, http.StatusConflict); !strings.Contains(body, "direct AI provider") {
			t.Errorf("body = %s", body)
		}
	}
}

func TestPutServiceGatewayOnly(t *testing.T) {
	ss := &fakeServiceStore{}
	d := newServiceDeps(ss, fakeLiveTunnels{}, "burrow.example.com")
	aud := &stubAuditAppender{}
	d.AuditAppender = aud
	srv, c := newServiceServer(t, d)
	defer srv.Close()

	r := c.put(t, "/api/v1/services/s1/gateway-only", map[string]bool{"gateway_only": true})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	var out map[string]bool
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil || !out["gateway_only"] {
		t.Fatalf("body = %v err=%v", out, err)
	}
	r.Body.Close()
	if len(ss.gatewayOnlyCalls) != 1 || ss.gatewayOnlyCalls[0].serviceID != "s1" || !ss.gatewayOnlyCalls[0].on || ss.gatewayOnlyCalls[0].role != "admin" {
		t.Fatalf("store calls = %+v", ss.gatewayOnlyCalls)
	}
	if len(aud.events) != 1 || aud.events[0].Action != audit.ActionServiceGatewayOnlyUpdate || aud.events[0].SubjectID != "s1" ||
		string(aud.events[0].Payload) != `{"gateway_only":true}` {
		t.Fatalf("audit = %+v", aud.events)
	}

	for _, tc := range []struct {
		err  error
		want int
	}{
		{db.ErrNotFound, http.StatusNotFound},
		{store.ErrForbidden, http.StatusForbidden},
		{store.ErrServiceNotHTTP, http.StatusConflict},
		{store.ErrDirectService, http.StatusConflict},
	} {
		ss.gatewayOnlyErr = tc.err
		n := len(aud.events)
		r = c.put(t, "/api/v1/services/s1/gateway-only", map[string]bool{"gateway_only": false})
		r.Body.Close()
		if r.StatusCode != tc.want {
			t.Errorf("%v: want %d, got %d", tc.err, tc.want, r.StatusCode)
		}
		if len(aud.events) != n {
			t.Errorf("%v: a failed write was audited", tc.err)
		}
	}
	// A body without the field must not silently clear the flag.
	ss.gatewayOnlyErr = nil
	r = c.put(t, "/api/v1/services/s1/gateway-only", map[string]string{})
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("missing field: want 400, got %d", r.StatusCode)
	}
}

func TestPutServiceGatewayOnly_RoleWithoutConfigurePermission(t *testing.T) {
	ss := &fakeServiceStore{gatewayOnlyErr: store.ErrForbidden}
	d := newServiceDeps(ss, fakeLiveTunnels{}, "burrow.example.com")
	d.Users = &fakeUserStore{role: "user"}
	srv, c := newServiceServer(t, d)
	defer srv.Close()
	r := c.put(t, "/api/v1/services/s1/gateway-only", map[string]bool{"gateway_only": true})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", r.StatusCode)
	}
	if ss.gatewayOnlyCalls[0].role != "user" {
		t.Fatalf("role = %q", ss.gatewayOnlyCalls[0].role)
	}
}

// A gateway-only service has no direct address: the list and the detail say
// so and carry an empty url.
func TestServices_GatewayOnlyHasNoURL(t *testing.T) {
	view := store.ServiceView{ID: "s1", Name: "llm", Type: "http", Subdomain: "abc123", AccessMode: "open", GatewayOnly: true}
	ss := &fakeServiceStore{listSvcs: []store.ServiceView{view}, getSvc: store.ServiceDetail{ServiceView: view}}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "burrow.example.com"))
	defer srv.Close()

	var list []map[string]any
	r := c.get(t, "/api/v1/services")
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	r.Body.Close()
	if list[0]["gateway_only"] != true || list[0]["url"] != "" || list[0]["slug"] != "abc123" {
		t.Fatalf("list item = %v", list[0])
	}
	var one map[string]any
	r = c.get(t, "/api/v1/services/s1")
	if err := json.NewDecoder(r.Body).Decode(&one); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if one["gateway_only"] != true || one["url"] != "" {
		t.Fatalf("detail = %v", one)
	}
}

// The slug answer must not advertise a /svc/ address for a gateway-only service.
func TestPutServiceSlug_GatewayOnlyHasNoURL(t *testing.T) {
	ss := &fakeServiceStore{setSlugOld: "p7baeh", setSlugGatewayOnly: true}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "burrow.example.com"))
	defer srv.Close()
	r := c.put(t, "/api/v1/services/s1/slug", map[string]string{"slug": "my-app"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", r.StatusCode)
	}
	var out map[string]string
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if out["slug"] != "my-app" || out["url"] != "" {
		t.Fatalf("body = %v", out)
	}
}

func TestPutServiceGatewayOnly_NotHTTPText(t *testing.T) {
	ss := &fakeServiceStore{gatewayOnlyErr: store.ErrServiceNotHTTP}
	srv, c := newServiceServer(t, newServiceDeps(ss, fakeLiveTunnels{}, "burrow.example.com"))
	defer srv.Close()
	r := c.put(t, "/api/v1/services/s1/gateway-only", map[string]bool{"gateway_only": true})
	body := readBody(t, r)
	if r.StatusCode != http.StatusConflict || !strings.Contains(body, "gateway-only requires an http service") || strings.Contains(body, "mtls") {
		t.Fatalf("status %d body %s", r.StatusCode, body)
	}
}
