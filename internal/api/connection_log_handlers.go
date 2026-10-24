package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
)

// ConnectionLogStore is the read surface for the connection_logs and
// connection_log_rollups tables that the JSON API consumes. *db.DB combined
// with the connlog.ListConnectionLogs / connlog.ListConnectionLogRollups
// helpers satisfies this implicitly via ConnLogDB (see below).
type ConnectionLogStore interface {
	// ListConnectionLogs returns connection_logs rows matching q.
	ListConnectionLogs(ctx context.Context, q connlog.ConnLogQuery) ([]db.ConnectionLog, error)
	// ListConnectionLogRollups returns connection_log_rollups rows matching q.
	ListConnectionLogRollups(ctx context.Context, q connlog.RollupQuery) ([]db.ConnectionLogRollup, error)
	// ListConnectionLogRollupTopIPs returns the persisted top-source-IPs rows
	// for one (day, service_id, kind) group (Q12, v0.5.1 Task 5). Empty
	// result means either the toggle was off at the last compaction or the
	// group has never been rolled up. The handler distinguishes the two by
	// consulting the connection_logs.rollup_include_top_ips setting before
	// calling — when the setting is "false" the handler omits the
	// top_source_ips field entirely (NOT empty).
	ListConnectionLogRollupTopIPs(ctx context.Context, day, serviceID, kind string) ([]connlog.TopIP, error)
	// ListConnectionLogRollupTopIPsBatch returns the persisted top-source-IPs
	// rows for all requested groups in a single round trip. Replaces the N+1
	// per-row pattern on the rollups endpoint (v0.5.2 BACKLOG #2).
	ListConnectionLogRollupTopIPsBatch(ctx context.Context, groups []connlog.TopIPsGroup) (map[connlog.TopIPsGroup][]connlog.TopIP, error)
}

// connLogDBAdapter wraps *db.DB + connlog helpers to satisfy ConnectionLogStore.
type connLogDBAdapter struct{ x *db.DB }

func (a connLogDBAdapter) ListConnectionLogs(ctx context.Context, q connlog.ConnLogQuery) ([]db.ConnectionLog, error) {
	return connlog.ListConnectionLogs(ctx, a.x, q)
}
func (a connLogDBAdapter) ListConnectionLogRollups(ctx context.Context, q connlog.RollupQuery) ([]db.ConnectionLogRollup, error) {
	return connlog.ListConnectionLogRollups(ctx, a.x, q)
}
func (a connLogDBAdapter) ListConnectionLogRollupTopIPs(ctx context.Context, day, serviceID, kind string) ([]connlog.TopIP, error) {
	return connlog.ListConnectionLogRollupTopIPs(ctx, a.x, day, serviceID, kind)
}
func (a connLogDBAdapter) ListConnectionLogRollupTopIPsBatch(ctx context.Context, groups []connlog.TopIPsGroup) (map[connlog.TopIPsGroup][]connlog.TopIP, error) {
	return connlog.ListConnectionLogRollupTopIPsBatch(ctx, a.x, groups)
}

// NewConnLogDBAdapter wraps a *db.DB so it satisfies ConnectionLogStore.
// cmd/server passes it via Deps.ConnLogDB after Task 17 wiring.
func NewConnLogDBAdapter(x *db.DB) ConnectionLogStore { return connLogDBAdapter{x: x} }

// connLogLimitDefault is the page size when ?limit= is absent.
const connLogLimitDefault = 100

// connLogLimitMax is the hard cap applied even when ?limit= is given.
const connLogLimitMax = 1000

// connectionLogResp is the JSON wire shape for one connection_logs row (spec E.2).
type connectionLogResp struct {
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
	// TokenName is the client token name of the user who forwards (kind
	// tcp_forward); "" otherwise.
	TokenName string `json:"token_name"`
	// Protocol is what the first bytes of a tcp connection were (tls, ssh,
	// rdp, http, unknown) and SNI the server name of its TLS ClientHello;
	// "" for other rows and for connections that were never looked at.
	Protocol string `json:"protocol"`
	SNI      string `json:"sni"`
	// GRPCMethod is the method of a gRPC call (/pkg.Service/Method) and
	// GRPCStatus its grpc-status; "" and null for every other row.
	GRPCMethod string `json:"grpc_method"`
	GRPCStatus *int   `json:"grpc_status"`
}

func toConnectionLogResp(r db.ConnectionLog) connectionLogResp {
	return connectionLogResp{
		ID:              r.ID,
		Kind:            r.Kind,
		ServiceID:       r.ServiceID,
		TunnelID:        r.TunnelID,
		UserID:          r.UserID,
		ClientSessionID: r.ClientSessionID,
		SourceIP:        r.SourceIP,
		UserAgent:       r.UserAgent,
		StartedAt:       r.StartedAt,
		EndedAt:         r.EndedAt,
		DurationMs:      r.DurationMs,
		BytesIn:         r.BytesIn,
		BytesOut:        r.BytesOut,
		Status:          r.Status,
		Reason:          r.Reason,
		TokenName:       r.TokenName,
		Protocol:        r.Protocol,
		SNI:             r.SNI,
		GRPCMethod:      r.GRPCMethod,
		GRPCStatus:      r.GRPCStatus,
	}
}

