package main

import (
	"context"
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
