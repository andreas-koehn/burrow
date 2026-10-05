// upstream_credential_handlers.go — upstream-credential binding JSON API
// (spec B.2 / v0.5.0 Task 5).
//
//	GET    /api/v1/upstream-credentials/slots          — list vault slots (global)
//	GET    /api/v1/services/{serviceID}/upstream-credential  — read binding
//	PUT    /api/v1/services/{serviceID}/upstream-credential  — bind
//	DELETE /api/v1/services/{serviceID}/upstream-credential  — unbind
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// CredentialStore is the narrow DB surface the upstream-credential handlers
// consume. *db.DB satisfies it via the methods added in
// internal/db/upstream_credentials.go.
type CredentialStore interface {
	GetUpstreamCredential(ctx context.Context, serviceID string) (db.ServiceUpstreamCredential, error)
	UpsertUpstreamCredential(ctx context.Context, c db.ServiceUpstreamCredential) error
	DeleteUpstreamCredential(ctx context.Context, serviceID string) error
}

// CredentialVaultIface is the narrow Vault surface the handlers consume.
// *credinject.EnvVault satisfies it; tests provide stubs.
type CredentialVaultIface interface {
	Get(slot string) (string, bool)
	Slots() []string
}

// Audit events (bind/unbind) are emitted via Deps.AuditAppender, which is the
// existing *audit.Logger adapter also used by backup_handlers.go.

// --- Permission gates -------------------------------------------------------

// requireSlotsRead gates GET /upstream-credentials/slots:
// admin OR ai:configure:any.
func (d Deps) requireSlotsRead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := d.callerRole(r)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if role == "admin" || authz.Can(role, authz.PermAIConfigureAny) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "forbidden")
	})
}

// ensureCredentialAccess gates all per-service upstream-credential routes.
//
//   - admin or PermAIConfigureAny → allow (and surface 404 when service missing).
//   - PermAIConfigureOwn          → allow only when the caller owns the service.
//   - else                        → 403.
//
// Returns true to continue; false means the handler already wrote the response.
func (d Deps) ensureCredentialAccess(w http.ResponseWriter, r *http.Request, serviceID string) bool {
	if serviceID == "" {
		writeErr(w, http.StatusBadRequest, "service id is required")
		return false
	}
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return false
	}
	uid := userID(r.Context())

	hasAny := role == "admin" || authz.Can(role, authz.PermAIConfigureAny)
	hasOwn := authz.Can(role, authz.PermAIConfigureOwn)
	if !hasAny && !hasOwn {
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}

	// We need the service row to check ownership (and to surface 404 cleanly
	// for :any callers). Use the IPGeoServices surface (GetServiceByID) which
	// is already available; fall back to CredentialServices if set.
	// Prefer CredentialServices (set in Deps); fall back to IPGeoServices.
	var svcLookup ServiceOwnerLookup
	if d.CredentialServices != nil {
		svcLookup = d.CredentialServices
	} else if d.IPGeoServices != nil {
		svcLookup = d.IPGeoServices
	}
	if svcLookup == nil {
		if !hasAny {
			writeErr(w, http.StatusInternalServerError, "service lookup unavailable")
			return false
		}
		// :any callers can proceed without the ownership lookup.
		return true
	}
	svc, err := svcLookup.GetServiceByID(r.Context(), serviceID)
	if errors.Is(err, db.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "service not found")
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "service lookup failed")
		return false
	}
	if !hasAny && svc.UserID != uid {
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}
	// A direct provider's credential is its own slot (PUT
	// /ai/providers/{slug}/upstream), not a binding on the backing row.
	if svc.Type == "direct" {
		writeDirectServiceErr(w)
		return false
	}
	return true
}

// writeDirectServiceErr answers a service route that does not apply to the
// backing row of a direct AI provider (store.ErrDirectService).
func writeDirectServiceErr(w http.ResponseWriter) {
	writeErr(w, http.StatusConflict, "this service backs a direct AI provider; this setting does not apply to it")
}

// --- Wire shapes ------------------------------------------------------------

// slotsResp is the wire shape for GET /upstream-credentials/slots.
type slotsResp struct {
	Slots []string `json:"slots"`
}

// credBindingResp is the wire shape for GET .../upstream-credential when bound.
type credBindingResp struct {
	Slot        string `json:"slot"`
	HeaderName  string `json:"header_name"`
	HeaderFmt   string `json:"header_format"`
	SlotPresent bool   `json:"slot_present"`
}

// credUnboundResp is the wire shape for GET .../upstream-credential when unbound.
type credUnboundResp struct {
	SlotPresent bool `json:"slot_present"`
}

// putCredReq is the PUT .../upstream-credential request body.
type putCredReq struct {
	Slot         string `json:"slot"`
	HeaderName   string `json:"header_name"`
	HeaderFormat string `json:"header_format"`
}

// --- Handlers ---------------------------------------------------------------

