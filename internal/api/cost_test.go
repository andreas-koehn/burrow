package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/db"
)

// fakeBudgetStore is an in-memory BudgetStore for the handler tests.
type fakeBudgetStore struct {
	mu    sync.Mutex
	rows  map[string]db.Budget
	usage []db.UsageRow
}

func newFakeBudgetStore() *fakeBudgetStore {
	return &fakeBudgetStore{rows: map[string]db.Budget{}}
}

func (f *fakeBudgetStore) ListBudgets(_ context.Context) ([]db.Budget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.Budget, 0, len(f.rows))
	for _, b := range f.rows {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].SubjectID < out[j].SubjectID
	})
	return out, nil
}

func (f *fakeBudgetStore) GetBudget(_ context.Context, id string) (db.Budget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.rows[id]
	if !ok {
		return db.Budget{}, db.ErrNotFound
	}
	return b, nil
}

func (f *fakeBudgetStore) CreateBudget(_ context.Context, b db.Budget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[b.ID] = b
	return nil
}

func (f *fakeBudgetStore) UpdateBudget(_ context.Context, b db.Budget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[b.ID]; !ok {
		return db.ErrNotFound
	}
	f.rows[b.ID] = b
	return nil
}

func (f *fakeBudgetStore) DeleteBudget(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[id]; !ok {
		return db.ErrNotFound
	}
	delete(f.rows, id)
	return nil
}

func (f *fakeBudgetStore) ListUsageForWindow(_ context.Context, _ string) ([]db.UsageRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.UsageRow, len(f.usage))
	copy(out, f.usage)
	return out, nil
}

// fakeCostEngine implements the CostEngine interface for handler tests.
type fakeCostEngine struct {
	mu      sync.Mutex
	pricing cost.Pricing
	summary cost.Summary

	groups      []cost.GroupRow
	groupCalls  []string
	budgetUsage map[string]cost.BudgetUsage // by budget subject_id
	usageErr    error
}

func (f *fakeCostEngine) Pricing() cost.Pricing {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pricing
}

func (f *fakeCostEngine) ReplacePricing(p cost.Pricing) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pricing = p
}

func (f *fakeCostEngine) Summary(_ context.Context, window string) (cost.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.summary
	s.Window = window
	return s, nil
}

func (f *fakeCostEngine) BudgetUsages(_ context.Context, budgets []db.Budget) ([]cost.BudgetUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	out := make([]cost.BudgetUsage, len(budgets))
	for i, b := range budgets {
		u := f.budgetUsage[b.SubjectID]
		u.Exceeded = cost.BudgetExceeded(b, u.USD, u.Tokens)
		out[i] = u
	}
	return out, nil
}

func (f *fakeCostEngine) SummaryBy(_ context.Context, window, dimension string) ([]cost.GroupRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupCalls = append(f.groupCalls, window+"/"+dimension)
	switch dimension {
	case "gateway_key", "model", "provider", "target_model", "dialect":
		return append([]cost.GroupRow{}, f.groups...), nil
	}
	return nil, cost.ErrBadDimension
}

func (f *fakeCostEngine) RowUSD(r db.UsageRow) float64 {
	key := r.Kind
	if _, ok := f.Pricing().Lookup(r.ProviderSlug + "/" + r.TargetModel); ok && r.TargetModel != "" {
		key = r.ProviderSlug + "/" + r.TargetModel
	}
	return r.ReportedUSD + f.UsdFor(key, int(r.PricedTokensIn), int(r.PricedTokensOut))
}

func (f *fakeCostEngine) UsdFor(model string, in, out int) float64 {
	e, ok := f.Pricing().Lookup(model)
	if !ok {
		return 0
	}
	return float64(in)*e.InputPerMillion/1_000_000 + float64(out)*e.OutputPerMillion/1_000_000
}

// --- /cost/pricing -----------------------------------------------------------

func TestCostHandler_GetPricing_Empty(t *testing.T) {
	d := Deps{
		Log:   discardLog(),
		Users: &fakeUserStore{role: "admin"},
		// Intentionally CostEngine == nil — should return empty table.
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/pricing")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got pricingResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if got.Entries == nil {
		t.Fatal("entries should be non-nil empty array")
	}
}

