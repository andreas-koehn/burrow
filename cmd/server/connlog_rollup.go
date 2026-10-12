// connlog_rollup.go — the daily rollup of the connection log, and its
// announcement as "connection.session_summary".
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/webhook"
)

// sessionSummaryEmitter is *webhook.Dispatcher as the roller needs it.
type sessionSummaryEmitter interface {
	EmitConnectionSessionSummary(serviceID, kind string, windowStart, windowEnd time.Time, rollup webhook.RollupData)
}

// When the day before is rolled up, UTC.
const connLogRollHour, connLogRollMinute = 0, 10

// connLogRoller computes the connection log's rollup of a finished UTC day
// and announces each group of it (a service and a kind) as
// "connection.session_summary". The rollup itself is what GET
// /connection-logs/rollups reads.
type connLogRoller struct {
	sink   *connlog.SQLSink
	db     *db.DB
	events sessionSummaryEmitter
	audit  auditAppender
	log    *slog.Logger
}

// tick runs runOnce for the day before, every day at 00:10 UTC, until ctx
// ends. A failed run is logged; the next day's is tried all the same.
func (r connLogRoller) tick(ctx context.Context) {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), connLogRollHour, connLogRollMinute, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		day := time.Now().UTC().Add(-24 * time.Hour)
		if n, err := r.runOnce(ctx, day); err != nil {
			r.log.Warn("connection log rollup failed", "day", day.Format("2006-01-02"), "err", err)
		} else {
			r.log.Info("connection log rollup complete", "day", day.Format("2006-01-02"), "groups", n)
		}
	}
}

// runOnce rolls up the UTC day that contains day and announces its groups.
// It returns how many groups the day has.
func (r connLogRoller) runOnce(ctx context.Context, day time.Time) (int, error) {
	start := time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if err := r.sink.Rollup(ctx, start); err != nil {
		return 0, err
	}
	rows, err := connlog.ListConnectionLogRollups(ctx, r.db, connlog.RollupQuery{Since: &start, Until: &start})
	if err != nil {
		return 0, err
	}
	// The day as the rollup wrote it into the top-IP table. row.Day is not
	// used for it: a driver may hand a DATE column back as a timestamp.
	dayStr := start.Format("2006-01-02")
	groups := make([]connlog.TopIPsGroup, 0, len(rows))
	for _, row := range rows {
		groups = append(groups, connlog.TopIPsGroup{Day: dayStr, ServiceID: row.ServiceID, Kind: row.Kind})
	}
	// Empty when the operator turned the top-IP aggregation off.
	top, err := connlog.ListConnectionLogRollupTopIPsBatch(ctx, r.db, groups)
	if err != nil {
		return 0, err
	}
	for i, row := range rows {
		data := webhook.RollupData{
			Sessions: row.Sessions, BytesIn: row.BytesIn, BytesOut: row.BytesOut,
			AvgDurationMs: row.AvgDurationMs, P95DurationMs: row.P95DurationMs,
		}
		for j, ip := range top[groups[i]] {
			if j == 10 {
				break
			}
			data.TopSourceIPs = append(data.TopSourceIPs, webhook.TopSourceIP{IP: ip.IP, Sessions: int(ip.Sessions)})
		}
		if r.events != nil {
			r.events.EmitConnectionSessionSummary(row.ServiceID, row.Kind, start, start.Add(24*time.Hour), data)
		}
	}
	if r.audit != nil && len(rows) > 0 {
		// One entry per run, not per group: the chain grows by a row a day.
		if err := r.audit.Append(ctx, audit.Event{
			Action: audit.ActionConnectionSessionSummary, SubjectLabel: dayStr, Result: "ok",
			Payload: audit.MustJSON(map[string]any{"day": dayStr, "groups": len(rows)}),
		}); err != nil {
			r.log.Warn("connection log rollup: audit write failed", "err", err)
		}
	}
	return len(rows), nil
}
