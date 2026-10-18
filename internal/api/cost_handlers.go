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
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
)

// CostEngine is the narrow runtime surface the cost handlers consume from
// internal/cost. *cost.Engine satisfies it. Tests provide a stub.
type CostEngine interface {
	Pricing() cost.Pricing
	ReplacePricing(p cost.Pricing)
	Summary(ctx context.Context, window string) (cost.Summary, error)
	// SummaryBy groups the window's usage by gateway_key, model, provider,
	// target_model or dialect; cost.ErrBadDimension for anything else.
	SummaryBy(ctx context.Context, window, dimension string) ([]cost.GroupRow, error)
	// BudgetUsages returns today's spend, tokens and exceeded state of each
	// budget, from one read of today's usage.
	BudgetUsages(ctx context.Context, budgets []db.Budget) ([]cost.BudgetUsage, error)
	// RowUSD prices one usage row as the summary does: the reported cost
	// plus the price table, by the provider and model that answered.
	RowUSD(r db.UsageRow) float64
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

// --- Permissions --------------------------------------------------------------
//
// Spec Part F mapping (routeAccess in access.go):
//   - GET  /cost/pricing   — quotas:read:own / :any or cost:pricing:manage
//   - PUT  /cost/pricing   — cost:pricing:manage
//   - GET  /cost/summary   — quotas:read:any
//   - GET  /cost/export    — quotas:read:any
//   - GET  /budgets        — budgets:manage (the read of the same surface is
//     gated the same way to keep the contract simple)
//   - POST/PUT/DELETE /budgets — budgets:manage

// --- GET /cost/pricing -------------------------------------------------------

// pricingEntryResp mirrors the spec wire shape for one row of the pricing table.
type pricingEntryResp struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	InputPerMillion  float64 `json:"input_per_million"`
	OutputPerMillion float64 `json:"output_per_million"`
	PerRequestUSD    float64 `json:"per_request_usd"`
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
			PerRequestUSD:    e.PerRequestUSD,
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

// costPricingKey is the settings row that holds the price table saved with
// PUT /cost/pricing, as the canonical JSON of the request (version and
// entries). ApplyStoredPricing reads it back at start-up.
const costPricingKey = "cost.pricing"

// PutCostPricing handles PUT /api/v1/cost/pricing — replaces the pricing
// table. Needs cost:pricing:manage. The request body MUST match the GET
// response shape (version + entries). The table is stored in the settings
// (costPricingKey), so it outlives a restart, and the engine swaps it
// atomically; 204 on success. With no settings store the swap is in memory
// only.
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
	pr, err := pricingFromWire(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if d.Settings != nil {
		canon, err := json.Marshal(in)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "marshal failed")
			return
		}
		if err := d.Settings.SaveSettings(r.Context(), map[string]string{costPricingKey: string(canon)}); err != nil {
			writeErr(w, http.StatusInternalServerError, "save pricing failed")
			return
		}
	}
	d.CostEngine.ReplacePricing(pr)
	w.WriteHeader(http.StatusNoContent)
}

// pricingFromWire checks a price table in the wire shape and builds the
// engine's table: each entry under "provider/model" and, for the first entry
// of a model, under the bare model name. The error is written to the caller
// as it is.
func pricingFromWire(in pricingResp) (cost.Pricing, error) {
	if strings.TrimSpace(in.Version) == "" {
		return cost.Pricing{}, errors.New("version is required")
	}
	if len(in.Entries) == 0 {
		return cost.Pricing{}, errors.New("entries must be non-empty")
	}
	pr := cost.Pricing{
		Version: in.Version,
		Entries: make(map[string]cost.Entry, len(in.Entries)*2),
	}
	for i, e := range in.Entries {
		if e.Model == "" {
			return cost.Pricing{}, fmt.Errorf("entry %d: model is required", i)
		}
		if e.InputPerMillion < 0 || e.OutputPerMillion < 0 || e.PerRequestUSD < 0 {
			return cost.Pricing{}, fmt.Errorf("entry %s/%s: prices must be >= 0", e.Provider, e.Model)
		}
		entry := cost.Entry{
			InputPerMillion:  e.InputPerMillion,
			OutputPerMillion: e.OutputPerMillion,
			PerRequestUSD:    e.PerRequestUSD,
		}
		if e.Provider != "" {
			pr.Entries[e.Provider+"/"+e.Model] = entry
		}
		if _, ok := pr.Entries[e.Model]; !ok {
			pr.Entries[e.Model] = entry
		}
	}
	return pr, nil
}