func TestCostHandler_GetPricing_NonAdmin(t *testing.T) {
	// Non-admin (role=user) can read the pricing table — spec says
	// session-authed.
	pr, _ := cost.LoadEmbedded()
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "user"},
		CostEngine: &fakeCostEngine{pricing: pr},
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/pricing")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got pricingResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if got.Version == "" {
		t.Error("version should be populated")
	}
	if len(got.Entries) == 0 {
		t.Error("entries should be populated from the bundled table")
	}
}

func TestCostHandler_PutPricing_AdminOK(t *testing.T) {
	eng := &fakeCostEngine{}
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "admin"},
		CostEngine: eng,
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	body := map[string]any{
		"version": "2026-05-19-test",
		"entries": []map[string]any{
			{"provider": "openai", "model": "gpt-x",
				"input_per_million": 1.0, "output_per_million": 2.0},
		},
	}
	r := c.put(t, "/api/v1/cost/pricing", body)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	if eng.Pricing().Version != "2026-05-19-test" {
		t.Errorf("engine version not updated: got %q", eng.Pricing().Version)
	}
	e, ok := eng.Pricing().Lookup("openai/gpt-x")
	if !ok {
		t.Fatal("new entry not present after PUT")
	}
	if e.InputPerMillion != 1.0 || e.OutputPerMillion != 2.0 {
		t.Errorf("entry round-trip wrong: %+v", e)
	}
}

func TestCostHandler_PutPricing_NonAdmin403(t *testing.T) {
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "user"},
		CostEngine: &fakeCostEngine{},
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.put(t, "/api/v1/cost/pricing", map[string]any{
		"version": "v1",
		"entries": []map[string]any{{"provider": "x", "model": "y"}},
	})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", r.StatusCode, readBody(t, r))
	}
}

func TestCostHandler_PutPricing_BadInputs(t *testing.T) {
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "admin"},
		CostEngine: &fakeCostEngine{},
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing version", map[string]any{
			"entries": []map[string]any{{"provider": "x", "model": "y"}}}},
		{"empty entries", map[string]any{"version": "v1", "entries": []any{}}},
		{"negative price", map[string]any{
			"version": "v1",
			"entries": []map[string]any{
				{"provider": "x", "model": "y", "input_per_million": -1.0},
			}}},
		{"missing model", map[string]any{
			"version": "v1",
			"entries": []map[string]any{
				{"provider": "x", "input_per_million": 1.0},
			}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := c.put(t, "/api/v1/cost/pricing", tc.body)
			if r.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
			}
			r.Body.Close()
		})
	}
}

// --- /cost/summary -----------------------------------------------------------

func TestCostHandler_GetSummary_OK(t *testing.T) {
	eng := &fakeCostEngine{summary: cost.Summary{
		TotalUSD:     1.23,
		TokensIn:     1000,
		TokensOut:    500,
		TopConsumers: []cost.SummaryConsumer{},
	}}
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "admin"},
		CostEngine: eng,
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/summary?window=today")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got cost.Summary
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if got.Window != "today" {
		t.Errorf("window = %q, want today", got.Window)
	}
	if got.TotalUSD != 1.23 || got.TokensIn != 1000 {
		t.Errorf("summary not echoed: %+v", got)
	}
}

func TestCostHandler_GetSummary_BadWindow(t *testing.T) {
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "admin"},
		CostEngine: &fakeCostEngine{},
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/summary?window=eternity")
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
}

func TestCostHandler_GetSummary_NonAdminWithoutPerm403(t *testing.T) {
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "user"},
		CostEngine: &fakeCostEngine{},
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/summary?window=today")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", r.StatusCode, readBody(t, r))
	}
}

// --- /cost/export ------------------------------------------------------------

