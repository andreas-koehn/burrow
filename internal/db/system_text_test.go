package db

import (
	"context"
	"strings"
	"testing"
)

func TestMigration0028_DownAndUp(t *testing.T) {
	checkMigration0028DownAndUp(t, testDB(t), "0028_v0.11.0_system_text_routing.sql", "u-mig28")
}

// checkMigration0028DownAndUp runs the Down block, writes a model and a key
// the way the schema before the migration allowed, and runs the Up block
// again: Up only succeeds when Down removed every column Up adds, and the
// rows from before read with the defaults. One rolled-back transaction.
func checkMigration0028DownAndUp(t *testing.T, x *DB, file, userID string) {
	t.Helper()
	ctx := context.Background()
	raw, err := migrationFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	_, down, found := strings.Cut(string(raw), "-- +goose Down")
	if !found || strings.TrimSpace(down) == "" {
		t.Fatalf("%s has no Down block", file)
	}
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })

	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_models(name) VALUES(?)`, "mig28-old-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_gateway_keys(id, name, key_hash, key_prefix, user_id) VALUES(?,?,?,?,?)`,
		"mig28-old-key", "old", "mig28-hash", "bgw_old1", userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, upBlock(string(raw))); err != nil {
		t.Fatalf("up after down (a column the Down block left behind fails here): %v", err)
	}
	var text, routing string
	if err := tx.QueryRowContext(ctx, `SELECT system_text, routing FROM ai_models WHERE name=?`, "mig28-old-model").Scan(&text, &routing); err != nil || text != "" || routing != "ordered" {
		t.Fatalf("old model: text=%q routing=%q err=%v, want '' and 'ordered'", text, routing, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT system_text FROM ai_gateway_keys WHERE id=?`, "mig28-old-key").Scan(&text); err != nil || text != "" {
		t.Fatalf("old key: text=%q err=%v, want ''", text, err)
	}
}

func TestAIModels_SystemTextAndRouting(t *testing.T) { checkAIModelSystemText(t, testDB(t), "u-st1") }

// checkAIModelSystemText: system_text and routing round-trip through create,
// update, get and list; a row written without them reads as ordered.
func checkAIModelSystemText(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	seedModelProviders(t, x, userID)
	targets := []AIModelTarget{{Dialect: "openai", ProviderSlug: "zai", TargetModel: "glm-5.1"}}
	for _, n := range []string{"st-set", "st-plain", "st-legacy"} {
		_ = x.DeleteAIModel(ctx, n)
	}
	if err := x.CreateAIModel(ctx, AIModel{Name: "st-set", Enabled: true, SystemText: "Answer in German.", Routing: "fastest", Targets: targets}); err != nil {
		t.Fatal(err)
	}
	if err := x.CreateAIModel(ctx, AIModel{Name: "st-plain", Enabled: true, Targets: targets}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.sqlDB.ExecContext(ctx, `INSERT INTO ai_models(name) VALUES(?)`, "st-legacy"); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"st-set":    {"Answer in German.", "fastest"},
		"st-plain":  {"", "ordered"},
		"st-legacy": {"", "ordered"},
	}
	for name, w := range want {
		got, err := x.GetAIModel(ctx, name)
		if err != nil || got.SystemText != w[0] || got.Routing != w[1] {
			t.Fatalf("get %s: %v text=%q routing=%q, want %q", name, err, got.SystemText, got.Routing, w)
		}
	}
	list, err := x.ListAIModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if w, ok := want[m.Name]; ok && (m.SystemText != w[0] || m.Routing != w[1]) {
			t.Fatalf("list %s: text=%q routing=%q", m.Name, m.SystemText, m.Routing)
		}
	}
	if err := x.UpdateAIModel(ctx, "st-plain", AIModel{Name: "st-plain", Enabled: true, SystemText: "Be brief.", Routing: "cheapest", Targets: targets}); err != nil {
		t.Fatal(err)
	}
	if got, err := x.GetAIModel(ctx, "st-plain"); err != nil || got.SystemText != "Be brief." || got.Routing != "cheapest" {
		t.Fatalf("after update: %v %+v", err, got)
	}
	// An empty Routing written by a caller is stored as ordered.
	if err := x.UpdateAIModel(ctx, "st-plain", AIModel{Name: "st-plain", Enabled: true, Targets: targets}); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.GetAIModel(ctx, "st-plain"); got.SystemText != "" || got.Routing != "ordered" {
		t.Fatalf("after clearing: %+v", got)
	}
}

func TestAIGatewayKeys_SystemTextAndUpdate(t *testing.T) {
	checkAIGatewayKeySystemText(t, testDB(t), "u-gkst")
}

func checkAIGatewayKeySystemText(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM ai_gateway_keys WHERE user_id=?`, userID)
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() {
		_, _ = x.sqlDB.ExecContext(ctx, `DELETE FROM ai_gateway_keys WHERE user_id=?`, userID)
		_ = x.DeleteUser(ctx, userID)
	})
	if err := x.CreateAIGatewayKey(ctx, AIGatewayKey{ID: "gkst1", Name: "a", KeyHash: "gkst-h1", KeyPrefix: "bgw_aaaa", UserID: userID, SystemText: "Hello."}); err != nil {
		t.Fatal(err)
	}
	if err := x.CreateAIGatewayKey(ctx, AIGatewayKey{ID: "gkst2", Name: "b", KeyHash: "gkst-h2", KeyPrefix: "bgw_bbbb", UserID: userID}); err != nil {
		t.Fatal(err)
	}
	if k, err := x.GetAIGatewayKey(ctx, "gkst1"); err != nil || k.SystemText != "Hello." {
		t.Fatalf("get: %v %+v", err, k)
	}
	if k, err := x.GetAIGatewayKeyByHash(ctx, "gkst-h2"); err != nil || k.SystemText != "" {
		t.Fatalf("by hash: %v %+v", err, k)
	}
	if err := x.UpdateAIGatewayKey(ctx, AIGatewayKey{ID: "gkst1", Name: "renamed", SystemText: "Other.", AllowedTools: []string{"gh__*"}, ToolsReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if k, _ := x.GetAIGatewayKey(ctx, "gkst1"); k.Name != "renamed" || k.SystemText != "Other." || len(k.AllowedTools) != 1 || !k.ToolsReadOnly {
		t.Fatalf("after update: %+v", k)
	}
	if err := x.UpdateAIGatewayKey(ctx, AIGatewayKey{ID: "missing", Name: "x"}); err != ErrNotFound {
		t.Fatalf("update missing = %v, want ErrNotFound", err)
	}
	list, err := x.ListAIGatewayKeys(ctx, userID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %+v", err, list)
	}
}
