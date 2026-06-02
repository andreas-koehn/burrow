// Package connlog implements the per-tunnel connection log sink (Task 8,
// v0.5.0 spec E.1). SQLSink records one row per connection into the
// connection_logs table and provides an idempotent daily Rollup that
// aggregates into connection_log_rollups.
//
// Usage on the hot path: the caller (proxy, yamux session handler) constructs
// an Entry on connection close and calls Sink.Record. The call spawns a
// goroutine so it never blocks the caller for more than 50 ms.
//
// Proxy / yamux instrumentation wiring is deferred to Task 17 (cmd/server).
// This package provides the Sink interface + SQLSink implementation so Task 17
// can wire them without code changes here.
package connlog

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/db"
)

// Kind classifies the type of connection being logged.
type Kind string

const (
	KindControl   Kind = "control"
	KindHTTPProxy Kind = "http_proxy"
	KindTCPProxy  Kind = "tcp_proxy"
)

// Status describes how the connection ended.
type Status string

const (
	StatusClosedClean Status = "closed_clean"
	StatusClosedError Status = "closed_error"
	StatusClosedIdle  Status = "closed_idle"
	StatusRejected    Status = "rejected"
)

// Entry is the data recorded for a single connection.
//
// If ID is empty, Record generates a UUID. StartedAt and EndedAt must both be
// set; DurationMs is computed from them when zero.
type Entry struct {
	ID              string
	Kind            Kind
	ServiceID       string
	TunnelID        string
	UserID          string
	ClientSessionID string
	SourceIP        string
	UserAgent       string
	StartedAt       time.Time
	EndedAt         time.Time
	BytesIn         int64
	BytesOut        int64
	Status          Status
	Reason          string
}

// Sink is the connection-log recording surface.
type Sink interface {
	// Record persists a single Entry. The implementation spawns a goroutine
	// so the caller is never blocked for more than 50 ms. Errors are logged
	// and swallowed.
	Record(ctx context.Context, e Entry) error
}

// SettingsReader is the narrow read surface SQLSink needs to honour the
// connection_logs.rollup_include_top_ips toggle (Q12, v0.5.1 Task 5). The
// production caller satisfies it via *store.Store; tests use a small in-memory
// fake. Optional: when settings is nil, Rollup behaves as if the toggle is
// "true" (i.e. the default — top IPs ARE aggregated). This matches the
// "missing row = enabled" default policy documented on the toggle.
type SettingsReader interface {
	GetSettings(ctx context.Context) (map[string]string, error)
}

// SQLSink is the production Sink backed by the Burrow SQLite database.
type SQLSink struct {
	b        *db.DB
	log      *slog.Logger
	settings SettingsReader // optional; nil → default-true for the Q12 toggle.
}

// NewSQLSink constructs a SQLSink. log may be nil; slog.Default() is used in
// that case.
func NewSQLSink(b *db.DB, log *slog.Logger) *SQLSink {
	if log == nil {
		log = slog.Default()
	}
	return &SQLSink{b: b, log: log}
}

// WithSettings returns a SQLSink that consults s for compaction-time toggles.
// The only toggle currently honoured is connection_logs.rollup_include_top_ips
// (default true when the row is missing) which gates the top-source-IPs
// aggregation in Rollup. Returning the receiver (after mutation) lets callers
// chain the constructor in one line. Production callsite: cmd/server/main.go
// — pass the *store.Store via this method right after NewSQLSink.
func (s *SQLSink) WithSettings(r SettingsReader) *SQLSink {
	if s == nil {
		return nil
	}
	s.settings = r
	return s
}