// connectionLogRollupResp is the JSON wire shape for one connection_log_rollups row.
//
// TopSourceIPs is a *pointer* to a slice so the JSON marshaler can express the
// three states of the Q12 toggle distinctly:
//   - toggle ON + group has data → pointer to populated slice → top_source_ips: [...]
//   - toggle ON + group is empty → pointer to empty []         → top_source_ips: []
//   - toggle OFF                  → nil pointer + omitempty     → field omitted entirely
//
// A plain []connlog.TopIP with `omitempty` cannot distinguish ON-but-empty
// from OFF (encoding/json treats both nil and zero-length slices as empty).
type connectionLogRollupResp struct {
	Day           string           `json:"day"`
	ServiceID     string           `json:"service_id"`
	Kind          string           `json:"kind"`
	Sessions      int64            `json:"sessions"`
	BytesIn       int64            `json:"bytes_in"`
	BytesOut      int64            `json:"bytes_out"`
	AvgDurationMs int64            `json:"avg_duration_ms"`
	P95DurationMs int64            `json:"p95_duration_ms"`
	TopSourceIPs  *[]connlog.TopIP `json:"top_source_ips,omitempty"`
}

func toConnectionLogRollupResp(r db.ConnectionLogRollup) connectionLogRollupResp {
	return connectionLogRollupResp{
		Day:           r.Day,
		ServiceID:     r.ServiceID,
		Kind:          r.Kind,
		Sessions:      r.Sessions,
		BytesIn:       r.BytesIn,
		BytesOut:      r.BytesOut,
		AvgDurationMs: r.AvgDurationMs,
		P95DurationMs: r.P95DurationMs,
	}
}

// parseConnLogQuery parses the shared query parameters for the list endpoint.
func parseConnLogQuery(r *http.Request) (connlog.ConnLogQuery, error) {
	q := connlog.ConnLogQuery{
		Kind:      r.URL.Query().Get("kind"),
		ServiceID: r.URL.Query().Get("service_id"),
		Q:         r.URL.Query().Get("q"),
		BeforeID:  r.URL.Query().Get("before_id"),
	}
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return q, errors.New("since must be RFC3339")
		}
		q.Since = &t
	}
	if v := r.URL.Query().Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return q, errors.New("until must be RFC3339")
		}
		q.Until = &t
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return q, errors.New("limit must be a positive integer")
		}
		if n > connLogLimitMax {
			n = connLogLimitMax
		}
		q.Limit = n
	} else {
		q.Limit = connLogLimitDefault
	}
	return q, nil
}

// GetConnectionLogs handles GET /api/v1/connection-logs.
//
// Returns a JSON array of connection log rows, newest first, with cursor
// pagination (?before_id=<last_id> for the next older page).
func (d Deps) GetConnectionLogs(w http.ResponseWriter, r *http.Request) {
	if d.ConnLogDB == nil {
		writeErr(w, http.StatusInternalServerError, "connection log store unavailable")
		return
	}
	q, err := parseConnLogQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := d.ConnLogDB.ListConnectionLogs(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list connection logs failed")
		return
	}
	out := make([]connectionLogResp, 0, len(rows))
	for _, row := range rows {
		out = append(out, toConnectionLogResp(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// parseRollupQuery parses the query parameters for the rollups endpoint.
func parseRollupQuery(r *http.Request) (connlog.RollupQuery, error) {
	q := connlog.RollupQuery{
		ServiceID: r.URL.Query().Get("service_id"),
		Kind:      r.URL.Query().Get("kind"),
	}
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			// Also try date-only form.
			t2, err2 := time.Parse("2006-01-02", v)
			if err2 != nil {
				return q, errors.New("since must be RFC3339 or YYYY-MM-DD")
			}
			t = t2
		}
		q.Since = &t
	}
	if v := r.URL.Query().Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			t2, err2 := time.Parse("2006-01-02", v)
			if err2 != nil {
				return q, errors.New("until must be RFC3339 or YYYY-MM-DD")
			}
			t = t2
		}
		q.Until = &t
	}
	return q, nil
}

