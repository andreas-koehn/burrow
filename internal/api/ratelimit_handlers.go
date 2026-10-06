package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/quota"
)

// RateLimitStore is the narrow CRUD surface the rate-limit handlers consume.
// *db.DB satisfies it; tests provide a fake.
type RateLimitStore interface {
	ListRateLimits(ctx context.Context) ([]db.RateLimit, error)
	GetRateLimit(ctx context.Context, id string) (db.RateLimit, error)
	CreateRateLimit(ctx context.Context, rl db.RateLimit) error
	UpdateRateLimit(ctx context.Context, rl db.RateLimit) error
	DeleteRateLimit(ctx context.Context, id string) error
}

// QuotaEngine is the narrow runtime surface the API + chain consume from
// internal/quota. The handlers call Reload after mutations so the in-memory
// snapshot of rules stays consistent with the DB row. UsageFor backs the
// /rate-limits/usage endpoint.
type QuotaEngine interface {
	Reload(ctx context.Context) error
	UsageFor(ctx context.Context, who quota.Subjects) []quota.Usage
	Limits() []quota.Limit
	// DropBucket evicts the token bucket for a specific rule shape from the
	// in-memory store. Called by DeleteRateLimit as a belt-and-suspenders
	// guard against Reload failures leaving a saturated bucket alive.
	DropBucket(scope, subject, dimension, window string)
}

// rateLimitResp is the wire shape for one configured rate-limit. Mirrors
// the spec Part D.2 Limit struct exactly so the JSON contract is stable.
type rateLimitResp struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"` // api_key|gateway_key|model|role|service|global
	Subject   string `json:"subject"`
	Dimension string `json:"dimension"` // rpm|bpm
	Limit     int    `json:"limit"`
	Burst     int    `json:"burst"`
	Window    string `json:"window"` // minute|day
}

func toRateLimitResp(rl db.RateLimit) rateLimitResp {
	return rateLimitResp{
		ID:        rl.ID,
		Scope:     rl.Scope,
		Subject:   rl.Subject,
		Dimension: rl.Dimension,
		Limit:     int(rl.Lim),
		Burst:     int(rl.Burst),
		Window:    rl.Window,
	}
}

// rateLimitReq is the wire shape for POST + PUT bodies. The id is allocated
// server-side on create and pulled from the path on update.
type rateLimitReq struct {
	Scope     string `json:"scope"`
	Subject   string `json:"subject"`
	Dimension string `json:"dimension"`
	Limit     int    `json:"limit"`
	Burst     int    `json:"burst"`
	Window    string `json:"window"`
}

// validScopes lists the closed set of allowed scope values. Validated on
// every POST/PUT to keep stale wire enums from leaking into the engine.
var validScopes = map[string]bool{
	quota.ScopeAPIKey:     true,
	quota.ScopeRole:       true,
	quota.ScopeService:    true,
	quota.ScopeGlobal:     true,
	quota.ScopeGatewayKey: true, // subject: the gateway key's id
	quota.ScopeModel:      true, // subject: the model name clients ask for
}

const msgBadRateLimitScope = "scope must be one of api_key|gateway_key|model|role|service|global"

var validDimensions = map[string]bool{
	quota.DimensionRPM: true,
	quota.DimensionBPM: true,
}

var validWindows = map[string]bool{
	quota.WindowMinute: true,
	quota.WindowDay:    true,
}

// validateRateLimit returns "" on success or a user-visible error string.
// Limit and Burst must both be positive integers; the global scope's
// subject must be empty (any non-empty value is rejected so the wire
// contract is unambiguous).
func validateRateLimit(in rateLimitReq) string {
	if !validScopes[in.Scope] {
		return msgBadRateLimitScope
	}
	if !validDimensions[in.Dimension] {
		return "dimension must be one of rpm|bpm"
	}
	if in.Window == "" {
		in.Window = quota.WindowMinute
	}
	if !validWindows[in.Window] {
		return "window must be one of minute|day"
	}
	if in.Limit <= 0 {
		return "limit must be > 0"
	}
	if in.Burst <= 0 {
		return "burst must be > 0"
	}
	if in.Scope == quota.ScopeGlobal && in.Subject != "" {
		return "global scope must not specify a subject"
	}
	if in.Scope != quota.ScopeGlobal && in.Subject == "" {
		return "subject is required for non-global scopes"
	}
	if len(in.Subject) > 256 {
		return "subject too long (max 256 chars)"
	}
	return ""
}

// requireQuotasReadOwnOrAny is the read-gate for GET /rate-limits and
// GET /rate-limits/usage. Admin always passes; non-admin requires either
// quotas:read:own or quotas:read:any.
func (d Deps) requireQuotasReadOwnOrAny(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := d.callerRole(r)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if role == "admin" ||
			authz.Can(role, authz.PermQuotasReadOwn) ||
			authz.Can(role, authz.PermQuotasReadAny) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "quotas:read required")
	})
}

