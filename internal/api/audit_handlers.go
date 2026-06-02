package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// AuditQueryStore is the read surface backing the JSON API. *db.DB
// satisfies it via ListAuditEvents.
type AuditQueryStore interface {
	ListAuditEvents(ctx context.Context, q db.AuditQuery) ([]db.AuditEvent, error)
}

// AuditChain is the action surface (verify, export, fingerprint) that the
// API delegates to. *audit.Logger satisfies it.
type AuditChain interface {
	Verify(ctx context.Context, fromID, toID string) (ok bool, mismatched string, err error)
	ExportNDJSON(ctx context.Context, w io.Writer, q audit.ExportQuery) error
	PublicKey() []byte
	FingerprintHex() string
}

// loggerChainAdapter wraps *audit.Logger to satisfy AuditChain. *audit.Logger
// returns ed25519.PublicKey from PublicKey(); the adapter narrows it to []byte
// so the API package doesn't need to import crypto/ed25519.
type loggerChainAdapter struct{ l *audit.Logger }

func (a loggerChainAdapter) Verify(ctx context.Context, fromID, toID string) (bool, string, error) {
	return a.l.Verify(ctx, fromID, toID)
}
func (a loggerChainAdapter) ExportNDJSON(ctx context.Context, w io.Writer, q audit.ExportQuery) error {
	return a.l.ExportNDJSON(ctx, w, q)
}
func (a loggerChainAdapter) PublicKey() []byte      { return []byte(a.l.PublicKey()) }
func (a loggerChainAdapter) FingerprintHex() string { return a.l.FingerprintHex() }

// NewAuditChainAdapter returns an AuditChain backed by the given Logger.
// cmd/server wires this in Deps.AuditChain so the API package can stay
// decoupled from the audit package's concrete type. The wrapper is exported
// to keep cmd/server free of any "needs to know the adapter type" coupling.
func NewAuditChainAdapter(l *audit.Logger) AuditChain { return loggerChainAdapter{l: l} }

// auditEventResp is the per-row wire shape for GET /audit/events.
// Mirrors db.AuditEvent — every column is included so the UI can render
// the full row without re-fetching.
type auditEventResp struct {
	ID           string          `json:"id"`
	Ts           time.Time       `json:"ts"`
	ActorID      string          `json:"actor_id"`
	ActorEmail   string          `json:"actor_email"`
	Action       string          `json:"action"`
	SubjectID    string          `json:"subject_id"`
	SubjectLabel string          `json:"subject_label"`
	Result       string          `json:"result"`
	SourceIP     string          `json:"source_ip"`
	UserAgent    string          `json:"user_agent"`
	RequestID    string          `json:"request_id"`
	Payload      json.RawMessage `json:"payload"`
	PrevHash     string          `json:"prev_hash"`
	Hash         string          `json:"hash"`
}

func toAuditEventResp(e db.AuditEvent) auditEventResp {
	pl := json.RawMessage(e.Payload)
	if len(pl) == 0 {
		pl = json.RawMessage(`{}`)
	}
	return auditEventResp{
		ID: e.ID, Ts: e.Ts, ActorID: e.ActorID, ActorEmail: e.ActorEmail,
		Action: e.Action, SubjectID: e.SubjectID, SubjectLabel: e.SubjectLabel,
		Result: e.Result, SourceIP: e.SourceIP, UserAgent: e.UserAgent,
		RequestID: e.RequestID, Payload: pl, PrevHash: e.PrevHash, Hash: e.Hash,
	}
}

// auditEventsLimit is the default per-page cap when ?limit= is absent.
const auditEventsLimit = 100

// auditEventsLimitMax is the hard cap applied even when ?limit= is given.
const auditEventsLimitMax = 1000

// requireAdminOrAuditRead is the middleware that gates every /audit route.
// Allows: role admin OR holds authz.PermAuditRead. Must run AFTER
// RequireSession (so 401 wins over 403 for unauthed requests).
func (d Deps) requireAdminOrAuditRead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		u, err := d.Users.GetUserByID(r.Context(), uid)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if u.Role == "admin" || authz.Can(u.Role, authz.PermAuditRead) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "audit:read required")
	})
}

// parseAuditQuery extracts the shared GET /audit/events + /audit/export
// query parameters.
func parseAuditQuery(r *http.Request) (db.AuditQuery, error) {
	q := db.AuditQuery{
		Action:   r.URL.Query().Get("action"),
		Actor:    r.URL.Query().Get("actor"),
		Q:        r.URL.Query().Get("q"),
		BeforeID: r.URL.Query().Get("before_id"),
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
		if n > auditEventsLimitMax {
			n = auditEventsLimitMax
		}
		q.Limit = n
	} else {
		q.Limit = auditEventsLimit
	}
	return q, nil
}

