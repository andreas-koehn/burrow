package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/inspector"
)

// InspectorRings is the narrow surface the inspector handlers consume. The
// concrete *inspector.Manager satisfies it via these two methods.
type InspectorRings interface {
	// GetOrCreate returns (or creates) the per-service ring with the given
	// capacity. v0.4.0 Task 10/24 wires the per-service inspector.max_requests
	// override; for Task 8 the API handlers create rings on-demand with the
	// default capacity if Get returned nil.
	GetOrCreate(serviceID string, maxRequests int) *inspector.Ring
	// Get returns the existing ring for serviceID, or nil. Read paths use
	// this to avoid allocating a ring on the read of an unknown service.
	Get(serviceID string) *inspector.Ring
}

// InspectorOwnerLookup is the narrow surface for service-ownership checks
// during inspector permission gating. *store.Store satisfies it via
// GetServiceOwner; tests provide a fake.
type InspectorOwnerLookup interface {
	GetServiceOwner(ctx context.Context, serviceID string) (string, error)
}

// InspectorReplayer is the optional dep that re-fires an inspector entry's
// request through the proxy chain. Task 10/25 wires the real implementation;
// for Task 8 a nil Replayer returns 503 on POST .../replay so the route
// exists but degrades gracefully.
//
// When the request is the second arm of a replay-compare call, the caller
// pre-sets the header "Burrow-Cache: bypass" so the cache middleware skips
// its Lookup step. This back-door is only honoured by the cache middleware
// — engine-level Lookup is unchanged.
type InspectorReplayer interface {
	// Replay re-issues the (possibly-overridden) request and returns the
	// resulting Entry. The Replayer is responsible for capturing the new
	// entry into the per-service ring before returning.
	Replay(ctx context.Context, serviceID string, req *http.Request) (inspector.Entry, error)
}

// inspectorEntryJSON is the wire shape for one inspector entry. Bodies are
// rendered as UTF-8 strings when valid, else base64. The encoding field
// signals which is in use so clients render the response correctly.
type inspectorEntryJSON struct {
	ID           string                   `json:"id"`
	ServiceID    string                   `json:"service_id"`
	APIKeyID     string                   `json:"api_key_id,omitempty"`
	TS           time.Time                `json:"ts"`
	Method       string                   `json:"method"`
	Path         string                   `json:"path"`
	Status       int                      `json:"status"`
	DurationMs   int64                    `json:"duration_ms"`
	BytesIn      int64                    `json:"bytes_in"`
	BytesOut     int64                    `json:"bytes_out"`
	ReqHeaders   map[string]string        `json:"req_headers,omitempty"`
	ReqBody      string                   `json:"req_body,omitempty"`
	ReqBodyEnc   string                   `json:"req_body_encoding,omitempty"` // "utf8" | "base64"
	RespHeaders  map[string]string        `json:"resp_headers,omitempty"`
	RespBody     string                   `json:"resp_body,omitempty"`
	RespBodyEnc  string                   `json:"resp_body_encoding,omitempty"` // "utf8" | "base64"
	Truncated    bool                     `json:"truncated,omitempty"`
	BytesOmitted int64                    `json:"bytes_omitted,omitempty"`
	Cache        string                   `json:"cache,omitempty"`
	Redactions   []inspector.RedactionHit `json:"redactions,omitempty"`
	TraceID      string                   `json:"trace_id,omitempty"`
	RemoteIP     string                   `json:"remote_ip,omitempty"`
	MCP          *inspector.MCPInfo       `json:"mcp,omitempty"`
	AdapterLossy bool                     `json:"adapter_lossy,omitempty"`
}

// inspectorEntryToJSON converts a captured Entry to the wire shape. Bodies
// that decode cleanly as UTF-8 are stringified directly; otherwise they are
// base64-encoded so the JSON envelope stays valid.
func inspectorEntryToJSON(e inspector.Entry) inspectorEntryJSON {
	out := inspectorEntryJSON{
		ID:           e.ID,
		ServiceID:    e.ServiceID,
		APIKeyID:     e.APIKeyID,
		TS:           e.TS,
		Method:       e.Method,
		Path:         e.Path,
		Status:       e.Status,
		DurationMs:   e.DurationMs,
		BytesIn:      e.BytesIn,
		BytesOut:     e.BytesOut,
		ReqHeaders:   e.ReqHeaders,
		RespHeaders:  e.RespHeaders,
		Truncated:    e.Truncated,
		BytesOmitted: e.BytesOmitted,
		Cache:        e.Cache,
		Redactions:   e.Redactions,
		TraceID:      e.TraceID,
		RemoteIP:     e.RemoteIP,
		MCP:          e.MCP,
		AdapterLossy: e.AdapterLossy,
	}
	if len(e.ReqBody) > 0 {
		out.ReqBody, out.ReqBodyEnc = encodeBody(e.ReqBody)
	}
	if len(e.RespBody) > 0 {
		out.RespBody, out.RespBodyEnc = encodeBody(e.RespBody)
	}
	return out
}

