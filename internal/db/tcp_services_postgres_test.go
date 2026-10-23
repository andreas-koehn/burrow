//go:build postgres

package db

import (
	"os"
	"testing"
)

// TestTCPServices_Postgres runs the checks of migration 0040 against a live
// Postgres.
//
// Requires a live Postgres URL in BURROW_TEST_POSTGRES_URL.
func TestTCPServices_Postgres(t *testing.T) {
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres tcp service checks")
	}
	b, err := OpenPostgres(pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	x := Wrap(b.DB())
	t.Cleanup(func() { _ = x.Close() })
	t.Run("tcp port", func(t *testing.T) { checkServiceTCPPort(t, x, "u-tcpport-pg", 9601) })
	t.Run("tcp port take over", func(t *testing.T) { checkServiceTCPPortTakeOver(t, x, "u-tcptake-pg", 9611) })
	t.Run("grants go with the service", func(t *testing.T) { checkServiceIPGrantsCascade(t, x, "u-grants-pg") })
	t.Run("ip grants", func(t *testing.T) { checkIPGrants(t, x, "u-ipgrants-pg") })
	t.Run("ip grants capped", func(t *testing.T) { checkIPGrantsCapped(t, x, "u-grantcap-pg") })
	t.Run("migration 0041 slugs and token name", func(t *testing.T) {
		checkMigration0041(t, x, "u-pg-tcpslug", "0041_v0.13.0_tcp_forwards.postgres.sql")
	})
	t.Run("migration 0040 down and up", func(t *testing.T) {
		checkMigration0029DownAndUp(t, x, "0040_v0.13.0_tcp_services.postgres.sql")
	})
}