// requireQuotasManageAny is the write-gate for POST/PUT/DELETE /rate-limits.
// Admin always passes; non-admin requires quotas:manage:any (there is no
// :own write permission in the spec — limit configuration is global state).
func (d Deps) requireQuotasManageAny(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := d.callerRole(r)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if role == "admin" || authz.Can(role, authz.PermQuotasManageAny) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "quotas:manage:any required")
	})
}

// GetRateLimits handles GET /api/v1/rate-limits.
// Returns the list of every configured rate-limit row.
func (d Deps) GetRateLimits(w http.ResponseWriter, r *http.Request) {
	if d.RateLimitDB == nil {
		writeJSON(w, http.StatusOK, []rateLimitResp{})
		return
	}
	rows, err := d.RateLimitDB.ListRateLimits(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list rate limits failed")
		return
	}
	out := make([]rateLimitResp, len(rows))
	for i, rl := range rows {
		out[i] = toRateLimitResp(rl)
	}
	writeJSON(w, http.StatusOK, out)
}

// PostRateLimit handles POST /api/v1/rate-limits. Returns 201 with the
// created row; 400 on validation failure. On success the engine is
// synchronously Reloaded so the new rule is in effect when the handler
// returns (predictable read-your-writes semantics).
func (d Deps) PostRateLimit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var in rateLimitReq
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Scope = strings.TrimSpace(in.Scope)
	in.Subject = strings.TrimSpace(in.Subject)
	in.Dimension = strings.TrimSpace(in.Dimension)
	in.Window = strings.TrimSpace(in.Window)
	if in.Window == "" {
		in.Window = quota.WindowMinute
	}
	if msg := validateRateLimit(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if d.RateLimitDB == nil {
		writeErr(w, http.StatusInternalServerError, "rate-limit store unavailable")
		return
	}
	row := db.RateLimit{
		ID:        uuid.NewString(),
		Scope:     in.Scope,
		Subject:   in.Subject,
		Dimension: in.Dimension,
		Lim:       int64(in.Limit),
		Burst:     int64(in.Burst),
		Window:    in.Window,
	}
	if err := d.RateLimitDB.CreateRateLimit(r.Context(), row); err != nil {
		writeErr(w, http.StatusInternalServerError, "create rate limit failed")
		return
	}
	// Reload synchronously so the new rule is enforced on the next charge.
	if d.RateLimits != nil {
		if err := d.RateLimits.Reload(r.Context()); err != nil {
			// Log-and-continue: the row is persisted; the next periodic
			// reload (or a fresh Charge) will pick it up.
			d.Log.Error("quota: reload after POST failed", "err", err.Error())
		}
	}
	// Read back so created_at reflects the DB default.
	created, err := d.RateLimitDB.GetRateLimit(r.Context(), row.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, toRateLimitResp(row))
		return
	}
	writeJSON(w, http.StatusCreated, toRateLimitResp(created))
}

