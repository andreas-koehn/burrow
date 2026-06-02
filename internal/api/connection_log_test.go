package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
)

// connLogTestStack is a freshly migrated *db.DB + ConnectionLogStore for
// the connection log API tests.
type connLogTestStack struct {
	x     *db.DB
	store ConnectionLogStore
	sink  *connlog.SQLSink
}

func newConnLogTestStack(t *testing.T) *connLogTestStack {
	t.Helper()
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "connlog_api.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(sqldb); err != nil {
		t.Fatal(err)
	}
	x := db.Wrap(sqldb)
	t.Cleanup(func() { _ = x.Close() })
	return &connLogTestStack{
		x:     x,
		store: NewConnLogDBAdapter(x),
		sink:  connlog.NewSQLSink(x, nil),
	}
}

// insertLog inserts a connection_log row directly (synchronous) into the DB
// and returns the row ID.
func (s *connLogTestStack) insertLog(t *testing.T, id, kind, serviceID string, startedAt time.Time, durationMs int) string {
	t.Helper()
	endedAt := startedAt.Add(time.Duration(durationMs) * time.Millisecond)
	_, err := s.x.DB().Exec(
		`INSERT INTO connection_logs
		  (id, kind, service_id, source_ip, started_at, ended_at,
		   duration_ms, bytes_in, bytes_out, status)
		 VALUES (?,?,?,?,?,?,?,0,0,'closed_clean')`,
		id, kind, nullOrStr(serviceID), "1.2.3.4",
		startedAt.UTC(), endedAt.UTC(), durationMs,
	)
	if err != nil {
		t.Fatalf("insertLog(%s): %v", id, err)
	}
	return id
}

func nullOrStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// fakeConnLogStore is a simple in-memory stub for unit tests that don't need
// a real DB.
//
// topIPs holds per-(day, service_id, kind) top-source-IP rows for the Q12
// toggle tests. The map key is "<day>|<service_id>|<kind>". Tests seed via
// seedTopIPs; the handler reads via ListConnectionLogRollupTopIPs.
type fakeConnLogStore struct {
	logs    []db.ConnectionLog
	rollups []db.ConnectionLogRollup
	topIPs  map[string][]connlog.TopIP
}

func (f *fakeConnLogStore) ListConnectionLogs(_ context.Context, q connlog.ConnLogQuery) ([]db.ConnectionLog, error) {
	var out []db.ConnectionLog
	for _, r := range f.logs {
		if q.Kind != "" && r.Kind != q.Kind {
			continue
		}
		if q.ServiceID != "" && r.ServiceID != q.ServiceID {
			continue
		}
		out = append(out, r)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeConnLogStore) ListConnectionLogRollups(_ context.Context, _ connlog.RollupQuery) ([]db.ConnectionLogRollup, error) {
	return f.rollups, nil
}

func (f *fakeConnLogStore) ListConnectionLogRollupTopIPs(_ context.Context, day, serviceID, kind string) ([]connlog.TopIP, error) {
	if f.topIPs == nil {
		return nil, nil
	}
	return f.topIPs[day+"|"+serviceID+"|"+kind], nil
}

func (f *fakeConnLogStore) ListConnectionLogRollupTopIPsBatch(_ context.Context, groups []connlog.TopIPsGroup) (map[connlog.TopIPsGroup][]connlog.TopIP, error) {
	if f.topIPs == nil {
		return map[connlog.TopIPsGroup][]connlog.TopIP{}, nil
	}
	out := make(map[connlog.TopIPsGroup][]connlog.TopIP, len(groups))
	for _, g := range groups {
		if rows, ok := f.topIPs[g.Day+"|"+g.ServiceID+"|"+g.Kind]; ok {
			out[g] = rows
		}
	}
	return out, nil
}

// TestConnectionLogsGETReturnsEmptyWhenNone asserts that the list endpoint
// returns an empty array when no rows exist.
func TestConnectionLogsGETReturnsEmptyWhenNone(t *testing.T) {
	store := &fakeConnLogStore{}
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "admin"},
		ConnLogDB: store,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []connectionLogResp
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(out) != 0 {
		t.Fatalf("want empty array, got %d rows", len(out))
	}
}

