package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
)

// CostEngine is the narrow runtime surface the cost handlers consume from
// internal/cost. *cost.Engine satisfies it. Tests provide a stub.
type CostEngine interface {
	Pricing() cost.Pricing
	ReplacePricing(p cost.Pricing)
	Summary(ctx context.Context, window string) (cost.Summary, error)
	CurrentUsdFor(ctx context.Context, b db.Budget) (float64, error)
	// UsdFor prices (tokensIn, tokensOut) for a model/kind via the pricing
	// table; unknown keys return 0. Used to derive per-endpoint cost.
	UsdFor(model string, tokensIn, tokensOut int) float64
}

// BudgetStore is the narrow CRUD surface the budget handlers consume.
// *db.DB satisfies it; tests provide a fake.
type BudgetStore interface {
	ListBudgets(ctx context.Context) ([]db.Budget, error)
	GetBudget(ctx context.Context, id string) (db.Budget, error)
	CreateBudget(ctx context.Context, b db.Budget) error
	UpdateBudget(ctx context.Context, b db.Budget) error
	DeleteBudget(ctx context.Context, id string) error
	ListUsageForWindow(ctx context.Context, window string) ([]db.UsageRow, error)
}

// --- Permission gates --------------------------------------------------------
//
// Spec Part F mapping:
//   - GET  /cost/pricing   — session-authed (any user may read)
//   - PUT  /cost/pricing   — admin only
//   - GET  /cost/summary   — admin OR quotas:read:any
//   - GET  /cost/export    — admin OR quotas:read:any
//   - GET  /budgets        — admin only (matches the spec note that
//     quotas:manage:any is admin-only — read of the same surface is gated
//     the same way to keep the contract simple)
//   - POST/PUT/DELETE /budgets — admin only

// requireQuotasReadAnyOrAdmin is the read-gate for /cost/summary and
// /cost/export. Admin always passes; non-admin requires quotas:read:any.
func (d Deps) requireQuotasReadAnyOrAdmin(next http.Handler) http.Handler {
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
		if role == "admin" || authz.Can(role, authz.PermQuotasReadAny) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "quotas:read:any required")
	})
}

// --- GET /cost/pricing -------------------------------------------------------

// pricingEntryResp mirrors the spec wire shape for one row of the pricing table.
type pricingEntryResp struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	InputPerMillion  float64 `json:"input_per_million"`
	OutputPerMillion float64 `json:"output_per_million"`
}

// pricingResp is the response body for GET /cost/pricing.
type pricingResp struct {
	Version string             `json:"version"`
	Entries []pricingEntryResp `json:"entries"`
}