// SettingsReader is the read side of the settings store.
type SettingsReader interface {
	GetSettings(ctx context.Context) (map[string]string, error)
}

// ApplyStoredPricing merges the price table saved with PUT /cost/pricing
// (see costPricingKey) over the table engine was built with (the bundled one
// or BURROW_PRICING_PATH): a saved entry wins for its key ("provider/model"
// and the bare model), an entry only the start-up table has stays. So a model
// a later release adds to pricing.yaml, or a later edit of the file, is not
// hidden by a table saved before it. The table takes the saved version. It
// returns how many saved entries were applied and the saved version; nothing
// stored leaves the engine as it is (0, ""), and a stored row that does not
// parse is an error and changes nothing. cmd/server calls it at start-up.
func ApplyStoredPricing(ctx context.Context, settings SettingsReader, engine CostEngine) (applied int, version string, err error) {
	if settings == nil || engine == nil {
		return 0, "", nil
	}
	m, err := settings.GetSettings(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("read stored pricing: %w", err)
	}
	raw := m[costPricingKey]
	if raw == "" {
		return 0, "", nil
	}
	var in pricingResp
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return 0, "", fmt.Errorf("stored pricing: %w", err)
	}
	stored, err := pricingFromWire(in)
	if err != nil {
		return 0, "", fmt.Errorf("stored pricing: %w", err)
	}
	startup := engine.Pricing()
	merged := cost.Pricing{Version: stored.Version, Entries: make(map[string]cost.Entry, len(startup.Entries)+len(stored.Entries))}
	for k, e := range startup.Entries {
		merged.Entries[k] = e
	}
	for k, e := range stored.Entries {
		merged.Entries[k] = e
	}
	engine.ReplacePricing(merged)
	return len(in.Entries), stored.Version, nil
}

// --- GET /cost/summary -------------------------------------------------------

var validCostWindows = map[string]bool{
	"today": true, "week": true, "month": true, "year": true,
}

// validCostGroupBy are the dimensions of GET /cost/summary?group_by=…. The
// value never reaches a query: the engine groups in memory by a fixed set.
var validCostGroupBy = map[string]bool{
	"gateway_key": true, "model": true, "provider": true, "target_model": true, "dialect": true,
}

const msgBadGroupBy = "group_by must be one of gateway_key|model|provider|target_model|dialect"

// groupedSummaryResp is the summary with the usage grouped by one dimension.
// A group's key is a gateway key's id, a model name, a provider slug,
// "<provider>/<model>" or a dialect: never a key's secret or its hash.
type groupedSummaryResp struct {
	cost.Summary
	GroupBy string          `json:"group_by"`
	Groups  []cost.GroupRow `json:"groups"`
}

