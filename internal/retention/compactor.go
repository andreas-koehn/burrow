// Package retention implements the daily data-retention compaction job and
// the settings accessors the job and HTTP API consume.
//
// Compaction is deliberately audit-chain-safe: only the six high-frequency
// "leaf" action types whose rows never sit between two structurally-linked
// chain rows are eligible for deletion. Rows whose action is NOT in
// EligibleAuditActions are left untouched even if they fall outside the
// configured window.
package retention

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// EligibleAuditActions is the closed set of action keys the daily compaction
// job will delete past audit.retention_days.  These are the six rate-limited
// "leaf" keys that are never referenced as structural chain links by upstream
// rows.
var EligibleAuditActions = []string{
	"redaction.applied",
	"guardrail.refused",
	"connection.session_summary",
	"retention.compact",
	"ai.cache.promoted",
	"ai.upstream_error",
}

// Settings holds the seven retention configuration knobs. Day values of 0
// mean "keep forever" (no deletion for that table). InspectorCount is a
// ring-buffer size, not compacted.
type Settings struct {
	AuditDays                int
	UsageDays                int
	RedactionDays            int
	ConnectionLogsDays       int
	ConnectionLogRollupsDays int
	WebhookDeliveriesDays    int
	InspectorCount           int // ring-buffer size; read-only from compactor's perspective
}

// Loader is the dependency the Compactor uses to read current settings before
// each run.  It is a separate interface so tests can inject a canned value
// without requiring a real database.
type Loader interface {
	Load(ctx context.Context) (Settings, error)
}

// AuditLogger is the dependency used to emit retention.compact audit events.
// *audit.Logger satisfies this interface via its Append method.
type AuditLogger interface {
	// AppendRetentionCompact records a compaction event for the given table.
	// The caller passes table name and rows-deleted count; the implementation
	// is responsible for mapping them to the audit chain format.
	AppendRetentionCompact(ctx context.Context, table string, rowsDeleted int) error
}

// Compactor holds the injected dependencies for the compaction job.
type Compactor struct {
	b        *db.DB
	settings Loader
	audit    AuditLogger
	log      *slog.Logger
}

// New constructs a Compactor.  All parameters are required; log may be a
// discard logger in tests.
func New(b *db.DB, ld Loader, al AuditLogger, log *slog.Logger) *Compactor {
	return &Compactor{b: b, settings: ld, audit: al, log: log}
}

// RunOnce executes every applicable DELETE for the current retention settings
// and returns per-table row counts.  The second call within the same compaction
// window will delete 0 rows because everything older than the cutoff has
// already been removed.
func (c *Compactor) RunOnce(ctx context.Context) (map[string]int, error) {
	s, err := c.settings.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("retention: load settings: %w", err)
	}

	now := time.Now().UTC()
	counts := map[string]int{}
	sqlDB := c.b.DB()

	// audit_events: only eligible actions, only when audit_days > 0
	if s.AuditDays > 0 {
		cutoff := now.Add(-time.Duration(s.AuditDays) * 24 * time.Hour)
		n, err := deleteAuditRows(ctx, sqlDB, cutoff)
		if err != nil {
			c.log.Error("retention: delete audit_events", "err", err)
		} else {
			counts["audit_events"] = n
		}
	}

	// usage_events
	if s.UsageDays > 0 {
		cutoff := now.Add(-time.Duration(s.UsageDays) * 24 * time.Hour)
		n, err := deleteWhere(ctx, sqlDB, "DELETE FROM usage_events WHERE ts < ?", cutoff)
		if err != nil {
			c.log.Error("retention: delete usage_events", "err", err)
		} else {
			counts["usage_events"] = n
		}
	}

	// connection_logs
	if s.ConnectionLogsDays > 0 {
		cutoff := now.Add(-time.Duration(s.ConnectionLogsDays) * 24 * time.Hour)
		n, err := deleteWhere(ctx, sqlDB, "DELETE FROM connection_logs WHERE created_at < ?", cutoff)
		if err != nil {
			c.log.Error("retention: delete connection_logs", "err", err)
		} else {
			counts["connection_logs"] = n
		}
	}

	// connection_log_rollups — keyed by DATE string (YYYY-MM-DD)
	if s.ConnectionLogRollupsDays > 0 {
		cutoffDay := now.AddDate(0, 0, -s.ConnectionLogRollupsDays).Format("2006-01-02")
		n, err := deleteWhere(ctx, sqlDB, "DELETE FROM connection_log_rollups WHERE day < ?", cutoffDay)
		if err != nil {
			c.log.Error("retention: delete connection_log_rollups", "err", err)
		} else {
			counts["connection_log_rollups"] = n
		}
	}

	// webhook_deliveries
	if s.WebhookDeliveriesDays > 0 {
		cutoff := now.Add(-time.Duration(s.WebhookDeliveriesDays) * 24 * time.Hour)
		n, err := deleteWhere(ctx, sqlDB, "DELETE FROM webhook_deliveries WHERE created_at < ?", cutoff)
		if err != nil {
			c.log.Error("retention: delete webhook_deliveries", "err", err)
		} else {
			counts["webhook_deliveries"] = n
		}
	}

	// client_login_requests: sign-in requests live ten minutes; whatever has
	// expired goes, independent of any retention setting.
	if n, err := c.b.DeleteExpiredClientLogins(ctx, now); err != nil {
		c.log.Error("retention: delete client_login_requests", "err", err)
	} else {
		counts["client_login_requests"] = n
	}

	// Emit one audit event per table with rows_deleted > 0.
	if c.audit != nil {
		for table, n := range counts {
			if n > 0 {
				if aerr := c.audit.AppendRetentionCompact(ctx, table, n); aerr != nil {
					c.log.Warn("retention: emit audit event", "table", table, "err", aerr)
				}
			}
		}
	}

	return counts, nil
}