func TestCostHandler_GetExport_NDJSON(t *testing.T) {
	st := newFakeBudgetStore()
	st.usage = []db.UsageRow{
		{ServiceID: "svcA", APIKeyID: "kA", Kind: "openai/gpt-4o",
			TokensIn: 1000, TokensOut: 500, BytesIn: 100, BytesOut: 200},
	}
	pr, _ := cost.LoadEmbedded()
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "admin"},
		Budgets:    st,
		CostEngine: &fakeCostEngine{pricing: pr},
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/export?format=ndjson&window=today")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if !strings.Contains(body, `"service_id":"svcA"`) {
		t.Errorf("ndjson missing service_id row: %s", body)
	}
	if !strings.Contains(body, `"usd":`) {
		t.Errorf("ndjson missing usd field: %s", body)
	}
}

// The export's usd is the dashboard's: what the upstream reported plus the
// price table for the tokens it put no price on.
func TestCostHandler_GetExport_NDJSONUsesReportedCost(t *testing.T) {
	st := newFakeBudgetStore()
	st.usage = []db.UsageRow{
		{ServiceID: "svcA", APIKeyID: "kA", Kind: "k",
			TokensIn: 3_000_000, TokensOut: 0, ReportedUSD: 0.25, PricedTokensIn: 1_000_000},
	}
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: st,
		CostEngine: &fakeCostEngine{pricing: cost.Pricing{Entries: map[string]cost.Entry{
			"k": {InputPerMillion: 1, OutputPerMillion: 2},
		}}},
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/export?format=ndjson&window=today")
	body := readBody(t, r)
	if r.StatusCode != http.StatusOK || !strings.Contains(body, `"usd":1.25`) || !strings.Contains(body, `"tokens_in":3000000`) {
		t.Fatalf("status=%d body=%s, want usd 1.25 over all 3000000 tokens", r.StatusCode, body)
	}
}

func TestCostHandler_GetExport_CSV(t *testing.T) {
	st := newFakeBudgetStore()
	st.usage = []db.UsageRow{
		{ServiceID: "svcB", APIKeyID: "kB", Kind: "anthropic/claude-3-5-sonnet",
			TokensIn: 200, TokensOut: 100, BytesIn: 50, BytesOut: 100},
	}
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "admin"},
		Budgets:    st,
		CostEngine: &fakeCostEngine{},
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/export?format=csv&window=today")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	body := readBody(t, r)
	if !strings.Contains(body, "service_id,api_key_id,kind") {
		t.Errorf("csv header missing: %s", body)
	}
	if !strings.Contains(body, "svcB") {
		t.Errorf("csv row missing: %s", body)
	}
}

func TestCostHandler_GetExport_BadFormat(t *testing.T) {
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: newFakeBudgetStore(),
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/export?format=xml")
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", r.StatusCode)
	}
}

// --- /budgets CRUD -----------------------------------------------------------

func TestBudgetHandler_GetEmpty(t *testing.T) {
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: newFakeBudgetStore(),
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/budgets")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var out []budgetResp
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if out == nil || len(out) != 0 {
		t.Fatalf("want empty non-nil array; got %v", out)
	}
}

func TestBudgetHandler_PostOK(t *testing.T) {
	st := newFakeBudgetStore()
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: st,
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	body := map[string]any{
		"scope":            "api_key",
		"subject_id":       "k1",
		"daily_usd":        10.0,
		"action_on_exceed": "alert_webhook",
	}
	r := c.post(t, "/api/v1/budgets", body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got budgetResp
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if got.ID == "" {
		t.Fatal("server must allocate an id")
	}
	if got.Scope != "api_key" || got.SubjectID != "k1" ||
		got.DailyUSD != 10.0 || got.ActionOnExceed != "alert_webhook" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Exceeded {
		t.Errorf("new budget should not be exceeded yet")
	}
}

func TestBudgetHandler_PostBadInputs(t *testing.T) {
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: newFakeBudgetStore(),
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad scope", map[string]any{
			"scope": "garbage", "subject_id": "k1",
			"daily_usd": 10.0, "action_on_exceed": "alert_webhook"}},
		{"bad action", map[string]any{
			"scope": "api_key", "subject_id": "k1",
			"daily_usd": 10.0, "action_on_exceed": "burn_down"}},
		{"zero daily_usd", map[string]any{
			"scope": "api_key", "subject_id": "k1",
			"daily_usd": 0.0, "action_on_exceed": "alert_webhook"}},
		{"negative daily_usd", map[string]any{
			"scope": "api_key", "subject_id": "k1",
			"daily_usd": -5.0, "action_on_exceed": "alert_webhook"}},
		{"global with subject", map[string]any{
			"scope": "global", "subject_id": "x",
			"daily_usd": 10.0, "action_on_exceed": "alert_webhook"}},
		{"non-global without subject", map[string]any{
			"scope": "api_key", "subject_id": "",
			"daily_usd": 10.0, "action_on_exceed": "alert_webhook"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := c.post(t, "/api/v1/budgets", tc.body)
			if r.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
			}
			r.Body.Close()
		})
	}
}

