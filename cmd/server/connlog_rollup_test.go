package main

import (
	"context"
	"fmt"
	"github.com/ankoehn/burrow/internal/retention"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/webhook"
)

// summaries records the session summaries the roller announces.
type summaries struct {
	mu  sync.Mutex
	got []summary
}

type summary struct {
	service, kind string
	from, to      time.Time
	data          webhook.RollupData
}

func (s *summaries) EmitConnectionSessionSummary(serviceID, kind string, from, to time.Time, data webhook.RollupData) {
	s.mu.Lock()
	s.got = append(s.got, summary{serviceID, kind, from, to, data})
	s.mu.Unlock()
}

// One day of connection logs is rolled up and each group announced once,
// with the day as its window; a day without connections announces nothing.
func TestConnLogRoller(t *testing.T) {
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "roll.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	wrapped := db.Wrap(sqldb)
	// The connection log refers to services, and those to a user.
	if _, err := sqldb.Exec(`INSERT INTO users(id,email,password_hash,role) VALUES('u1','u1@test.invalid','hash','user')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"svc1", "svc2"} {
		if _, err := sqldb.Exec(`INSERT INTO services(id,user_id,name,type,subdomain,access_mode,api_key_header)
			VALUES(?,'u1',?,'http',?,'open','Authorization')`, id, id, id); err != nil {
			t.Fatal(err)
		}
	}
	sink := connlog.NewSQLSink(wrapped, discardLog())
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for i, e := range []connlog.Entry{
		{Kind: connlog.KindHTTPProxy, ServiceID: "svc1", SourceIP: "203.0.113.7", BytesIn: 10, BytesOut: 100},
		{Kind: connlog.KindHTTPProxy, ServiceID: "svc1", SourceIP: "203.0.113.7", BytesIn: 20, BytesOut: 200},
		{Kind: connlog.KindHTTPProxy, ServiceID: "svc2", SourceIP: "198.51.100.4", BytesIn: 1, BytesOut: 2},
	} {
		e.StartedAt = day.Add(time.Duration(i+1) * time.Hour)
		e.EndedAt = e.StartedAt.Add(time.Second)
		e.Status = connlog.StatusClosedClean
		if err := sink.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	// Record writes behind the call.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := sqldb.QueryRow(`SELECT COUNT(*) FROM connection_logs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connection log rows, want 3", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	got, au := &summaries{}, &recAudit{}
	roller := connLogRoller{sink: sink, db: wrapped, events: got, audit: au, log: discardLog()}
	n, err := roller.runOnce(context.Background(), day.Add(13*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("runOnce: %d groups, %v; want 2", n, err)
	}
	if len(got.got) != 2 {
		t.Fatalf("%d summaries, want 2: %+v", len(got.got), got.got)
	}
	bySvc := map[string]summary{}
	for _, s := range got.got {
		bySvc[s.service] = s
	}
	one := bySvc["svc1"]
	if one.kind != "http_proxy" || !one.from.Equal(day) || !one.to.Equal(day.Add(24*time.Hour)) ||
		one.data.Sessions != 2 || one.data.BytesIn != 30 || one.data.BytesOut != 300 ||
		len(one.data.TopSourceIPs) != 1 || one.data.TopSourceIPs[0] != (webhook.TopSourceIP{IP: "203.0.113.7", Sessions: 2}) {
		t.Fatalf("svc1 summary: %+v", one)
	}
	if acts := au.actions(); len(acts) != 1 || acts[0] != audit.ActionConnectionSessionSummary+":" {
		t.Fatalf("audit entries: %v", acts)
	}

	// An empty day: nothing to announce, no audit entry.
	if n, err := roller.runOnce(context.Background(), day.Add(-48*time.Hour)); err != nil || n != 0 {
		t.Fatalf("empty day: %d groups, %v", n, err)
	}
	if len(got.got) != 2 || len(au.actions()) != 1 {
		t.Fatal("an empty day was announced")
	}
}

// nextRollAt: the next 00:10 UTC strictly after now.
func TestNextRollAt(t *testing.T) {
	utc := func(y int, m time.Month, d, h, mi, s int) time.Time { return time.Date(y, m, d, h, mi, s, 0, time.UTC) }
	for _, c := range []struct{ now, want time.Time }{
		{utc(2026, 10, 12, 0, 9, 59), utc(2026, 10, 12, 0, 10, 0)},                                      // just before
		{utc(2026, 10, 12, 0, 10, 0), utc(2026, 10, 13, 0, 10, 0)},                                      // at: today's is not after now
		{utc(2026, 10, 12, 0, 10, 1), utc(2026, 10, 13, 0, 10, 0)},                                      // just after
		{utc(2026, 10, 31, 23, 59, 0), utc(2026, 11, 1, 0, 10, 0)},                                      // month end
		{utc(2026, 12, 31, 12, 0, 0), utc(2027, 1, 1, 0, 10, 0)},                                        // year end
		{utc(2028, 2, 28, 1, 0, 0), utc(2028, 2, 29, 0, 10, 0)},                                         // leap day
		{time.Date(2026, 10, 12, 2, 5, 0, 0, time.FixedZone("x", 2*3600)), utc(2026, 10, 12, 0, 10, 0)}, // other zone
	} {
		if got := nextRollAt(c.now); !got.Equal(c.want) {
			t.Errorf("nextRollAt(%s) = %s, want %s", c.now.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
	}
}

type fixedSettings struct{ days int }

func (f fixedSettings) Load(context.Context) (retention.Settings, error) {
	return retention.Settings{ConnectionLogsDays: f.days}, nil
}

// rollerFixture is a database with connection log rows on the given days
// (one row each, on service svc1 and svc2: two groups a day).
func rollerFixture(t *testing.T, days []time.Time) (*db.DB, *connlog.SQLSink) {
	t.Helper()
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "catchup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.Exec(`INSERT INTO users(id,email,password_hash,role) VALUES('u1','u1@test.invalid','hash','user')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"svc1", "svc2"} {
		if _, err := sqldb.Exec(`INSERT INTO services(id,user_id,name,type,subdomain,access_mode,api_key_header)
			VALUES(?,'u1',?,'http',?,'open','Authorization')`, id, id, id); err != nil {
			t.Fatal(err)
		}
	}
	for i, d := range days {
		for _, svc := range []string{"svc1", "svc2"} {
			ts := d.Add(3 * time.Hour)
			if _, err := sqldb.Exec(`INSERT INTO connection_logs
				(id, kind, service_id, source_ip, started_at, ended_at, duration_ms, bytes_in, bytes_out, status)
				VALUES(?,?,?,?,?,?,?,1,1,'closed_clean')`,
				fmt.Sprintf("%s-%d", svc, i), "http_proxy", svc, "203.0.113.9", ts.UTC(), ts.Add(time.Second).UTC(), 1000); err != nil {
				t.Fatal(err)
			}
		}
	}
	w := db.Wrap(sqldb)
	return w, connlog.NewSQLSink(w, discardLog())
}

// A relay that missed two days rolls and announces both, oldest first; a
// second run the same day does nothing; the current day is not rolled.
func TestConnLogRoller_CatchesUpMissedDays(t *testing.T) {
	today := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	d2, d1 := today.AddDate(0, 0, -2), today.AddDate(0, 0, -1)
	w, sink := rollerFixture(t, []time.Time{d2, d1, today})
	got, au := &summaries{}, &recAudit{}
	roller := connLogRoller{sink: sink, db: w, events: got, audit: au, log: discardLog(), settings: fixedSettings{0},
		sleep: func(context.Context, time.Duration) bool { return true }}

	days, published, err := roller.catchUp(context.Background(), today.Add(time.Hour))
	if err != nil || days != 2 || published != 4 {
		t.Fatalf("catchUp: %d days, %d summaries, %v; want 2, 4", days, published, err)
	}
	if len(got.got) != 4 || !got.got[0].from.Equal(d2) || !got.got[1].from.Equal(d2) || !got.got[2].from.Equal(d1) || !got.got[3].from.Equal(d1) {
		t.Fatalf("announced in the wrong order or window: %+v", got.got)
	}
	if len(au.actions()) != 2 {
		t.Fatalf("audit entries: %v, want one per day", au.actions())
	}
	days, published, err = roller.catchUp(context.Background(), today.Add(2*time.Hour))
	if err != nil || days != 0 || published != 0 || len(got.got) != 4 {
		t.Fatalf("second run: %d days, %d summaries, %v, %d announced; want nothing", days, published, err, len(got.got))
	}
}

// Days the connection log no longer keeps are not attempted.
func TestConnLogRoller_StaysInsideRetention(t *testing.T) {
	today := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	old, recent := today.AddDate(0, 0, -10), today.AddDate(0, 0, -2)
	w, sink := rollerFixture(t, []time.Time{old, recent})
	got := &summaries{}
	roller := connLogRoller{sink: sink, db: w, events: got, log: discardLog(), settings: fixedSettings{3},
		sleep: func(context.Context, time.Duration) bool { return true }}
	days, _, err := roller.catchUp(context.Background(), today.Add(time.Hour))
	if err != nil || days != 1 || len(got.got) != 2 || !got.got[0].from.Equal(recent) {
		t.Fatalf("catchUp: %d days, %v, %+v; want only %s", days, err, got.got, recent)
	}
	rows, err := connlog.ListConnectionLogRollups(context.Background(), w, connlog.RollupQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Day[:10] == old.Format("2006-01-02") {
			t.Fatalf("the day outside the window was rolled: %+v", r)
		}
	}
}

// Publishing pauses after every connLogBatch summaries, and a pause cut short
// by shutdown ends the run's publishing.
func TestConnLogRoller_PacesPublishing(t *testing.T) {
	r := connLogRoller{}
	var pauses int
	r.sleep = func(_ context.Context, d time.Duration) bool {
		if d != connLogPause {
			t.Errorf("pause %v", d)
		}
		pauses++
		return pauses < 3
	}
	st := &rollStats{}
	stopped := -1
	for i := 1; i <= 5*connLogBatch; i++ {
		st.published++
		if !r.pace(context.Background(), st) {
			stopped = i
			break
		}
	}
	if pauses != 3 || stopped != 3*connLogBatch {
		t.Fatalf("%d pauses, stopped at %d; want 3 pauses (one per batch) and a stop at %d", pauses, stopped, 3*connLogBatch)
	}
	// Fewer than a batch: no pause at all.
	pauses = 0
	st = &rollStats{}
	for i := 1; i < connLogBatch; i++ {
		st.published++
		r.pace(context.Background(), st)
	}
	if pauses != 0 {
		t.Fatalf("%d pauses for less than a batch", pauses)
	}
}
