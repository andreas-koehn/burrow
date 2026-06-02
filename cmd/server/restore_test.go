package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// rewriteArchiveWithGarbageDB reads srcArchive's manifest.json verbatim (so
// the manifest's db_sha256 still claims the ORIGINAL snapshot bytes) and
// writes a NEW archive whose db.sqlite entry is garbage. The CLI must catch
// the mismatch before touching the live database.
func rewriteArchiveWithGarbageDB(t *testing.T, srcArchive, dstArchive string) {
	t.Helper()
	entries := readArchive(t, srcArchive)
	manifestBytes, ok := entries["manifest.json"]
	if !ok {
		t.Fatal("source archive missing manifest.json")
	}
	out, err := os.Create(dstArchive)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	garbage := []byte("this is not a sqlite file")
	if err := tw.WriteHeader(&tar.Header{
		Name: "db.sqlite", Mode: 0o600, Size: int64(len(garbage)),
		ModTime: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(garbage); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "manifest.json", Mode: 0o644, Size: int64(len(manifestBytes)),
		ModTime: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		t.Fatal(err)
	}
}

// writeTraversalArchive emits a tar.gz with a single "../escape.txt" entry,
// used to verify the extractor refuses path traversal.
func writeTraversalArchive(t *testing.T, path string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	body := []byte("payload")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escape.txt", Mode: 0o600, Size: int64(len(body)),
		ModTime: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
}

// makeBackup is a test helper that drives runBackup on a freshly migrated
// DB and returns the resulting archive path.
func makeBackup(t *testing.T, dir, name string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "src.db")
	outPath := filepath.Join(dir, name)
	seedRestorableDB(t, dbPath)
	if err := runBackup(context.Background(), backupOptions{
		DBPath: dbPath, OutPath: outPath,
	}, io.Discard); err != nil {
		t.Fatalf("makeBackup: %v", err)
	}
	return outPath
}

// TestRestoreCLI_FreshRestore covers the happy path: an archive is restored
// into a NEW db path (no prior file) and the CLI succeeds with stdout
// confirmation. The new DB must hold the schema (we re-Migrate to confirm
// it's a usable SQLite file) and an audit.restore genesis row.
func TestRestoreCLI_FreshRestore(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	archive := makeBackup(t, src, "backup.tar.gz")
	dbPath := filepath.Join(dst, "burrow.db")

	cmd := newRestoreCmd()
	cmd.SetArgs([]string{"--from", archive, "--db", dbPath})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%q)", err, stderr.String())
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("restored db missing: %v", err)
	}
	if !strings.Contains(stdout.String(), "Restored ") {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}

	// Re-open and assert at least one audit.restore row exists.
	sqldb, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	x := db.Wrap(sqldb)
	rows, err := x.ListAuditEvents(context.Background(), db.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Action == audit.ActionBackupRestore {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no audit.restore row appended; rows=%d", len(rows))
	}
}

// TestRestoreCLI_LockfileBlocksConcurrentRun creates a stale lock and asserts
// the CLI refuses with the expected error.
func TestRestoreCLI_LockfileBlocksConcurrentRun(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	archive := makeBackup(t, src, "backup.tar.gz")
	dbPath := filepath.Join(dst, "burrow.db")
	lockPath := dbPath + ".restore.lock"
	if err := os.WriteFile(lockPath, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newRestoreCmd()
	cmd.SetArgs([]string{"--from", archive, "--db", dbPath})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(io.Discard)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected error: lock held")
	}
	if !strings.Contains(stderr.String(), "another restore in progress") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	// CLI MUST NOT remove an externally-owned lock file.
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock removed by failed restore: %v", err)
	}
}

// TestRestoreCLI_ShaMismatchAborts tampers with the archive after backup so
// the recomputed sha256 no longer matches the manifest. The restore CLI must
// abort BEFORE touching the live database.
func TestRestoreCLI_ShaMismatchAborts(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	archive := makeBackup(t, src, "backup.tar.gz")
	dbPath := filepath.Join(dst, "burrow.db")
	// Seed an existing DB so we can confirm it survives untouched.
	seedRestorableDB(t, dbPath)
	preStat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Tamper: rewrite the archive's db.sqlite entry with garbage. We rebuild
	// the tar.gz from scratch to keep the rest of the wire format valid.
	tampered := filepath.Join(src, "tampered.tar.gz")
	rewriteArchiveWithGarbageDB(t, archive, tampered)

	cmd := newRestoreCmd()
	cmd.SetArgs([]string{"--from", tampered, "--db", dbPath})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(io.Discard)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected restore to abort on sha mismatch")
	}
	if !strings.Contains(stderr.String(), "db_sha256 mismatch") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	// Original database must still be there and unchanged in size.
	postStat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("original db missing after failed restore: %v", err)
	}
	if postStat.Size() != preStat.Size() {
		t.Fatalf("original db modified by failed restore: pre=%d post=%d",
			preStat.Size(), postStat.Size())
	}
}

// TestRestoreCLI_PathTraversalRejected asserts a malicious archive carrying
// a "../" entry is refused before any swap touches disk.
func TestRestoreCLI_PathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	badArchive := filepath.Join(dir, "evil.tar.gz")
	writeTraversalArchive(t, badArchive)

	cmd := newRestoreCmd()
	cmd.SetArgs([]string{"--from", badArchive, "--db", filepath.Join(dir, "burrow.db")})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(io.Discard)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected error on path-traversal archive")
	}
	if !strings.Contains(stderr.String(), "escapes archive root") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}