// encodeBody renders body as utf8 when it is valid UTF-8, otherwise base64.
func encodeBody(body []byte) (string, string) {
	if utf8.Valid(body) {
		return string(body), "utf8"
	}
	return base64.StdEncoding.EncodeToString(body), "base64"
}

// decodeBody is the inverse of encodeBody for the replay request body
// override. Defaults to utf8 when the encoding is empty.
func decodeBody(body, enc string) ([]byte, error) {
	switch enc {
	case "", "utf8":
		return []byte(body), nil
	case "base64":
		return base64.StdEncoding.DecodeString(body)
	default:
		return nil, fmt.Errorf("unknown body encoding %q", enc)
	}
}

// requireInspectorRead enforces the read permissions for the inspector
// (list, get, stream): inspector:read:any (admin) OR
// (inspector:read:own AND caller owns the service).
//
// Returns "" (handled) when the caller may proceed. Otherwise it writes the
// appropriate status (401/403/404/500) and the caller should return.
func (d Deps) requireInspectorRead(w http.ResponseWriter, r *http.Request, serviceID string) bool {
	return d.requireInspectorPerm(w, r, serviceID,
		authz.PermInspectorReadAny, authz.PermInspectorReadOwn)
}

// requireInspectorReplay enforces the write permissions for the inspector
// (replay, replay-compare): inspector:replay:any OR
// (inspector:replay:own AND caller owns the service).
func (d Deps) requireInspectorReplay(w http.ResponseWriter, r *http.Request, serviceID string) bool {
	return d.requireInspectorPerm(w, r, serviceID,
		authz.PermInspectorReplayAny, authz.PermInspectorReplayOwn)
}

// requireInspectorPerm is the shared gate used by the read and replay
// surfaces. It loads the caller role, short-circuits when the role holds
// the :any permission, otherwise requires the :own permission AND a
// successful ownership check against InspectorServices.GetServiceOwner.
//
// Returns true when the caller may proceed (no response written), false
// otherwise (response already written).
func (d Deps) requireInspectorPerm(w http.ResponseWriter, r *http.Request, serviceID string,
	any, own authz.Permission) bool {
	if serviceID == "" {
		writeErr(w, http.StatusBadRequest, "service id is required")
		return false
	}
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if authz.Can(role, any) {
		// :any short-circuits ownership. We still want a 404 for unknown
		// services so the UI gets a meaningful error.
		if d.InspectorServices != nil {
			if _, err := d.InspectorServices.GetServiceOwner(r.Context(), serviceID); errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "service not found")
				return false
			}
		}
		return true
	}
	if !authz.Can(role, own) {
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}
	if d.InspectorServices == nil {
		writeErr(w, http.StatusInternalServerError, "service lookup unavailable")
		return false
	}
	owner, err := d.InspectorServices.GetServiceOwner(r.Context(), serviceID)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "service not found")
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "service lookup failed")
		return false
	}
	if owner != userID(r.Context()) {
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

// inspectorRing returns the per-service ring, creating one on demand with
// the default capacity if d.InspectorRings is non-nil. Returns nil when
// d.InspectorRings is nil (the routes are wired but the manager is not yet
// available — handlers degrade to an empty result).
func (d Deps) inspectorRing(serviceID string) *inspector.Ring {
	if d.InspectorRings == nil {
		return nil
	}
	return d.InspectorRings.GetOrCreate(serviceID, inspector.DefaultMaxRequests)
}

// ListInspectorRequests handles GET /api/v1/services/{serviceID}/inspector/requests.
// Query params: status (int), q (substring), since (RFC3339), limit (int).
// Response: 200 [InspectorEntry] in descending TS.
func (d Deps) ListInspectorRequests(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.requireInspectorRead(w, r, serviceID) {
		return
	}
	q, ok := parseListQuery(w, r.URL.Query())
	if !ok {
		return
	}
	ring := d.inspectorRing(serviceID)
	if ring == nil {
		writeJSON(w, http.StatusOK, []inspectorEntryJSON{})
		return
	}
	entries := ring.List(q)
	out := make([]inspectorEntryJSON, len(entries))
	for i, e := range entries {
		out[i] = inspectorEntryToJSON(e)
	}
	writeJSON(w, http.StatusOK, out)
}

// parseListQuery extracts the query filter from the URL values. Bad inputs
// write a 400 and return ok=false.
func parseListQuery(w http.ResponseWriter, vals url.Values) (inspector.ListQuery, bool) {
	q := inspector.ListQuery{Q: vals.Get("q")}
	if s := vals.Get("status"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 100 || n > 599 {
			writeErr(w, http.StatusBadRequest, "invalid status")
			return q, false
		}
		q.Status = n
	}
	if s := vals.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid since")
			return q, false
		}
		q.Since = t
	}
	if s := vals.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit")
			return q, false
		}
		q.Limit = n
	}
	return q, true
}