// GetCostSummary handles GET /api/v1/cost/summary?window=today|week|month|year
// and, with group_by=gateway_key|model|provider|target_model|dialect, adds
// "group_by" and "groups" to the body. Both parameters are checked before
// anything is read.
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
	groupBy := r.URL.Query().Get("group_by")
	if groupBy != "" && !validCostGroupBy[groupBy] {
		writeErr(w, http.StatusBadRequest, msgBadGroupBy)
		return
	}
	s, err := d.CostEngine.Summary(r.Context(), window)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cost summary failed")
		return
	}
	if groupBy == "" {
		writeJSON(w, http.StatusOK, s)
		return
	}
	groups, err := d.CostEngine.SummaryBy(r.Context(), window, groupBy)
	if errors.Is(err, cost.ErrBadDimension) {
		writeErr(w, http.StatusBadRequest, msgBadGroupBy)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cost summary failed")
		return
	}
	if groups == nil {
		groups = []cost.GroupRow{}
	}
	writeJSON(w, http.StatusOK, groupedSummaryResp{Summary: s, GroupBy: groupBy, Groups: groups})
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
	// The per-row USD cost is what the summary computes for the row.
	usd := func(row db.UsageRow) float64 {
		if d.CostEngine == nil {
			return row.ReportedUSD
		}
		return d.CostEngine.RowUSD(row)
	}
	switch format {
	case "ndjson":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for _, r := range rows {
			_ = enc.Encode(map[string]any{
				"service_id":      r.ServiceID,
				"api_key_id":      r.APIKeyID,
				"kind":            r.Kind,
				"tokens_in":       r.TokensIn,
				"tokens_out":      r.TokensOut,
				"bytes_in":        r.BytesIn,
				"bytes_out":       r.BytesOut,
				"usd":             usd(r),
				"gateway_key_id":  r.GatewayKeyID,
				"dialect":         r.Dialect,
				"provider":        r.ProviderSlug,
				"requested_model": r.RequestedModel,
				"target_model":    r.TargetModel,
				"requests":        r.Requests,
			})
		}
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(http.StatusOK)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"service_id", "api_key_id", "kind",
			"tokens_in", "tokens_out", "bytes_in", "bytes_out",
			"gateway_key_id", "dialect", "provider", "requested_model", "target_model", "requests"})
		for _, r := range rows {
			_ = cw.Write([]string{r.ServiceID, r.APIKeyID, r.Kind,
				fmt.Sprintf("%d", r.TokensIn),
				fmt.Sprintf("%d", r.TokensOut),
				fmt.Sprintf("%d", r.BytesIn),
				fmt.Sprintf("%d", r.BytesOut),
				csvCell(r.GatewayKeyID), r.Dialect, csvCell(r.ProviderSlug),
				csvCell(r.RequestedModel), csvCell(r.TargetModel),
				fmt.Sprintf("%d", r.Requests),
			})
		}
		cw.Flush()
	}
}

// csvCell keeps a value a client chose (a model name) from being read as a
// formula by a spreadsheet: a leading =, +, - or @ is prefixed with a quote.
func csvCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// --- Budgets CRUD -----------------------------------------------------------

// budgetResp is the wire shape for one budget. current_usd, current_tokens
// and exceeded are computed live by the cost engine (not stored on the row).
type budgetResp struct {
	ID             string  `json:"id"`
	Scope          string  `json:"scope"`
	SubjectID      string  `json:"subject_id"`
	DailyUSD       float64 `json:"daily_usd"`
	DailyTokens    int64   `json:"daily_tokens"`
	ActionOnExceed string  `json:"action_on_exceed"`
	AlertWebhookID *string `json:"alert_webhook_id"`
	CurrentUSD     float64 `json:"current_usd"`
	CurrentTokens  int64   `json:"current_tokens"`
	Exceeded       bool    `json:"exceeded"`
}

func toBudgetResp(b db.Budget, u cost.BudgetUsage) budgetResp {
	return budgetResp{
		ID:             b.ID,
		Scope:          b.Scope,
		SubjectID:      b.SubjectID,
		DailyUSD:       b.DailyUSD,
		DailyTokens:    b.DailyTokens,
		ActionOnExceed: b.ActionOnExceed,
		AlertWebhookID: b.AlertWebhookID,
		CurrentUSD:     u.USD,
		CurrentTokens:  u.Tokens,
		Exceeded:       cost.BudgetExceeded(b, u.USD, u.Tokens),
	}
}

// budgetLabel names a budget in the audit log: its scope and subject.
func budgetLabel(b db.Budget) string {
	return strings.TrimSpace(b.Scope + " " + b.SubjectID)
}

