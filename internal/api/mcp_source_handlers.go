package api

// mcp_source_handlers.go — /mcp/sources: the tool sources of the MCP gateway.
//
// A source is a row of the mcp_sources table: an MCP server whose tools the
// gateway serves behind gateway keys. Kind "remote" is an https server the
// relay calls itself (its URL, and the name of the vault slot that holds the
// credential); kind "tunnel" is a server behind a client, bound to an existing
// http service. Every source has a backing service row (D6): a remote source's
// is hidden and owned by the admin who created the source; a tunnel source's
// is the service it was bound to.
//
// Reads need mcp:sources:read or :manage; every write needs mcp:sources:manage
// and, while no services domain is set, a recent password confirmation (the
// write carries a URL and a credential slot). The credential itself is never
// part of a request or a response: credential_slot names a vault slot and
// credential_present tells whether that slot holds a value.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcpgw"
	"github.com/ankoehn/burrow/internal/store"
)

// defaultMCPMaxConcurrent is a new source's max_concurrent when the create
// body leaves it out. Sources created before keep what they have.
const defaultMCPMaxConcurrent = 16

// mcpSourceResp is the JSON wire shape of one tool source.
type mcpSourceResp struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	ServiceID string `json:"service_id"`
	// EndpointURL is where MCP clients reach the source:
	// https://<auth_domain>/mcp/<slug>. Empty while the relay has no auth domain.
	EndpointURL string `json:"endpoint_url"`

	// Remote sources. credential_slot names a vault slot; credential_present
	// tells whether the vault holds a value for it. Never the value.
	URL               string `json:"url"`
	CredentialSlot    string `json:"credential_slot"`
	CredentialPresent bool   `json:"credential_present"`
	AuthHeader        string `json:"auth_header"`
	AuthFormat        string `json:"auth_format"`
	// ProtocolVersion pins the MCP revision toward the upstream; "" negotiates.
	ProtocolVersion string `json:"protocol_version"`

	RPM           int  `json:"rpm"`
	MaxConcurrent int  `json:"max_concurrent"`
	TimeoutS      int  `json:"timeout_s"`
	Enabled       bool `json:"enabled"`

	LastSyncAt    *time.Time `json:"last_sync_at"`
	LastSyncError string     `json:"last_sync_error"`
	ToolCount     int        `json:"tool_count"`
	PendingCount  int        `json:"pending_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func (d Deps) mcpSourceView(s db.MCPSource) mcpSourceResp {
	v := mcpSourceResp{
		Slug: s.Slug, Name: s.Name, Kind: s.Kind, ServiceID: s.ServiceID,
		URL: s.URL, CredentialSlot: s.CredentialSlot, CredentialPresent: d.credentialPresent(s.CredentialSlot),
		AuthHeader: s.AuthHeader, AuthFormat: s.AuthFormat, ProtocolVersion: s.ProtocolVersion,
		RPM: s.RPM, MaxConcurrent: s.MaxConcurrent, TimeoutS: s.TimeoutS, Enabled: s.Enabled,
		LastSyncAt: s.LastSyncAt, LastSyncError: s.LastSyncError, ToolCount: s.ToolCount, PendingCount: s.PendingCount,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
	if d.AuthDomain != "" {
		v.EndpointURL = "https://" + d.AuthDomain + "/mcp/" + s.Slug
	}
	return v
}

// postMCPSourceReq is the body of POST /api/v1/mcp/sources. url and the
// credential fields are for kind "remote", service_id for kind "tunnel".
type postMCPSourceReq struct {
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	ServiceID       string `json:"service_id"`
	URL             string `json:"url"`
	CredentialSlot  string `json:"credential_slot"`
	AuthHeader      string `json:"auth_header"`
	AuthFormat      string `json:"auth_format"`
	ProtocolVersion string `json:"protocol_version"`
	RPM             *int   `json:"rpm"`
	MaxConcurrent   *int   `json:"max_concurrent"`
	TimeoutS        *int   `json:"timeout_s"`
	Enabled         *bool  `json:"enabled"`
}

// putMCPSourceReq is the body of PUT /api/v1/mcp/sources/{slug}: a field that
// is left out keeps its stored value, "" clears credential_slot and
// protocol_version.
type putMCPSourceReq struct {
	Name            *string `json:"name"`
	URL             *string `json:"url"`
	CredentialSlot  *string `json:"credential_slot"`
	AuthHeader      *string `json:"auth_header"`
	AuthFormat      *string `json:"auth_format"`
	ProtocolVersion *string `json:"protocol_version"`
	RPM             *int    `json:"rpm"`
	MaxConcurrent   *int    `json:"max_concurrent"`
	TimeoutS        *int    `json:"timeout_s"`
	Enabled         *bool   `json:"enabled"`
}

// mcpSources returns the store, or answers 503 when the relay has none.
func (d Deps) mcpSources(w http.ResponseWriter) (MCPSourceStore, bool) {
	if d.MCPSources == nil {
		writeErr(w, http.StatusServiceUnavailable, "tool sources are not available")
		return nil, false
	}
	return d.MCPSources, true
}

// mapMCPSourceErr writes the HTTP error for a tool source store error and
// reports whether it handled it.
func mapMCPSourceErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrInvalidMCPSourceSlug):
		writeErr(w, http.StatusBadRequest, store.MCPSourceSlugRule)
	case errors.Is(err, store.ErrMCPSourceNotFound):
		writeErr(w, http.StatusNotFound, "tool source not found")
	case errors.Is(err, store.ErrMCPSourceExists):
		writeErr(w, http.StatusConflict, "tool source slug or service already in use")
	case errors.Is(err, store.ErrMCPSourceBusy):
		writeErr(w, http.StatusConflict, "the tool source was changed by someone else at the same time; try again")
	case errors.Is(err, store.ErrMCPSourceService):
		writeErr(w, http.StatusConflict, "a tunnel tool source needs an existing http service")
	case errors.Is(err, store.ErrInvalidMCPSource):
		// The reason names the field; it never repeats the URL or a header value.
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), store.ErrInvalidMCPSource.Error()+": "))
	default:
		return false
	}
	return true
}

// auditMCPSource appends one tool source audit event (best-effort).
func (d Deps) auditMCPSource(r *http.Request, action string, s db.MCPSource, payload map[string]any) {
	if d.AuditAppender == nil {
		return
	}
	lc := audit.LogContextFrom(r.Context())
	_ = d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
		Action:    action,
		SubjectID: s.Slug, SubjectLabel: s.Name,
		Result:   "ok",
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		Payload: audit.MustJSON(payload),
	})
}

// mcpSourceAudit is the audit payload of a source's settings: names only,
// never a credential.
func mcpSourceAudit(s db.MCPSource) map[string]any {
	return map[string]any{
		"kind": s.Kind, "service_id": s.ServiceID, "url": s.URL, "credential_slot": s.CredentialSlot,
		"auth_header": s.AuthHeader, "protocol_version": s.ProtocolVersion,
		"rpm": s.RPM, "max_concurrent": s.MaxConcurrent, "timeout_s": s.TimeoutS, "enabled": s.Enabled,
	}
}

// GetMCPSources handles GET /api/v1/mcp/sources.
func (d Deps) GetMCPSources(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	rows, err := st.ListMCPSources(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]mcpSourceResp, 0, len(rows))
	for _, s := range rows {
		out = append(out, d.mcpSourceView(s))
	}
	writeJSON(w, http.StatusOK, out)
}

// GetMCPSource handles GET /api/v1/mcp/sources/{slug}.
func (d Deps) GetMCPSource(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	s, err := st.MCPSourceBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		if !mapMCPSourceErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, d.mcpSourceView(s))
}

// PostMCPSource handles POST /api/v1/mcp/sources (mcp:sources:manage).
func (d Deps) PostMCPSource(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var in postMCPSourceReq
	if msg, unknown := decodeStrictJSON(r.Body, &in); msg != "" {
		if unknown {
			msg += credentialHint
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !store.ValidMCPSourceSlug(in.Slug) {
		writeErr(w, http.StatusBadRequest, store.MCPSourceSlugRule)
		return
	}
	if in.Name == "" {
		in.Name = in.Slug
	}
	if in.MaxConcurrent == nil {
		// One source must not take every worker of the relay by default;
		// 0 (no limit) is there for whoever asks for it.
		conc := defaultMCPMaxConcurrent
		in.MaxConcurrent = &conc
	}
	wasGatewayOnly := false
	switch in.Kind {
	case "remote":
		if !d.upstreamHostAllowedAs(w, r, in.URL, "url") {
			return
		}
	case "tunnel":
		// The source closes the service's direct doors, so the caller must be
		// able to configure the service; the route's own gate is
		// mcp:sources:manage, and a token must also declare that it
		// configures services.
		role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
		var svc store.ServiceDetail
		if err == nil {
			svc, err = d.Services.GetService(r.Context(), userID(r.Context()), role, in.ServiceID)
		}
		switch {
		case errors.Is(err, store.ErrForbidden) || (err == nil && !mayConfigureServices(r, role)):
			writeErr(w, http.StatusForbidden, "binding a service needs the right to configure it")
			return
		case errors.Is(err, db.ErrNotFound):
			writeErr(w, http.StatusConflict, "a tunnel tool source needs an existing http service")
			return
		case err != nil:
			// Fail closed: a check that could not run is no permission.
			d.warn("tool source: service check failed", "service_id", in.ServiceID, "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		wasGatewayOnly = svc.GatewayOnly
	default:
		writeErr(w, http.StatusBadRequest, "kind must be 'remote' or 'tunnel'")
		return
	}
	src, err := st.CreateMCPSource(r.Context(), userID(r.Context()), store.MCPSourceInput{
		Slug: in.Slug, Name: in.Name, Kind: in.Kind, ServiceID: in.ServiceID,
		URL: in.URL, CredentialSlot: in.CredentialSlot, AuthHeader: in.AuthHeader, AuthFormat: in.AuthFormat,
		ProtocolVersion: in.ProtocolVersion, RPM: in.RPM, MaxConcurrent: in.MaxConcurrent, TimeoutS: in.TimeoutS,
		Enabled: in.Enabled,
	})
	if err != nil {
		if !mapMCPSourceErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditMCPSource(r, audit.ActionMCPSourceCreate, src, mcpSourceAudit(src))
	if src.Kind == "tunnel" && !wasGatewayOnly {
		// Binding closed the service's direct doors; say so on the service.
		d.auditGatewayOnly(r, src.ServiceID)
	}
	writeJSON(w, http.StatusCreated, d.mcpSourceView(src))
}

// PutMCPSource handles PUT /api/v1/mcp/sources/{slug} (mcp:sources:manage).
func (d Deps) PutMCPSource(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	slug := chi.URLParam(r, "slug")
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var in putMCPSourceReq
	if msg, unknown := decodeStrictJSON(r.Body, &in); msg != "" {
		if unknown {
			msg += credentialHint
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	cur, err := st.MCPSourceBySlug(r.Context(), slug)
	if err != nil {
		if !mapMCPSourceErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	// A tunnel source has no URL; the store refuses one with a 400 of its own.
	if in.URL != nil && cur.Kind == "remote" && !d.upstreamHostAllowedAs(w, r, *in.URL, "url") {
		return
	}
	src, repending, err := st.UpdateMCPSource(r.Context(), slug, store.MCPSourceUpdate{
		Name: in.Name, URL: in.URL, CredentialSlot: in.CredentialSlot, AuthHeader: in.AuthHeader, AuthFormat: in.AuthFormat,
		ProtocolVersion: in.ProtocolVersion, RPM: in.RPM, MaxConcurrent: in.MaxConcurrent, TimeoutS: in.TimeoutS,
		Enabled: in.Enabled,
	})
	if err != nil {
		if !mapMCPSourceErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if d.MCPGateway != nil {
		d.MCPGateway.Invalidate(slug)
	}
	// A new url or credential slot voided the tools' approvals (the store
	// moved them back to pending); the row says how many.
	payload := mcpSourceAudit(src)
	payload["tools_back_to_pending"] = repending
	d.auditMCPSource(r, audit.ActionMCPSourceUpdate, src, payload)
	if src.URL != cur.URL || src.CredentialSlot != cur.CredentialSlot {
		// Nothing can be approved until the new upstream's tools are read:
		// read them now, off the request.
		d.syncInBackground(r, slug)
	}
	writeJSON(w, http.StatusOK, d.mcpSourceView(src))
}

// backgroundSyncRetry is how long a background sync waits before it tries
// again while another sync of the source is running.
const backgroundSyncRetry = 250 * time.Millisecond

// syncInBackground syncs the source off the request path, with the pool's
// own sync, and audits the outcome as mcp.source.sync (background). A sync
// already running may have read the old upstream, so this one waits for it
// and runs after it, until mcpSyncTimeout has passed.
func (d Deps) syncInBackground(r *http.Request, slug string) {
	gw := d.MCPGateway
	if gw == nil {
		return
	}
	lc := audit.LogContextFrom(r.Context())
	job := func() {
		ctx, cancel := context.WithTimeout(audit.WithLogContext(context.Background(), lc), mcpSyncTimeout)
		defer cancel()
		var res mcpgw.SyncResult
		var err error
		for {
			res, err = gw.Sync(ctx, slug)
			if !errors.Is(err, mcpgw.ErrSyncRunning) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backgroundSyncRetry):
			}
		}
		if d.AuditAppender == nil {
			return
		}
		e := audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail, Action: audit.ActionMCPSourceSync,
			SubjectID: slug, SubjectLabel: slug, Result: "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		}
		switch {
		case errors.Is(err, mcpgw.ErrSourceNotFound):
			return
		case err != nil:
			msg := mcpgw.Describe(err) // never a URL, a header value or a credential
			d.warn("tool source background sync failed", "source", slug, "reason", msg)
			e.Result, e.Payload = "error", audit.MustJSON(map[string]any{"error": msg, "background": true})
		default:
			e.Payload = audit.MustJSON(map[string]any{
				"added": res.Added, "changed": res.Changed, "removed": res.Removed, "pending": res.Pending,
				"skipped": len(res.Skipped), "background": true,
			})
		}
		_ = d.AuditAppender.Append(ctx, e)
	}
	if d.runInBackground != nil {
		d.runInBackground(job)
		return
	}
	go job()
}

// DeleteMCPSource handles DELETE /api/v1/mcp/sources/{slug}
// (mcp:sources:manage). The source's tools go with it; a remote source's
// hidden backing service goes too, a tunnel source's service is only unbound.
// The OAuth grants issued for /mcp/<slug> are revoked.
func (d Deps) DeleteMCPSource(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	slug := chi.URLParam(r, "slug")
	cur, err := st.MCPSourceBySlug(r.Context(), slug)
	if err != nil {
		if !mapMCPSourceErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	// The OAuth grants issued for /mcp/<slug> end first: if that fails the
	// source stays, so no later source of the same slug inherits them.
	revoked := 0
	if d.OAuth != nil {
		if revoked, err = d.OAuth.RevokeResourceGrants(r.Context(), "/mcp/"+slug); err != nil {
			d.Log.Error("delete tool source: revoke its oauth grants", "source", slug, "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err := st.DeleteMCPSource(r.Context(), slug); err != nil {
		if !mapMCPSourceErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if d.MCPGateway != nil {
		d.MCPGateway.Invalidate(slug)
	}
	// The inspector ring and the like of a remote source's backing row go
	// with it, as for any deleted service.
	if cur.Kind == "remote" && d.OnServiceDeleted != nil {
		d.OnServiceDeleted(cur.ServiceID)
	}
	d.auditMCPSource(r, audit.ActionMCPSourceDelete, cur, map[string]any{"kind": cur.Kind, "service_id": cur.ServiceID, "oauth_grants_revoked": revoked})
	w.WriteHeader(http.StatusNoContent)
}
