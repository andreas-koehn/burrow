package db

import (
	"context"
	"testing"
	"time"
)

// Tool calls are usage rows of kind 'mcp'. A day-window quota counts model
// requests: the tool calls of a key, a service or an api key leave every one
// of the day-window queries as they were.
func TestDayQuotaIgnoresToolCalls(t *testing.T) {
	checkDayQuotaIgnoresToolCalls(t, testDB(t), "u-mcpq")
}

func checkDayQuotaIgnoresToolCalls(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
	svc := seedSvc(t, x, userID, "svc-mcpq")
	gk, key, model := "gk-"+userID, "k-"+userID, "model-"+userID
	now := time.Now().UTC()
	rows := []struct {
		id, kind, gk, apiKey string
		bytes                int64
	}{
		{"m1", "openai", gk, "", 400},
		{"m2", "mcp", gk, "", 4000},
		{"m3", "mcp", gk, "", 4000},
		{"m4", "openai", "", key, 40},
		{"m5", "mcp", "", key, 4000},
	}
	for _, r := range rows {
		if _, err := x.sqlDB.ExecContext(ctx,
			`INSERT INTO usage_events(id, service_id, api_key_id, ts, kind, bytes_in, bytes_out, gateway_key_id, requested_model)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			"mcpq-"+userID+"-"+r.id, svc, r.apiKey, now, r.kind, r.bytes, 0, r.gk, model); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	for _, c := range []struct {
		name string
		got  func() (int64, error)
		want int64
	}{
		{"count gateway key", func() (int64, error) { return x.CountDailyUsageEventsByGatewayKey(ctx, gk) }, 1},
		{"sum gateway key", func() (int64, error) { return x.SumDailyUsageEventsByGatewayKey(ctx, gk) }, 100},
		{"count gw subject", func() (int64, error) { return x.CountDailyUsageEventsByAPIKey(ctx, "gw:"+gk) }, 1},
		{"count api key", func() (int64, error) { return x.CountDailyUsageEventsByAPIKey(ctx, key) }, 1},
		{"sum api key", func() (int64, error) { return x.SumDailyUsageEventsByAPIKey(ctx, key) }, 10},
		{"count service", func() (int64, error) { return x.CountDailyUsageEventsByService(ctx, svc) }, 2},
		{"sum service", func() (int64, error) { return x.SumDailyUsageEventsByService(ctx, svc) }, 110},
		{"count model", func() (int64, error) { return x.CountDailyUsageEventsByModel(ctx, model) }, 2},
		{"sum model", func() (int64, error) { return x.SumDailyUsageEventsByModel(ctx, model) }, 110},
	} {
		if got, err := c.got(); err != nil || got != c.want {
			t.Errorf("%s = %d (%v), want %d", c.name, got, err, c.want)
		}
	}
}
