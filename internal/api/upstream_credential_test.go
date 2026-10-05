package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// --- Stubs ------------------------------------------------------------------

// stubCredVault implements CredentialVaultIface with a fixed slot set.
type stubCredVault struct{ slots []string }

func (s *stubCredVault) Get(slot string) (string, bool) {
	for _, sl := range s.slots {
		if sl == slot {
			return "fake-key-for-" + sl, true
		}
	}
	return "", false
}
func (s *stubCredVault) Slots() []string { return append([]string(nil), s.slots...) }

// stubCredStore implements CredentialStore.
type stubCredStore struct {
	row     db.ServiceUpstreamCredential
	present bool
	getErr  error
	putErr  error
	delErr  error
	lastPut db.ServiceUpstreamCredential
	lastDel string
}

func (s *stubCredStore) GetUpstreamCredential(_ context.Context, _ string) (db.ServiceUpstreamCredential, error) {
	if s.getErr != nil {
		return db.ServiceUpstreamCredential{}, s.getErr
	}
	if !s.present {
		return db.ServiceUpstreamCredential{}, db.ErrNotFound
	}
	return s.row, nil
}
func (s *stubCredStore) UpsertUpstreamCredential(_ context.Context, c db.ServiceUpstreamCredential) error {
	s.lastPut = c
	return s.putErr
}
func (s *stubCredStore) DeleteUpstreamCredential(_ context.Context, serviceID string) error {
	s.lastDel = serviceID
	return s.delErr
}

// stubAuditAppender captures Append calls for assertion.
type stubAuditAppender struct {
	events []audit.Event
}

func (s *stubAuditAppender) Append(_ context.Context, e audit.Event) error {
	s.events = append(s.events, e)
	return nil
}

// stubSvcLookup implements ServiceOwnerLookup — returns a service owned by a
// given userID.
type stubSvcLookup struct {
	ownerID string
	missing bool
	svcType string // "" = an ordinary service
}

func (s *stubSvcLookup) GetServiceByID(_ context.Context, _ string) (db.Service, error) {
	if s.missing {
		return db.Service{}, db.ErrNotFound
	}
	return db.Service{ID: "svc1", UserID: s.ownerID, Type: s.svcType}, nil
}

// --- Helpers ----------------------------------------------------------------

// newCredDeps returns a Deps pre-wired for upstream-credential handler tests.
// The caller is an admin by default (via fakeUserStore). Pass nil vault/store
// to leave them as zero (nil interface) — do NOT pass typed nil pointers.
func newCredDeps(vault *stubCredVault, store *stubCredStore) Deps {
	d := Deps{
		Users: &fakeUserStore{role: "admin"},
		Log:   discardLog(),
	}
	if vault != nil {
		d.CredentialVault = vault
	}
	if store != nil {
		d.CredentialDB = store
	}
	return d
}

// --- Tests: GET /upstream-credentials/slots ---------------------------------

func TestGetSlots_ReturnsVaultSlots(t *testing.T) {
	vault := &stubCredVault{slots: []string{"OPENAI", "X"}}
	d := newCredDeps(vault, nil)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/upstream-credentials/slots")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Slots []string `json:"slots"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Slots) != 2 || body.Slots[0] != "OPENAI" || body.Slots[1] != "X" {
		t.Errorf("slots=%v; want [OPENAI X]", body.Slots)
	}
}