// Record persists e into connection_logs. The actual INSERT is run in a
// goroutine to satisfy the "never blocks > 50 ms" budget on the proxy hot
// path. The goroutine uses a context detached from the caller's cancellation
// (via context.WithoutCancel) so that a deferred call on the proxy hot path
// (e.g. "defer sink.Record(r.Context(), entry)") does not lose the row when
// the request context is cancelled on handler return.  A hard 5 s timeout is
// added so a stuck DB cannot leak goroutines indefinitely.
func (s *SQLSink) Record(ctx context.Context, e Entry) error {
	if s == nil || s.b == nil {
		return nil
	}
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now().UTC()
	}
	if e.EndedAt.IsZero() {
		e.EndedAt = e.StartedAt
	}
	durationMs := int(e.EndedAt.Sub(e.StartedAt).Milliseconds())
	if durationMs < 0 {
		durationMs = 0
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := s.b.DB().ExecContext(ctx,
			`INSERT INTO connection_logs
			  (id, kind, service_id, tunnel_id, user_id, client_session_id,
			   source_ip, user_agent, started_at, ended_at, duration_ms,
			   bytes_in, bytes_out, status, reason)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID,
			string(e.Kind),
			nullableStr(e.ServiceID),
			nullableStr(e.TunnelID),
			nullableStr(e.UserID),
			nullableStr(e.ClientSessionID),
			e.SourceIP,
			nullableStr(e.UserAgent),
			e.StartedAt.UTC(),
			e.EndedAt.UTC(),
			durationMs,
			e.BytesIn,
			e.BytesOut,
			string(e.Status),
			nullableStr(e.Reason),
		)
		if err != nil {
			s.log.Warn("connlog: insert failed",
				slog.String("id", e.ID),
				slog.String("kind", string(e.Kind)),
				slog.String("service_id", e.ServiceID),
				slog.String("err", err.Error()),
			)
		}
	}()
	return nil
}

// nullableStr returns nil for an empty string (so SQLite stores NULL) and s
// for any non-empty string. This keeps optional FK columns tidy.
func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ConnLogQuery is the filter shape for ListConnectionLogs.
type ConnLogQuery struct {
	Kind      string
	ServiceID string
	Since     *time.Time
	Until     *time.Time
	Q         string // free-text over service_id / source_ip / reason
	BeforeID  string // cursor: started_at < (SELECT started_at … WHERE id = ?)
	Limit     int    // 0 → defaults to 100 inside the call
}

// ConnectionLogResp is the JSON wire shape for one connection_logs row.
type ConnectionLogResp struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	ServiceID       string    `json:"service_id"`
	TunnelID        string    `json:"tunnel_id"`
	UserID          string    `json:"user_id"`
	ClientSessionID string    `json:"client_session_id"`
	SourceIP        string    `json:"source_ip"`
	UserAgent       string    `json:"user_agent"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	DurationMs      int       `json:"duration_ms"`
	BytesIn         int64     `json:"bytes_in"`
	BytesOut        int64     `json:"bytes_out"`
	Status          string    `json:"status"`
	Reason          string    `json:"reason"`
}

// ListConnectionLogs queries connection_logs with cursor pagination (newest
// first). Called by the JSON API handler; lives here so the db package stays
// free of handler-specific types.
func ListConnectionLogs(ctx context.Context, x *db.DB, q ConnLogQuery) ([]db.ConnectionLog, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}

	var where []string
	var args []any

	if q.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, q.Kind)
	}
	if q.ServiceID != "" {
		where = append(where, "service_id = ?")
		args = append(args, q.ServiceID)
	}
	if q.Since != nil {
		// Store time as RFC3339 string; compare lexicographically.
		where = append(where, "started_at >= ?")
		args = append(args, q.Since.UTC().Format(time.RFC3339))
	}
	if q.Until != nil {
		where = append(where, "started_at <= ?")
		args = append(args, q.Until.UTC().Format(time.RFC3339))
	}
	if q.Q != "" {
		like := "%" + strings.ToLower(q.Q) + "%"
		where = append(where, "(lower(service_id) LIKE ? OR lower(source_ip) LIKE ? OR lower(reason) LIKE ?)")
		args = append(args, like, like, like)
	}
	// Cursor: rows where started_at < started_at of the cursor row.
	if q.BeforeID != "" {
		where = append(where, "started_at < (SELECT started_at FROM connection_logs WHERE id = ?)")
		args = append(args, q.BeforeID)
	}

	sql := `SELECT id, kind, COALESCE(service_id,''), COALESCE(tunnel_id,''),
		COALESCE(user_id,''), COALESCE(client_session_id,''),
		source_ip, COALESCE(user_agent,''),
		started_at, ended_at, duration_ms,
		bytes_in, bytes_out, status, COALESCE(reason,''), created_at
		FROM connection_logs`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, q.Limit)

	rows, err := x.DB().QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list connection_logs: %w", err)
	}
	defer rows.Close()

	var out []db.ConnectionLog
	for rows.Next() {
		var r db.ConnectionLog
		if err := rows.Scan(
			&r.ID, &r.Kind, &r.ServiceID, &r.TunnelID,
			&r.UserID, &r.ClientSessionID,
			&r.SourceIP, &r.UserAgent,
			&r.StartedAt, &r.EndedAt, &r.DurationMs,
			&r.BytesIn, &r.BytesOut, &r.Status, &r.Reason, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan connection_log: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("connection_logs rows: %w", err)
	}
	return out, nil
}

