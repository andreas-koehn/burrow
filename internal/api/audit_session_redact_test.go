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

// Older relays stored the raw session cookie as the subject of session.*
// rows. The audit API and the export must not hand it out; the stored rows
// and the chain stay as they were.
func TestAudit_LegacyRawSessionIDsAreRedactedOnRead(t *testing.T) {
	s := newAuditTestStack(t)
	const raw = "0b9c1f4e-7d2a-4c53-9a6e-3f1d2b8a7c10" // what uuid.NewString() produced
	for _, a := range []string{audit.ActionSessionCreate, audit.ActionSessionDelete} {
		if err := s.logger.Append(context.Background(), audit.Event{
			ActorID: "u-admin", ActorEmail: "admin@x", Action: a, SubjectID: raw, Result: "ok",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// session.revoke_others names a user, not a session: untouched.
	if err := s.logger.Append(context.Background(), audit.Event{
		ActorID: "u-admin", Action: audit.ActionSessionRevokeOthers,
		SubjectID: "0b9c1f4e-7d2a-4c53-9a6e-3f1d2b8a7c99", Result: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, AuditEvents: s.x, AuditChain: s.chain}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	want := audit.SessionRef(raw)
	resp := c.get(t, "/api/v1/audit/events")
	body := readBody(t, resp)
	if strings.Contains(body, raw) {
		t.Fatalf("the list holds a raw session id: %s", body)
	}
	var rows []auditEventResp
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		switch r.Action {
		case audit.ActionSessionCreate, audit.ActionSessionDelete:
			if r.SubjectID != want {
				t.Errorf("%s subject %q, want %q", r.Action, r.SubjectID, want)
			}
		case audit.ActionSessionRevokeOthers:
			if !strings.HasSuffix(r.SubjectID, "99") {
				t.Errorf("revoke_others subject changed: %q", r.SubjectID)
			}
		}
	}

	resp = c.get(t, "/api/v1/audit/export")
	export := readBody(t, resp)
	if strings.Contains(export, raw) {
		t.Fatalf("the export holds a raw session id")
	}
	if !strings.Contains(export, want) {
		t.Fatalf("the export lacks the hashed id")
	}
	// The signed export still verifies offline (the redacted rows are marked),
	// and so does the chain in the database, which still holds the raw rows.
	ok, _, _, mismatched, err := audit.VerifySignedExport(strings.NewReader(export), s.pub)
	if err != nil || !ok {
		t.Fatalf("offline verify: ok=%v mismatched=%q err=%v", ok, mismatched, err)
	}
	if ok, bad, err := s.chain.Verify(context.Background(), "", ""); err != nil || !ok {
		t.Fatalf("chain verify: ok=%v mismatched=%q err=%v", ok, bad, err)
	}
	stored, _ := s.x.ListAuditEvents(context.Background(), db.AuditQuery{Limit: 10})
	found := false
	for _, e := range stored {
		if e.SubjectID == raw {
			found = true
		}
	}
	if !found {
		t.Fatal("the stored rows were rewritten")
	}
	_ = http.StatusOK
}