// GetConnectionLogRollups handles GET /api/v1/connection-logs/rollups.
//
// When the connection_logs.rollup_include_top_ips setting is "true" (or unset
// — default true), each row's top_source_ips field is populated from the aux
// table connection_log_rollup_top_ips. When the setting is "false", the field
// is omitted from the JSON response entirely (omitempty marshals nil away).
// The handler tolerates a missing or failing settings reader by treating the
// toggle as enabled.
func (d Deps) GetConnectionLogRollups(w http.ResponseWriter, r *http.Request) {
	if d.ConnLogDB == nil {
		writeErr(w, http.StatusInternalServerError, "connection log store unavailable")
		return
	}
	q, err := parseRollupQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := d.ConnLogDB.ListConnectionLogRollups(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list rollups failed")
		return
	}

	// Resolve the Q12 toggle. Default-true policy: a missing Settings
	// dependency, a missing key, or any value other than the literal string
	// "false" evaluates to enabled.
	topIPsEnabled := true
	if d.Settings != nil {
		m, gerr := d.Settings.GetSettings(r.Context())
		if gerr == nil {
			if v, ok := m["connection_logs.rollup_include_top_ips"]; ok && v == "false" {
				topIPsEnabled = false
			}
		}
	}

	// When the toggle is enabled, fetch top-IPs for ALL rollup rows in a
	// single batched query (v0.5.2 BACKLOG #2: N+1 -> 2 round trips). The
	// result map is keyed by TopIPsGroup{Day, ServiceID, Kind}; a row with
	// no aux data is absent from the map (-> empty []TopIP via lookup).
	var topByGroup map[connlog.TopIPsGroup][]connlog.TopIP
	if topIPsEnabled && len(rows) > 0 {
		groups := make([]connlog.TopIPsGroup, 0, len(rows))
		seen := make(map[connlog.TopIPsGroup]struct{}, len(rows))
		for _, row := range rows {
			g := connlog.TopIPsGroup{Day: row.Day, ServiceID: row.ServiceID, Kind: row.Kind}
			if _, dup := seen[g]; dup {
				continue
			}
			seen[g] = struct{}{}
			groups = append(groups, g)
		}
		var berr error
		topByGroup, berr = d.ConnLogDB.ListConnectionLogRollupTopIPsBatch(r.Context(), groups)
		if berr != nil {
			writeErr(w, http.StatusInternalServerError, "list rollups failed")
			return
		}
	}

	out := make([]connectionLogRollupResp, 0, len(rows))
	for _, row := range rows {
		resp := toConnectionLogRollupResp(row)
		if topIPsEnabled {
			ips := topByGroup[connlog.TopIPsGroup{Day: row.Day, ServiceID: row.ServiceID, Kind: row.Kind}]
			// Pointer-to-slice: a non-nil pointer to an empty slice marshals
			// as `[]`, a nil pointer is omitted by `omitempty`. This is the
			// only way to express "toggle ON but no data" distinctly from
			// "toggle OFF" without a custom marshaler. See the doc comment
			// on connectionLogRollupResp.
			if ips == nil {
				ips = []connlog.TopIP{}
			}
			resp.TopSourceIPs = &ips
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// GetConnectionLogsExport handles GET /api/v1/connection-logs/export.
//
// Streams the matching rows as NDJSON (default) or CSV (?format=csv).
// The limit parameter is not applied to exports; all matching rows are
// returned.
func (d Deps) GetConnectionLogsExport(w http.ResponseWriter, r *http.Request) {
	if d.ConnLogDB == nil {
		writeErr(w, http.StatusInternalServerError, "connection log store unavailable")
		return
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "ndjson"
	}
	if format != "ndjson" && format != "csv" {
		writeErr(w, http.StatusBadRequest, "format must be ndjson or csv")
		return
	}

	// Parse filters (no limit for exports).
	q := connlog.ConnLogQuery{
		Kind:      r.URL.Query().Get("kind"),
		ServiceID: r.URL.Query().Get("service_id"),
		Limit:     connLogLimitMax, // cap at max; Task 17 may wire streaming
	}
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		q.Since = &t
	}
	if v := r.URL.Query().Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "until must be RFC3339")
			return
		}
		q.Until = &t
	}

	rows, err := d.ConnLogDB.ListConnectionLogs(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "export failed")
		return
	}

	switch format {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="connection-logs.csv"`)
		w.WriteHeader(http.StatusOK)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{
			"id", "kind", "service_id", "tunnel_id", "user_id", "client_session_id",
			"source_ip", "user_agent", "started_at", "ended_at", "duration_ms",
			"bytes_in", "bytes_out", "status", "reason", "token_name", "protocol", "sni", "grpc_method", "grpc_status",
		})
		for _, row := range rows {
			_ = cw.Write([]string{
				row.ID, row.Kind, row.ServiceID, row.TunnelID,
				row.UserID, row.ClientSessionID,
				row.SourceIP, row.UserAgent,
				row.StartedAt.Format(time.RFC3339),
				row.EndedAt.Format(time.RFC3339),
				fmt.Sprintf("%d", row.DurationMs),
				fmt.Sprintf("%d", row.BytesIn),
				fmt.Sprintf("%d", row.BytesOut),
				row.Status, row.Reason, row.TokenName, row.Protocol, row.SNI,
				row.GRPCMethod, grpcStatusCSV(row.GRPCStatus),
			})
		}
		cw.Flush()

	default: // ndjson
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="connection-logs.ndjson"`)
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for _, row := range rows {
			_ = enc.Encode(toConnectionLogResp(row))
		}
	}
}

// grpcStatusCSV is a grpc-status as a CSV cell: the number, "" for a row that is no gRPC call.
func grpcStatusCSV(s *int) string {
	if s == nil {
		return ""
	}
	return strconv.Itoa(*s)
}
