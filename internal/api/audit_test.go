package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// auditTestStack is a freshly migrated *db.DB + *audit.Logger + adapters
// ready to plumb into a Deps for the API tests.
type auditTestStack struct {
	x      *db.DB
	logger *audit.Logger
	chain  AuditChain
}

func newAuditTestStack(t *testing.T) *auditTestStack {
	t.Helper()
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "audit_api.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	x := db.Wrap(sqldb)
	t.Cleanup(func() { _ = x.Close() })
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l := audit.NewLogger(x, priv, discardLog())
	return &auditTestStack{x: x, logger: l, chain: NewAuditChainAdapter(l)}
}

func (s *auditTestStack) appendN(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.logger.Append(context.Background(), audit.Event{
			ActorID: "u-admin", ActorEmail: "admin@x", Action: audit.ActionUserCreate,
			SubjectID: "u-sub", SubjectLabel: "new@x", Result: "ok",
			Payload: json.RawMessage(`{"role":"user"}`),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// TestAuditEventsAdminReadsList asserts an admin caller gets every row
// the chain holds, id-DESC.
func TestAuditEventsAdminReadsList(t *testing.T) {
	s := newAuditTestStack(t)
	s.appendN(t, 3)
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		AuditEvents: s.x, AuditChain: s.chain,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/audit/events")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []auditEventResp
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(out) != 3 {
		t.Fatalf("want 3 rows, got %d", len(out))
	}
	// id DESC: out[0].id > out[1].id > out[2].id
	if out[0].ID <= out[1].ID || out[1].ID <= out[2].ID {
		t.Fatalf("ids not DESC: %s %s %s", out[0].ID, out[1].ID, out[2].ID)
	}
}

// TestAuditEventsNonAdminWithoutPermForbidden asserts a "user"-role caller
// without authz.PermAuditRead is rejected with 403.
func TestAuditEventsNonAdminWithoutPermForbidden(t *testing.T) {
	s := newAuditTestStack(t)
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "user"},
		AuditEvents: s.x, AuditChain: s.chain,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/audit/events")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d (want 403)", r.StatusCode)
	}
}

// TestAuditEventsCursorBeforeID asserts ?before_id=<id> returns only rows
// strictly older than that id.
func TestAuditEventsCursorBeforeID(t *testing.T) {
	s := newAuditTestStack(t)
	s.appendN(t, 3)
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		AuditEvents: s.x, AuditChain: s.chain,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/audit/events")
	var out []auditEventResp
	json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if len(out) != 3 {
		t.Fatalf("want 3, got %d", len(out))
	}
	// page after the newest row → 2 older rows.
	r = c.get(t, "/api/v1/audit/events?before_id="+out[0].ID)
	var page []auditEventResp
	json.NewDecoder(r.Body).Decode(&page)
	r.Body.Close()
	if len(page) != 2 {
		t.Fatalf("want 2 older rows, got %d", len(page))
	}
	for _, e := range page {
		if e.ID >= out[0].ID {
			t.Fatalf("row %s not older than cursor %s", e.ID, out[0].ID)
		}
	}
}

// TestAuditFingerprintReturnsKey asserts the GET fingerprint endpoint
// returns a non-empty public_key + 64-char-hex fingerprint.
func TestAuditFingerprintReturnsKey(t *testing.T) {
	s := newAuditTestStack(t)
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		AuditEvents: s.x, AuditChain: s.chain,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/audit/fingerprint")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got auditFingerprintResp
	json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	raw, err := base64.StdEncoding.DecodeString(got.PublicKey)
	if err != nil {
		t.Fatalf("decode public_key: %v", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		t.Fatalf("public_key wrong length: %d", len(raw))
	}
	if len(got.Fingerprint) != 64 {
		t.Fatalf("fingerprint not 64 hex chars: %q", got.Fingerprint)
	}
}

// TestAuditExportSignedNDJSON asserts the export endpoint returns NDJSON
// whose trailer verifies against the public key returned by the
// fingerprint endpoint.
func TestAuditExportSignedNDJSON(t *testing.T) {
	s := newAuditTestStack(t)
	s.appendN(t, 2)
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		AuditEvents: s.x, AuditChain: s.chain,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/audit/export")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	if got := r.Header.Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("content-type=%q", got)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	// Fetch the public key via the API.
	r = c.get(t, "/api/v1/audit/fingerprint")
	var fp auditFingerprintResp
	json.NewDecoder(r.Body).Decode(&fp)
	r.Body.Close()
	pubBytes, _ := base64.StdEncoding.DecodeString(fp.PublicKey)

	ok, firstID, lastID, mismatched, err := audit.VerifySignedExport(bytes.NewReader(body), ed25519.PublicKey(pubBytes))
	if err != nil {
		t.Fatalf("verify export: %v", err)
	}
	if !ok {
		t.Fatalf("export signature did not verify; mismatched=%s", mismatched)
	}
	if firstID == "" || lastID == "" {
		t.Fatalf("first/last empty: %s %s", firstID, lastID)
	}
}

// TestAuditVerifyOK asserts POST /verify on an untampered chain returns
// {ok:true} with first/last ids populated.
func TestAuditVerifyOK(t *testing.T) {
	s := newAuditTestStack(t)
	s.appendN(t, 3)
	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		AuditEvents: s.x, AuditChain: s.chain,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.post(t, "/api/v1/audit/verify", map[string]string{})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got auditVerifyResp
	json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if !got.OK {
		t.Fatalf("want ok=true, got %+v", got)
	}
	if got.FirstID == "" || got.LastID == "" || got.MismatchedID != "" {
		t.Fatalf("verify response shape: %+v", got)
	}
}

// TestAuditVerifyTamperedReturnsMismatched asserts the response surfaces
// the mismatched id after an in-place payload tamper.
func TestAuditVerifyTamperedReturnsMismatched(t *testing.T) {
	s := newAuditTestStack(t)
	s.appendN(t, 3)
	// Find row 2 in id-ASC order and tamper it.
	rows, _ := s.x.ListAuditEvents(context.Background(), db.AuditQuery{Limit: 100})
	// list is DESC; row index 1 is the chronological second-newest.
	target := rows[1].ID
	if err := s.x.TamperAuditPayload(context.Background(), target, `{"role":"admin"}`); err != nil {
		t.Fatal(err)
	}

	d := Deps{
		Log:         discardLog(),
		Users:       &fakeUserStore{role: "admin"},
		AuditEvents: s.x, AuditChain: s.chain,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.post(t, "/api/v1/audit/verify", map[string]string{})
	var got auditVerifyResp
	json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if got.OK {
		t.Fatalf("want ok=false")
	}
	if got.MismatchedID != target {
		t.Fatalf("mismatched_id: got %s want %s", got.MismatchedID, target)
	}
}