func TestGetSlots_NilVaultReturnsEmpty(t *testing.T) {
	// CredentialVault intentionally left nil (zero-value interface) — handler
	// must return 200 [] without panicking.
	d := Deps{
		Users: &fakeUserStore{role: "admin"},
		Log:   discardLog(),
		// CredentialVault: nil — use zero interface, NOT (*stubCredVault)(nil).
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/upstream-credentials/slots")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestGetSlots_NonAdminForbidden(t *testing.T) {
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	d := Deps{
		Users:           &fakeUserStore{role: "user"},
		Log:             discardLog(),
		CredentialVault: vault,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/upstream-credentials/slots")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

// --- Tests: GET .../upstream-credential -------------------------------------

func TestGetServiceCred_Unbound(t *testing.T) {
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{present: false}
	d := newCredDeps(vault, store)
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/services/svc1/upstream-credential")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if sp, _ := body["slot_present"].(bool); sp {
		t.Error("unbound should return slot_present=false")
	}
}

func TestGetServiceCred_Bound_SlotPresent(t *testing.T) {
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{
		present: true,
		row: db.ServiceUpstreamCredential{
			ServiceID:    "svc1",
			Slot:         "OPENAI",
			HeaderName:   "Authorization",
			HeaderFormat: "Bearer {key}",
		},
	}
	d := newCredDeps(vault, store)
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/services/svc1/upstream-credential")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if sp, _ := body["slot_present"].(bool); !sp {
		t.Error("slot is in vault; slot_present should be true")
	}
	if body["slot"] != "OPENAI" {
		t.Errorf("slot=%v; want OPENAI", body["slot"])
	}
}

// TestGetServiceCred_BoundSlotMissing covers spec B.2: the binding row exists in
// the store but the env var has been removed from the vault at request time.
// The handler must return 200 with slot_present=false and still return the slot
// name and header fields so the dashboard can render the handle.
func TestGetServiceCred_BoundSlotMissing(t *testing.T) {
	// Vault only has "OPENAI"; the binding points to "REMOVED" which is gone.
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{
		present: true,
		row: db.ServiceUpstreamCredential{
			ServiceID:    "svc1",
			Slot:         "REMOVED",
			HeaderName:   "Authorization",
			HeaderFormat: "Bearer {key}",
		},
	}
	d := newCredDeps(vault, store)
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.get(t, "/api/v1/services/svc1/upstream-credential")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	// slot_present must be false — the env var was removed.
	if sp, _ := body["slot_present"].(bool); sp {
		t.Error("env var removed: slot_present should be false")
	}
	// slot name must still be returned so the dashboard can display the handle.
	if body["slot"] != "REMOVED" {
		t.Errorf("slot=%v; want REMOVED", body["slot"])
	}
	// header fields must also be returned.
	if body["header_name"] != "Authorization" {
		t.Errorf("header_name=%v; want Authorization", body["header_name"])
	}
	if body["header_format"] != "Bearer {key}" {
		t.Errorf("header_format=%v; want 'Bearer {key}'", body["header_format"])
	}
}

// --- Tests: PUT .../upstream-credential -------------------------------------

func TestPutServiceCred_HappyPath(t *testing.T) {
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{}
	auditApp := &stubAuditAppender{}
	d := newCredDeps(vault, store)
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	d.AuditAppender = auditApp
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.put(t, "/api/v1/services/svc1/upstream-credential",
		map[string]string{"slot": "OPENAI"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if store.lastPut.Slot != "OPENAI" {
		t.Errorf("lastPut.Slot=%q; want OPENAI", store.lastPut.Slot)
	}
	if store.lastPut.HeaderName != "Authorization" {
		t.Errorf("default header_name should be Authorization; got %q", store.lastPut.HeaderName)
	}
	if store.lastPut.HeaderFormat != "Bearer {key}" {
		t.Errorf("default header_format should be 'Bearer {key}'; got %q", store.lastPut.HeaderFormat)
	}
	// Audit event emitted.
	if len(auditApp.events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(auditApp.events))
	}
	if auditApp.events[0].Action != audit.ActionServiceUpstreamCredentialBind {
		t.Errorf("audit action=%q; want %q", auditApp.events[0].Action, audit.ActionServiceUpstreamCredentialBind)
	}
	// Payload must contain slot but NOT a credential value.
	payloadStr := string(auditApp.events[0].Payload)
	if payloadStr == "" {
		t.Error("audit payload should be non-empty")
	}
	var payload map[string]string
	if err := json.Unmarshal(auditApp.events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["slot"] != "OPENAI" {
		t.Errorf("audit payload slot=%q; want OPENAI", payload["slot"])
	}
}

func TestPutServiceCred_UnknownSlot(t *testing.T) {
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{}
	d := newCredDeps(vault, store)
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.put(t, "/api/v1/services/svc1/upstream-credential",
		map[string]string{"slot": "NOTEXIST"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "unknown slot" {
		t.Errorf("error=%q; want 'unknown slot'", body["error"])
	}
}

func TestPutServiceCred_InvalidHeaderFormat(t *testing.T) {
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{}
	d := newCredDeps(vault, store)
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.put(t, "/api/v1/services/svc1/upstream-credential",
		map[string]string{"slot": "OPENAI", "header_format": "Bearer TOKEN"}) // no {key}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "invalid header_format" {
		t.Errorf("error=%q; want 'invalid header_format'", body["error"])
	}
}

func TestPutServiceCred_NonOwnerWithOwnPermDenied(t *testing.T) {
	// The caller has ai:configure:own but does NOT own the service → 403.
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{}
	d := Deps{
		// role "user" has PermAIConfigureOwn but NOT PermAIConfigureAny.
		Users:           &fakeUserStore{role: "user", selfID: "u-other"},
		Log:             discardLog(),
		CredentialVault: vault,
		CredentialDB:    store,
		// Service is owned by "u-owner", not "u-other".
		CredentialServices: &stubSvcLookup{ownerID: "u-owner"},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.put(t, "/api/v1/services/svc1/upstream-credential",
		map[string]string{"slot": "OPENAI"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403; got %d", resp.StatusCode)
	}
}

// --- Tests: DELETE .../upstream-credential ----------------------------------

func TestDeleteServiceCred_HappyPath(t *testing.T) {
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{
		present: true,
		row: db.ServiceUpstreamCredential{
			ServiceID: "svc1",
			Slot:      "OPENAI",
		},
	}
	auditApp := &stubAuditAppender{}
	d := newCredDeps(vault, store)
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	d.AuditAppender = auditApp
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.delete(t, "/api/v1/services/svc1/upstream-credential")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if store.lastDel != "svc1" {
		t.Errorf("lastDel=%q; want svc1", store.lastDel)
	}
	// Audit event emitted with correct action.
	if len(auditApp.events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(auditApp.events))
	}
	if auditApp.events[0].Action != audit.ActionServiceUpstreamCredentialUnbind {
		t.Errorf("action=%q; want unbind", auditApp.events[0].Action)
	}
}

func TestDeleteServiceCred_SlotNotInAuditPayload_ValueAbsent(t *testing.T) {
	// Verifies the audit payload has {slot} but no credential value.
	vault := &stubCredVault{slots: []string{"OPENAI"}}
	store := &stubCredStore{
		present: true,
		row:     db.ServiceUpstreamCredential{ServiceID: "svc1", Slot: "OPENAI"},
	}
	auditApp := &stubAuditAppender{}
	d := newCredDeps(vault, store)
	d.CredentialServices = &stubSvcLookup{ownerID: "u-self"}
	d.AuditAppender = auditApp
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	ac := authedClient(t, srv)

	resp := ac.delete(t, "/api/v1/services/svc1/upstream-credential")
	resp.Body.Close()

	if len(auditApp.events) == 0 {
		t.Fatal("no audit events")
	}
	var payload map[string]string
	if err := json.Unmarshal(auditApp.events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["slot"] != "OPENAI" {
		t.Errorf("audit payload slot=%q; want OPENAI", payload["slot"])
	}
	// Ensure the fake key value is NOT present anywhere in the payload.
	payloadStr := string(auditApp.events[0].Payload)
	if len(payloadStr) > 0 && containsCredValue(payloadStr) {
		t.Errorf("audit payload must not contain credential value; got %s", payloadStr)
	}
}

// containsCredValue returns true if the stubCredVault's fake-key prefix
// ("fake-key") appears anywhere in s. The stub's Get method returns
// "fake-key-for-<SLOT>", so any leak of that value will be caught.
func containsCredValue(s string) bool {
	return strings.Contains(s, "fake-key")
}

// The backing row of a direct AI provider has no upstream credential binding
// and no custom domains: the provider has its own credential slot and is
// reached under /ai/ only.
func TestServiceRoutes_RefuseDirectBackingRow(t *testing.T) {
	credStore := &stubCredStore{present: false}
	d := newCredDeps(&stubCredVault{slots: []string{"OPENAI"}}, credStore)
	lookup := &stubSvcLookup{ownerID: "u-self", svcType: "direct"}
	d.CredentialServices, d.IPGeoServices = lookup, lookup
	domains := newStubDomainStore()
	d.CustomDomains = domains
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	for name, resp := range map[string]*http.Response{
		"get credential":    c.get(t, "/api/v1/services/svc1/upstream-credential"),
		"put credential":    c.put(t, "/api/v1/services/svc1/upstream-credential", map[string]string{"slot": "OPENAI"}),
		"delete credential": c.delete(t, "/api/v1/services/svc1/upstream-credential"),
		"list domains":      c.get(t, "/api/v1/services/svc1/domains"),
		"post domain":       c.post(t, "/api/v1/services/svc1/domains", map[string]string{"hostname": "x.example.com"}),
		"get domain":        c.get(t, "/api/v1/services/svc1/domains/d1"),
		"put domain":        c.put(t, "/api/v1/services/svc1/domains/d1", map[string]string{"cert_pem": "x", "key_pem": "y"}),
		"delete domain":     c.delete(t, "/api/v1/services/svc1/domains/d1"),
	} {
		if body := wantStatus(t, resp, http.StatusConflict); !strings.Contains(body, "direct AI provider") {
			t.Errorf("%s: body = %s", name, body)
		}
	}
	if credStore.lastPut.Slot != "" || credStore.lastDel != "" {
		t.Errorf("the direct row's binding was touched: put %+v del %q", credStore.lastPut, credStore.lastDel)
	}
}