// TestConnectionLogsGETFiltersAndCursorPaginates inserts 5 rows (staggered
// by 1 s) and asserts:
//   - ?kind=http_proxy returns only http_proxy rows.
//   - ?before_id=<id> returns rows older than that row.
func TestConnectionLogsGETFiltersAndCursorPaginates(t *testing.T) {
	s := newConnLogTestStack(t)
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "admin"},
		ConnLogDB: s.store,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	// Seed 3 http_proxy + 2 control rows.
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		s.insertLog(t, "http-"+string(rune('A'+i)), "http_proxy", "",
			base.Add(time.Duration(i)*time.Second), 100)
	}
	for i := 0; i < 2; i++ {
		s.insertLog(t, "ctrl-"+string(rune('A'+i)), "control", "",
			base.Add(time.Duration(i+3)*time.Second), 50)
	}

	// ?kind=http_proxy returns only 3 rows.
	r := c.get(t, "/api/v1/connection-logs?kind=http_proxy")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("kind filter: status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var filtered []connectionLogResp
	if err := json.NewDecoder(r.Body).Decode(&filtered); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(filtered) != 3 {
		t.Fatalf("want 3 http_proxy rows, got %d", len(filtered))
	}
	for _, row := range filtered {
		if row.Kind != "http_proxy" {
			t.Errorf("unexpected kind %q", row.Kind)
		}
	}

	// Page 1: newest 2 rows (no filter).
	r = c.get(t, "/api/v1/connection-logs?limit=2")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("page1: status=%d", r.StatusCode)
	}
	var page1 []connectionLogResp
	json.NewDecoder(r.Body).Decode(&page1) //nolint:errcheck
	r.Body.Close()
	if len(page1) != 2 {
		t.Fatalf("want 2 rows on page1, got %d", len(page1))
	}
	cursorID := page1[len(page1)-1].ID

	// Page 2: rows older than cursor.
	r = c.get(t, "/api/v1/connection-logs?before_id="+cursorID+"&limit=10")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("page2: status=%d", r.StatusCode)
	}
	var page2 []connectionLogResp
	json.NewDecoder(r.Body).Decode(&page2) //nolint:errcheck
	r.Body.Close()
	if len(page2) != 3 {
		t.Fatalf("want 3 rows on page2, got %d", len(page2))
	}
	cursorTime := page1[len(page1)-1].StartedAt
	for _, row := range page2 {
		if !row.StartedAt.Before(cursorTime) {
			t.Errorf("page2 row %s started_at %v not before cursor %v", row.ID, row.StartedAt, cursorTime)
		}
	}
}

// TestConnectionLogsRollupsGET asserts the rollups endpoint returns rollup rows.
func TestConnectionLogsRollupsGET(t *testing.T) {
	store := &fakeConnLogStore{
		rollups: []db.ConnectionLogRollup{
			{Day: "2026-01-10", ServiceID: "s1", Kind: "http_proxy", Sessions: 42,
				BytesIn: 1000, BytesOut: 2000, AvgDurationMs: 150, P95DurationMs: 195},
		},
	}
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "admin"},
		ConnLogDB: store,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs/rollups")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []connectionLogRollupResp
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(out) != 1 {
		t.Fatalf("want 1 rollup row, got %d", len(out))
	}
	if out[0].Sessions != 42 {
		t.Errorf("want sessions=42, got %d", out[0].Sessions)
	}
	if out[0].P95DurationMs != 195 {
		t.Errorf("want p95=195, got %d", out[0].P95DurationMs)
	}
}

// TestGetRollups_TopIPsToggle exercises the v0.5.1 Q12 toggle on the rollups
// read path. Three states are asserted by inspecting the raw JSON because the
// distinction between "field absent" and "field present but empty" is the
// whole point of the toggle's wire contract:
//
//	default-unset → top_source_ips present (populated when seeded)
//	"true"       → top_source_ips present (populated when seeded)
//	"false"      → top_source_ips field omitted entirely (NOT empty array)
func TestGetRollups_TopIPsToggle(t *testing.T) {
	day := "2026-01-15"
	rollupRow := db.ConnectionLogRollup{
		Day: day, ServiceID: "s1", Kind: "http_proxy",
		Sessions: 100, BytesIn: 1000, BytesOut: 2000,
		AvgDurationMs: 150, P95DurationMs: 195,
	}
	topIPs := []connlog.TopIP{
		{IP: "10.0.0.1", Sessions: 42},
		{IP: "10.0.0.2", Sessions: 17},
	}
	key := day + "|s1|http_proxy"

	check := func(t *testing.T, settingValue string, wantTopPresent bool, wantIPCount int) {
		t.Helper()
		store := &fakeConnLogStore{
			rollups: []db.ConnectionLogRollup{rollupRow},
			topIPs:  map[string][]connlog.TopIP{key: topIPs},
		}
		settings := &fakeCacheSettingsStore{}
		if settingValue != "" {
			settings.saved = map[string]string{
				"connection_logs.rollup_include_top_ips": settingValue,
			}
		}
		d := Deps{
			Log:       discardLog(),
			Users:     &fakeUserStore{role: "admin"},
			ConnLogDB: store,
			Settings:  settings,
		}
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		r := c.get(t, "/api/v1/connection-logs/rollups")
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
		}
		// Decode into a slice of raw maps so we can detect "field absent"
		// versus "field present and []".
		var raw []map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if len(raw) != 1 {
			t.Fatalf("want 1 rollup row, got %d", len(raw))
		}
		topRaw, hasField := raw[0]["top_source_ips"]
		if hasField != wantTopPresent {
			t.Fatalf("setting=%q: top_source_ips present=%v want=%v (raw=%v)",
				settingValue, hasField, wantTopPresent, raw[0])
		}
		if wantTopPresent {
			var ips []connlog.TopIP
			if err := json.Unmarshal(topRaw, &ips); err != nil {
				t.Fatalf("decode top_source_ips: %v", err)
			}
			if len(ips) != wantIPCount {
				t.Fatalf("setting=%q: want %d top IPs, got %d", settingValue, wantIPCount, len(ips))
			}
		}
	}

	t.Run("default unset → toggle on, ips present", func(t *testing.T) {
		check(t, "", true, 2)
	})
	t.Run("explicit true → toggle on, ips present", func(t *testing.T) {
		check(t, "true", true, 2)
	})
	t.Run("explicit false → field omitted entirely", func(t *testing.T) {
		check(t, "false", false, 0)
	})
}

