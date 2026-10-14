// backup_wiring.go — cmd/server adapters that bridge runBackup / runRestore
// into the internal/api BackupRunner / RestoreRunner seams.
//
// The API package owns the JSON wire shape; cmd/server owns the actual
// snapshot+archive code path. These thin adapters expose the latter via the
// former's interfaces so neither package has to know about the other's
// concrete types.

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/ankoehn/burrow/internal/config"
)

// backupRunnerAdapter satisfies api.BackupRunner by delegating to runBackup
// with the deployment's resolved database path + the operator-supplied
// TLS cert/key (so dashboard-initiated backups carry the same set as the
// CLI does).
type backupRunnerAdapter struct {
	cfg *config.ServerConfig
	// events announces a finished backup as "backup.completed". nil =
	// nobody is told.
	events eventPublisher
}

func (a backupRunnerAdapter) RunBackup(ctx context.Context, outPath string) error {
	opts := backupOptions{
		DBPath:  a.cfg.DatabasePath,
		OutPath: outPath,
		TLSCert: a.cfg.HTTPTLSCert,
		TLSKey:  a.cfg.HTTPTLSKey,
	}
	// stdout is unused on the dashboard path; pass io.Discard.
	if err := runBackup(ctx, opts, io.Discard); err != nil {
		return err
	}
	if a.events != nil {
		var size int64
		if fi, err := os.Stat(outPath); err == nil {
			size = fi.Size()
		}
		// The archive's name, not where the relay keeps it.
		a.events.Publish(context.Background(), "backup.completed", map[string]any{
			"Path": filepath.Base(outPath), "SizeBytes": size,
		})
	}
	return nil
}

// restoreRunnerAdapter satisfies api.RestoreRunner. The destination DB path
// is fixed at wiring time so an attacker who can upload an archive cannot
// also pick a path to overwrite.
type restoreRunnerAdapter struct {
	cfg *config.ServerConfig
}

func (a restoreRunnerAdapter) RunRestore(ctx context.Context, srcArchive string) error {
	opts := restoreOptions{
		From:   srcArchive,
		DBPath: a.cfg.DatabasePath,
	}
	return runRestore(ctx, opts, io.Discard)
}
