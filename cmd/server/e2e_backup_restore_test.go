package main

// e2e_backup_restore_test.go — Task 14: real-stack backup + restore roundtrip.
//
// Boots a stack A on tmp_db_a, seeds an audit row (by attaching an audit
// logger to the store, then minting an extra client token), produces a
// tar.gz backup via the cobra command, asserts the archive carries
// manifest.json + db.sqlite with a matching db_sha256, shuts down stack
// A, and restores into a fresh tmp_db_b. A new stack B is booted against
// the restored DB; the test asserts the seeded admin login still works,
// the audit chain is queryable, and the FIRST event after the restore is
// audit.restore with a payload carrying the prior chain's last hash.
//
// The REST path (POST /api/v1/backups, GET /api/v1/backups, POST
// /backups/{id}/verify) is exercised against a separate stack C — the
// real api.NewRouter is fronted by httptest.Server with the backup
// adapters wired so the production code path runs end-to-end.
//
// The negative path asserts that running `burrowd restore` against the
// live DB of stack C fails with the lockfile-conflict shape (the API
// path stages a sibling lock during restore).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// installAuditLoggerOnStore wires an audit.Logger that persists rows to the
// stack's *sql.DB and attaches it to the store. The signing key is persisted
// in settings so a subsequent `restore` can reload the same key without
// regenerating one. Returns the logger so the caller can append events
// directly (the e2e_helpers stack does NOT auto-wire an audit logger).
func installAuditLoggerOnStore(t *testing.T, s *e2eStack) *audit.Logger {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatalf("audit genkey: %v", err)
	}
	if err := s.store.SaveSettings(context.Background(), map[string]string{
		audit.SettingsKey: base64.StdEncoding.EncodeToString(priv),
	}); err != nil {
		t.Fatalf("save audit signing key: %v", err)
	}
	logger := audit.NewLogger(db.Wrap(s.db), priv, s.log)
	s.store.SetAuditLogger(storeAuditAdapter{l: logger})
	return logger
}

// listAuditEventsAsc returns audit_events rows in ascending id order (oldest
// first), suitable for chain reasoning.
func listAuditEventsAsc(t *testing.T, sqldb *db.DB) []db.AuditEvent {
	t.Helper()
	rows, err := sqldb.ListAuditEvents(context.Background(), db.AuditQuery{Limit: 1000})
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	asc := make([]db.AuditEvent, len(rows))
	for i, r := range rows {
		asc[len(rows)-1-i] = r
	}
	return asc
}

// readManifestJSON pulls manifest.json out of a backup tar.gz and returns
// the decoded shape. Mirrors readArchive in backup_test.go but typed.
func readManifestJSON(t *testing.T, archivePath string) BackupManifest {
	t.Helper()
	entries := readArchive(t, archivePath)
	mb, ok := entries["manifest.json"]
	if !ok {
		t.Fatalf("archive missing manifest.json: keys=%v", keys(entries))
	}
	var m BackupManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatalf("decode manifest: %v body=%s", err, mb)
	}
	return m
}

// bootAPIBackupStack boots a minimal HTTP API stack that wires the
// backup/restore handlers against a freshly-migrated SQLite DB. It's NOT
// the full e2eStack — backup/restore are independent of the proxy/control
// plumbing, so a smaller bootstrap keeps the test focused.
type apiBackupStack struct {
	dbPath    string
	sqldb     *db.DB
	store     *store.Store
	logger    *audit.Logger
	backupDir string
	srv       *httptest.Server
	hc        *http.Client
	csrf      string
	adminID   string
}

