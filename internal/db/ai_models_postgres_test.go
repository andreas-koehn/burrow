//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestGatewayModels_Postgres runs the statements of migration 0023's tables
// against a live Postgres: synthetic models with their targets, gateway keys,
// usage attempts and the gateway_only flag of a service. The checks are the
// ones the SQLite tests run.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestGatewayModels_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres gateway models check")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("models", func(t *testing.T) { checkAIModels(t, x, "u-models-pg") })
	t.Run("provider rename", func(t *testing.T) { checkAIModelsFollowProviderRename(t, x, "u-models-pg") })
	t.Run("gateway keys", func(t *testing.T) { checkAIGatewayKeys(t, x, "u-gwkey-pg-1", "u-gwkey-pg-2") })
	t.Run("usage attempts", func(t *testing.T) { checkUsageAttempts(t, x) })
	t.Run("gateway only", func(t *testing.T) { checkServiceGatewayOnly(t, x, "u-gwonly-pg") })
}