func TestBudgetHandler_PutOK(t *testing.T) {
	st := newFakeBudgetStore()
	st.rows["b1"] = db.Budget{
		ID: "b1", Scope: "api_key", SubjectID: "k1",
		DailyUSD: 10.0, ActionOnExceed: "alert_webhook",
	}
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: st,
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.put(t, "/api/v1/budgets/b1", map[string]any{
		"scope":            "api_key",
		"subject_id":       "k1",
		"daily_usd":        25.5,
		"action_on_exceed": "disable_key",
	})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	row, _ := st.GetBudget(context.Background(), "b1")
	if row.DailyUSD != 25.5 || row.ActionOnExceed != "disable_key" {
		t.Fatalf("update did not persist: %+v", row)
	}
}

func TestBudgetHandler_PutNotFound(t *testing.T) {
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: newFakeBudgetStore(),
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.put(t, "/api/v1/budgets/nope", map[string]any{
		"scope": "api_key", "subject_id": "k1",
		"daily_usd": 10.0, "action_on_exceed": "alert_webhook",
	})
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", r.StatusCode)
	}
}

func TestBudgetHandler_DeleteOK(t *testing.T) {
	st := newFakeBudgetStore()
	st.rows["b1"] = db.Budget{
		ID: "b1", Scope: "global", SubjectID: "",
		DailyUSD: 100.0, ActionOnExceed: "alert_webhook",
	}
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: st,
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.delete(t, "/api/v1/budgets/b1")
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", r.StatusCode)
	}
	if _, err := st.GetBudget(context.Background(), "b1"); err != db.ErrNotFound {
		t.Fatalf("row should be deleted: %v", err)
	}
}

func TestBudgetHandler_DeleteNotFound(t *testing.T) {
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "admin"},
		Budgets: newFakeBudgetStore(),
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.delete(t, "/api/v1/budgets/nope")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", r.StatusCode)
	}
}

func TestBudgetHandler_NonAdmin403(t *testing.T) {
	d := Deps{
		Log:     discardLog(),
		Users:   &fakeUserStore{role: "user"},
		Budgets: newFakeBudgetStore(),
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/budgets", nil},
		{http.MethodPost, "/api/v1/budgets", map[string]any{
			"scope": "api_key", "subject_id": "k1",
			"daily_usd": 10.0, "action_on_exceed": "alert_webhook"}},
		{http.MethodPut, "/api/v1/budgets/b1", map[string]any{
			"scope": "api_key", "subject_id": "k1",
			"daily_usd": 10.0, "action_on_exceed": "alert_webhook"}},
		{http.MethodDelete, "/api/v1/budgets/b1", nil},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var r *http.Response
			switch tc.method {
			case http.MethodGet:
				r = c.get(t, tc.path)
			case http.MethodPost:
				r = c.post(t, tc.path, tc.body)
			case http.MethodPut:
				r = c.put(t, tc.path, tc.body)
			case http.MethodDelete:
				r = c.delete(t, tc.path)
			}
			if r.StatusCode != http.StatusForbidden {
				t.Fatalf("status=%d body=%s, want 403", r.StatusCode, readBody(t, r))
			}
			r.Body.Close()
		})
	}
}

// --- Smoke: pricing PUT then GET shows the new entries ----------------------