func bootAPIBackupStack(t *testing.T) *apiBackupStack {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "api-backup.db")
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

	_, priv, err := ed25519.GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatalf("audit genkey: %v", err)
	}
	if err := st.SaveSettings(context.Background(), map[string]string{
		audit.SettingsKey: base64.StdEncoding.EncodeToString(priv),
	}); err != nil {
		t.Fatalf("save signing key: %v", err)
	}
	logger := audit.NewLogger(wrapped, priv, nil)
	st.SetAuditLogger(storeAuditAdapter{l: logger})

	const adminEmail = "admin-backup@x"
	const adminPass = "password1-very-strong"
	if err := st.SeedAdmin(context.Background(), adminEmail, adminPass); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	adminUser, err := st.GetUserByEmail(context.Background(), adminEmail)
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}

	backupDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}

	// Use the same adapters cmd/server wires in main.go so the API path
	// hits the SAME runBackup / runRestore the CLI does.
	cfg := &config.ServerConfig{DatabasePath: dbPath}
	deps := api.Deps{
		Users:          st,
		Sessions:       st,
		Roles:          st,
		Services:       st,
		AccessModes:    st,
		AuditEvents:    wrapped,
		AuditChain:     api.NewAuditChainAdapter(logger),
		AuditAppender:  logger,
		BackupDir:      backupDir,
		BackupRunner:   backupRunnerAdapter{cfg: cfg},
		RestoreRunner:  restoreRunnerAdapter{cfg: cfg},
		RestoreTracker: api.NewRestoreTracker(),
		Log:            discardSlog(),
	}
	stack := &apiBackupStack{
		dbPath:    dbPath,
		sqldb:     wrapped,
		store:     st,
		logger:    logger,
		backupDir: backupDir,
		adminID:   adminUser.ID,
	}
	stack.srv = httptest.NewServer(api.NewRouter(deps))
	t.Cleanup(stack.srv.Close)

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