// budgetLimits is what a budget caps and what it does about it, for audit
// details.
func budgetLimits(b db.Budget) map[string]any {
	return map[string]any{
		"daily_usd":        b.DailyUSD,
		"daily_tokens":     b.DailyTokens,
		"action_on_exceed": b.ActionOnExceed,
		"alert_webhook_id": b.AlertWebhookID,
	}
}

// budgetUsages returns today's usage of each budget; zero usage for all when
// there is no engine or today's usage cannot be read (logged).
func (d Deps) budgetUsages(ctx context.Context, budgets []db.Budget) []cost.BudgetUsage {
	if d.CostEngine != nil {
		usages, err := d.CostEngine.BudgetUsages(ctx, budgets)
		if err == nil && len(usages) == len(budgets) {
			return usages
		}
		d.warn("budget usage could not be read", "err", err)
	}
	return make([]cost.BudgetUsage, len(budgets))
}

// budgetReq is the wire shape for POST + PUT bodies.
type budgetReq struct {
	Scope          string  `json:"scope"`
	SubjectID      string  `json:"subject_id"`
	DailyUSD       float64 `json:"daily_usd"`
	DailyTokens    int64   `json:"daily_tokens"`
	ActionOnExceed string  `json:"action_on_exceed"`
	AlertWebhookID *string `json:"alert_webhook_id"`
}

// validBudgetScopes: a gateway_key budget's subject is the gateway key's id,
// a model budget's the model name clients ask for (a synthetic model or a
// direct address "<provider>/<model>").
var validBudgetScopes = map[string]bool{
	"api_key": true, "service": true, "user": true, "global": true,
	"gateway_key": true, "model": true,
}
var validBudgetActions = map[string]bool{
	"alert_webhook": true, "throttle_zero": true, "disable_key": true,
}

// validateBudget: a budget caps the day's spend (daily_usd), its tokens
// (daily_tokens), or both; at least one cap is needed. A flat-rate provider
// costs 0 USD, so a token cap alone is a complete budget.
func validateBudget(in budgetReq) string {
	if !validBudgetScopes[in.Scope] {
		return "scope must be one of api_key|service|user|global|gateway_key|model"
	}
	if !validBudgetActions[in.ActionOnExceed] {
		return "action_on_exceed must be one of alert_webhook|throttle_zero|disable_key"
	}
	if in.DailyTokens < 0 {
		return "daily_tokens must not be negative"
	}
	// NaN fails both comparisons below, so it is named here.
	if in.DailyUSD < 0 || in.DailyUSD != in.DailyUSD {
		return "daily_usd must not be negative"
	}
	if in.DailyUSD == 0 && in.DailyTokens == 0 {
		return "daily_usd or daily_tokens must be greater than zero"
	}
	if in.Scope != "global" && strings.TrimSpace(in.SubjectID) == "" {
		return "subject_id is required for non-global scopes"
	}
	if in.Scope == "global" && in.SubjectID != "" {
		return "global scope must not specify a subject_id"
	}
	// The subject is compared byte for byte with what the gateway records;
	// it is taken as sent or refused, never trimmed.
	if in.SubjectID != strings.TrimSpace(in.SubjectID) {
		return "subject_id must not have leading or trailing whitespace"
	}
	if strings.ContainsFunc(in.SubjectID, unicode.IsControl) {
		return "subject_id must not contain control characters"
	}
	if len(in.SubjectID) > 256 {
		return "subject_id too long (max 256 chars)"
	}
	return ""
}

// msgUnknownGatewayKey answers a gateway_key budget whose subject_id is no
// gateway key's id.
const msgUnknownGatewayKey = "unknown gateway key"

