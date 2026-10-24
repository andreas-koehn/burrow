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
	t.Run("tcp settings", func(t *testing.T) { checkServiceTCPSettings(t, x, "u-tcpset-pg") })
	t.Run("tcp hostname", func(t *testing.T) { checkServiceTCPHostname(t, x, "u-tcphost-pg") })
	t.Run("host ownership", func(t *testing.T) { checkHostOwnership(t, x, "u-hostown-pg") })
	t.Run("host ownership race", func(t *testing.T) { checkHostOwnershipRace(t, x, "u-hostrace-pg") })
	t.Run("custom domain covers", func(t *testing.T) { checkCustomDomainCovers(t, x, "u-covers-pg") })
	t.Run("tcp complete tables", func(t *testing.T) { checkTCPCompleteTables(t, x, "u-tcptables-pg") })
	t.Run("tcp limits", func(t *testing.T) { checkServiceTCPLimits(t, x, "u-tcplim-pg") })
	t.Run("tcp bans and usage", func(t *testing.T) { checkTCPBansAndUsage(t, x, "u-tcpban-pg") })
	t.Run("tcp usage prune", func(t *testing.T) { checkPruneTCPDailyUsage(t, x, "u-tcpprune-pg") })
	t.Run("tcp hostname only", func(t *testing.T) { checkServiceTCPHostnameOnly(t, x, "u-hostonly-pg") })
	t.Run("http settings", func(t *testing.T) { checkServiceHTTPSettings(t, x, "u-httpset-pg") })
	t.Run("custom domain cert source", func(t *testing.T) { checkCustomDomainCertSource(t, x, "u-certsrc-pg") })
	t.Run("connection log grpc columns", func(t *testing.T) { checkConnectionLogGRPCColumns(t, x) })
	t.Run("migration 0045 down and up", func(t *testing.T) {
		checkMigration0029DownAndUp(t, x, "0045_v0.13.0_http2_grpc.postgres.sql")
	})
	t.Run("migration 0044 down and up", func(t *testing.T) {
		checkMigration0029DownAndUp(t, x, "0044_v0.13.0_tcp_hostname_only.postgres.sql")
	})
	t.Run("migration 0042 down and up", func(t *testing.T) {
		checkMigration0029DownAndUp(t, x, "0042_v0.13.0_tcp_complete.postgres.sql")
	})
	t.Run("migration 0040 down and up", func(t *testing.T) {
		checkMigration0029DownAndUp(t, x, "0040_v0.13.0_tcp_services.postgres.sql")
	})
}