// GetUpstreamCredentialSlots handles GET /api/v1/upstream-credentials/slots.
// Permission: admin OR ai:configure:any (middleware-gated via requireSlotsRead).
// Returns {"slots": ["OPENAI", "X", ...]} — sorted, from the vault.
func (d Deps) GetUpstreamCredentialSlots(w http.ResponseWriter, r *http.Request) {
	if d.CredentialVault == nil {
		writeJSON(w, http.StatusOK, slotsResp{Slots: []string{}})
		return
	}
	slots := d.CredentialVault.Slots()
	if slots == nil {
		slots = []string{}
	}
	writeJSON(w, http.StatusOK, slotsResp{Slots: slots})
}

// GetServiceUpstreamCredential handles GET /api/v1/services/{serviceID}/upstream-credential.
//
// Returns:
//   - 200 {slot, header_name, header_format, slot_present:<bool>} when bound.
//   - 200 {slot_present: false} when unbound.
func (d Deps) GetServiceUpstreamCredential(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.ensureCredentialAccess(w, r, serviceID) {
		return
	}
	if d.CredentialDB == nil {
		// Store unavailable — treat as unbound.
		writeJSON(w, http.StatusOK, credUnboundResp{SlotPresent: false})
		return
	}
	cred, err := d.CredentialDB.GetUpstreamCredential(r.Context(), serviceID)
	if errors.Is(err, db.ErrNotFound) {
		writeJSON(w, http.StatusOK, credUnboundResp{SlotPresent: false})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load credential failed")
		return
	}
	slotPresent := false
	if d.CredentialVault != nil {
		_, slotPresent = d.CredentialVault.Get(cred.Slot)
	}
	writeJSON(w, http.StatusOK, credBindingResp{
		Slot:        cred.Slot,
		HeaderName:  cred.HeaderName,
		HeaderFmt:   cred.HeaderFormat,
		SlotPresent: slotPresent,
	})
}

// PutServiceUpstreamCredential handles PUT /api/v1/services/{serviceID}/upstream-credential.
// Body: {slot, header_name?, header_format?}.
// Defaults: header_name="Authorization", header_format="Bearer {key}".
// 400 if slot unknown or header_format missing {key}. 204 on success.
func (d Deps) PutServiceUpstreamCredential(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.ensureCredentialAccess(w, r, serviceID) {
		return
	}
	if d.CredentialDB == nil {
		writeErr(w, http.StatusInternalServerError, "credential store unavailable")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in putCredReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Slot = strings.TrimSpace(in.Slot)
	in.HeaderName = strings.TrimSpace(in.HeaderName)
	in.HeaderFormat = strings.TrimSpace(in.HeaderFormat)

	// Apply defaults.
	if in.HeaderName == "" {
		in.HeaderName = "Authorization"
	}
	if in.HeaderFormat == "" {
		in.HeaderFormat = "Bearer {key}"
	}

	// Validate slot is known to the vault.
	if d.CredentialVault == nil || !slotKnown(d.CredentialVault, in.Slot) {
		writeErr(w, http.StatusBadRequest, "unknown slot")
		return
	}
	// Validate header_format contains {key}.
	if !strings.Contains(in.HeaderFormat, "{key}") {
		writeErr(w, http.StatusBadRequest, "invalid header_format")
		return
	}

	cred := db.ServiceUpstreamCredential{
		ServiceID:    serviceID,
		Slot:         in.Slot,
		HeaderName:   in.HeaderName,
		HeaderFormat: in.HeaderFormat,
	}
	if err := d.CredentialDB.UpsertUpstreamCredential(r.Context(), cred); err != nil {
		writeErr(w, http.StatusInternalServerError, "save credential failed")
		return
	}

	// Audit (best-effort, same pattern as backup_handlers.go).
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceUpstreamCredentialBind,
			SubjectID: serviceID, SubjectLabel: serviceID,
			Result:   "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]string{"slot": in.Slot}),
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// DeleteServiceUpstreamCredential handles DELETE /api/v1/services/{serviceID}/upstream-credential.
// Clears the binding row. 204 on success (also 204 when no row existed).
func (d Deps) DeleteServiceUpstreamCredential(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	if !d.ensureCredentialAccess(w, r, serviceID) {
		return
	}
	if d.CredentialDB == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Read the slot before deleting so we can log it in the audit event.
	slot := ""
	if cred, err := d.CredentialDB.GetUpstreamCredential(r.Context(), serviceID); err == nil {
		slot = cred.Slot
	}

	if err := d.CredentialDB.DeleteUpstreamCredential(r.Context(), serviceID); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete credential failed")
		return
	}

	// Audit (only when a binding was present; best-effort).
	if slot != "" && d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		_ = d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
			Action:    audit.ActionServiceUpstreamCredentialUnbind,
			SubjectID: serviceID, SubjectLabel: serviceID,
			Result:   "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]string{"slot": slot}),
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// slotKnown returns true if the given slot handle is present in the vault.
func slotKnown(v CredentialVaultIface, slot string) bool {
	if slot == "" {
		return false
	}
	_, ok := v.Get(slot)
	return ok
}
