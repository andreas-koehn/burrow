package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWebhooksCRUD(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	// Empty list returns a non-nil empty slice.
	rows, err := x.ListWebhooks(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("want empty non-nil slice, got %v", rows)
	}

	// Create two webhooks.
	w1 := Webhook{
		ID: "wh1", Name: "alerts", URL: "https://example.com/wh",
		SecretHash: "deadbeef", Events: `["tunnel.connected"]`,
	}
	if err := x.CreateWebhook(ctx, w1); err != nil {
		t.Fatalf("create wh1: %v", err)
	}
	// Brief sleep so created_at strictly orders.
	time.Sleep(2 * time.Millisecond)
	w2 := Webhook{
		ID: "wh2", Name: "audit", URL: "https://example.com/audit",
		SecretHash: "cafebabe", Events: `["audit.exported"]`,
		Paused: true,
	}
	if err := x.CreateWebhook(ctx, w2); err != nil {
		t.Fatalf("create wh2: %v", err)
	}

	// Get round-trip.
	got, err := x.GetWebhook(ctx, "wh1")
	if err != nil {
		t.Fatalf("get wh1: %v", err)
	}
	if got.Name != "alerts" || got.URL != "https://example.com/wh" ||
		got.SecretHash != "deadbeef" || got.Events != `["tunnel.connected"]` ||
		got.Paused != false || got.ConsecutiveFailures != 0 || got.FirstFailureAt != nil {
		t.Fatalf("wh1 round-trip: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at zero (sqlite default not applied?)")
	}
	got2, err := x.GetWebhook(ctx, "wh2")
	if err != nil {
		t.Fatalf("get wh2: %v", err)
	}
	if !got2.Paused {
		t.Fatalf("wh2 paused must be true")
	}

	// List returns both ordered by created_at.
	rows, err = x.ListWebhooks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len=%d want 2", len(rows))
	}
	if rows[0].ID != "wh1" || rows[1].ID != "wh2" {
		t.Fatalf("order: %+v", rows)
	}

	// Update mutable fields.
	upd := Webhook{
		ID: "wh1", Name: "alerts-renamed", URL: "https://example.com/new",
		Events: `["tunnel.connected","tunnel.failed"]`, Paused: true,
	}
	if err := x.UpdateWebhook(ctx, upd); err != nil {
		t.Fatalf("update wh1: %v", err)
	}
	got, err = x.GetWebhook(ctx, "wh1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alerts-renamed" || got.URL != "https://example.com/new" ||
		!got.Paused || got.Events != `["tunnel.connected","tunnel.failed"]` {
		t.Fatalf("update did not stick: %+v", got)
	}
	// Secret hash must NOT have changed (Update never touches it).
	if got.SecretHash != "deadbeef" {
		t.Fatalf("secret_hash leaked through Update: %q", got.SecretHash)
	}

	// SetWebhookPaused toggles only paused.
	if err := x.SetWebhookPaused(ctx, "wh1", false); err != nil {
		t.Fatalf("unpause: %v", err)
	}
	got, _ = x.GetWebhook(ctx, "wh1")
	if got.Paused {
		t.Fatal("unpause did not stick")
	}

	// SetWebhookFailureCounters writes counter + first-fail-at, and clears.
	when := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	if err := x.SetWebhookFailureCounters(ctx, "wh1", 7, &when); err != nil {
		t.Fatalf("set counters: %v", err)
	}
	got, _ = x.GetWebhook(ctx, "wh1")
	if got.ConsecutiveFailures != 7 {
		t.Fatalf("counter=%d want 7", got.ConsecutiveFailures)
	}
	if got.FirstFailureAt == nil || !got.FirstFailureAt.Equal(when) {
		t.Fatalf("first_failure_at: got %v want %v", got.FirstFailureAt, when)
	}
	if err := x.SetWebhookFailureCounters(ctx, "wh1", 0, nil); err != nil {
		t.Fatalf("clear counters: %v", err)
	}
	got, _ = x.GetWebhook(ctx, "wh1")
	if got.ConsecutiveFailures != 0 || got.FirstFailureAt != nil {
		t.Fatalf("clear didn't stick: %+v", got)
	}

	// Delete cascades to webhook_deliveries.
	req := "req-bytes"
	resp := "resp-bytes"
	dlv := WebhookDelivery{
		ID: "d1", WebhookID: "wh2", Event: "audit.exported",
		StatusCode: 200, Attempt: 1, LatencyMs: 42,
		RequestPreview: &req, ResponsePreview: &resp,
	}
	if err := x.InsertWebhookDelivery(ctx, dlv); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	if err := x.DeleteWebhook(ctx, "wh2"); err != nil {
		t.Fatalf("delete wh2: %v", err)
	}
	dls, _ := x.ListWebhookDeliveries(ctx, WebhookDeliveryQuery{WebhookID: "wh2"})
	if len(dls) != 0 {
		t.Fatalf("delete must cascade webhook_deliveries; got %d rows", len(dls))
	}

	// GetWebhook on a missing id returns ErrNotFound.
	if _, err := x.GetWebhook(ctx, "no-such"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get: want ErrNotFound, got %v", err)
	}
	if err := x.DeleteWebhook(ctx, "no-such"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete: want ErrNotFound, got %v", err)
	}
	if err := x.UpdateWebhook(ctx, Webhook{ID: "no-such", Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update: want ErrNotFound, got %v", err)
	}
	if err := x.SetWebhookPaused(ctx, "no-such", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing pause: want ErrNotFound, got %v", err)
	}
	if err := x.SetWebhookFailureCounters(ctx, "no-such", 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing counters: want ErrNotFound, got %v", err)
	}
}

func TestWebhookDeliveriesListAndCount(t *testing.T) {
	x := testDB(t)
	ctx := context.Background()

	// Two webhooks.
	if err := x.CreateWebhook(ctx, Webhook{
		ID: "wh-a", Name: "a", URL: "https://example.com/a",
		SecretHash: "h", Events: `["webhook.test"]`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := x.CreateWebhook(ctx, Webhook{
		ID: "wh-b", Name: "b", URL: "https://example.com/b",
		SecretHash: "h", Events: `["webhook.test"]`,
	}); err != nil {
		t.Fatal(err)
	}

	// Insert deliveries with explicit ts so DESC ordering is deterministic.
	now := time.Now().UTC()
	mk := func(id, wh string, ts time.Time, status int) WebhookDelivery {
		return WebhookDelivery{
			ID: id, WebhookID: wh, Event: "webhook.test",
			Ts: ts, StatusCode: status, Attempt: 1, LatencyMs: 10,
		}
	}
	rows := []WebhookDelivery{
		mk("d1", "wh-a", now.Add(-3*time.Minute), 200),
		mk("d2", "wh-a", now.Add(-2*time.Minute), 404),
		mk("d3", "wh-a", now.Add(-1*time.Minute), 401),
		mk("d4", "wh-b", now.Add(-30*time.Second), 500),
	}
	for _, r := range rows {
		if err := x.InsertWebhookDelivery(ctx, r); err != nil {
			t.Fatalf("insert %s: %v", r.ID, err)
		}
	}

	// Unscoped list: ts DESC.
	all, err := x.ListWebhookDeliveries(ctx, WebhookDeliveryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("len=%d want 4", len(all))
	}
	if all[0].ID != "d4" || all[3].ID != "d1" {
		t.Fatalf("ts DESC order broken: ids=%v", []string{all[0].ID, all[1].ID, all[2].ID, all[3].ID})
	}

	// Scoped list filters by webhook_id.
	a, _ := x.ListWebhookDeliveries(ctx, WebhookDeliveryQuery{WebhookID: "wh-a"})
	if len(a) != 3 {
		t.Fatalf("wh-a len=%d want 3", len(a))
	}
	for _, d := range a {
		if d.WebhookID != "wh-a" {
			t.Fatalf("scope leak: %+v", d)
		}
	}

	// Limit cap.
	one, _ := x.ListWebhookDeliveries(ctx, WebhookDeliveryQuery{Limit: 1})
	if len(one) != 1 {
		t.Fatalf("limit=1: got %d", len(one))
	}
	// 0 → default; > 1000 → 1000 (we only verify 0 default takes effect).
	def, _ := x.ListWebhookDeliveries(ctx, WebhookDeliveryQuery{Limit: 0})
	if len(def) != 4 {
		t.Fatalf("default limit changed: got %d want 4", len(def))
	}

	// CountConsecutive4xxSince counts only 4xx in the window.
	n, err := x.CountConsecutive4xxSince(ctx, "wh-a", now.Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("4xx in 10m for wh-a: got %d want 2", n)
	}
	n, _ = x.CountConsecutive4xxSince(ctx, "wh-b", now.Add(-10*time.Minute))
	if n != 0 {
		t.Fatalf("wh-b has no 4xx (only 500); got %d want 0", n)
	}
}