// deleteAuditRows deletes rows from audit_events where action is in the
// eligible set AND ts < cutoff.
func deleteAuditRows(ctx context.Context, sqlDB *sql.DB, cutoff time.Time) (int, error) {
	placeholders := make([]string, len(EligibleAuditActions))
	args := make([]any, len(EligibleAuditActions)+1)
	for i, a := range EligibleAuditActions {
		placeholders[i] = "?"
		args[i] = a
	}
	args[len(EligibleAuditActions)] = cutoff.UTC()
	q := fmt.Sprintf(
		"DELETE FROM audit_events WHERE action IN (%s) AND ts < ?",
		strings.Join(placeholders, ","),
	)
	return execRowsAffected(ctx, sqlDB, q, args...)
}

// deleteWhere executes a single DELETE statement and returns rows affected.
func deleteWhere(ctx context.Context, sqlDB *sql.DB, q string, args ...any) (int, error) {
	return execRowsAffected(ctx, sqlDB, q, args...)
}

func execRowsAffected(ctx context.Context, sqlDB *sql.DB, q string, args ...any) (int, error) {
	res, err := sqlDB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// compactAtUTC default used by Tick when no schedule is given.
const defaultCompactAt = "00:30"

// Tick blocks until ctx is cancelled, firing RunOnce daily at compactAtUTC.
// compactAtUTC is an "HH:MM" string (UTC); an empty or invalid string falls
// back to "00:30".
func (c *Compactor) Tick(ctx context.Context, compactAtUTC string) {
	hhmm := compactAtUTC
	if hhmm == "" {
		hhmm = defaultCompactAt
	}
	h, m := parseHHMM(hhmm)

	for {
		next := nextFiring(time.Now().UTC(), h, m)
		c.log.Info("retention: next compaction", "at", next.Format(time.RFC3339))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		counts, err := c.RunOnce(ctx)
		if err != nil {
			c.log.Error("retention: compaction failed", "err", err)
		} else {
			c.log.Info("retention: compaction complete", "counts", counts)
		}
	}
}

// parseHHMM parses "HH:MM" and returns (hour, minute). On parse error it
// falls back to the default (00:30).
func parseHHMM(s string) (int, int) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 30 // default
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 30
	}
	return h, m
}

// nextFiring returns the next wall-clock moment in UTC when the compactor
// should fire.  If the target HH:MM today is still in the future, that is
// returned; otherwise the target HH:MM tomorrow is returned.
func nextFiring(now time.Time, h, m int) time.Time {
	target := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.UTC)
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target
}
