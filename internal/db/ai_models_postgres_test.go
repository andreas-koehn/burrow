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
	t.Run("translate flag", func(t *testing.T) { checkAIModelTranslate(t, x, "u-models-pg") })
	t.Run("migration 0025 down and up", func(t *testing.T) {
		checkMigration0025DownAndUp(t, x, "0025_v0.9.0_translation.postgres.sql", "u-mig25-pg")
	})
	t.Run("provider rename", func(t *testing.T) { checkAIModelsFollowProviderRename(t, x, "u-models-pg") })
	t.Run("gateway keys", func(t *testing.T) { checkAIGatewayKeys(t, x, "u-gwkey-pg-1", "u-gwkey-pg-2") })
	t.Run("delete keeps other errors", func(t *testing.T) {
		const fn = "burrow_test_refuse_delete"
		if _, err := x.DB().Exec(`CREATE OR REPLACE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS ` +
			`'BEGIN RAISE EXCEPTION ''refused by test''; END'`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = x.DB().Exec(`DROP FUNCTION IF EXISTS ` + fn + `() CASCADE`) })
		checkDeleteKeepsOtherErrors(t, x, "u-models-pg", func(name, table, when string) string {
			return `CREATE TRIGGER ` + name + ` BEFORE DELETE ON ` + table + ` FOR EACH ROW WHEN (` + when + `) EXECUTE FUNCTION ` + fn + `()`
		}, func(name, table string) string { return `DROP TRIGGER IF EXISTS ` + name + ` ON ` + table })
	})
	// The migration leaves the foreign key of ai_model_targets.provider_slug
	// with the name Postgres gives it; the delete mapping compares against it.
	t.Run("target foreign key name", func(t *testing.T) {
		var name string
		if err := x.DB().QueryRow(`
			SELECT c.conname
			  FROM pg_constraint c
			  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
			 WHERE c.contype = 'f' AND c.conrelid = 'ai_model_targets'::regclass AND a.attname = 'provider_slug'`).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != pgTargetProviderFK {
			t.Fatalf("constraint = %q, the mapping expects %q", name, pgTargetProviderFK)
		}
	})
	t.Run("usage attempts", func(t *testing.T) { checkUsageAttempts(t, x) })
	t.Run("gateway only", func(t *testing.T) { checkServiceGatewayOnly(t, x, "u-gwonly-pg") })
}
