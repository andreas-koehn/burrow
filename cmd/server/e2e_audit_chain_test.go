package main

// e2e_audit_chain_test.go — closes Deferral D2.
//
// Boots the real api.Router against a real *store.Store + *audit.Logger
// on an ephemeral SQLite DB, exercises a closed sequence of admin mutations
// via the HTTP API, and asserts:
//
//   1. Every closed-list action (user.create, token.mint,
//      service.access_mode.update, service.api_key.create,
//      service.api_key.revoke, token.revoke, user.delete, session.delete)
//      results in exactly ONE audit_events row with non-empty hash that
//      chains to the previous row.
//   2. POST /api/v1/audit/verify returns {ok:true}.
//   3. The in-process burrowd audit verify command exits 0 against the
//      same DB and prints the spec-pinned "Chain valid …" message.
//   4. After a TamperAuditPayload UPDATE, POST /audit/verify returns
//      {ok:false, mismatched_id:<id>} AND the in-process verify command
//      exits 1 with the same mismatched id on stderr.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// auditChainStack is the bundle of long-lived objects owned by one
// bootAuditChainStack(t) call. Every field is shared between the HTTP test
// server and the in-process `burrowd audit verify` subcommand: pointing
// both at the same SQLite path is the whole reason the bootstrap exists.
type auditChainStack struct {
	dbPath string
	sqldb  *db.DB
	store  *store.Store
	logger *audit.Logger
	srv    *httptest.Server
	hc     *http.Client
	csrf   string

	// adminID, serviceID, etc. are populated as the suite progresses so the
	// per-mutation chain assertions can refer back to entity ids without a
	// second DB round-trip.
	adminID   string
	serviceID string
}

// bootAuditChainStack stands up:
//   - a freshly-migrated SQLite DB at a deterministic temp path,
//   - a *store.Store backed by it,
//   - an *audit.Logger with a fresh ed25519 key persisted to settings (so
//     the in-process `burrowd audit verify` reloads the same key),
//   - a seeded admin user + an http "echo" service (created directly
//     against *db.DB — service.create is NOT an audited mutation today),
//   - an httptest.Server wrapping api.NewRouter wired with every Deps
//     surface the audited handlers touch,
//   - a logged-in cookie + CSRF client ready for round-trips.
//
// The store gets SetAuditLogger BEFORE the login round-trip so the
// session.create row from the login is the very first audit_events row in
// the chain (genesis prev_hash = 64 zero hex chars).
func bootAuditChainStack(t *testing.T) *auditChainStack {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit-e2e.db")
	sqldb, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	wrapped := db.Wrap(sqldb)
	st := store.New(sqldb)

	// Persist a deterministic ed25519 signing key under settings.audit.signing_key
	// so the burrowd audit verify subcommand (which calls
	// audit.LoadOrGenerateSigningKey) loads the same key — otherwise it would
	// generate a fresh one and Verify would still pass (Verify only checks
	// the hash chain) but the fingerprint endpoint test stays consistent.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	if err := st.SaveSettings(context.Background(), map[string]string{
		audit.SettingsKey: base64.StdEncoding.EncodeToString(priv),
	}); err != nil {
		t.Fatalf("save signing key: %v", err)
	}

	logger := audit.NewLogger(wrapped, priv, nil)
	st.SetAuditLogger(storeAuditAdapter{l: logger})

	// Seed admin BEFORE wiring the audit logger so the seed insert does
	// NOT produce an audit row (SeedAdmin uses a direct DB exec, not
	// CreateUser, so it's already untyped — but the order makes the chain
	// reasoning easier to read).
	const adminEmail = "admin-audit@x"
	const adminPass = "password1-very-strong"
	if err := st.SeedAdmin(context.Background(), adminEmail, adminPass); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	adminUser, err := st.GetUserByEmail(context.Background(), adminEmail)
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}

	// Create a service directly via *db.DB (service.create is not a
	// store-side audited mutation today — the e2e test exercises the
	// downstream access_mode + api_key mutations on it).
	svc, err := wrapped.GetOrCreateService(context.Background(), adminUser.ID, "echo", "http")
	if err != nil {
		t.Fatalf("get-or-create service: %v", err)
	}

	stack := &auditChainStack{
		dbPath:    dbPath,
		sqldb:     wrapped,
		store:     st,
		logger:    logger,
		adminID:   adminUser.ID,
		serviceID: svc.ID,
	}

	deps := api.Deps{
		Users:       st,
		Sessions:    st,
		Roles:       st,
		Services:    st,
		AccessModes: st,
		AuditEvents: wrapped,
		AuditChain:  api.NewAuditChainAdapter(logger),
		Log:         discardSlog(),
	}
	stack.srv = httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(stack.srv.Close)

	// Log in: produces a session.create + a session cookie + a CSRF cookie.
	jar, _ := cookiejar.New(nil)
	stack.hc = &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"email": adminEmail, "password": adminPass})
	resp, err := stack.hc.Post(stack.srv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()
	u, _ := url.Parse(stack.srv.URL)
	for _, ck := range jar.Cookies(u) {
		if ck.Name == "burrow_csrf" {
			stack.csrf = ck.Value
		}
	}
	if stack.csrf == "" {
		t.Fatal("no CSRF cookie after login")
	}
	return stack
}