// TestGetRollups_TopIPsToggle_EmptyButEnabled asserts the wire contract for a
// rollup row whose group has no top-IP rows but the toggle is still ON: the
// top_source_ips field MUST be present as an empty array, NOT omitted. This
// is the third state that distinguishes ON-but-empty from OFF.
func TestGetRollups_TopIPsToggle_EmptyButEnabled(t *testing.T) {
	day := "2026-01-16"
	store := &fakeConnLogStore{
		rollups: []db.ConnectionLogRollup{
			{Day: day, ServiceID: "s2", Kind: "control",
				Sessions: 1, AvgDurationMs: 10, P95DurationMs: 10},
		},
		// topIPs map intentionally empty — group has no aux rows yet.
	}
	settings := &fakeCacheSettingsStore{
		saved: map[string]string{
			"connection_logs.rollup_include_top_ips": "true",
		},
	}
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "admin"},
		ConnLogDB: store,
		Settings:  settings,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs/rollups")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var raw []map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(raw) != 1 {
		t.Fatalf("want 1 row, got %d", len(raw))
	}
	topRaw, hasField := raw[0]["top_source_ips"]
	if !hasField {
		t.Fatal("toggle ON + no aux rows: top_source_ips MUST be present (not omitted)")
	}
	if string(topRaw) != "[]" {
		t.Errorf("want top_source_ips=[], got %s", string(topRaw))
	}
}

// TestGetRollups_TopIPsBatchedOneQuery pins the v0.5.2 BACKLOG #2 fix: when
// the rollups handler returns N rows, the top-IPs fetch issues a single
// batched call instead of N per-row calls (N+1 -> 2 queries).
//
// The fake records each call to ListConnectionLogRollupTopIPs vs the new
// ListConnectionLogRollupTopIPsBatch; the assertion is that the batched
// variant was invoked exactly once and the per-group variant zero times
// regardless of how many rollup rows the handler walks.
func TestGetRollups_TopIPsBatchedOneQuery(t *testing.T) {
	day := "2026-02-01"
	// Seed 5 rollup rows across 3 (day, service_id, kind) groups so the
	// batched call has > 1 distinct group to coalesce.
	rollups := []db.ConnectionLogRollup{
		{Day: day, ServiceID: "s1", Kind: "http_proxy", Sessions: 10},
		{Day: day, ServiceID: "s1", Kind: "control", Sessions: 2},
		{Day: day, ServiceID: "s2", Kind: "http_proxy", Sessions: 7},
		{Day: day, ServiceID: "s2", Kind: "tcp_proxy", Sessions: 4},
		{Day: day, ServiceID: "s3", Kind: "http_proxy", Sessions: 1},
	}
	topIPs := []connlog.TopIP{{IP: "10.0.0.1", Sessions: 3}}
	keyMap := map[string][]connlog.TopIP{}
	for _, r := range rollups {
		keyMap[r.Day+"|"+r.ServiceID+"|"+r.Kind] = topIPs
	}
	store := &countingConnLogStore{
		fakeConnLogStore: fakeConnLogStore{rollups: rollups, topIPs: keyMap},
	}
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "admin"},
		ConnLogDB: store,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs/rollups")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []connectionLogRollupResp
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(out) != len(rollups) {
		t.Fatalf("want %d response rows, got %d", len(rollups), len(out))
	}
	// Verify all 5 rows carry their TopSourceIPs (sanity that the batch
	// distribution wired correctly).
	for i, row := range out {
		if row.TopSourceIPs == nil || len(*row.TopSourceIPs) != 1 {
			t.Errorf("row[%d] (%s/%s): want 1 top IP, got %+v",
				i, row.ServiceID, row.Kind, row.TopSourceIPs)
		}
	}
	// Query budget:
	//   pre-fix: 1 rollup list + 5 per-group ListConnectionLogRollupTopIPs = 6
	//   post-fix: 1 rollup list + 1 batched ListConnectionLogRollupTopIPsBatch = 2
	if store.perGroupCalls != 0 {
		t.Errorf("per-group ListConnectionLogRollupTopIPs called %d times; want 0", store.perGroupCalls)
	}
	if store.batchCalls != 1 {
		t.Errorf("batched ListConnectionLogRollupTopIPsBatch called %d times; want 1", store.batchCalls)
	}
}