// RollupQuery is the filter for ListConnectionLogRollups.
type RollupQuery struct {
	ServiceID string
	Kind      string
	Since     *time.Time
	Until     *time.Time
}

// ListConnectionLogRollups queries connection_log_rollups.
func ListConnectionLogRollups(ctx context.Context, x *db.DB, q RollupQuery) ([]db.ConnectionLogRollup, error) {
	var where []string
	var args []any
	if q.ServiceID != "" {
		where = append(where, "service_id = ?")
		args = append(args, q.ServiceID)
	}
	if q.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, q.Kind)
	}
	if q.Since != nil {
		where = append(where, "day >= ?")
		args = append(args, q.Since.UTC().Format("2006-01-02"))
	}
	if q.Until != nil {
		where = append(where, "day <= ?")
		args = append(args, q.Until.UTC().Format("2006-01-02"))
	}
	sql := `SELECT day, service_id, kind, sessions, bytes_in, bytes_out,
		avg_duration_ms, p95_duration_ms, created_at
		FROM connection_log_rollups`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY day DESC"

	rows, err := x.DB().QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list connection_log_rollups: %w", err)
	}
	defer rows.Close()

	var out []db.ConnectionLogRollup
	for rows.Next() {
		var r db.ConnectionLogRollup
		if err := rows.Scan(
			&r.Day, &r.ServiceID, &r.Kind, &r.Sessions,
			&r.BytesIn, &r.BytesOut, &r.AvgDurationMs, &r.P95DurationMs,
			&r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan connection_log_rollup: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("connection_log_rollups rows: %w", err)
	}
	return out, nil
}

// TopIP is one (ip, sessions) pair for a (day, service_id, kind) group. Used
// by the rollup read path to surface per-group top-source-IP attribution when
// the connection_logs.rollup_include_top_ips toggle is enabled (Q12). Sessions
// is the per-day per-group hit count attributed to ip.
type TopIP struct {
	IP       string `json:"ip"`
	Sessions int64  `json:"sessions"`
}

// TopIPsGroup identifies one (day, service_id, kind) rollup group; used as a
// composite map key by the batched top-IPs accessor (v0.5.2 BACKLOG #2).
type TopIPsGroup struct {
	Day       string
	ServiceID string
	Kind      string
}