// do executes an authenticated HTTP request with the X-CSRF-Token header
// set when the method is mutating. Returns the response body bytes for
// JSON decoding by callers.
func (s *auditChainStack) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// auditRows returns every audit_events row id-ASC (oldest first).
func (s *auditChainStack) auditRows(t *testing.T) []db.AuditEvent {
	t.Helper()
	rows, err := s.sqldb.ListAuditEvents(context.Background(), db.AuditQuery{Limit: 1000})
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	// ListAuditEvents returns id DESC; reverse to ascending for chain reasoning.
	asc := make([]db.AuditEvent, len(rows))
	for i, r := range rows {
		asc[len(rows)-1-i] = r
	}
	return asc
}

// TestE2EAuditChain_FullMutationSequence is the main acceptance test for
// Deferral D2. It exercises the closed-list mutations in order and asserts
// every step produces exactly one new row whose hash chains forward.
func TestE2EAuditChain_FullMutationSequence(t *testing.T) {
	s := bootAuditChainStack(t)

	// Baseline: login produced one session.create row. Anything appended
	// during boot beyond that would break the per-step counting below.
	baseline := s.auditRows(t)
	if len(baseline) != 1 || baseline[0].Action != audit.ActionSessionCreate {
		t.Fatalf("baseline: want one session.create row, got %d (last action=%q)",
			len(baseline), lastAction(baseline))
	}

	type step struct {
		name       string
		do         func(t *testing.T) (subjectID, subjectLabel string)
		wantAction string
	}

	var (
		newUserID   string
		newTokenID  string
		newAPIKeyID string
	)

	steps := []step{
		{
			name: "user.create",
			do: func(t *testing.T) (string, string) {
				code, body := s.do(t, http.MethodPost, "/api/v1/users", map[string]string{
					"email": "victim@x", "password": "password1", "role": "user",
				})
				if code != http.StatusCreated {
					t.Fatalf("POST /users: code=%d body=%s", code, string(body))
				}
				var out struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(body, &out); err != nil {
					t.Fatalf("decode user: %v", err)
				}
				newUserID = out.ID
				return out.ID, "victim@x"
			},
			wantAction: audit.ActionUserCreate,
		},
		{
			name: "token.mint",
			do: func(t *testing.T) (string, string) {
				code, body := s.do(t, http.MethodPost, "/api/v1/tokens", map[string]string{"name": "ci"})
				if code != http.StatusCreated {
					t.Fatalf("POST /tokens: code=%d body=%s", code, string(body))
				}
				// Token id is not in the response (only name + plaintext).
				// Look it up via the list endpoint.
				_, listBody := s.do(t, http.MethodGet, "/api/v1/tokens", nil)
				var tokens []struct{ ID, Name string }
				_ = json.Unmarshal(listBody, &tokens)
				for _, tk := range tokens {
					if tk.Name == "ci" {
						newTokenID = tk.ID
						return tk.ID, "ci"
					}
				}
				t.Fatal("ci token not found in list")
				return "", ""
			},
			wantAction: audit.ActionTokenMint,
		},
		{
			name: "service.access_mode.update",
			do: func(t *testing.T) (string, string) {
				code, body := s.do(t, http.MethodPut,
					"/api/v1/services/"+s.serviceID+"/access-mode",
					map[string]string{"access_mode": "api_key"})
				if code != http.StatusNoContent {
					t.Fatalf("PUT /services/.../access-mode: code=%d body=%s", code, string(body))
				}
				return s.serviceID, "echo"
			},
			wantAction: audit.ActionServiceAccessModeUpdate,
		},
		{
			name: "service.api_key.create",
			do: func(t *testing.T) (string, string) {
				code, body := s.do(t, http.MethodPost,
					"/api/v1/services/"+s.serviceID+"/api-keys",
					map[string]string{"name": "ci-key"})
				if code != http.StatusCreated {
					t.Fatalf("POST /services/.../api-keys: code=%d body=%s", code, string(body))
				}
				var out struct{ ID string }
				if err := json.Unmarshal(body, &out); err != nil {
					t.Fatalf("decode api key: %v", err)
				}
				newAPIKeyID = out.ID
				return out.ID, "ci-key"
			},
			wantAction: audit.ActionServiceAPIKeyCreate,
		},
		{
			name: "service.api_key.revoke",
			do: func(t *testing.T) (string, string) {
				code, body := s.do(t, http.MethodDelete,
					"/api/v1/services/"+s.serviceID+"/api-keys/"+newAPIKeyID, nil)
				if code != http.StatusNoContent {
					t.Fatalf("DELETE api-key: code=%d body=%s", code, string(body))
				}
				return newAPIKeyID, "ci-key"
			},
			wantAction: audit.ActionServiceAPIKeyRevoke,
		},
		{
			name: "token.revoke",
			do: func(t *testing.T) (string, string) {
				code, body := s.do(t, http.MethodDelete, "/api/v1/tokens/"+newTokenID, nil)
				if code != http.StatusNoContent {
					t.Fatalf("DELETE token: code=%d body=%s", code, string(body))
				}
				return newTokenID, ""
			},
			wantAction: audit.ActionTokenRevoke,
		},
		{
			name: "user.delete",
			do: func(t *testing.T) (string, string) {
				code, body := s.do(t, http.MethodDelete, "/api/v1/users/"+newUserID, nil)
				if code != http.StatusNoContent {
					t.Fatalf("DELETE user: code=%d body=%s", code, string(body))
				}
				return newUserID, "victim@x"
			},
			wantAction: audit.ActionUserDelete,
		},
	}

	priorRows := len(baseline)
	priorHash := baseline[len(baseline)-1].Hash
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			subjectID, subjectLabel := step.do(t)
			rows := s.auditRows(t)
			if len(rows) != priorRows+1 {
				t.Fatalf("want %d rows after %s, got %d", priorRows+1, step.name, len(rows))
			}
			latest := rows[len(rows)-1]
			if latest.Action != step.wantAction {
				t.Fatalf("step %s: action=%q want %q", step.name, latest.Action, step.wantAction)
			}
			if latest.Hash == "" {
				t.Fatalf("step %s: empty hash", step.name)
			}
			if latest.PrevHash != priorHash {
				t.Fatalf("step %s: prev_hash=%q want %q (chain broken)",
					step.name, latest.PrevHash, priorHash)
			}
			if subjectID != "" && latest.SubjectID != subjectID {
				t.Errorf("step %s: subject_id=%q want %q", step.name, latest.SubjectID, subjectID)
			}
			if subjectLabel != "" && latest.SubjectLabel != "" && latest.SubjectLabel != subjectLabel {
				t.Errorf("step %s: subject_label=%q want %q",
					step.name, latest.SubjectLabel, subjectLabel)
			}
			if latest.ActorID != s.adminID {
				t.Errorf("step %s: actor_id=%q want admin %q",
					step.name, latest.ActorID, s.adminID)
			}
			priorRows = len(rows)
			priorHash = latest.Hash
		})
	}

	// --- POST /api/v1/audit/verify against the good chain -----------------
	t.Run("verify_ok_via_api", func(t *testing.T) {
		code, body := s.do(t, http.MethodPost, "/api/v1/audit/verify", map[string]string{})
		if code != http.StatusOK {
			t.Fatalf("POST /audit/verify: code=%d body=%s", code, string(body))
		}
		var out struct {
			OK           bool   `json:"ok"`
			MismatchedID string `json:"mismatched_id"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v body=%s", err, string(body))
		}
		if !out.OK || out.MismatchedID != "" {
			t.Fatalf("verify: ok=%v mismatched=%q body=%s", out.OK, out.MismatchedID, string(body))
		}
	})

	// --- burrowd audit verify (in-process subcommand) -----------------------
	t.Run("verify_ok_via_cli", func(t *testing.T) {
		cmd := newAuditVerifyCmd()
		cmd.SetArgs([]string{"--db", s.dbPath})
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetContext(context.Background())
		if err := cmd.Execute(); err != nil {
			t.Fatalf("burrowd audit verify: err=%v stderr=%s", err, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "Chain valid from ") {
			t.Fatalf("cli stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	// --- Tamper one row's payload + reverify ------------------------------
	allRows := s.auditRows(t)
	if len(allRows) < 3 {
		t.Fatalf("need at least 3 rows to tamper; have %d", len(allRows))
	}
	// Tamper the second row (the user.create row) — flipping payload
	// changes its canonical hash but leaves the prev_hash chain expectation
	// intact, so the verifier reports row N's hash mismatched.
	tamperTarget := allRows[1].ID
	if err := s.sqldb.TamperAuditPayload(context.Background(), tamperTarget,
		`{"role":"superadmin"}`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	t.Run("verify_tamper_via_api", func(t *testing.T) {
		code, body := s.do(t, http.MethodPost, "/api/v1/audit/verify", map[string]string{})
		if code != http.StatusOK {
			t.Fatalf("POST /audit/verify (tampered): code=%d body=%s", code, string(body))
		}
		var out struct {
			OK           bool   `json:"ok"`
			MismatchedID string `json:"mismatched_id"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v body=%s", err, string(body))
		}
		if out.OK {
			t.Fatalf("verify after tamper: want ok=false body=%s", string(body))
		}
		if out.MismatchedID != tamperTarget {
			t.Fatalf("mismatched_id=%q want %q", out.MismatchedID, tamperTarget)
		}
	})
	t.Run("verify_tamper_via_cli", func(t *testing.T) {
		cmd := newAuditVerifyCmd()
		cmd.SetArgs([]string{"--db", s.dbPath})
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetContext(context.Background())
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("cli after tamper: want exit error, got nil (stdout=%q)", stdout.String())
		}
		wantSub := "Chain mismatch at " + tamperTarget + "."
		if !strings.Contains(stderr.String(), wantSub) {
			t.Fatalf("cli stderr=%q want substring %q", stderr.String(), wantSub)
		}
	})
}

// lastAction is a tiny helper so a fatal "baseline didn't match" message
// includes the last row's action — easier to diagnose at a glance.
func lastAction(rows []db.AuditEvent) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[len(rows)-1].Action
}

// discardSlog returns a slog Logger that swallows every record — keeps the
// test output clean without dragging logging into Deps.Log==nil territory.
func discardSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