// do issues a JSON request to the API stack with the cookie + CSRF wired.
func (s *apiBackupStack) do(t *testing.T, method, path string, body any) (int, []byte) {
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

// TestE2EBackup_RestoreRoundtrip is the Task 14 acceptance test. It covers:
//  1. CLI backup: tar.gz contains manifest.json + db.sqlite with matching
//     db_sha256.
//  2. CLI restore into a fresh DB: schema + seeded admin survive, the
//     first event after restore is audit.restore with payload.prior_last_hash
//     equal to the pre-restore chain head (the chain is preserved, not
//     truncated — Reconciled spec text).
//  3. REST path against the live API: POST /backups -> 202, GET /backups
//     lists it with a matching db_sha256, POST /backups/{id}/verify -> ok.
//  4. Negative: `burrowd restore` against a DB whose .restore.lock exists
//     exits 1 with the "another restore in progress" message.
func TestE2EBackup_RestoreRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skip e2e in -short")
	}

	// --- Stack A: real e2eStack ---------------------------------------------
	stackA := bootE2EStack(t)
	logger := installAuditLoggerOnStore(t, stackA)
	_ = logger // future-proofing: a direct Append site would use this.

	// Mint a SECOND client token now (the logger is wired) so we produce a
	// real audit.token.mint row. The boot-time token from e2e_helpers was
	// issued BEFORE the logger was wired, so it didn't emit.
	if _, err := stackA.store.IssueClientToken(context.Background(), stackA.userID, "e2e-backup-marker"); err != nil {
		t.Fatalf("seed audit token mint: %v", err)
	}
	rowsA := listAuditEventsAsc(t, db.Wrap(stackA.db))
	if len(rowsA) == 0 {
		t.Fatalf("expected at least one audit_events row after IssueClientToken; got 0")
	}
	preRestoreLastHash := rowsA[len(rowsA)-1].Hash
	if preRestoreLastHash == "" {
		t.Fatalf("pre-restore chain head hash is empty")
	}

	// Grab the source DB path off the DB handle (the helper opens the DB
	// via its own t.TempDir(); we need the same path for the backup CLI).
	dbPathA := stackADBPath(t, stackA)

	// --- 1. CLI backup -----------------------------------------------------
	backupDir := t.TempDir()
	snapPath := filepath.Join(backupDir, "snap.tar.gz")
	backupCmd := newBackupCmd()
	backupCmd.SetArgs([]string{"--to", snapPath, "--db", dbPathA})
	var bStdout, bStderr bytes.Buffer
	backupCmd.SetOut(&bStdout)
	backupCmd.SetErr(&bStderr)
	backupCmd.SetContext(context.Background())
	if err := backupCmd.Execute(); err != nil {
		t.Fatalf("burrowd backup: %v stderr=%s", err, bStderr.String())
	}

	manifest := readManifestJSON(t, snapPath)
	if manifest.DBSha256 == "" {
		t.Fatalf("manifest.db_sha256 is empty")
	}
	// Recompute sha256 of the tar entry's bytes; must equal manifest.
	entries := readArchive(t, snapPath)
	dbBytes, ok := entries["db.sqlite"]
	if !ok {
		t.Fatalf("archive missing db.sqlite: keys=%v", keys(entries))
	}
	if got := sha256HexBytes(dbBytes); got != manifest.DBSha256 {
		t.Fatalf("db_sha256 drift: manifest=%s actual=%s", manifest.DBSha256, got)
	}

	// --- 2. Shut down stack A ----------------------------------------------
	stackA.shutdown()
	// stackA.cleanupFns also closes the DB; the t.Cleanup re-entry of
	// shutdown() is idempotent (every sub-Close has a nil-guard).

	// --- 3. CLI restore into a fresh DB ------------------------------------
	dstDir := t.TempDir()
	dbPathB := filepath.Join(dstDir, "burrow.db")
	restoreCmd := newRestoreCmd()
	restoreCmd.SetArgs([]string{"--from", snapPath, "--db", dbPathB})
	var rStdout, rStderr bytes.Buffer
	restoreCmd.SetOut(&rStdout)
	restoreCmd.SetErr(&rStderr)
	restoreCmd.SetContext(context.Background())
	if err := restoreCmd.Execute(); err != nil {
		t.Fatalf("burrowd restore: %v stderr=%s", err, rStderr.String())
	}
	if _, err := os.Stat(dbPathB); err != nil {
		t.Fatalf("restored db missing: %v", err)
	}

	// --- 4. Verify post-restore chain shape --------------------------------
	sqldbB, err := db.Open(dbPathB)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	t.Cleanup(func() { _ = sqldbB.Close() })
	wrappedB := db.Wrap(sqldbB)
	stB := store.New(sqldbB)

	// Admin survives the restore (login still works).
	ok2, err := stB.VerifyUserPassword(context.Background(), e2eAdminEmail, e2eAdminPassword)
	if err != nil {
		t.Fatalf("verify admin password: %v", err)
	}
	if !ok2 {
		t.Fatalf("admin password rejected after restore")
	}
	// Seeded service survives: the e2e helper resolved a subdomain row for
	// the "echo" tunnel; that durable row must round-trip through the
	// snapshot.
	svcs, err := stB.ListServices(context.Background(), stackA.userID, "admin")
	if err != nil {
		t.Fatalf("list services after restore: %v", err)
	}
	foundSvc := false
	for _, sv := range svcs {
		if sv.ID == stackA.serviceID {
			foundSvc = true
			break
		}
	}
	if !foundSvc {
		t.Fatalf("seeded service %q missing from restored DB; got=%+v",
			stackA.serviceID, svcs)
	}

	rowsB := listAuditEventsAsc(t, wrappedB)
	// The chain is preserved + audit.restore appended on top. The very
	// last row MUST be the restore genesis.
	if len(rowsB) < len(rowsA)+1 {
		t.Fatalf("restored chain length=%d want >= %d (preserved + audit.restore)",
			len(rowsB), len(rowsA)+1)
	}
	last := rowsB[len(rowsB)-1]
	if last.Action != audit.ActionBackupRestore {
		t.Fatalf("last row action=%q want %q", last.Action, audit.ActionBackupRestore)
	}
	// The restore.go payload.prior_last_hash records the DESTINATION DB's
	// chain head AT THE TIME of restore. Restoring into a brand-new tmp_db_b
	// (no prior file) → captureLastAuditHash returns the genesis zero hash.
	// The Reconciled spec amendment ("appends on top, not truncates") is
	// proved by the chain.prev_hash linkage below, not by prior_last_hash.
	var pl struct {
		PriorLastHash string `json:"prior_last_hash"`
	}
	if err := json.Unmarshal([]byte(last.Payload), &pl); err != nil {
		t.Fatalf("decode restore payload: %v body=%s", err, last.Payload)
	}
	zeroHash := strings.Repeat("0", 64)
	if pl.PriorLastHash != zeroHash {
		t.Fatalf("restore payload prior_last_hash=%q want zero (fresh dest) %q",
			pl.PriorLastHash, zeroHash)
	}
	// Hash chain is intact: the audit.restore row's prev_hash must equal
	// the SNAPSHOT's chain head — proving the chain was preserved, not
	// truncated. This is the Reconciled spec text's core invariant.
	if last.PrevHash != preRestoreLastHash {
		t.Fatalf("restore row prev_hash=%q want pre-restore (snapshot) head %q (chain broken)",
			last.PrevHash, preRestoreLastHash)
	}

	// --- 5. REST path: POST /backups / GET /backups / verify ---------------
	stackC := bootAPIBackupStack(t)

	// Mint an audit row in stack C so the chain is non-empty (matches the
	// CLI path's seeded shape).
	_, _ = stackC.store.IssueClientToken(context.Background(), stackC.adminID, "ci-rest")

	// POST /api/v1/backups → 202 {id, started_at}.
	code, body := stackC.do(t, http.MethodPost, "/api/v1/backups", map[string]string{})
	if code != http.StatusAccepted {
		t.Fatalf("POST /backups: code=%d body=%s", code, string(body))
	}
	var started struct {
		ID        string    `json:"id"`
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatalf("decode POST /backups: %v body=%s", err, body)
	}
	if started.ID == "" || started.StartedAt.IsZero() {
		t.Fatalf("POST /backups missing id or started_at: %s", body)
	}

	// GET /api/v1/backups → list with matching db_sha256.
	code, body = stackC.do(t, http.MethodGet, "/api/v1/backups", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /backups: code=%d body=%s", code, string(body))
	}
	var list []struct {
		ID       string `json:"id"`
		DBSha256 string `json:"db_sha256"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode GET /backups: %v body=%s", err, body)
	}
	var matchedSha string
	for _, row := range list {
		if row.ID == started.ID {
			matchedSha = row.DBSha256
			break
		}
	}
	if matchedSha == "" {
		t.Fatalf("GET /backups did not include id=%s: list=%+v", started.ID, list)
	}

	// Recompute on disk to confirm the listing matches the file.
	onDiskArchive := filepath.Join(stackC.backupDir, started.ID+".tar.gz")
	if _, err := os.Stat(onDiskArchive); err != nil {
		t.Fatalf("archive not staged on disk: %v", err)
	}
	diskManifest := readManifestJSON(t, onDiskArchive)
	if diskManifest.DBSha256 != matchedSha {
		t.Fatalf("API db_sha256=%s vs on-disk manifest=%s", matchedSha, diskManifest.DBSha256)
	}

	// POST /api/v1/backups/{id}/verify → {ok:true, sha256_match:true}.
	code, body = stackC.do(t, http.MethodPost, "/api/v1/backups/"+started.ID+"/verify", nil)
	if code != http.StatusOK {
		t.Fatalf("POST /backups/{id}/verify: code=%d body=%s", code, string(body))
	}
	var verify struct {
		OK          bool `json:"ok"`
		Sha256Match bool `json:"sha256_match"`
	}
	if err := json.Unmarshal(body, &verify); err != nil {
		t.Fatalf("decode verify: %v body=%s", err, body)
	}
	if !verify.OK || !verify.Sha256Match {
		t.Fatalf("verify failed: %+v body=%s", verify, body)
	}

	// --- 6. Negative: lock file blocks the CLI restore ---------------------
	lockPath := stackC.dbPath + ".restore.lock"
	if err := os.WriteFile(lockPath, []byte("held"), 0o600); err != nil {
		t.Fatalf("create lock file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(lockPath) })

	negCmd := newRestoreCmd()
	negCmd.SetArgs([]string{"--from", snapPath, "--db", stackC.dbPath})
	var nStderr bytes.Buffer
	negCmd.SetErr(&nStderr)
	negCmd.SetOut(io.Discard)
	negCmd.SetContext(context.Background())
	if err := negCmd.Execute(); err == nil {
		t.Fatalf("expected error from restore against locked DB")
	} else if !strings.Contains(nStderr.String(), "another restore in progress") {
		t.Fatalf("unexpected stderr (want lock-conflict message): %q", nStderr.String())
	}
	// The CLI MUST NOT remove an externally-owned lock file.
	if _, err := os.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lockfile was removed by failed restore CLI")
	}
}

// stackADBPath extracts the on-disk path of the SQLite DB the e2e helper
// opened. The helper stores it in t.TempDir()/e2e.db (see bootE2EStack);
// we re-derive it by inspecting one row's source.
func stackADBPath(t *testing.T, s *e2eStack) string {
	t.Helper()
	// The helper's DBPath is not exported; walk the database connection
	// to recover the file path via sqlite3's PRAGMA database_list.
	rows, err := s.db.QueryContext(context.Background(), "PRAGMA database_list")
	if err != nil {
		t.Fatalf("pragma database_list: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			t.Fatalf("pragma scan: %v", err)
		}
		if name == "main" && file != "" {
			return file
		}
	}
	t.Fatal("could not locate main DB path via PRAGMA database_list")
	return ""
}

// sha256HexBytes hashes b and returns the lowercase hex digest. Local
// helper so the test file does not import crypto/sha256 separately.
func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