// ListConnectionLogRollupTopIPsBatch returns the persisted top-source-IPs rows
// for all requested (day, service_id, kind) groups in a single SQL query.
// Groups with no aux rows are absent from the result map. Within each group
// the rows are ordered most-frequent-first (sessions DESC, ip ASC).
//
// Replaces the N+1 pattern of calling ListConnectionLogRollupTopIPs once per
// rollup row (v0.5.2 BACKLOG #2). At the 90-day x 10-service x 3-kind = 2,700
// rollup-row scale this collapses from 2,701 to 2 round trips.
func ListConnectionLogRollupTopIPsBatch(ctx context.Context, x *db.DB, groups []TopIPsGroup) (map[TopIPsGroup][]TopIP, error) {
	out := make(map[TopIPsGroup][]TopIP, len(groups))
	if len(groups) == 0 {
		return out, nil
	}
	// Compose ((day=? AND service_id=? AND kind=?) OR (...) ...). Portable
	// across SQLite (no row constructors in older builds) and Postgres (the
	// v0.5.1 ?-rewriter handles the placeholder translation transparently).
	var sb strings.Builder
	sb.WriteString(
		`SELECT day, service_id, kind, ip, sessions
		   FROM connection_log_rollup_top_ips WHERE `)
	args := make([]any, 0, len(groups)*3)
	for i, g := range groups {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		sb.WriteString("(day=? AND service_id=? AND kind=?)")
		args = append(args, g.Day, g.ServiceID, g.Kind)
	}
	sb.WriteString(` ORDER BY day, service_id, kind, sessions DESC, ip ASC`)

	rows, err := x.DB().QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list top_ips batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g TopIPsGroup
		var ip TopIP
		if err := rows.Scan(&g.Day, &g.ServiceID, &g.Kind, &ip.IP, &ip.Sessions); err != nil {
			return nil, fmt.Errorf("scan top_ip batch row: %w", err)
		}
		out[g] = append(out[g], ip)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list top_ips batch rows: %w", err)
	}
	return out, nil
}