// GetInspectorRequest handles GET /api/v1/services/{serviceID}/inspector/requests/{rid}.
// Returns the full Entry (including bodies). 404 when the ring is missing
// or the id is not found.
func (d Deps) GetInspectorRequest(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.requireInspectorRead(w, r, serviceID) {
		return
	}
	rid := chi.URLParam(r, "rid")
	ring := d.inspectorRing(serviceID)
	if ring == nil {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	}
	e, ok := ring.Get(rid)
	if !ok {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	}
	writeJSON(w, http.StatusOK, inspectorEntryToJSON(e))
}

// replayReq is the JSON body of the replay / replay-compare endpoints.
type replayReq struct {
	Headers       map[string]string `json:"headers"`
	Body          string            `json:"body"`
	BodyEncoding  string            `json:"body_encoding"`
	FollowRouting bool              `json:"follow_routing"`
}

// ReplayInspectorRequest handles POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay.
// Response: 200 {new_entry: InspectorEntry}.
func (d Deps) ReplayInspectorRequest(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.requireInspectorReplay(w, r, serviceID) {
		return
	}
	rid := chi.URLParam(r, "rid")
	orig, ok := d.lookupEntry(w, serviceID, rid)
	if !ok {
		return
	}
	in, ok := parseReplayBody(w, r)
	if !ok {
		return
	}
	req, ok := d.buildReplayRequest(w, r.Context(), orig, in, false)
	if !ok {
		return
	}
	if d.InspectorReplayer == nil {
		writeErr(w, http.StatusServiceUnavailable, "replay engine unavailable")
		return
	}
	newEntry, err := d.InspectorReplayer.Replay(r.Context(), serviceID, req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "replay failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"new_entry": inspectorEntryToJSON(newEntry),
	})
}

// ReplayCompareInspectorRequest handles
// POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay-compare.
// Issues the replay with the Burrow-Cache: bypass header so the cache
// middleware skips Lookup, then diffs the original against the replayed
// response. Textual responses get a unified diff; binary fall back to
// metadata-only.
func (d Deps) ReplayCompareInspectorRequest(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.requireInspectorReplay(w, r, serviceID) {
		return
	}
	rid := chi.URLParam(r, "rid")
	orig, ok := d.lookupEntry(w, serviceID, rid)
	if !ok {
		return
	}
	in, ok := parseReplayBody(w, r)
	if !ok {
		return
	}
	req, ok := d.buildReplayRequest(w, r.Context(), orig, in, true)
	if !ok {
		return
	}
	if d.InspectorReplayer == nil {
		writeErr(w, http.StatusServiceUnavailable, "replay engine unavailable")
		return
	}
	replayed, err := d.InspectorReplayer.Replay(r.Context(), serviceID, req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "replay failed: "+err.Error())
		return
	}
	diff := buildCompareDiff(orig, replayed)
	writeJSON(w, http.StatusOK, map[string]any{
		"original": inspectorEntryToJSON(orig),
		"replayed": inspectorEntryToJSON(replayed),
		"diff":     diff,
	})
}

