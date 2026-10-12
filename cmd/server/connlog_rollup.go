// connlog_rollup.go — the daily rollup of the connection log, and its
// announcement as "connection.session_summary".
package main

import (
	"context"
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
func (r connLogRoller) run(ctx context.Context) {
	days, published, err := r.catchUp(ctx, time.Now().UTC())
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
	if set.ConnectionLogsDays > 0 {
		if oldest := today.AddDate(0, 0, -set.ConnectionLogsDays); from.Before(oldest) {
			from = oldest
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

// pace pauses after every connLogBatch summaries; it reports false when ctx
// ended during the pause.
func (r connLogRoller) pace(ctx context.Context, st *rollStats) bool {
	if st.published == 0 || st.published%connLogBatch != 0 {
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

func (r connLogRoller) rollDay(ctx context.Context, day time.Time, st *rollStats) (int, error) {
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
			st.published++
			if !r.pace(ctx, st) {
				break
			}
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