// checkBudgetSubject checks what validateBudget cannot: that the subject of a
// gateway_key budget is the id of a key that exists (revoked or not). It
// writes the error itself and reports whether the budget may be stored.
// Who may set budgets (budgets:manage) sets them for anyone's key.
func (d Deps) checkBudgetSubject(w http.ResponseWriter, r *http.Request, in budgetReq) bool {
	if in.Scope != "gateway_key" {
		return true
	}
	if d.AIGatewayKeys != nil {
		keys, err := d.AIGatewayKeys.ListGatewayKeys(r.Context(), userID(r.Context()), true)
		if err != nil {
			d.warn("gateway keys could not be read", "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return false
		}
		for _, k := range keys {
			if k.ID == in.SubjectID {
				return true
			}
		}
	}
	writeErr(w, http.StatusBadRequest, msgUnknownGatewayKey)
	return false
}

// GetBudgets handles GET /api/v1/budgets — budgets:manage. Live current_usd +
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
	usages := d.budgetUsages(r.Context(), rows)
	for i, b := range rows {
		out[i] = toBudgetResp(b, usages[i])
	}
	writeJSON(w, http.StatusOK, out)
}

// PostBudget handles POST /api/v1/budgets — budgets:manage.
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
	in.ActionOnExceed = strings.TrimSpace(in.ActionOnExceed)
	if msg := validateBudget(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !d.checkBudgetSubject(w, r, in) {
		return
	}
	row := db.Budget{
		ID:             uuid.NewString(),
		Scope:          in.Scope,
		SubjectID:      in.SubjectID,
		DailyUSD:       in.DailyUSD,
		DailyTokens:    in.DailyTokens,
		ActionOnExceed: in.ActionOnExceed,
		AlertWebhookID: in.AlertWebhookID,
	}
	if err := d.Budgets.CreateBudget(r.Context(), row); err != nil {
		writeErr(w, http.StatusInternalServerError, "create budget failed")
		return
	}
	payload := budgetLimits(row)
	payload["scope"], payload["subject_id"] = row.Scope, row.SubjectID
	d.auditAI(r, audit.ActionBudgetCreate, row.ID, budgetLabel(row), payload)
	// Read-back so created_at reflects the SQLite default.
	created, err := d.Budgets.GetBudget(r.Context(), row.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, toBudgetResp(row, cost.BudgetUsage{}))
		return
	}
	writeJSON(w, http.StatusCreated, toBudgetResp(created, d.budgetUsages(r.Context(), []db.Budget{created})[0]))
}

// PutBudget handles PUT /api/v1/budgets/{id} — budgets:manage.
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
	in.ActionOnExceed = strings.TrimSpace(in.ActionOnExceed)
	if msg := validateBudget(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !d.checkBudgetSubject(w, r, in) {
		return
	}
	row := db.Budget{
		ID:             id,
		Scope:          in.Scope,
		SubjectID:      in.SubjectID,
		DailyUSD:       in.DailyUSD,
		DailyTokens:    in.DailyTokens,
		ActionOnExceed: in.ActionOnExceed,
		AlertWebhookID: in.AlertWebhookID,
	}
	before, getErr := d.Budgets.GetBudget(r.Context(), id)
	if err := d.Budgets.UpdateBudget(r.Context(), row); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "budget not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "update budget failed")
		return
	}
	payload := budgetLimits(row)
	payload["scope"], payload["subject_id"] = row.Scope, row.SubjectID
	if getErr == nil {
		payload["previous"] = budgetLimits(before)
	}
	d.auditAI(r, audit.ActionBudgetUpdate, id, budgetLabel(row), payload)
	w.WriteHeader(http.StatusNoContent)
}

// DeleteBudget handles DELETE /api/v1/budgets/{id} — budgets:manage.
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
	before, getErr := d.Budgets.GetBudget(r.Context(), id)
	if err := d.Budgets.DeleteBudget(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "budget not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete budget failed")
		return
	}
	payload, label := map[string]any{}, ""
	if getErr == nil {
		payload = budgetLimits(before)
		payload["scope"], payload["subject_id"] = before.Scope, before.SubjectID
		label = budgetLabel(before)
	}
	d.auditAI(r, audit.ActionBudgetDelete, id, label, payload)
	w.WriteHeader(http.StatusNoContent)
}