// countingConnLogStore wraps fakeConnLogStore and counts which top-IPs
// accessor the handler calls — the per-group method or the batched method.
type countingConnLogStore struct {
	fakeConnLogStore
	perGroupCalls int
	batchCalls    int
}

func (f *countingConnLogStore) ListConnectionLogRollupTopIPs(ctx context.Context, day, serviceID, kind string) ([]connlog.TopIP, error) {
	f.perGroupCalls++
	return f.fakeConnLogStore.ListConnectionLogRollupTopIPs(ctx, day, serviceID, kind)
}

func (f *countingConnLogStore) ListConnectionLogRollupTopIPsBatch(_ context.Context, groups []connlog.TopIPsGroup) (map[connlog.TopIPsGroup][]connlog.TopIP, error) {
	f.batchCalls++
	out := make(map[connlog.TopIPsGroup][]connlog.TopIP, len(groups))
	for _, g := range groups {
		if rows, ok := f.topIPs[g.Day+"|"+g.ServiceID+"|"+g.Kind]; ok {
			out[g] = rows
		}
	}
	return out, nil
}

// TestConnectionLogsExportNDJSON asserts the export endpoint streams NDJSON.
func TestConnectionLogsExportNDJSON(t *testing.T) {
	store := &fakeConnLogStore{
		logs: []db.ConnectionLog{
			{ID: "x1", Kind: "http_proxy", SourceIP: "1.2.3.4", Status: "closed_clean",
				StartedAt: time.Now(), EndedAt: time.Now().Add(100 * time.Millisecond)},
		},
	}
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "admin"},
		ConnLogDB: store,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs/export")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" || ct[:len("application/x-ndjson")] != "application/x-ndjson" {
		t.Errorf("unexpected Content-Type %q", ct)
	}
	body := readBody(t, r)
	if body == "" {
		t.Fatal("empty export body")
	}
	// Each line is a JSON object.
	var row connectionLogResp
	if err := json.Unmarshal([]byte(body[:len(body)-1]), &row); err != nil { // strip trailing newline
		t.Fatalf("unmarshal ndjson: %v", err)
	}
	if row.ID != "x1" {
		t.Errorf("want id=x1, got %q", row.ID)
	}
}

// TestConnectionLogsExportCSV asserts the export endpoint streams CSV.
func TestConnectionLogsExportCSV(t *testing.T) {
	store := &fakeConnLogStore{
		logs: []db.ConnectionLog{
			{ID: "x2", Kind: "control", SourceIP: "5.6.7.8", Status: "closed_clean",
				StartedAt: time.Now(), EndedAt: time.Now()},
		},
	}
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "admin"},
		ConnLogDB: store,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs/export?format=csv")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if body == "" {
		t.Fatal("empty csv body")
	}
	// First line should be headers.
	lines := splitLines(body)
	if len(lines) < 2 {
		t.Fatalf("want at least 2 CSV lines (header+data), got %d", len(lines))
	}
}

// TestConnectionLogsStoreUnavailable asserts 500 when ConnLogDB is nil.
func TestConnectionLogsStoreUnavailable(t *testing.T) {
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "admin"},
		ConnLogDB: nil,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	for _, path := range []string{
		"/api/v1/connection-logs",
		"/api/v1/connection-logs/rollups",
		"/api/v1/connection-logs/export",
	} {
		r := c.get(t, path)
		if r.StatusCode != http.StatusInternalServerError {
			t.Errorf("path %s: want 500, got %d", path, r.StatusCode)
		}
		r.Body.Close()
	}
}

// TestConnectionLogsPermissionDenied asserts non-admin without audit:read
// gets 403.
func TestConnectionLogsPermissionDenied(t *testing.T) {
	store := &fakeConnLogStore{}
	d := Deps{
		Log:       discardLog(),
		Users:     &fakeUserStore{role: "user"},
		ConnLogDB: store,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/connection-logs")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
}

// splitLines splits a string by newlines, dropping empty trailing lines.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
