// restore.go — `burrowd restore` subcommand.
//
// Spec Part L (locked invariants):
//   - Restore refuses when the target database lock file already exists
//     (<db>.restore.lock, O_CREATE|O_EXCL). The lock is the only safeguard
//     against running the CLI while burrowd serve is also up — the operator
//     is expected to stop the server before restoring.
//   - The archive is extracted into a temp directory, the manifest's
//     db_sha256 is recomputed and matched against the extracted db.sqlite
//     before any swap touches the live filesystem.
//   - The swap is atomic: the existing <db> is renamed to <db>.pre-restore
//     and the extracted file is renamed into <db>'s place. If the swap
//     succeeds the audit chain in the new DB is reset with a single
//     audit.restore genesis row recording the pre-restore last hash.
//   - On any failure the lock file is removed and the original DB stays
//     untouched (the temp dir is cleaned up by defer).

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// MaxArchiveEntrySize bounds a single entry's size when extracting. A
// malformed (or malicious) tar.gz cannot blow up host memory or disk by
// claiming gigabyte-sized entries: any entry larger than this is rejected.
// 2 GiB is well above any plausible burrow.db size.
const MaxArchiveEntrySize = int64(2 << 30)

// restoreOptions captures the operator-supplied flags + resolved paths the
// CLI passes into runRestore. Same testable-seam pattern as backupOptions.
type restoreOptions struct {
	From   string // source .tar.gz
	DBPath string // destination SQLite database (will be replaced)
}