// pricingTableToResp converts the engine's by-key map into the wire-shape
// slice. We only emit the canonical "provider/model" entries (skip bare
// model fallbacks) so the response is unambiguous.
func pricingTableToResp(p cost.Pricing) pricingResp {
	out := pricingResp{Version: p.Version, Entries: []pricingEntryResp{}}
	seen := map[string]bool{}
	for k, e := range p.Entries {
		// Skip bare-model fallbacks (no slash in key).
		if !strings.Contains(k, "/") {
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		parts := strings.SplitN(k, "/", 2)
		out.Entries = append(out.Entries, pricingEntryResp{
			Provider:         parts[0],
			Model:            parts[1],
			InputPerMillion:  e.InputPerMillion,
			OutputPerMillion: e.OutputPerMillion,
		})
	}
	return out
}

// GetCostPricing handles GET /api/v1/cost/pricing.
func (d Deps) GetCostPricing(w http.ResponseWriter, r *http.Request) {
	if d.CostEngine == nil {
		writeJSON(w, http.StatusOK, pricingResp{Version: "", Entries: []pricingEntryResp{}})
		return
	}
	writeJSON(w, http.StatusOK, pricingTableToResp(d.CostEngine.Pricing()))
}

// PutCostPricing handles PUT /api/v1/cost/pricing — replaces the in-memory
// pricing table. Admin-only. The request body MUST match the GET response
// shape (version + entries). On success the engine swaps the table
// atomically and returns 204.
func (d Deps) PutCostPricing(w http.ResponseWriter, r *http.Request) {
	if d.CostEngine == nil {
		writeErr(w, http.StatusInternalServerError, "cost engine unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB cap
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var in pricingResp
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Version) == "" {
		writeErr(w, http.StatusBadRequest, "version is required")
		return
	}
	if len(in.Entries) == 0 {
		writeErr(w, http.StatusBadRequest, "entries must be non-empty")
		return
	}
	pr := cost.Pricing{
		Version: in.Version,
		Entries: make(map[string]cost.Entry, len(in.Entries)*2),
	}
	for i, e := range in.Entries {
		if e.Model == "" {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("entry %d: model is required", i))
			return
		}
		if e.InputPerMillion < 0 || e.OutputPerMillion < 0 {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("entry %s/%s: prices must be >= 0", e.Provider, e.Model))
			return
		}
		entry := cost.Entry{
			InputPerMillion:  e.InputPerMillion,
			OutputPerMillion: e.OutputPerMillion,
		}
		if e.Provider != "" {
			pr.Entries[e.Provider+"/"+e.Model] = entry
		}
		if _, ok := pr.Entries[e.Model]; !ok {
			pr.Entries[e.Model] = entry
		}
	}
	d.CostEngine.ReplacePricing(pr)
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /cost/summary -------------------------------------------------------

var validCostWindows = map[string]bool{
	"today": true, "week": true, "month": true, "year": true,
}

// GetCostSummary handles GET /api/v1/cost/summary?window=today|week|month|year.
func (d Deps) GetCostSummary(w http.ResponseWriter, r *http.Request) {
	if d.CostEngine == nil {
		writeJSON(w, http.StatusOK, cost.Summary{
			Window:       "today",
			TopConsumers: []cost.SummaryConsumer{},
		})
		return
	}
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "today"
	}
	if !validCostWindows[window] {
		writeErr(w, http.StatusBadRequest, "window must be one of today|week|month|year")
		return
	}
	s, err := d.CostEngine.Summary(r.Context(), window)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cost summary failed")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// --- GET /cost/export --------------------------------------------------------

// GetCostExport handles GET /api/v1/cost/export?format=ndjson|csv&window=...
// The endpoint streams a file download. Defaults: format=ndjson, window=today.
func (d Deps) GetCostExport(w http.ResponseWriter, r *http.Request) {
	if d.Budgets == nil {
		writeErr(w, http.StatusInternalServerError, "cost export unavailable")
		return
	}
	q := r.URL.Query()
	format := q.Get("format")
	if format == "" {
		format = "ndjson"
	}
	if format != "ndjson" && format != "csv" {
		writeErr(w, http.StatusBadRequest, "format must be ndjson or csv")
		return
	}
	window := q.Get("window")
	if window == "" {
		window = "today"
	}
	if !validCostWindows[window] {
		writeErr(w, http.StatusBadRequest, "window must be one of today|week|month|year")
		return
	}
	rows, err := d.Budgets.ListUsageForWindow(r.Context(), window)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cost export read failed")
		return
	}
	filename := fmt.Sprintf("burrow-cost-%s.%s", window, format)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// Resolve the per-row USD cost using the live pricing table when
	// available — operators want the export to mirror the dashboard math.
	priceFor := func(kind string, tIn, tOut int64) float64 {
		if d.CostEngine == nil {
			return 0
		}
		pr := d.CostEngine.Pricing()
		e, ok := pr.Lookup(kind)
		if !ok {
			return 0
		}
		return float64(tIn)*e.InputPerMillion/1_000_000 +
			float64(tOut)*e.OutputPerMillion/1_000_000
	}
	switch format {
	case "ndjson":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for _, r := range rows {
			_ = enc.Encode(map[string]any{
				"service_id": r.ServiceID,
				"api_key_id": r.APIKeyID,
				"kind":       r.Kind,
				"tokens_in":  r.TokensIn,
				"tokens_out": r.TokensOut,
				"bytes_in":   r.BytesIn,
				"bytes_out":  r.BytesOut,
				"usd":        r.ReportedUSD + priceFor(r.Kind, r.PricedTokensIn, r.PricedTokensOut),
			})
		}
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(http.StatusOK)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"service_id", "api_key_id", "kind",
			"tokens_in", "tokens_out", "bytes_in", "bytes_out"})
		for _, r := range rows {
			_ = cw.Write([]string{r.ServiceID, r.APIKeyID, r.Kind,
				fmt.Sprintf("%d", r.TokensIn),
				fmt.Sprintf("%d", r.TokensOut),
				fmt.Sprintf("%d", r.BytesIn),
				fmt.Sprintf("%d", r.BytesOut),
			})
		}
		cw.Flush()
	}
}

// --- Budgets CRUD -----------------------------------------------------------

// budgetResp is the wire shape for one budget. current_usd + exceeded are
// computed live by the cost engine (not stored on the row).
type budgetResp struct {
	ID             string  `json:"id"`
	Scope          string  `json:"scope"`
	SubjectID      string  `json:"subject_id"`
	DailyUSD       float64 `json:"daily_usd"`
	ActionOnExceed string  `json:"action_on_exceed"`
	AlertWebhookID *string `json:"alert_webhook_id"`
	CurrentUSD     float64 `json:"current_usd"`
	Exceeded       bool    `json:"exceeded"`
}