func TestCostHandler_PricingPutThenGetRoundTrip(t *testing.T) {
	eng := &fakeCostEngine{pricing: cost.Pricing{Version: "init", Entries: map[string]cost.Entry{}}}
	d := Deps{
		Log:        discardLog(),
		Users:      &fakeUserStore{role: "admin"},
		CostEngine: eng,
	}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	body := map[string]any{
		"version": "v2",
		"entries": []map[string]any{
			{"provider": "openai", "model": "gpt-z",
				"input_per_million": 5.5, "output_per_million": 11.0},
		},
	}
	if rp := c.put(t, "/api/v1/cost/pricing", body); rp.StatusCode != http.StatusNoContent {
		t.Fatalf("put status=%d", rp.StatusCode)
	}
	r := c.get(t, "/api/v1/cost/pricing")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d", r.StatusCode)
	}
	var got pricingResp
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if got.Version != "v2" {
		t.Fatalf("version after PUT = %q, want v2", got.Version)
	}
	if len(got.Entries) != 1 || got.Entries[0].Provider != "openai" || got.Entries[0].Model != "gpt-z" {
		t.Fatalf("entries after PUT: %+v", got.Entries)
	}
}

// Compile-time assertions that the production *cost.Engine satisfies our
// CostEngine interface, and *db.DB satisfies BudgetStore. Catches drift at
// build time.
var (
	_ CostEngine  = (*cost.Engine)(nil)
	_ BudgetStore = (*db.DB)(nil)
)

// --- usage by key and model, token budgets ------------------------------------

func TestCostHandler_GetSummary_GroupBy(t *testing.T) {
	eng := &fakeCostEngine{
		summary: cost.Summary{TotalUSD: 2.5, TopConsumers: []cost.SummaryConsumer{}},
		groups:  []cost.GroupRow{{Key: "gk1", Requests: 3, TokensIn: 10, TokensOut: 5, USD: 2.25}, {Key: "", Requests: 1, USD: 0.25}},
	}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, CostEngine: eng}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)

	for _, dim := range []string{"gateway_key", "model", "provider", "target_model", "dialect"} {
		r := c.get(t, "/api/v1/cost/summary?window=week&group_by="+dim)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", dim, r.StatusCode, readBody(t, r))
		}
		var got struct {
			Window   string           `json:"window"`
			TotalUSD float64          `json:"total_usd"`
			GroupBy  string           `json:"group_by"`
			Groups   []map[string]any `json:"groups"`
		}
		if err := json.Unmarshal([]byte(readBody(t, r)), &got); err != nil {
			t.Fatal(err)
		}
		if got.Window != "week" || got.TotalUSD != 2.5 || got.GroupBy != dim || len(got.Groups) != 2 {
			t.Fatalf("%s: %+v", dim, got)
		}
		g := got.Groups[0]
		if g["key"] != "gk1" || g["requests"] != 3.0 || g["tokens_in"] != 10.0 || g["tokens_out"] != 5.0 || g["usd"] != 2.25 || len(g) != 5 {
			t.Fatalf("%s: group = %+v", dim, g)
		}
	}
	if eng.groupCalls[0] != "week/gateway_key" {
		t.Fatalf("calls = %v", eng.groupCalls)
	}

	// Without group_by the body is what it always was: no groups at all.
	body := readBody(t, c.get(t, "/api/v1/cost/summary?window=today"))
	if strings.Contains(body, "groups") || strings.Contains(body, "group_by") {
		t.Fatalf("summary without group_by = %s", body)
	}
	// No groups in the window: an empty list.
	eng.groups = nil
	if body := readBody(t, c.get(t, "/api/v1/cost/summary?group_by=model")); !strings.Contains(body, `"groups":[]`) {
		t.Fatalf("empty groups = %s", body)
	}

	for _, bad := range []string{"service", "api_key", "GATEWAY_KEY", "model%3BDROP%20TABLE", "gateway_key,model"} {
		r := c.get(t, "/api/v1/cost/summary?window=today&group_by="+bad)
		body := readBody(t, r)
		if r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "group_by must be one of gateway_key|model|provider|target_model|dialect") {
			t.Fatalf("%s: status=%d body=%s", bad, r.StatusCode, body)
		}
	}
	// A bad window is refused before anything is read.
	n := len(eng.groupCalls)
	if r := c.get(t, "/api/v1/cost/summary?window=eternity&group_by=model"); r.StatusCode != http.StatusBadRequest || len(eng.groupCalls) != n {
		t.Fatalf("bad window with group_by: status=%d", r.StatusCode)
	}
}

