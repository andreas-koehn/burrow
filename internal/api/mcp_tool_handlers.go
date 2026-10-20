package api

// mcp_tool_handlers.go — the tools of a tool source: sync, list, approve and
// overrides (D9, D10, D11).
//
// A sync reads the upstream's tool list (internal/mcpgw). A tool seen for the
// first time, or whose definition changed, is pending: it is not served until
// an admin approves it, and the response shows the approved definition next
// to the current one. Approving names the hash the admin saw, so a definition
// that changed in between is refused instead of approved unseen.
//
// Reads need mcp:sources:read or :manage; sync, approve and overrides need
// mcp:sources:manage and, while no services domain is set, a recent password
// confirmation. Approving and overrides are not offered as management MCP
// tools (x-burrow-mcp: false): an agent must not be able to approve tool text
// that may have been written to steer it, or rename and reclassify tools.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mcpgw"
	"github.com/ankoehn/burrow/internal/store"
)

// mcpSourceToolResp is the JSON wire shape of one tool of a source.
type mcpSourceToolResp struct {
	// Name is the upstream's own name; the path parameter of the tool routes.
	Name string `json:"name"`
	// Status is "approved", "pending" or "removed". Only an approved tool
	// that is not hidden is served.
	Status string `json:"status"`
	// CurrentHash and Definition are the upstream's definition at the last
	// sync; ApprovedHash and ApprovedDefinition (null before the first
	// approval) are what was approved. A definition is the tool's name,
	// title, description, inputSchema, outputSchema and annotations.
	CurrentHash        string          `json:"current_hash"`
	ApprovedHash       string          `json:"approved_hash"`
	Definition         json.RawMessage `json:"definition"`
	ApprovedDefinition json.RawMessage `json:"approved_definition"`
	// Overrides. read_only null means the upstream's readOnlyHint applies.
	Hidden              bool   `json:"hidden"`
	Rename              string `json:"rename"`
	DescriptionOverride string `json:"description_override"`
	ReadOnly            *bool  `json:"read_only"`
	// UpstreamReadOnly is the readOnlyHint of the current definition;
	// EffectiveReadOnly is the classification a read-only key sees.
	UpstreamReadOnly  bool `json:"upstream_read_only"`
	EffectiveReadOnly bool `json:"effective_read_only"`
	// ServedName is the name on /mcp/<source>: the rename, or the name.
	ServedName string `json:"served_name"`
	// Served tells whether the gateway serves the tool now.
	Served bool `json:"served"`
	// ServeIssue says why an approved, visible tool is not served:
	// "name_conflict" when another tool of the source would be served under
	// the same name (neither is served). "" otherwise.
	ServeIssue string `json:"serve_issue"`
	// AggregatedName is the name on the aggregated endpoint /mcp:
	// "<slug>__<served name>". null when the tool is not offered there: it is
	// not served at all, or AggregatedIssue says why its name is unusable.
	AggregatedName *string `json:"aggregated_name"`
	// AggregatedIssue is why a served-or-approved tool is left off /mcp:
	// "name_too_long" (over 64 characters), "name_invalid" (characters outside
	// [A-Za-z0-9_-]) or "name_conflict" (see ServeIssue). "" otherwise. A
	// rename fixes the first two.
	AggregatedIssue string `json:"aggregated_issue"`
	// GuardrailHits are the guardrail request patterns that matched the
	// current definition's title, description or schemas.
	GuardrailHits []mcpgw.GuardrailHit `json:"guardrail_hits"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

// mcpToolView is the wire shape of t; conflicts are the source's name
// conflicts (mcpgw.ServedSet).
func mcpToolView(t db.MCPTool, conflicts map[string]string) mcpSourceToolResp {
	v := mcpSourceToolResp{
		Name: t.Name, Status: t.Status, CurrentHash: t.CurrentHash, ApprovedHash: t.ApprovedHash,
		Definition: rawOrNull(t.Definition), ApprovedDefinition: rawOrNull(t.ApprovedDefinition),
		Hidden: t.Hidden, Rename: t.Rename, DescriptionOverride: t.DescriptionOverride, ReadOnly: t.ReadOnly,
		ServedName: t.Name, GuardrailHits: mcpgw.ParseGuardrailHits(t.GuardrailHits), UpdatedAt: t.UpdatedAt,
	}
	if t.Rename != "" {
		v.ServedName = t.Rename
	}
	if d, err := mcpgw.ParseDefinition(t.Definition); err == nil {
		v.UpstreamReadOnly = d.ReadOnlyHint()
	}
	v.EffectiveReadOnly = v.UpstreamReadOnly
	if t.ReadOnly != nil {
		v.EffectiveReadOnly = *t.ReadOnly
	}
	_, v.Served = mcpgw.Served(t)
	if _, clash := conflicts[t.Name]; clash && v.Served {
		v.Served, v.ServeIssue = false, mcpgw.IssueNameConflict
	}
	switch {
	case v.Served:
		if name, issue := mcpgw.AggregatedName(t.SourceSlug, v.ServedName); issue == "" {
			v.AggregatedName = &name
		} else {
			v.AggregatedIssue = issue
		}
	case v.ServeIssue != "":
		v.AggregatedIssue = v.ServeIssue
	}
	return v
}

// mcpToolViewOf is mcpToolView for one tool, with the name conflicts of its
// source read now (none when the list cannot be read).
func mcpToolViewOf(ctx context.Context, st MCPSourceStore, t db.MCPTool) mcpSourceToolResp {
	var conflicts map[string]string
	if rows, err := st.ListMCPTools(ctx, t.SourceSlug); err == nil {
		_, conflicts = mcpgw.ServedSet(rows)
	}
	return mcpToolView(t, conflicts)
}

// rawOrNull returns stored JSON as is, and null for "" or text that is not
// valid JSON.
func rawOrNull(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}

// putMCPToolReq is the body of PUT /api/v1/mcp/sources/{slug}/tools/{name}.
// A field left out keeps its value; "" clears rename and
// description_override; read_only null goes back to the upstream's hint.
type putMCPToolReq struct {
	Hidden              *bool           `json:"hidden"`
	Rename              *string         `json:"rename"`
	DescriptionOverride *string         `json:"description_override"`
	ReadOnly            json.RawMessage `json:"read_only"`
}

// approveMCPToolReq is the body of POST …/tools/{name}/approve: hash is the
// current_hash the approver reviewed (required).
type approveMCPToolReq struct {
	Hash string `json:"hash"`
}

// approveAllMCPToolsReq is the body of POST …/approve-all: the tools to
// approve, each with the current_hash that was reviewed (required).
type approveAllMCPToolsReq struct {
	Hashes map[string]string `json:"hashes"`
}

// mcpApproveAllResp is the answer of POST …/approve-all.
type mcpApproveAllResp struct {
	Approved []string `json:"approved"`
	// Conflicts are the tools left pending because another approved tool
	// of the source is served under their name.
	Conflicts []string `json:"conflicts"`
}

// mcpSyncTimeout bounds a sync started from the API.
const mcpSyncTimeout = 90 * time.Second

func mapMCPToolErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrMCPToolNotFound):
		writeErr(w, http.StatusNotFound, "tool not found")
	case errors.Is(err, store.ErrMCPToolNotPending):
		writeErr(w, http.StatusConflict, "the tool has no definition waiting for approval")
	case errors.Is(err, store.ErrMCPToolChanged):
		writeErr(w, http.StatusConflict, "the tool's definition changed since it was read; review it again")
	case errors.Is(err, store.ErrMCPToolNameTaken):
		writeErr(w, http.StatusConflict, strings.TrimPrefix(err.Error(), store.ErrMCPToolNameTaken.Error()+": "))
	case errors.Is(err, store.ErrInvalidMCPTool):
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), store.ErrInvalidMCPTool.Error()+": "))
	default:
		return mapMCPSourceErr(w, err)
	}
	return true
}

// decodeOptionalJSON decodes a body that may be empty, strictly. It writes the
// 400 itself.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		writeErr(w, http.StatusBadRequest, "request body too large")
		return false
	}
	if len(bytes.TrimSpace(buf.Bytes())) == 0 {
		return true
	}
	if msg, _ := decodeStrictJSON(&buf, v); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return false
	}
	return true
}

// auditMCPTool appends a tool audit event; the subject is the source.
func (d Deps) auditMCPTool(r *http.Request, action, slug, result string, payload map[string]any) {
	if d.AuditAppender == nil {
		return
	}
	lc := audit.LogContextFrom(r.Context())
	_ = d.AuditAppender.Append(r.Context(), audit.Event{
		ActorID: lc.ActorID, ActorEmail: lc.ActorEmail,
		Action:    action,
		SubjectID: slug, SubjectLabel: slug,
		Result:   result,
		SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
		Payload: audit.MustJSON(payload),
	})
}

// PostMCPSourceSync handles POST /api/v1/mcp/sources/{slug}/sync
// (mcp:sources:manage): reads the upstream's tools now.
func (d Deps) PostMCPSourceSync(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	if d.MCPGateway == nil {
		writeErr(w, http.StatusServiceUnavailable, "the tool gateway is not available on this relay")
		return
	}
	slug := chi.URLParam(r, "slug")
	if _, err := st.MCPSourceBySlug(r.Context(), slug); err != nil {
		if !mapMCPSourceErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mcpSyncTimeout)
	defer cancel()
	res, err := d.MCPGateway.Sync(ctx, slug)
	switch {
	case errors.Is(err, mcpgw.ErrSourceNotFound):
		writeErr(w, http.StatusNotFound, "tool source not found")
		return
	case errors.Is(err, mcpgw.ErrSyncRunning):
		writeErr(w, http.StatusConflict, mcpgw.ErrSyncRunning.Error())
		return
	case err != nil:
		// Describe never carries a URL, a header value or a credential.
		msg := mcpgw.Describe(err)
		d.warn("tool source sync failed", "source", slug, "reason", msg)
		d.auditMCPTool(r, audit.ActionMCPSourceSync, slug, "error", map[string]any{"error": msg})
		writeErr(w, http.StatusBadGateway, upstreamTextFor(r, msg))
		return
	}
	d.auditMCPTool(r, audit.ActionMCPSourceSync, slug, "ok", map[string]any{
		"added": res.Added, "changed": res.Changed, "removed": res.Removed, "pending": res.Pending, "skipped": len(res.Skipped),
	})
	if bearerTokenID(r.Context()) != "" {
		// The names of skipped tools are the upstream's words: a token
		// learns how many there were, not what they say.
		withheld := make([]string, len(res.Skipped))
		for i := range withheld {
			withheld[i] = "(name withheld)"
		}
		res.Skipped = withheld
	}
	writeJSON(w, http.StatusOK, res)
}

// GetMCPSourceTools handles GET /api/v1/mcp/sources/{slug}/tools: every
// tool of the source, removed ones included, ordered by name.
func (d Deps) GetMCPSourceTools(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	tools, err := st.ListMCPTools(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		if !mapMCPToolErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	_, conflicts := mcpgw.ServedSet(tools)
	out := make([]mcpSourceToolResp, 0, len(tools))
	for _, t := range tools {
		out = append(out, mcpToolView(t, conflicts))
	}
	writeJSON(w, http.StatusOK, out)
}

// PutMCPSourceTool handles PUT /api/v1/mcp/sources/{slug}/tools/{name}
// (mcp:sources:manage): the tool's overrides.
func (d Deps) PutMCPSourceTool(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	slug, name := chi.URLParam(r, "slug"), chi.URLParam(r, "name")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in putMCPToolReq
	if msg, _ := decodeStrictJSON(r.Body, &in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	u := store.MCPToolOverridesUpdate{Hidden: in.Hidden, Rename: in.Rename, DescriptionOverride: in.DescriptionOverride}
	if in.ReadOnly != nil {
		u.ReadOnlySet = true
		if string(bytes.TrimSpace(in.ReadOnly)) != "null" {
			var b bool
			if err := json.Unmarshal(in.ReadOnly, &b); err != nil {
				writeErr(w, http.StatusBadRequest, "read_only must be true, false or null")
				return
			}
			u.ReadOnly = &b
		}
	}
	t, err := st.UpdateMCPToolOverrides(r.Context(), slug, name, u)
	if err != nil {
		if !mapMCPToolErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditMCPTool(r, audit.ActionMCPToolOverride, slug, "ok", map[string]any{
		"tool": name, "hidden": t.Hidden, "rename": t.Rename, "description_override": t.DescriptionOverride != "", "read_only": t.ReadOnly,
	})
	out := putMCPToolResp{mcpSourceToolResp: mcpToolViewOf(r.Context(), st, t)}
	if in.Rename != nil {
		keys, err := d.keysNaming(r.Context(), slug, out.ServedName)
		if err != nil {
			d.Log.Warn("tool rename: keys not read", "source", slug, "err", err)
			keys = []mcpKeyRef{}
		}
		// The keys are for the caller who may read every user's keys
		// (ai:read:any). mcp:sources:manage alone learns nothing about them,
		// not even how many there are.
		if d.mayReadAllKeys(r) {
			out.KeysMatchingRename = &keys
			grants, err := d.grantsNaming(r.Context(), slug, out.ServedName)
			if err != nil {
				d.Log.Warn("tool rename: grants not read", "source", slug, "err", err)
				grants = []mcpGrantRef{}
			}
			out.GrantsMatchingRename = &grants
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// putMCPToolResp is the answer of PUT …/tools/{name}: the tool, and with a
// rename in the body the keys the new name may hand the tool to.
type putMCPToolResp struct {
	mcpSourceToolResp
	// KeysMatchingRename lists the active gateway keys, of every user, whose
	// allow-list names "<slug>__<new served name>" exactly: the rename gives
	// them the tool if they did not have it. Keys with "<slug>__*" had it
	// before and are not listed. Only present when the body changed the
	// rename, and only for a caller who also holds ai:read:any: the list
	// shows other users' keys.
	KeysMatchingRename *[]mcpKeyRef `json:"keys_matching_rename,omitempty"`
	// GrantsMatchingRename lists the live OAuth grants, of every user, whose
	// allow-list names the new served name exactly, on the same terms.
	GrantsMatchingRename *[]mcpGrantRef `json:"grants_matching_rename,omitempty"`
}

// mcpGrantRef names an OAuth grant.
type mcpGrantRef struct {
	ID         string `json:"id"`
	ClientName string `json:"client_name"`
	UserID     string `json:"user_id"`
}

// mcpKeyRef names a gateway key without its secret.
type mcpKeyRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	UserID    string `json:"user_id"`
}

// mayReadAllKeys reports whether the caller holds ai:read:any, the right to
// see every user's gateway keys (a token needs to declare it and its user's
// role to still grant it, see effectivePerms). Unlike keysReach it does not
// count ai:configure:any: this answer is about reading.
func (d Deps) mayReadAllKeys(r *http.Request) bool {
	role, err := d.callerRoleForAuth(r)
	if err != nil {
		return false
	}
	return effectivePerms(r.Context(), role, authz.PermAIReadAny)
}

// keysNaming returns the active keys of every user whose allow-list holds
// "<slug>__<served>".
func (d Deps) keysNaming(ctx context.Context, slug, served string) ([]mcpKeyRef, error) {
	out := []mcpKeyRef{}
	if d.AIGatewayKeys == nil {
		return out, nil
	}
	keys, err := d.AIGatewayKeys.ListGatewayKeys(ctx, "", true)
	if err != nil {
		return nil, err
	}
	entry := slug + "__" + served
	for _, k := range keys {
		if k.RevokedAt != nil {
			continue
		}
		for _, e := range k.AllowedTools {
			if e == entry {
				out = append(out, mcpKeyRef{ID: k.ID, Name: k.Name, KeyPrefix: k.KeyPrefix, UserID: k.UserID})
				break
			}
		}
	}
	return out, nil
}

// grantsNaming returns the live OAuth grants of every user whose allow-list
// holds "<slug>__<served>".
func (d Deps) grantsNaming(ctx context.Context, slug, served string) ([]mcpGrantRef, error) {
	out := []mcpGrantRef{}
	if d.OAuth == nil {
		return out, nil
	}
	grants, err := d.OAuth.ListGrants(ctx, "")
	if err != nil {
		return nil, err
	}
	entry := slug + "__" + served
	for _, g := range grants {
		if slices.Contains(g.AllowedTools, entry) {
			out = append(out, mcpGrantRef{ID: g.ID, ClientName: g.ClientName, UserID: g.UserID})
		}
	}
	return out, nil
}

// PostMCPSourceToolApprove handles POST
// /api/v1/mcp/sources/{slug}/tools/{name}/approve (mcp:sources:manage).
func (d Deps) PostMCPSourceToolApprove(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	slug, name := chi.URLParam(r, "slug"), chi.URLParam(r, "name")
	var in approveMCPToolReq
	if !decodeOptionalJSON(w, r, &in) {
		return
	}
	if in.Hash == "" {
		writeErr(w, http.StatusBadRequest, "hash is required: the current_hash of the definition that was reviewed")
		return
	}
	t, err := st.ApproveMCPTool(r.Context(), slug, name, in.Hash)
	if err != nil {
		if !mapMCPToolErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.auditMCPTool(r, audit.ActionMCPToolApprove, slug, "ok", map[string]any{"tools": []string{name}, "hash": t.ApprovedHash})
	writeJSON(w, http.StatusOK, mcpToolViewOf(r.Context(), st, t))
}

// PostMCPSourceApproveAll handles POST /api/v1/mcp/sources/{slug}/approve-all
// (mcp:sources:manage).
func (d Deps) PostMCPSourceApproveAll(w http.ResponseWriter, r *http.Request) {
	st, ok := d.mcpSources(w)
	if !ok {
		return
	}
	slug := chi.URLParam(r, "slug")
	var in approveAllMCPToolsReq
	if !decodeOptionalJSON(w, r, &in) {
		return
	}
	if in.Hashes == nil {
		writeErr(w, http.StatusBadRequest, "hashes is required: tool name → the current_hash that was reviewed")
		return
	}
	names, conflicts, err := st.ApproveAllMCPTools(r.Context(), slug, in.Hashes)
	if err != nil {
		if !mapMCPToolErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if len(names) > 0 {
		d.auditMCPTool(r, audit.ActionMCPToolApprove, slug, "ok", map[string]any{"tools": names, "all": true})
	}
	writeJSON(w, http.StatusOK, mcpApproveAllResp{Approved: names, Conflicts: conflicts})
}