// ListConnectionLogRollupTopIPs returns the persisted top-source-IPs rows for
// one (day, service_id, kind) group, ordered most-frequent-first. The list is
// empty when (a) the toggle was off at the most recent Rollup compaction, or
// (b) the group has never been rolled up. Callers that need the toggle
// distinguished from a never-rolled-up group should consult settings
// independently.
//
// Prefer ListConnectionLogRollupTopIPsBatch when fetching multiple groups —
// this single-group accessor is retained for back-compat callers (CSV export
// surfaces, ad-hoc diagnostics).
func ListConnectionLogRollupTopIPs(ctx context.Context, x *db.DB, day, serviceID, kind string) ([]TopIP, error) {
	rows, err := x.DB().QueryContext(ctx,
		`SELECT ip, sessions FROM connection_log_rollup_top_ips
		  WHERE day = ? AND service_id = ? AND kind = ?
		  ORDER BY sessions DESC, ip ASC`,
		day, serviceID, kind,
	)
	if err != nil {
		return nil, fmt.Errorf("list top_ips: %w", err)
	}
	defer rows.Close()
	var out []TopIP
	for rows.Next() {
		var r TopIP
		if err := rows.Scan(&r.IP, &r.Sessions); err != nil {
			return nil, fmt.Errorf("scan top_ip: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("top_ips rows: %w", err)
	}
	return out, nil
}

// Rollup computes the daily rollup for the calendar day that contains t (UTC).
// It is idempotent: re-running for the same day upserts (ON CONFLICT DO
// UPDATE) so no duplicate rows are produced.
//
// P95 is computed in Go: SELECT all duration_ms values for the day+service+kind
// group, sort ascending, take index floor(0.95 * n).
func (s *SQLSink) Rollup(ctx context.Context, day time.Time) error {
	if s == nil || s.b == nil {
		return nil
	}
	dayStr := day.UTC().Format("2006-01-02")

	// Pull the aggregable groups.
	// Note: modernc.org/sqlite stores time.Time as "2006-01-02T15:04:05Z"
	// (ISO 8601 with T separator). SQLite's date() / strftime() do not parse
	// this format — only the "YYYY-MM-DD HH:MM:SS" form. We use substr(ts,1,10)
	// to extract the date prefix reliably across all driver versions.
	type group struct {
		serviceID string
		kind      string
	}
	groupRows, err := s.b.DB().QueryContext(ctx,
		`SELECT COALESCE(service_id,''), kind,
		        COUNT(*), SUM(bytes_in), SUM(bytes_out),
		        AVG(duration_ms)
		 FROM connection_logs
		 WHERE substr(started_at,1,10) = ?
		 GROUP BY service_id, kind`,
		dayStr,
	)
	if err != nil {
		return fmt.Errorf("rollup groups: %w", err)
	}
	defer groupRows.Close()

	type groupData struct {
		sessions      int64
		bytesIn       int64
		bytesOut      int64
		avgDurationMs float64
	}
	groups := make(map[group]groupData)
	var groupOrder []group
	for groupRows.Next() {
		var g group
		var d groupData
		if err := groupRows.Scan(&g.serviceID, &g.kind, &d.sessions, &d.bytesIn, &d.bytesOut, &d.avgDurationMs); err != nil {
			return fmt.Errorf("rollup scan: %w", err)
		}
		groups[g] = d
		groupOrder = append(groupOrder, g)
	}
	if err := groupRows.Err(); err != nil {
		return fmt.Errorf("rollup groups rows: %w", err)
	}
	// We need to close before re-querying.
	groupRows.Close()

	// Resolve the top-IPs toggle (Q12, v0.5.1 Task 5). Default true — missing
	// row, missing settings reader, OR an unparseable value all evaluate to
	// enabled. Only an explicit "false" disables the aggregation.
	topIPsEnabled := s.topIPsEnabled(ctx)

	// For each group, compute p95 from raw durations.
	for _, g := range groupOrder {
		p95, err := s.computeP95(ctx, dayStr, g.serviceID, g.kind)
		if err != nil {
			return fmt.Errorf("rollup p95 %s/%s: %w", g.serviceID, g.kind, err)
		}
		d := groups[g]
		avg := int64(d.avgDurationMs)
		_, err = s.b.DB().ExecContext(ctx,
			`INSERT INTO connection_log_rollups
			  (day, service_id, kind, sessions, bytes_in, bytes_out, avg_duration_ms, p95_duration_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(day, service_id, kind) DO UPDATE SET
			   sessions        = excluded.sessions,
			   bytes_in        = excluded.bytes_in,
			   bytes_out       = excluded.bytes_out,
			   avg_duration_ms = excluded.avg_duration_ms,
			   p95_duration_ms = excluded.p95_duration_ms`,
			dayStr, g.serviceID, g.kind,
			d.sessions, d.bytesIn, d.bytesOut, avg, p95,
		)
		if err != nil {
			return fmt.Errorf("rollup upsert %s/%s: %w", g.serviceID, g.kind, err)
		}

		// Top source IPs sub-step (Q12).
		if topIPsEnabled {
			if err := s.upsertTopIPs(ctx, dayStr, g.serviceID, g.kind); err != nil {
				return fmt.Errorf("rollup top_ips %s/%s: %w", g.serviceID, g.kind, err)
			}
		} else {
			// Setting flipped off: scrub any pre-existing aux rows for this
			// (day, service_id, kind) so the read path no longer surfaces
			// stale top-IP data.
			if _, err := s.b.DB().ExecContext(ctx,
				`DELETE FROM connection_log_rollup_top_ips
				  WHERE day = ? AND service_id = ? AND kind = ?`,
				dayStr, g.serviceID, g.kind,
			); err != nil {
				return fmt.Errorf("rollup top_ips delete %s/%s: %w", g.serviceID, g.kind, err)
			}
		}
	}
	return nil
}

// topIPsEnabled returns the effective value of the
// connection_logs.rollup_include_top_ips setting. Default-true policy: a
// missing settings reader, a missing key, or any value other than "false"
// evaluates to enabled.
func (s *SQLSink) topIPsEnabled(ctx context.Context) bool {
	if s == nil || s.settings == nil {
		return true
	}
	m, err := s.settings.GetSettings(ctx)
	if err != nil {
		s.log.Warn("connlog: GetSettings failed; assuming top_ips=true",
			slog.String("err", err.Error()))
		return true
	}
	v, ok := m["connection_logs.rollup_include_top_ips"]
	if !ok {
		return true
	}
	return v != "false"
}

// topIPsLimit is the per-(day, service_id, kind) cap on top-source-IP rows
// upserted by Rollup. Per the v0.5.1 plan / spec Q12.
const topIPsLimit = 10

// upsertTopIPs computes the top 10 source IPs by session count for the given
// (day, serviceID, kind) group and upserts them into
// connection_log_rollup_top_ips. Any pre-existing rows for that group that
// are NOT in the new top-10 set are deleted (so a re-rolled day doesn't leak
// stale low-count entries). Idempotent: re-running with the same underlying
// connection_logs rows yields identical aux-table contents.
func (s *SQLSink) upsertTopIPs(ctx context.Context, dayStr, serviceID, kind string) error {
	rows, err := s.b.DB().QueryContext(ctx,
		`SELECT source_ip, COUNT(*) AS sessions
		   FROM connection_logs
		  WHERE substr(started_at,1,10) = ?
		    AND COALESCE(service_id,'') = ?
		    AND kind = ?
		  GROUP BY source_ip
		  ORDER BY sessions DESC, source_ip ASC
		  LIMIT ?`,
		dayStr, serviceID, kind, topIPsLimit,
	)
	if err != nil {
		return fmt.Errorf("top_ips query: %w", err)
	}
	type ipRow struct {
		ip       string
		sessions int64
	}
	var top []ipRow
	for rows.Next() {
		var r ipRow
		if err := rows.Scan(&r.ip, &r.sessions); err != nil {
			rows.Close()
			return fmt.Errorf("top_ips scan: %w", err)
		}
		top = append(top, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("top_ips rows: %w", err)
	}
	rows.Close()

	// Two-phase: delete prior aux rows for this group, then insert the new
	// top-N — wrapped in a single transaction so concurrent readers never
	// observe an empty top-IPs window during the DELETE->INSERT gap (v0.5.2
	// BACKLOG #3). The delete is unconditional so a previously larger top-N
	// (or a previously different set of IPs) does not leak rows.
	tx, err := s.b.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("top_ips begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM connection_log_rollup_top_ips
		  WHERE day = ? AND service_id = ? AND kind = ?`,
		dayStr, serviceID, kind,
	); err != nil {
		return fmt.Errorf("top_ips clear: %w", err)
	}
	// Post-DELETE the INSERTs cannot conflict on (day, service_id, kind, ip),
	// so the ON CONFLICT DO UPDATE clause from v0.5.1 was dead code; dropped
	// here for clarity (v0.5.2 BACKLOG #3 second half).
	for _, r := range top {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO connection_log_rollup_top_ips
			  (day, service_id, kind, ip, sessions)
			 VALUES (?, ?, ?, ?, ?)`,
			dayStr, serviceID, kind, r.ip, r.sessions,
		); err != nil {
			return fmt.Errorf("top_ips insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("top_ips commit: %w", err)
	}
	return nil
}

// computeP95 pulls all duration_ms values for (day, serviceID, kind), sorts
// them ascending, and returns the value at index floor(0.95 * n).
func (s *SQLSink) computeP95(ctx context.Context, dayStr, serviceID, kind string) (int64, error) {
	rows, err := s.b.DB().QueryContext(ctx,
		`SELECT duration_ms FROM connection_logs
		 WHERE substr(started_at,1,10) = ?
		   AND COALESCE(service_id,'') = ?
		   AND kind = ?
		 ORDER BY duration_ms ASC`,
		dayStr, serviceID, kind,
	)
	if err != nil {
		return 0, fmt.Errorf("p95 query: %w", err)
	}
	defer rows.Close()

	var durations []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return 0, fmt.Errorf("p95 scan: %w", err)
		}
		durations = append(durations, d)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("p95 rows: %w", err)
	}
	if len(durations) == 0 {
		return 0, nil
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	idx := int(float64(len(durations)) * 0.95)
	if idx >= len(durations) {
		idx = len(durations) - 1
	}
	return durations[idx], nil
}