// GetAuditEvents handles GET /api/v1/audit/events.
//
// Returns the page of matching rows (id DESC) as a JSON array. The next
// cursor is the id of the LAST row — clients re-issue with ?before_id=<id>
// to fetch the next (older) page. An empty array means no more rows.
func (d Deps) GetAuditEvents(w http.ResponseWriter, r *http.Request) {
	if d.AuditEvents == nil {
		writeErr(w, http.StatusInternalServerError, "audit store unavailable")
		return
	}
	q, err := parseAuditQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := d.AuditEvents.ListAuditEvents(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list audit events failed")
		return
	}
	out := make([]auditEventResp, 0, len(rows))
	for _, e := range rows {
		out = append(out, toAuditEventResp(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// auditFingerprintResp is the JSON shape of GET /audit/fingerprint.
//
// public_key is the base64 (StdEncoding, no padding for ed25519 32-byte
// keys — but std lib emits padding for 44 chars; we use stdEncoding to
// match the trailer encoding consumers already do). fingerprint is the
// lowercase hex sha256 of the raw public key bytes.
type auditFingerprintResp struct {
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

// GetAuditFingerprint handles GET /api/v1/audit/fingerprint.
//
// Returns the audit signing key's public half so consumers can verify
// exported NDJSON files offline. The PRIVATE key is never returned
// (LoadOrGenerateSigningKey persists it under audit.signing_key in
// settings; settings.GET hides audit.* keys).
func (d Deps) GetAuditFingerprint(w http.ResponseWriter, r *http.Request) {
	if d.AuditChain == nil {
		writeErr(w, http.StatusInternalServerError, "audit chain unavailable")
		return
	}
	pub := d.AuditChain.PublicKey()
	if len(pub) == 0 {
		writeErr(w, http.StatusInternalServerError, "audit signing key not initialised")
		return
	}
	writeJSON(w, http.StatusOK, auditFingerprintResp{
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
		Fingerprint: d.AuditChain.FingerprintHex(),
	})
}

// GetAuditExport handles GET /api/v1/audit/export.
//
// Streams the matching rows as signed NDJSON (Content-Type
// application/x-ndjson). The final line is the
// {"_signature","fingerprint"} trailer; consumers verify against the
// fingerprint endpoint's public_key.
func (d Deps) GetAuditExport(w http.ResponseWriter, r *http.Request) {
	if d.AuditChain == nil {
		writeErr(w, http.StatusInternalServerError, "audit chain unavailable")
		return
	}
	q, err := parseAuditQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="audit.ndjson"`)
	w.WriteHeader(http.StatusOK)
	exportQ := audit.ExportQuery{
		Since: q.Since, Until: q.Until,
		Action: q.Action, Actor: q.Actor,
		FromID: q.FromID, ToID: q.ToID,
	}
	if err := d.AuditChain.ExportNDJSON(r.Context(), w, exportQ); err != nil {
		// Headers already flushed; log only — partial writes are the
		// expected failure mode for a streaming endpoint.
		d.Log.Warn("audit export failed mid-stream", "err", err)
	}
}

// auditVerifyReq is the optional POST body. Empty body = verify the whole
// chain. fromID/toID let an operator narrow the window (the inspector UI
// will use this for "verify this page").
type auditVerifyReq struct {
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
}

// auditVerifyResp is the JSON shape of POST /audit/verify.
type auditVerifyResp struct {
	OK           bool   `json:"ok"`
	FirstID      string `json:"first_id,omitempty"`
	LastID       string `json:"last_id,omitempty"`
	MismatchedID string `json:"mismatched_id,omitempty"`
}

// PostAuditVerify handles POST /api/v1/audit/verify.
//
// Body is optional ({} or {from_id,to_id}); empty body = whole chain.
// Returns ok + the first/last row id and the mismatched_id (if any).
func (d Deps) PostAuditVerify(w http.ResponseWriter, r *http.Request) {
	if d.AuditChain == nil || d.AuditEvents == nil {
		writeErr(w, http.StatusInternalServerError, "audit chain unavailable")
		return
	}
	req := auditVerifyReq{}
	if r.ContentLength != 0 {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "request body too large")
			return
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
		}
	}
	ok, mismatched, err := d.AuditChain.Verify(r.Context(), req.FromID, req.ToID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "audit verify failed")
		return
	}
	first, last := firstLastIDs(r.Context(), d.AuditEvents, req.FromID, req.ToID)
	writeJSON(w, http.StatusOK, auditVerifyResp{
		OK: ok, FirstID: first, LastID: last, MismatchedID: mismatched,
	})
}

// firstLastIDs returns the smallest and largest ids in (fromID, toID).
// Used by PostAuditVerify to populate first_id/last_id on the response.
// Errors are silently swallowed; the empty values are an acceptable fallback.
func firstLastIDs(ctx context.Context, store AuditQueryStore, fromID, toID string) (string, string) {
	// "Last" is the newest row in the range — id DESC, first hit.
	last, _ := store.ListAuditEvents(ctx, db.AuditQuery{
		FromID: fromID, ToID: toID, Limit: 1,
	})
	var lastID string
	if len(last) > 0 {
		lastID = last[0].ID
	}
	// "First" is the oldest row in the range. ListAuditEvents is id DESC
	// without a tail-only mode, so request a large enough page and take
	// the final element; for a sane audit footprint this is cheap.
	all, _ := store.ListAuditEvents(ctx, db.AuditQuery{
		FromID: fromID, ToID: toID, Limit: auditEventsLimitMax,
	})
	var firstID string
	if len(all) > 0 {
		firstID = all[len(all)-1].ID
	}
	return firstID, lastID
}