// lookupEntry resolves (serviceID, rid) → Entry, writing the appropriate
// 404 / 500 when missing. The owner check has already run by this point.
func (d Deps) lookupEntry(w http.ResponseWriter, serviceID, rid string) (inspector.Entry, bool) {
	ring := d.inspectorRing(serviceID)
	if ring == nil {
		writeErr(w, http.StatusNotFound, "request not found")
		return inspector.Entry{}, false
	}
	e, ok := ring.Get(rid)
	if !ok {
		writeErr(w, http.StatusNotFound, "request not found")
		return inspector.Entry{}, false
	}
	return e, true
}

// parseReplayBody reads the optional JSON body (headers/body/follow_routing
// overrides). Missing body is fine — the replay uses the original entry
// verbatim.
func parseReplayBody(w http.ResponseWriter, r *http.Request) (replayReq, bool) {
	var in replayReq
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB ceiling
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return in, false
	}
	if len(raw) == 0 {
		return in, true
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return in, false
	}
	return in, true
}

// buildReplayRequest constructs the *http.Request the Replayer will fire.
// When compare is true the Burrow-Cache: bypass header is set so the cache
// middleware skips its Lookup step.
func (d Deps) buildReplayRequest(w http.ResponseWriter, ctx context.Context,
	orig inspector.Entry, in replayReq, compare bool) (*http.Request, bool) {
	method := orig.Method
	path := orig.Path
	var body []byte
	if in.Body != "" {
		b, err := decodeBody(in.Body, in.BodyEncoding)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body encoding")
			return nil, false
		}
		body = b
	} else {
		body = append([]byte(nil), orig.ReqBody...)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, strings.NewReader(string(body)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return nil, false
	}
	// Start from the original entry's headers, then apply overrides.
	for k, v := range orig.ReqHeaders {
		req.Header.Set(k, v)
	}
	for k, v := range in.Headers {
		req.Header.Set(k, v)
	}
	if compare {
		req.Header.Set("Burrow-Cache", "bypass")
	}
	return req, true
}

// compareDiff is the wire shape of the diff field on the replay-compare
// response. Body is the unified diff string (textual) or the metadata-only
// summary (binary). Headers is a []string of "<name>: <old> → <new>" lines.
type compareDiff struct {
	Headers []string `json:"headers"`
	Body    string   `json:"body"`
}

// buildCompareDiff renders headers + body diff. Binary content-types fall
// through to a metadata-only summary.
func buildCompareDiff(orig, replayed inspector.Entry) compareDiff {
	headers := inspector.HeadersDiff(orig.RespHeaders, replayed.RespHeaders)
	if headers == nil {
		headers = []string{}
	}
	ct := ""
	if v, ok := lookupCIHeader(orig.RespHeaders, "content-type"); ok {
		ct = v
	}
	if !inspector.IsTextualContentType(ct) {
		return compareDiff{
			Headers: headers,
			Body:    inspector.MetadataOnlyBody(len(orig.RespBody), len(replayed.RespBody)),
		}
	}
	return compareDiff{
		Headers: headers,
		Body:    inspector.UnifiedBody(orig.RespBody, replayed.RespBody),
	}
}

// lookupCIHeader is a case-insensitive map lookup matching the helper in
// inspector/diff.go but local to the api package.
func lookupCIHeader(m map[string]string, key string) (string, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// InspectorStream handles GET /api/v1/services/{serviceID}/inspector/stream.
// Subscribes to the per-service ring's bus and emits one SSE "event: request"
// frame per captured entry. Flushes after every frame so SSE clients see
// events immediately (no buffering — same invariant as Task 3).
func (d Deps) InspectorStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	serviceID := chi.URLParam(r, "serviceID")
	if !d.requireInspectorRead(w, r, serviceID) {
		return
	}
	if d.InspectorRings == nil {
		writeErr(w, http.StatusInternalServerError, "inspector unavailable")
		return
	}
	ring := d.InspectorRings.GetOrCreate(serviceID, inspector.DefaultMaxRequests)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := ring.Subscribe()
	defer cancel()

	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(inspectorEntryToJSON(e))
			if err != nil {
				// Skip this frame on encoding failure; keep the stream alive.
				continue
			}
			fmt.Fprintf(w, "event: request\ndata: %s\n\n", payload)
			flusher.Flush()
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}
