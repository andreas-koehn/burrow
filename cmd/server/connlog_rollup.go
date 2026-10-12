// connlog_rollup.go — the daily rollup of the connection log, and its
// announcement as "connection.session_summary".
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/retention"
	"github.com/ankoehn/burrow/internal/webhook"
)

// sessionSummaryEmitter is *webhook.Dispatcher as the roller needs it.
type sessionSummaryEmitter interface {
	EmitConnectionSessionSummary(serviceID, kind string, windowStart, windowEnd time.Time, rollup webhook.RollupData)
}

// When the day before is rolled up, UTC.
const connLogRollHour, connLogRollMinute = 0, 10

// Publishing is paced: the dispatcher's queue holds 256 events for one
// delivery worker, and other events share it. After every connLogBatch
// summaries the roller pauses connLogPause.
const (
	connLogBatch = 32
	connLogPause = time.Second
)

// connLogRoller computes the connection log's rollup of the finished UTC days
// that have none and announces each group of them (a service and a kind) as
// "connection.session_summary". The rollup itself is what GET
// /connection-logs/rollups reads.
type connLogRoller struct {
	sink   *connlog.SQLSink
	db     *db.DB
	events sessionSummaryEmitter
	audit  auditAppender
	log    *slog.Logger

	// Optional; the zero value is right for production.
	settings retention.Loader                                // default: the settings table
	sleep    func(ctx context.Context, d time.Duration) bool // default: a timer; false when ctx ended
	batch    int                                             // default: connLogBatch
}

// rollStats is what one run has published so far.
type rollStats struct{ published int }

// nextRollAt is the next 00:10 UTC strictly after now.
func nextRollAt(now time.Time) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), connLogRollHour, connLogRollMinute, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// tick catches up at once (a relay that was down at 00:10 owes its
// summaries), then every day at 00:10 UTC until ctx ends. It is meant to run
// on its own goroutine. A failed run is logged and tried again at the next
// tick, not in a loop.
func (r connLogRoller) tick(ctx context.Context) {
	for {
		r.run(ctx)
		timer := time.NewTimer(time.Until(nextRollAt(time.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// run rolls up what is missing as of now and logs the outcome once.
func (r connLogRoller) run(ctx context.Context) { r.runAt(ctx, time.Now().UTC()) }

func (r connLogRoller) runAt(ctx context.Context, now time.Time) {
	days, published, err := r.catchUp(ctx, now)
	switch {
	case err != nil:
		r.log.Warn("connection log rollup failed", "days", days, "summaries", published, "err", err)
	case days > 0:
		r.log.Info("connection log rollup complete", "days", days, "summaries", published)
	}
}

// catchUp rolls up, oldest first, every finished UTC day before now's that has
// connection log rows and no rollup rows yet, within the days the connection
// log is kept. It returns how many days it rolled and how many summaries it
// published. A day that has rollup rows is neither rolled nor announced again.
func (r connLogRoller) catchUp(ctx context.Context, now time.Time) (days, published int, err error) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	ld := r.settings
	if ld == nil {
		ld = retention.NewDBLoader(r.db)
	}
	set, err := ld.Load(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("load retention settings: %w", err)
	}
	first, any, err := connlog.EarliestStart(ctx, r.db)
	if err != nil || !any {
		return 0, 0, err
	}
	from := time.Date(first.UTC().Year(), first.UTC().Month(), first.UTC().Day(), 0, 0, 0, 0, time.UTC)
	// Both windows bound it (0 keeps forever): a day older than the log is
	// kept has no rows to roll, and a day older than the rollups are kept
	// would be deleted by the compaction and then rolled and announced again
	// on every run.
	for _, keep := range []int{set.ConnectionLogsDays, set.ConnectionLogRollupsDays} {
		if keep > 0 {
			if oldest := today.AddDate(0, 0, -keep); from.Before(oldest) {
				from = oldest
			}
		}
	}
	st := &rollStats{}
	for day := from; day.Before(today); day = day.AddDate(0, 0, 1) {
		if ctx.Err() != nil {
			return days, st.published, ctx.Err()
		}
		have, err := connlog.ListConnectionLogRollups(ctx, r.db, connlog.RollupQuery{Since: &day, Until: &day})
		if err != nil {
			return days, st.published, err
		}
		if len(have) > 0 {
			continue
		}
		n, err := r.rollDay(ctx, day, st)
		if err != nil {
			return days, st.published, fmt.Errorf("day %s: %w", day.Format("2006-01-02"), err)
		}
		if n > 0 {
			days++
		}
	}
	return days, st.published, nil
}

// runOnce rolls up the UTC day that contains day and announces its groups,
// whether or not it was rolled before. It returns how many groups the day has.
func (r connLogRoller) runOnce(ctx context.Context, day time.Time) (int, error) {
	return r.rollDay(ctx, day, &rollStats{})
}

// pace is called before each summary is published. It pauses once
// connLogBatch summaries have gone out since the last pause, and reports false
// when the pause was cut short by shutdown.
func (r connLogRoller) pace(ctx context.Context, st *rollStats) bool {
	batch := r.batch
	if batch <= 0 {
		batch = connLogBatch
	}
	if st.published == 0 || st.published%batch != 0 {
		return true
	}
	if r.sleep != nil {
		return r.sleep(ctx, connLogPause)
	}
	t := time.NewTimer(connLogPause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// interrupted is the error of an announcement cut short.
func interrupted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("announcement interrupted")
}

// rollDay rolls up one day and announces its groups, paced; st counts them.
//
// The rollup rows of the day are written in one transaction (Rollup), so a
// day is rolled completely or not at all. What the announcements need is
// computed by that run itself (RollupDay), so nothing is read after the
// commit. The announcements come after it and are best-effort: if the relay shuts down in a pause between two
// batches, the remaining summaries of the day are not announced later (the
// day has rollup rows by then and is not rolled again), and no audit entry is
// written for it; rollDay returns the number announced and the error.
func (r connLogRoller) rollDay(ctx context.Context, day time.Time, st *rollStats) (int, error) {
	start := time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(), 0, 0, 0, 0, time.UTC)
	// Everything the announcement needs comes from the rollup itself: no read
	// after the commit that could fail and leave a rolled day unannounced.
	rows, err := r.sink.RollupDay(ctx, start)
	if err != nil {
		return 0, err
	}
	dayStr := start.Format("2006-01-02")
	for i, row := range rows {
		data := webhook.RollupData{
			Sessions: row.Sessions, BytesIn: row.BytesIn, BytesOut: row.BytesOut,
			AvgDurationMs: row.AvgDurationMs, P95DurationMs: row.P95DurationMs,
		}
		for j, ip := range row.TopIPs {
			if j == 10 {
				break
			}
			data.TopSourceIPs = append(data.TopSourceIPs, webhook.TopSourceIP{IP: ip.IP, Sessions: int(ip.Sessions)})
		}
		if r.events != nil {
			if !r.pace(ctx, st) {
				return i, interrupted(ctx)
			}
			r.events.EmitConnectionSessionSummary(row.ServiceID, row.Kind, start, start.Add(24*time.Hour), data)
			st.published++
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