// newRestoreCmd returns the `burrowd restore` cobra subcommand.
//
// Usage:
//
//	burrowd restore --from <file.tar.gz> [--db <path>]
//
// Exit codes:
//
//	0 — restore succeeded (db replaced; pre-restore renamed to <db>.pre-restore)
//	1 — any error: lock held, manifest mismatch, swap failed, etc.
func newRestoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore the burrow database from a backup tar.gz",
		Long: `Acquires an exclusive lock file at <db>.restore.lock, extracts
the archive into a temp directory, verifies the manifest's db_sha256 against
the extracted db.sqlite, then atomically swaps the file into place. The
prior database is preserved at <db>.pre-restore (the operator may delete it
after confirming the restore looks correct). A single audit.restore genesis
event is appended to the restored chain, recording the previous chain's
last hash in payload.

The CLI refuses to run while another restore is in progress (lock file
present). It is the operator's responsibility to stop burrowd serve before
running this command.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, _ := cmd.Flags().GetString("from")
			dbPath, _ := cmd.Flags().GetString("db")
			if from == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "error: --from is required")
				return cobraExit1()
			}
			opts, err := resolveRestoreOptions(dbPath, from)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
				return cobraExit1()
			}
			if err := runRestore(cmd.Context(), opts, cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
				return cobraExit1()
			}
			return nil
		},
	}
	cmd.Flags().String("from", "", "source .tar.gz archive (required)")
	cmd.Flags().String("db", "", "path to burrow.db (default: $BURROW_DATABASE_PATH or ./burrow.db)")
	return cmd
}

// resolveRestoreOptions normalises CLI flags + the same config-resolution
// path the serve command uses, so `burrowd restore` "just works" against an
// existing deployment.
func resolveRestoreOptions(dbPath, from string) (restoreOptions, error) {
	opts := restoreOptions{From: from}
	if dbPath != "" {
		opts.DBPath = dbPath
	} else {
		cfg, err := config.LoadServer(nil)
		if err != nil {
			opts.DBPath = "./burrow.db"
		} else {
			opts.DBPath = cfg.DatabasePath
		}
	}
	if _, err := os.Stat(opts.From); err != nil {
		return opts, fmt.Errorf("archive %s: %w", opts.From, err)
	}
	return opts, nil
}

// runRestore is the testable seam for the restore command. See file header
// for the locked invariants.
func runRestore(ctx context.Context, opts restoreOptions, stdout io.Writer) error {
	if opts.From == "" {
		return errors.New("restore: --from is required")
	}
	if opts.DBPath == "" {
		return errors.New("restore: --db is required")
	}

	// Acquire the lock file (O_CREATE|O_EXCL). The presence of the file
	// is the lock — owners are responsible for cleaning it up. We always
	// remove it on exit; a crashed CLI leaves a stale lock that the
	// operator must rm before retrying.
	lockPath := opts.DBPath + ".restore.lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("restore: another restore in progress (lock %s exists)", lockPath)
		}
		return fmt.Errorf("restore: lock: %w", err)
	}
	_ = lf.Close()
	defer os.Remove(lockPath)

	// Extract into a sibling temp directory so the eventual os.Rename
	// stays on the same filesystem.
	dbDir := filepath.Dir(opts.DBPath)
	tmpDir, err := os.MkdirTemp(dbDir, ".burrow-restore-*")
	if err != nil {
		return fmt.Errorf("restore: temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractArchive(opts.From, tmpDir); err != nil {
		return fmt.Errorf("restore: extract: %w", err)
	}

	manifestPath := filepath.Join(tmpDir, "manifest.json")
	mbytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("restore: read manifest: %w", err)
	}
	var manifest BackupManifest
	if err := json.Unmarshal(mbytes, &manifest); err != nil {
		return fmt.Errorf("restore: parse manifest: %w", err)
	}
	if manifest.ManifestVersion != 0 && manifest.ManifestVersion > ManifestVersion {
		return fmt.Errorf("restore: manifest_version %d is newer than this binary supports (%d)",
			manifest.ManifestVersion, ManifestVersion)
	}

	extractedDB := filepath.Join(tmpDir, "db.sqlite")
	gotSha, err := sha256File(extractedDB)
	if err != nil {
		return fmt.Errorf("restore: hash db.sqlite: %w", err)
	}
	if !strings.EqualFold(gotSha, manifest.DBSha256) {
		return fmt.Errorf("restore: db_sha256 mismatch: archive=%s actual=%s",
			manifest.DBSha256, gotSha)
	}

	// Capture the prior chain's last hash so it can be embedded in the new
	// genesis audit.restore payload. If the destination DB doesn't exist
	// yet we use the zero-hash sentinel (genesis prev_hash).
	priorHash := captureLastAuditHash(ctx, opts.DBPath)

	// Atomic swap: rename existing → .pre-restore, then extracted → live.
	preRestore := opts.DBPath + ".pre-restore"
	if _, err := os.Stat(opts.DBPath); err == nil {
		// Remove any stale .pre-restore from a previous run so the rename
		// below isn't blocked on Windows (where rename-over is allowed but
		// rename-to-existing-non-empty-dir would fail on POSIX).
		_ = os.Remove(preRestore)
		if err := os.Rename(opts.DBPath, preRestore); err != nil {
			return fmt.Errorf("restore: preserve original: %w", err)
		}
	}
	if err := os.Rename(extractedDB, opts.DBPath); err != nil {
		// Best-effort revert: put the original back so the operator is not
		// left without a database.
		_ = os.Rename(preRestore, opts.DBPath)
		return fmt.Errorf("restore: install snapshot: %w", err)
	}
	// Clear out any sqlite -wal / -shm sidecars from the prior database so
	// the restored file opens cleanly: WAL recovery would otherwise try to
	// apply pages from an unrelated journal.
	_ = os.Remove(opts.DBPath + "-wal")
	_ = os.Remove(opts.DBPath + "-shm")

	// Append the audit.restore genesis event. The Logger.Append code path
	// reads the (now-restored) chain's head; on a fresh restore the head
	// already exists (it's the chain from the snapshot). The payload
	// records the operator-meaningful provenance fields plus the prior
	// chain's last hash so a forensic auditor can confirm the cut-over.
	if err := writeRestoreGenesis(ctx, opts.DBPath, manifest, priorHash); err != nil {
		// Non-fatal: the database swap already succeeded. We log the
		// failure to stderr but still return 0 so the operator does not
		// re-run the swap. They can manually re-append a marker.
		fmt.Fprintf(os.Stderr, "warning: restore audit append failed: %v\n", err)
	}

	if stdout != nil {
		fmt.Fprintf(stdout, "Restored %s from %s (prior preserved at %s)\n",
			opts.DBPath, opts.From, preRestore)
	}
	return nil
}

// extractArchive untars a .tar.gz archive into dstDir, refusing path
// traversal entries (..) and any entry that would write outside dstDir.
func extractArchive(srcPath, dstDir string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	cleanDst, err := filepath.Abs(dstDir)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if hdr.Size > MaxArchiveEntrySize {
			return fmt.Errorf("entry %q exceeds size cap (%d bytes)", hdr.Name, hdr.Size)
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes archive root", hdr.Name)
		}
		target := filepath.Join(cleanDst, clean)
		// Belt-and-braces: ensure target is still rooted under cleanDst.
		rel, err := filepath.Rel(cleanDst, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("tar entry %q escapes archive root", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			n, copyErr := io.CopyN(out, tr, hdr.Size)
			_ = out.Close()
			if copyErr != nil && !errors.Is(copyErr, io.EOF) {
				return fmt.Errorf("copy %s: %w", target, copyErr)
			}
			if n != hdr.Size {
				return fmt.Errorf("copy %s: short read (%d/%d)", target, n, hdr.Size)
			}
		default:
			// Symlinks / devices etc. are not produced by writeBackupArchive
			// — refusing them is consistent with a closed, audited shape.
			return fmt.Errorf("unsupported tar entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
	}
	return nil
}

// captureLastAuditHash opens the destination DB (if it exists) and reads
// the audit-chain head so it can be embedded in the new genesis row. On
// any failure (no file yet, schema missing) it returns the genesis zero
// hash — restore is still allowed; the marker just records "no prior".
func captureLastAuditHash(ctx context.Context, dbPath string) string {
	if _, err := os.Stat(dbPath); err != nil {
		return strings.Repeat("0", 64)
	}
	sqldb, err := db.Open(dbPath)
	if err != nil {
		return strings.Repeat("0", 64)
	}
	defer sqldb.Close()
	x := db.Wrap(sqldb)
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return strings.Repeat("0", 64)
	}
	defer func() { _ = tx.Rollback() }()
	prev, ok, err := x.LatestAuditHash(ctx, tx)
	if err != nil || !ok {
		return strings.Repeat("0", 64)
	}
	return prev
}

// writeRestoreGenesis appends a single audit.restore row to the just-
// restored database. The payload records the manifest's version + taken_at
// + db_sha256 and the prior chain's last hash so an offline verifier can
// confirm continuity across the cut-over.
func writeRestoreGenesis(ctx context.Context, dbPath string, manifest BackupManifest, priorHash string) error {
	sqldb, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open restored db: %w", err)
	}
	defer sqldb.Close()
	if err := db.Migrate(sqldb); err != nil {
		return fmt.Errorf("migrate restored db: %w", err)
	}
	x := db.Wrap(sqldb)
	st := store.New(sqldb)
	priv, err := audit.LoadOrGenerateSigningKey(ctx, st)
	if err != nil {
		return fmt.Errorf("audit key: %w", err)
	}
	logger := audit.NewLogger(x, priv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	payload, _ := json.Marshal(struct {
		Version         string `json:"version"`
		ManifestVersion int    `json:"manifest_version"`
		TakenAt         string `json:"taken_at"`
		DBSha256        string `json:"db_sha256"`
		PriorLastHash   string `json:"prior_last_hash"`
	}{
		Version:         manifest.Version,
		ManifestVersion: manifest.ManifestVersion,
		TakenAt:         manifest.TakenAt.Format("2006-01-02T15:04:05Z07:00"),
		DBSha256:        manifest.DBSha256,
		PriorLastHash:   priorHash,
	})
	return logger.Append(ctx, audit.Event{
		Action:       audit.ActionBackupRestore,
		Result:       "ok",
		SubjectLabel: "burrowd restore",
		Payload:      payload,
	})
}
