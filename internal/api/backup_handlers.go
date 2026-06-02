// backup_handlers.go — JSON API for the backup directory (spec Part L.3).
//
//	POST   /api/v1/backups                  -> 202 {id, started_at}
//	GET    /api/v1/backups                  -> [{id, taken_at, version, size_bytes, db_sha256, path}]
//	GET    /api/v1/backups/{id}/download    -> 200 application/x-gzip
//	POST   /api/v1/backups/{id}/verify      -> {ok, sha256_match}
//	DELETE /api/v1/backups/{id}             -> 204
//	POST   /api/v1/backups/restore          -> multipart -> 202 {restore_id}
//	GET    /api/v1/backups/restores/{id}    -> {status, error?}
//
// Permission model (spec Part L): admin OR backup:run. Both the CLI and the
// JSON layer use the same archive on-disk shape (db.sqlite + manifest.json
// + optional tls/* + config/burrow.yaml) so an operator can interchange
// archives produced by either path.
//
// The backup index is NOT stored in the settings table; instead the handlers
// scan the configured backup directory on each list / lookup. The simpler
// shape lets `burrowd backup` (CLI) and the JSON API coexist without a
// shared registry — a backup placed in the directory out-of-band by the CLI
// shows up in GET /api/v1/backups immediately.
//
// The restore route spawns the same in-process code path the CLI uses; the
// in-memory tracker keys progress by ULID. v0.4.0 keeps the runner simple
// (synchronous from the caller's perspective via the tracker), without
// shelling out — operators wanting the lock-file semantics still run
// `burrowd restore` from the shell.

package api

import (
	"archive/tar"
	"compress/gzip"
	"context"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// BackupArchiveExt is the on-disk suffix for archives produced by both the
// CLI and the JSON API. The directory scanner keys off this suffix so any
// other file co-located in the directory is ignored.
const BackupArchiveExt = ".tar.gz"

// BackupRunner is the testable seam the API uses to drive a snapshot+archive
// run from POST /api/v1/backups. cmd/server wires a concrete adapter that
// invokes the same code path as the CLI; tests substitute a fake.
type BackupRunner interface {
	// RunBackup writes a tar.gz to outPath. The implementation MUST refuse
	// to overwrite an existing file (the CLI semantics).
	RunBackup(ctx context.Context, outPath string) error
}

// AuditAppender is the narrow append-only surface the backup handlers use
// to write audit.backup.run / audit.backup.restore rows. *audit.Logger
// satisfies it via the trivial method-set match; an adapter for the
// existing AuditChain wrap is provided in cmd/server.
type AuditAppender interface {
	Append(ctx context.Context, e audit.Event) error
}

// RestoreRunner is the testable seam for POST /api/v1/backups/restore. The
// CLI shells the same code path inline; in v0.4.0 the API delegates to a
// thin adapter that calls into cmd/server's runRestore. Tests substitute a
// fake.
type RestoreRunner interface {
	// RunRestore extracts srcArchive and atomically swaps it into the live
	// DB path. dbPath is the destination database file.
	RunRestore(ctx context.Context, srcArchive string) error
}

// backupManifestWire is the shape backup_handlers.go expects to find in
// manifest.json. Mirrors cmd/server.BackupManifest. Defined locally so the
// API does not import cmd/server.
type backupManifestWire struct {
	Version         string    `json:"version"`
	ManifestVersion int       `json:"manifest_version"`
	TakenAt         time.Time `json:"taken_at"`
	DBSha256        string    `json:"db_sha256"`
	Included        []string  `json:"included"`
}

// backupRow is the wire shape of a single archive in the directory listing.
type backupRow struct {
	ID        string    `json:"id"`         // archive filename without the .tar.gz suffix
	Path      string    `json:"path"`       // absolute filesystem path (operator-facing)
	TakenAt   time.Time `json:"taken_at"`   // from manifest.json
	Version   string    `json:"version"`    // burrowd version that produced the archive
	SizeBytes int64     `json:"size_bytes"` // on-disk size in bytes
	DBSha256  string    `json:"db_sha256"`  // from manifest.json
}

// startBackupResp is the wire shape of POST /api/v1/backups (202).
type startBackupResp struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
}