// Usage by key and model is the cost dashboard's: a user without
// quotas:read:any does not get it, grouped or not.
func TestCostHandler_GetSummary_GroupBy_NonAdmin403(t *testing.T) {
	eng := &fakeCostEngine{groups: []cost.GroupRow{{Key: "gk1", USD: 1}}}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "user"}, CostEngine: eng}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/cost/summary?window=today&group_by=gateway_key")
	if body := readBody(t, r); r.StatusCode != http.StatusForbidden || strings.Contains(body, "gk1") || len(eng.groupCalls) != 0 {
		t.Fatalf("status=%d body=%s calls=%v", r.StatusCode, body, eng.groupCalls)
	}
}

func TestCostHandler_GetExport_RouteColumns(t *testing.T) {
	st := newFakeBudgetStore()
	st.usage = []db.UsageRow{{ServiceID: "svcA", Kind: "openai", GatewayKeyID: "gk1", Dialect: "openai", ProviderSlug: "zai",
		RequestedModel: "burrow-smart", TargetModel: "glm-5.1", Requests: 2, TokensIn: 1_000_000, TokensOut: 0, PricedTokensIn: 1_000_000}}
	eng := &fakeCostEngine{pricing: cost.Pricing{Entries: map[string]cost.Entry{
		"zai/glm-5.1": {InputPerMillion: 3}, "openai": {InputPerMillion: 9},
	}}}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Budgets: st, CostEngine: eng}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)
	var row map[string]any
	if err := json.Unmarshal([]byte(readBody(t, c.get(t, "/api/v1/cost/export?format=ndjson"))), &row); err != nil {
		t.Fatal(err)
	}
	// Priced by the provider and model that answered, as the summary is.
	if row["gateway_key_id"] != "gk1" || row["dialect"] != "openai" || row["provider"] != "zai" || row["requested_model"] != "burrow-smart" ||
		row["target_model"] != "glm-5.1" || row["requests"] != 2.0 || row["usd"] != 3.0 {
		t.Fatalf("ndjson row = %+v", row)
	}
	csvBody := readBody(t, c.get(t, "/api/v1/cost/export?format=csv"))
	lines := strings.Split(strings.TrimSpace(csvBody), "\n")
	if len(lines) != 2 ||
		lines[0] != "service_id,api_key_id,kind,tokens_in,tokens_out,bytes_in,bytes_out,gateway_key_id,dialect,provider,requested_model,target_model,requests" ||
		lines[1] != "svcA,,openai,1000000,0,0,0,gk1,openai,zai,burrow-smart,glm-5.1,2" {
		t.Fatalf("csv = %q", csvBody)
	}
}