// PutRateLimit handles PUT /api/v1/rate-limits/{id}. 204 on success;
// 404 when no row matches; 400 on validation failure.
func (d Deps) PutRateLimit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var in rateLimitReq
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Scope = strings.TrimSpace(in.Scope)
	in.Subject = strings.TrimSpace(in.Subject)
	in.Dimension = strings.TrimSpace(in.Dimension)
	in.Window = strings.TrimSpace(in.Window)
	if in.Window == "" {
		in.Window = quota.WindowMinute
	}
	if msg := validateRateLimit(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if d.RateLimitDB == nil {
		writeErr(w, http.StatusInternalServerError, "rate-limit store unavailable")
		return
	}
	row := db.RateLimit{
		ID:        id,
		Scope:     in.Scope,
		Subject:   in.Subject,
		Dimension: in.Dimension,
		Lim:       int64(in.Limit),
		Burst:     int64(in.Burst),
		Window:    in.Window,
	}
	if err := d.RateLimitDB.UpdateRateLimit(r.Context(), row); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "rate limit not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "update rate limit failed")
		return
	}
	if d.RateLimits != nil {
		if err := d.RateLimits.Reload(r.Context()); err != nil {
			d.Log.Error("quota: reload after PUT failed", "err", err.Error())
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteRateLimit handles DELETE /api/v1/rate-limits/{id}. 204 on success;
// 404 when no row matches.
func (d Deps) DeleteRateLimit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if d.RateLimitDB == nil {
		writeErr(w, http.StatusInternalServerError, "rate-limit store unavailable")
		return
	}
	// Fetch the rule before deleting so we can evict its bucket unconditionally
	// below. GetRateLimit failure is non-fatal — we proceed with delete and
	// fall back to Reload-only invalidation.
	existing, fetchErr := d.RateLimitDB.GetRateLimit(r.Context(), id)
	if err := d.RateLimitDB.DeleteRateLimit(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "rate limit not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete rate limit failed")
		return
	}
	if d.RateLimits != nil {
		// Belt-and-suspenders: drop the bucket directly so a saturated bucket
		// cannot survive a Reload failure. If GetRateLimit succeeded we have the
		// exact shape; if it failed we skip DropBucket and rely on Reload alone.
		if fetchErr == nil {
			win := existing.Window
			if win == "" {
				win = quota.WindowMinute
			}
			d.RateLimits.DropBucket(existing.Scope, existing.Subject, existing.Dimension, win)
		}
		if err := d.RateLimits.Reload(r.Context()); err != nil {
			d.Log.Error("quota: reload after DELETE failed", "err", err.Error())
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// usageRow is the wire shape for one element of /rate-limits/usage's
// "limits" array. It is the configured rate-limit's fields plus the live
// counters (used + reset_seconds).
type usageRow struct {
	rateLimitResp
	Used         int64 `json:"used"`
	ResetSeconds int   `json:"reset_seconds"`
}

// usageResp is the wire shape for GET /rate-limits/usage.
type usageResp struct {
	Limits []usageRow `json:"limits"`
}

// GetRateLimitUsage handles GET /api/v1/rate-limits/usage?scope=...&subject=...
//
// The query params are used to scope the response: a caller without
// quotas:read:any can only see usage for their own subjects (typically the
// session's own api_key/role/service). For v0.4.0 Task 11 we accept the
// query params as the explicit subject identity and rely on the
// quotas:read:own/:any gate at the router level for permission. (Spec D.2
// describes the contract surface; the cross-check against "is this YOUR
// api_key" lands in a follow-up task once Subjects are wired through the
// session.)
func (d Deps) GetRateLimitUsage(w http.ResponseWriter, r *http.Request) {
	if d.RateLimits == nil {
		writeJSON(w, http.StatusOK, usageResp{Limits: []usageRow{}})
		return
	}
	q := r.URL.Query()
	scope := q.Get("scope")
	subject := q.Get("subject")

	who := quota.Subjects{}
	switch scope {
	case quota.ScopeGatewayKey:
		// A gateway key's usage is its owner's to see; anyone else needs
		// quotas:read:any. The answer is the same for another user's key and
		// for one that does not exist.
		if !d.mayReadGatewayKeyUsage(r, subject) {
			writeErr(w, http.StatusForbidden, "quotas:read:any required")
			return
		}
		who.GatewayKeyID = subject
	case quota.ScopeModel:
		// A model's usage is that of every key that asked for it.
		if !d.mayReadAnyQuota(r) {
			writeErr(w, http.StatusForbidden, "quotas:read:any required")
			return
		}
		who.Model = subject
	case quota.ScopeAPIKey:
		who.APIKeyID = subject
	case quota.ScopeRole:
		who.RoleName = subject
	case quota.ScopeService:
		who.ServiceID = subject
	case quota.ScopeGlobal, "":
		// no subject — engine matches global limits regardless.
	default:
		writeErr(w, http.StatusBadRequest, msgBadRateLimitScope)
		return
	}

	rows := d.RateLimits.UsageFor(r.Context(), who)
	out := usageResp{Limits: make([]usageRow, 0, len(rows))}
	for _, u := range rows {
		// u embeds quota.Limit; the inner Limit int field collides with the
		// embedded type name, so it alone needs the explicit u.Limit.Limit
		// qualifier. The other fields are promoted and accessed directly.
		out.Limits = append(out.Limits, usageRow{
			rateLimitResp: rateLimitResp{
				ID:        u.ID,
				Scope:     u.Scope,
				Subject:   u.Subject,
				Dimension: u.Dimension,
				Limit:     u.Limit.Limit,
				Burst:     u.Burst,
				Window:    u.Window,
			},
			Used:         u.Used,
			ResetSeconds: u.ResetSeconds,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// mayReadAnyQuota reports whether the caller is an admin or holds
// quotas:read:any. A caller whose role cannot be read may not.
func (d Deps) mayReadAnyQuota(r *http.Request) bool {
	role, err := d.callerRole(r)
	return err == nil && (role == "admin" || authz.Can(role, authz.PermQuotasReadAny))
}

// mayReadGatewayKeyUsage reports whether the caller may see the usage of
// gateway key id: anyone who may read any quota, and the key's owner.
func (d Deps) mayReadGatewayKeyUsage(r *http.Request, id string) bool {
	if d.mayReadAnyQuota(r) {
		return true
	}
	uid := userID(r.Context())
	if d.AIGatewayKeys == nil || uid == "" || id == "" {
		return false
	}
	// As a non-admin: the store returns the caller's own keys only.
	keys, err := d.AIGatewayKeys.ListGatewayKeys(r.Context(), uid, "")
	if err != nil {
		d.warn("gateway keys could not be read", "err", err)
		return false
	}
	for _, k := range keys {
		if k.ID == id && k.UserID == uid {
			return true
		}
	}
	return false
}
