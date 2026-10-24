package customdomain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// StatusTickDeps is the narrow surface StatusTick consumes. Each field may be
// nil — nil DB makes StatusTick a no-op; nil Audit / Webhook silently skip
// the corresponding side-effect (matches Burrow's "degrade gracefully"
// convention for periodic jobs).
type StatusTickDeps struct {
	// DB lists cert-bearing custom domains and persists transitions.
	// *db.DB satisfies it directly.
	DB StatusTickDB

	// Audit appends a custom_domain.status_changed event per transition.
	// *audit.Logger satisfies it directly (Append method).
	Audit StatusTickAudit

	// Webhook fires custom_domain.cert.expiring on active -> cert_expiring
	// edges. *webhook.Dispatcher satisfies it directly (Publish method).
	Webhook StatusTickWebhook

	// Log receives one info line per tick with the transition count and a
	// warn line for unexpected DB / audit / webhook errors. Nil falls back
	// to slog.Default().
	Log *slog.Logger
}

// StatusTickDB is the database surface the tick consumes.
type StatusTickDB interface {
	ListAllCustomDomains(ctx context.Context) ([]db.ServiceCustomDomain, error)
	UpdateCustomDomainStatus(ctx context.Context, id, status string) error
}

// StatusTickAudit is the audit surface the tick consumes.
type StatusTickAudit interface {
	Append(ctx context.Context, e audit.Event) error
}

// StatusTickWebhook is the webhook surface the tick consumes.
type StatusTickWebhook interface {
	Publish(ctx context.Context, event string, payload any)
}

// RunStatusTick walks every cert-bearing custom domain, computes the status
// it should hold at `now`, and persists transitions. For each transition it
// appends a custom_domain.status_changed audit event; on the
// `active -> cert_expiring` edge specifically it fires a one-shot
// custom_domain.cert.expiring webhook.
//
// Rows already in StatusPending are skipped — they are reserved for future
// ACME / auto-renew flows and have no cert to evaluate. Rows whose stored
// status equals the computed status are also skipped (the once-per-edge fire
// invariant: a second tick with no underlying clock change emits zero
// webhooks).
//
// Returns the number of persisted transitions. A nil error indicates the
// list query succeeded; per-row update / audit / webhook errors are logged
// and counted toward the transition tally only when the UPDATE succeeded.
func RunStatusTick(ctx context.Context, deps StatusTickDeps, now time.Time) (int, error) {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	if deps.DB == nil {
		return 0, nil
	}
	rows, err := deps.DB.ListAllCustomDomains(ctx)
	if err != nil {
		return 0, fmt.Errorf("custom-domain status tick: list: %w", err)
	}
	transitions := 0
	for _, r := range rows {
		// Defensive ctx check between rows: with up to 10k rows per tick, an
		// in-flight tick at shutdown would otherwise keep iterating (and
		// publishing webhooks) long after ctx was cancelled. Bailing here
		// narrows the shutdown race window to one row's processing time.
		if err := ctx.Err(); err != nil {
			return transitions, err
		}
		// Pending rows have no cert to evaluate; skip until a cert is
		// uploaded. Nor do auto rows: the relay obtains their certificate
		// and certmagic renews it (H10).
		if r.Status == StatusPending || r.CertSource == db.CertSourceAuto {
			continue
		}
		want := ComputeStatus(r.NotAfter, now)
		if want == r.Status {
			continue
		}
		if err := deps.DB.UpdateCustomDomainStatus(ctx, r.ID, want); err != nil {
			log.Warn("custom-domain status tick: update failed",
				"id", r.ID, "hostname", r.Hostname, "err", err)
			continue
		}
		transitions++

		// Audit append (best-effort).
		if deps.Audit != nil {
			payload, _ := json.Marshal(map[string]any{
				"id":   r.ID,
				"from": r.Status,
				"to":   want,
			})
			if aerr := deps.Audit.Append(ctx, audit.Event{
				Action:       audit.ActionServiceCustomDomainStatusChanged,
				SubjectID:    r.ID,
				SubjectLabel: r.Hostname,
				Result:       "ok",
				Payload:      json.RawMessage(payload),
			}); aerr != nil {
				log.Warn("custom-domain status tick: audit append failed",
					"id", r.ID, "err", aerr)
			}
		}

		// Webhook fire on active -> cert_expiring edge only.
		if r.Status == StatusActive && want == StatusCertExpiring && deps.Webhook != nil {
			deps.Webhook.Publish(ctx, "custom_domain.cert.expiring", map[string]any{
				"id":         r.ID,
				"hostname":   r.Hostname,
				"service_id": r.ServiceID,
				"not_after":  r.NotAfter.UTC().Format(time.RFC3339),
			})
		}
	}
	log.Info("custom-domain status tick complete", "rows", len(rows), "transitions", transitions)
	return transitions, nil
}

// StatusTick blocks until ctx is cancelled, firing RunStatusTick daily at
// fireAtUTC (a "HH:MM" string in UTC; empty or invalid → "00:30"). The
// signature mirrors retention.Compactor.Tick so the goroutine lifecycle is
// identical to the v0.5.0 retention compactor.
func StatusTick(ctx context.Context, deps StatusTickDeps, fireAtUTC string) {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	hhmm := fireAtUTC
	if hhmm == "" {
		hhmm = "00:30"
	}
	h, m := parseHHMM(hhmm)
	for {
		next := nextFiring(time.Now().UTC(), h, m)
		log.Info("custom-domain status tick: next", "at", next.Format(time.RFC3339))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if _, err := RunStatusTick(ctx, deps, time.Now().UTC()); err != nil {
			log.Warn("custom-domain status tick: run failed", "err", err)
		}
	}
}

// parseHHMM parses "HH:MM" and returns (hour, minute). On parse error it
// falls back to (0, 30). Defensive copy of retention.parseHHMM kept in this
// package to avoid an import cycle.
func parseHHMM(s string) (int, int) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 30
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 30
	}
	return h, m
}

// nextFiring returns the next wall-clock moment in UTC when the tick should
// fire. If today's target HH:MM is still in the future, that is returned;
// otherwise tomorrow's target is returned. Defensive copy of
// retention.nextFiring.
func nextFiring(now time.Time, h, m int) time.Time {
	target := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.UTC)
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target
}