func toBudgetResp(b db.Budget, currentUSD float64) budgetResp {
	return budgetResp{
		ID:             b.ID,
		Scope:          b.Scope,
		SubjectID:      b.SubjectID,
		DailyUSD:       b.DailyUSD,
		ActionOnExceed: b.ActionOnExceed,
		AlertWebhookID: b.AlertWebhookID,
		CurrentUSD:     currentUSD,
		Exceeded:       currentUSD > b.DailyUSD,
	}
}

// budgetReq is the wire shape for POST + PUT bodies.
type budgetReq struct {
	Scope          string  `json:"scope"`
	SubjectID      string  `json:"subject_id"`
	DailyUSD       float64 `json:"daily_usd"`
	ActionOnExceed string  `json:"action_on_exceed"`
	AlertWebhookID *string `json:"alert_webhook_id"`
}

var validBudgetScopes = map[string]bool{
	"api_key": true, "service": true, "user": true, "global": true,
}
var validBudgetActions = map[string]bool{
	"alert_webhook": true, "throttle_zero": true, "disable_key": true,
}

func validateBudget(in budgetReq) string {
	if !validBudgetScopes[in.Scope] {
		return "scope must be one of api_key|service|user|global"
	}
	if !validBudgetActions[in.ActionOnExceed] {
		return "action_on_exceed must be one of alert_webhook|throttle_zero|disable_key"
	}
	if in.DailyUSD <= 0 {
		return "daily_usd must be greater than zero"
	}
	if in.Scope != "global" && strings.TrimSpace(in.SubjectID) == "" {
		return "subject_id is required for non-global scopes"
	}
	if in.Scope == "global" && in.SubjectID != "" {
		return "global scope must not specify a subject_id"
	}
	return ""
}

// GetBudgets handles GET /api/v1/budgets — admin-only. Live current_usd +
// exceeded are computed for every row.
func (d Deps) GetBudgets(w http.ResponseWriter, r *http.Request) {
	if d.Budgets == nil {
		writeJSON(w, http.StatusOK, []budgetResp{})
		return
	}
	rows, err := d.Budgets.ListBudgets(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list budgets failed")
		return
	}
	out := make([]budgetResp, len(rows))
	for i, b := range rows {
		var cur float64
		if d.CostEngine != nil {
			cur, _ = d.CostEngine.CurrentUsdFor(r.Context(), b)
		}
		out[i] = toBudgetResp(b, cur)
	}
	writeJSON(w, http.StatusOK, out)
}

// PostBudget handles POST /api/v1/budgets — admin-only.
func (d Deps) PostBudget(w http.ResponseWriter, r *http.Request) {
	if d.Budgets == nil {
		writeErr(w, http.StatusInternalServerError, "budget store unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var in budgetReq
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Scope = strings.TrimSpace(in.Scope)
	in.SubjectID = strings.TrimSpace(in.SubjectID)
	in.ActionOnExceed = strings.TrimSpace(in.ActionOnExceed)
	if msg := validateBudget(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	row := db.Budget{
		ID:             uuid.NewString(),
		Scope:          in.Scope,
		SubjectID:      in.SubjectID,
		DailyUSD:       in.DailyUSD,
		ActionOnExceed: in.ActionOnExceed,
		AlertWebhookID: in.AlertWebhookID,
	}
	if err := d.Budgets.CreateBudget(r.Context(), row); err != nil {
		writeErr(w, http.StatusInternalServerError, "create budget failed")
		return
	}
	// Read-back so created_at reflects the SQLite default.
	created, err := d.Budgets.GetBudget(r.Context(), row.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, toBudgetResp(row, 0))
		return
	}
	var cur float64
	if d.CostEngine != nil {
		cur, _ = d.CostEngine.CurrentUsdFor(r.Context(), created)
	}
	writeJSON(w, http.StatusCreated, toBudgetResp(created, cur))
}

// PutBudget handles PUT /api/v1/budgets/{id} — admin-only.
func (d Deps) PutBudget(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if d.Budgets == nil {
		writeErr(w, http.StatusInternalServerError, "budget store unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var in budgetReq
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Scope = strings.TrimSpace(in.Scope)
	in.SubjectID = strings.TrimSpace(in.SubjectID)
	in.ActionOnExceed = strings.TrimSpace(in.ActionOnExceed)
	if msg := validateBudget(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	row := db.Budget{
		ID:             id,
		Scope:          in.Scope,
		SubjectID:      in.SubjectID,
		DailyUSD:       in.DailyUSD,
		ActionOnExceed: in.ActionOnExceed,
		AlertWebhookID: in.AlertWebhookID,
	}
	if err := d.Budgets.UpdateBudget(r.Context(), row); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "budget not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "update budget failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteBudget handles DELETE /api/v1/budgets/{id} — admin-only.
func (d Deps) DeleteBudget(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if d.Budgets == nil {
		writeErr(w, http.StatusInternalServerError, "budget store unavailable")
		return
	}
	if err := d.Budgets.DeleteBudget(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "budget not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete budget failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