func TestBudgetHandler_GatewayKeyModelAndTokens(t *testing.T) {
	st := newFakeBudgetStore()
	eng := &fakeCostEngine{budgetUsage: map[string]cost.BudgetUsage{
		"gk1":          {USD: 0, Tokens: 1500},
		"burrow-smart": {USD: 2.5, Tokens: 10},
	}}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Budgets: st, CostEngine: eng}
	srv := newTestServer(d)
	defer srv.Close()
	c := authedClient(t, srv)

	// A token budget needs no USD cap: a flat-rate provider costs 0 USD.
	r := c.post(t, "/api/v1/budgets", map[string]any{
		"scope": "gateway_key", "subject_id": "gk1", "daily_tokens": 1000, "action_on_exceed": "throttle_zero"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST token budget: status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(readBody(t, r)), &created); err != nil {
		t.Fatal(err)
	}
	if created["scope"] != "gateway_key" || created["daily_tokens"] != 1000.0 || created["daily_usd"] != 0.0 ||
		created["current_tokens"] != 1500.0 || created["current_usd"] != 0.0 || created["exceeded"] != true {
		t.Fatalf("created = %+v", created)
	}
	r = c.post(t, "/api/v1/budgets", map[string]any{
		"scope": "model", "subject_id": "burrow-smart", "daily_usd": 5.0, "daily_tokens": 1_000_000, "action_on_exceed": "alert_webhook"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST model budget: status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	_ = readBody(t, r)

	var list []map[string]any
	if err := json.Unmarshal([]byte(readBody(t, c.get(t, "/api/v1/budgets"))), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0]["scope"] != "gateway_key" || list[0]["exceeded"] != true || list[0]["current_tokens"] != 1500.0 ||
		list[1]["scope"] != "model" || list[1]["exceeded"] != false || list[1]["current_usd"] != 2.5 || list[1]["daily_tokens"] != 1_000_000.0 {
		t.Fatalf("list = %+v", list)
	}

	// PUT carries daily_tokens too.
	id := created["id"].(string)
	r = c.put(t, "/api/v1/budgets/"+id, map[string]any{
		"scope": "gateway_key", "subject_id": "gk1", "daily_usd": 1.0, "daily_tokens": 2000, "action_on_exceed": "disable_key"})
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT: status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	if b := st.rows[id]; b.DailyTokens != 2000 || b.DailyUSD != 1 || b.ActionOnExceed != "disable_key" {
		t.Fatalf("stored = %+v", b)
	}

	for name, c2 := range map[string]struct {
		body map[string]any
		msg  string
	}{
		"negative tokens": {map[string]any{"scope": "gateway_key", "subject_id": "gk1", "daily_usd": 1.0, "daily_tokens": -1, "action_on_exceed": "throttle_zero"},
			"daily_tokens must not be negative"},
		"neither cap": {map[string]any{"scope": "model", "subject_id": "m", "action_on_exceed": "throttle_zero"},
			"daily_usd or daily_tokens must be greater than zero"},
		"negative usd with tokens": {map[string]any{"scope": "model", "subject_id": "m", "daily_usd": -1.0, "daily_tokens": 5, "action_on_exceed": "throttle_zero"},
			"daily_usd must not be negative"},
		"no subject": {map[string]any{"scope": "gateway_key", "daily_tokens": 5, "action_on_exceed": "throttle_zero"},
			"subject_id is required for non-global scopes"},
		"long subject": {map[string]any{"scope": "model", "subject_id": strings.Repeat("m", 257), "daily_tokens": 5, "action_on_exceed": "throttle_zero"},
			"subject_id too long (max 256 chars)"},
		"bad scope": {map[string]any{"scope": "provider", "subject_id": "zai", "daily_tokens": 5, "action_on_exceed": "throttle_zero"},
			"scope must be one of api_key|service|user|global|gateway_key|model"},
	} {
		for _, method := range []string{"POST", "PUT"} {
			var r *http.Response
			if method == "POST" {
				r = c.post(t, "/api/v1/budgets", c2.body)
			} else {
				r = c.put(t, "/api/v1/budgets/"+id, c2.body)
			}
			if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, c2.msg) {
				t.Errorf("%s %s: status=%d body=%s", method, name, r.StatusCode, body)
			}
		}
	}
}

// When today's usage cannot be read the budgets are still listed, with zero
// usage: the list is configuration first.
func TestBudgetHandler_GetWithoutUsage(t *testing.T) {
	st := newFakeBudgetStore()
	st.rows["b1"] = db.Budget{ID: "b1", Scope: "model", SubjectID: "m", DailyTokens: 5, ActionOnExceed: "throttle_zero"}
	eng := &fakeCostEngine{usageErr: errors.New("db is down")}
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}, Budgets: st, CostEngine: eng}
	srv := newTestServer(d)
	defer srv.Close()
	r := authedClient(t, srv).get(t, "/api/v1/budgets")
	body := readBody(t, r)
	if r.StatusCode != http.StatusOK || !strings.Contains(body, `"id":"b1"`) || !strings.Contains(body, `"current_tokens":0`) {
		t.Fatalf("status=%d body=%s", r.StatusCode, body)
	}
}