// verifyResp is the wire shape of POST /api/v1/backups/{id}/verify (200).
type verifyResp struct {
	OK           bool   `json:"ok"`
	Sha256Match  bool   `json:"sha256_match"`
	ExpectedSha  string `json:"expected_sha256,omitempty"`
	ActualSha    string `json:"actual_sha256,omitempty"`
	ErrorMessage string `json:"error,omitempty"`
}

// restoreStartResp is the wire shape of POST /api/v1/backups/restore (202).
type restoreStartResp struct {
	RestoreID string `json:"restore_id"`
}

// restoreStatusResp is the wire shape of GET /api/v1/backups/restores/{id}.
type restoreStatusResp struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`          // "running" | "done" | "failed"
	Error     string    `json:"error,omitempty"` // populated when status == "failed"
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// restoreTracker is the in-memory progress map shared across handler calls.
// The zero value is unusable; cmd/server constructs a singleton and injects
// it via Deps.RestoreTracker.
type restoreTracker struct {
	mu  sync.Mutex
	rec map[string]restoreStatusResp
}

// NewRestoreTracker builds a fresh tracker. Exported so cmd/server may
// construct a singleton without importing internal helpers.
func NewRestoreTracker() *restoreTracker {
	return &restoreTracker{rec: map[string]restoreStatusResp{}}
}

func (t *restoreTracker) start(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rec[id] = restoreStatusResp{ID: id, Status: "running", StartedAt: time.Now().UTC()}
}

func (t *restoreTracker) finish(id string, runErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.rec[id]
	r.EndedAt = time.Now().UTC()
	if runErr != nil {
		r.Status = "failed"
		r.Error = runErr.Error()
	} else {
		r.Status = "done"
	}
	t.rec[id] = r
}

func (t *restoreTracker) get(id string) (restoreStatusResp, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rec[id]
	return r, ok
}

// --- Permission gate --------------------------------------------------------

// requireBackupRun is the admin OR backup:run gate. Cookie callers resolve
// their role via callerRoleForAuth (bearer-set ctx wins; otherwise fresh
// GetUserByID). Same shape as requireWebhooksManage.
func (d Deps) requireBackupRun(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := d.callerRoleForAuth(r)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if role == "admin" || effectivePerms(r.Context(), role, authz.PermBackupRun) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "backup:run required")
	})
}

// --- Helpers ---------------------------------------------------------------

// readManifestFromArchive opens path, scans the tar.gz for manifest.json,
// and returns its decoded shape. Used by the list/get and verify handlers.
func readManifestFromArchive(path string) (backupManifestWire, error) {
	var empty backupManifestWire
	f, err := os.Open(path)
	if err != nil {
		return empty, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return empty, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return empty, errors.New("manifest.json not found in archive")
		}
		if err != nil {
			return empty, fmt.Errorf("tar next: %w", err)
		}
		if hdr.Name != "manifest.json" {
			continue
		}
		var m backupManifestWire
		dec := json.NewDecoder(io.LimitReader(tr, 1<<20))
		if err := dec.Decode(&m); err != nil {
			return empty, fmt.Errorf("decode manifest: %w", err)
		}
		return m, nil
	}
}

// recomputeDBSha256 streams the archive's db.sqlite entry through a SHA-256
// hasher and returns the lowercase hex digest. Used by POST .../verify.
func recomputeDBSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("db.sqlite not found in archive")
		}
		if err != nil {
			return "", err
		}
		if hdr.Name != "db.sqlite" {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
}

// scanBackupDir lists all *.tar.gz files in d.BackupDir, decodes each
// manifest, and returns a sorted result (newest first).
func (d Deps) scanBackupDir() ([]backupRow, error) {
	if d.BackupDir == "" {
		return nil, errors.New("backup directory not configured")
	}
	if err := os.MkdirAll(d.BackupDir, 0o700); err != nil {
		return nil, fmt.Errorf("backup dir: %w", err)
	}
	entries, err := os.ReadDir(d.BackupDir)
	if err != nil {
		return nil, err
	}
	var rows []backupRow
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, BackupArchiveExt) {
			continue
		}
		full := filepath.Join(d.BackupDir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		m, err := readManifestFromArchive(full)
		if err != nil {
			// Skip unreadable archives — the operator may have a malformed
			// file in the directory; expose only what we can describe.
			continue
		}
		rows = append(rows, backupRow{
			ID:        strings.TrimSuffix(name, BackupArchiveExt),
			Path:      full,
			TakenAt:   m.TakenAt,
			Version:   m.Version,
			SizeBytes: info.Size(),
			DBSha256:  m.DBSha256,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].TakenAt.After(rows[j].TakenAt)
	})
	return rows, nil
}

// resolveArchivePath returns the safe absolute path of the archive named by
// id, refusing path traversal and any extension other than .tar.gz.
func (d Deps) resolveArchivePath(id string) (string, error) {
	if id == "" {
		return "", errors.New("id is required")
	}
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return "", errors.New("invalid id")
	}
	if d.BackupDir == "" {
		return "", errors.New("backup directory not configured")
	}
	full := filepath.Join(d.BackupDir, id+BackupArchiveExt)
	cleanDir, err := filepath.Abs(d.BackupDir)
	if err != nil {
		return "", err
	}
	cleanFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(cleanDir, cleanFull)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("invalid id")
	}
	return cleanFull, nil
}

// --- Handlers --------------------------------------------------------------

// GetBackups handles GET /api/v1/backups.
func (d Deps) GetBackups(w http.ResponseWriter, r *http.Request) {
	rows, err := d.scanBackupDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	if rows == nil {
		rows = []backupRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// PostBackup handles POST /api/v1/backups.
//
// Returns 202 with the synthesised id and started_at. The actual snapshot
// happens synchronously inside the handler — by the time the response
// returns the .tar.gz already exists at <BackupDir>/<id>.tar.gz. The 202
// status is preserved to match the spec wire shape (the caller may poll
// GET /api/v1/backups to confirm the archive landed).
func (d Deps) PostBackup(w http.ResponseWriter, r *http.Request) {
	if d.BackupDir == "" {
		writeErr(w, http.StatusInternalServerError, "backup directory not configured")
		return
	}
	if d.BackupRunner == nil {
		writeErr(w, http.StatusInternalServerError, "backup runner unavailable")
		return
	}
	if err := os.MkdirAll(d.BackupDir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "ensure backup dir: "+err.Error())
		return
	}
	started := time.Now().UTC()
	id := started.Format("20060102T150405Z") + "-" + randomTag()
	outPath := filepath.Join(d.BackupDir, id+BackupArchiveExt)

	if err := d.BackupRunner.RunBackup(r.Context(), outPath); err != nil {
		writeErr(w, http.StatusInternalServerError, "backup failed: "+err.Error())
		return
	}
	// audit.backup.run (best-effort).
	if d.AuditAppender != nil {
		actorID := userID(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID:      actorID,
			Action:       audit.ActionBackupRun,
			Result:       "ok",
			SubjectLabel: id,
		})
	}
	writeJSON(w, http.StatusAccepted, startBackupResp{ID: id, StartedAt: started})
}

// GetBackupDownload streams the archive at id as application/x-gzip.
func (d Deps) GetBackupDownload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path, err := d.resolveArchivePath(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "backup not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "open failed")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err == nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	}
	w.Header().Set("Content-Type", "application/x-gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+BackupArchiveExt))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// PostBackupVerify handles POST /api/v1/backups/{id}/verify — recomputes
// the archive's db.sqlite sha256 and compares to the manifest.
func (d Deps) PostBackupVerify(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path, err := d.resolveArchivePath(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(path); err != nil {
		writeErr(w, http.StatusNotFound, "backup not found")
		return
	}
	m, err := readManifestFromArchive(path)
	if err != nil {
		writeJSON(w, http.StatusOK, verifyResp{OK: false, ErrorMessage: err.Error()})
		return
	}
	actual, err := recomputeDBSha256(path)
	if err != nil {
		writeJSON(w, http.StatusOK, verifyResp{OK: false, ErrorMessage: err.Error()})
		return
	}
	match := strings.EqualFold(actual, m.DBSha256)
	writeJSON(w, http.StatusOK, verifyResp{
		OK:          match,
		Sha256Match: match,
		ExpectedSha: m.DBSha256,
		ActualSha:   actual,
	})
}

// DeleteBackup unlinks the named archive.
func (d Deps) DeleteBackup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path, err := d.resolveArchivePath(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "backup not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostBackupRestore handles POST /api/v1/backups/restore. Body is a
// multipart upload of the archive; the handler stages it in the configured
// backup directory and (when a runner is wired) drives a real restore.
//
// In v0.4.0 the in-memory tracker is keyed by ULID; the route returns 202
// immediately, then the handler runs the restore inline before unlocking
// the tracker. Spawning a separate `burrowd restore` subprocess is left as
// a v1.0 hardening item — the multipart staging + tracker shape is the
// stable surface the dashboard uses today.
//
// On any failure before the runner is invoked the tracker is marked
// "failed" and the corresponding error message is surfaced via the GET
// status endpoint.
func (d Deps) PostBackupRestore(w http.ResponseWriter, r *http.Request) {
	if d.BackupDir == "" {
		writeErr(w, http.StatusInternalServerError, "backup directory not configured")
		return
	}
	if d.RestoreTracker == nil {
		writeErr(w, http.StatusInternalServerError, "restore tracker unavailable")
		return
	}
	// 64 MiB upload cap.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid multipart body")
		return
	}
	file, _, err := r.FormFile("archive")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing form field 'archive'")
		return
	}
	defer file.Close()

	restoreID := time.Now().UTC().Format("20060102T150405Z") + "-" + randomTag()
	stagedPath := filepath.Join(d.BackupDir, "restore-"+restoreID+BackupArchiveExt)
	out, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stage upload: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		_ = out.Close()
		_ = os.Remove(stagedPath)
		writeErr(w, http.StatusInternalServerError, "stage upload: "+err.Error())
		return
	}
	_ = out.Close()

	d.RestoreTracker.start(restoreID)
	go func(srcPath, id string) {
		defer os.Remove(srcPath)
		var runErr error
		if d.RestoreRunner == nil {
			runErr = errors.New("restore runner unavailable: use 'burrowd restore' CLI to apply the archive")
		} else {
			runErr = d.RestoreRunner.RunRestore(context.Background(), srcPath)
		}
		d.RestoreTracker.finish(id, runErr)

		// audit.backup.restore — spec Part L mandates a single row whose
		// payload carries the manifest's version + taken_at. We append it
		// even on failure (with the recorded error) so the chain reflects
		// the attempt; the CLI's restore_genesis path appends one too when
		// the swap succeeds and we go through the in-process runner.
		if d.AuditAppender != nil {
			m, mErr := readManifestFromArchive(srcPath)
			payload, _ := json.Marshal(struct {
				Version  string    `json:"version"`
				TakenAt  time.Time `json:"taken_at"`
				DBSha256 string    `json:"db_sha256"`
				Error    string    `json:"error,omitempty"`
			}{
				Version:  m.Version,
				TakenAt:  m.TakenAt,
				DBSha256: m.DBSha256,
				Error: func() string {
					if runErr != nil {
						return runErr.Error()
					}
					return ""
				}(),
			})
			result := "ok"
			if runErr != nil || mErr != nil {
				result = "error"
			}
			_ = d.AuditAppender.Append(context.Background(), audit.Event{
				Action:       audit.ActionBackupRestore,
				Result:       result,
				SubjectLabel: id,
				Payload:      payload,
			})
		}
	}(stagedPath, restoreID)

	writeJSON(w, http.StatusAccepted, restoreStartResp{RestoreID: restoreID})
}

// GetBackupRestoreStatus handles GET /api/v1/backups/restores/{id}.
func (d Deps) GetBackupRestoreStatus(w http.ResponseWriter, r *http.Request) {
	if d.RestoreTracker == nil {
		writeErr(w, http.StatusInternalServerError, "restore tracker unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	rec, ok := d.RestoreTracker.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "restore not found")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// randomTag returns 8 random lowercase hex bytes; combined with the unix
// timestamp this is plenty for an archive id within a directory.
func randomTag() string {
	var b [4]byte
	_, _ = io.ReadFull(cryptoRand.Reader, b[:])
	return hex.EncodeToString(b[:])
}
