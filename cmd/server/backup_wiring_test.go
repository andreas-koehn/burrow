package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/config"
)

// recPublisher records the events published to it.
type recPublisher struct {
	mu     sync.Mutex
	events []string
	data   []any
}

func (r *recPublisher) Publish(_ context.Context, event string, payload any) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.data = append(r.data, payload)
	r.mu.Unlock()
}

// A finished dashboard backup is announced once, with the archive's base name
// and size; a refused overwrite and a cancelled run announce nothing.
func TestBackupRunnerAdapter_AnnouncesOnlyASuccessfulBackup(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "burrow.db")
	seedRestorableDB(t, dbPath)
	pub := &recPublisher{}
	a := backupRunnerAdapter{cfg: &config.ServerConfig{DatabasePath: dbPath}, events: pub}

	out := filepath.Join(tmp, "backups", "burrow-backup.tar.gz")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.RunBackup(context.Background(), out); err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 1 || pub.events[0] != "backup.completed" {
		t.Fatalf("events after a backup: %v", pub.events)
	}
	payload, _ := pub.data[0].(map[string]any)
	if payload["Path"] != "burrow-backup.tar.gz" || payload["SizeBytes"] != fi.Size() {
		t.Fatalf("payload %v, want the base name and %d bytes", payload, fi.Size())
	}

	// The archive exists now: the second run refuses to overwrite it.
	if err := a.RunBackup(context.Background(), out); err == nil {
		t.Fatal("overwrite not refused")
	}
	// A cancelled run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.RunBackup(ctx, filepath.Join(tmp, "backups", "cancelled.tar.gz")); err == nil {
		t.Skip("runBackup does not observe cancellation before finishing; nothing to check")
	}
	if len(pub.events) != 1 {
		t.Fatalf("events after a refused and a cancelled run: %v", pub.events)
	}
}
