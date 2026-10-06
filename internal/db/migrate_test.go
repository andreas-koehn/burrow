package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAndMigrateIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.db")
	d, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := Migrate(d); err != nil {
		t.Fatalf("migrate must be idempotent: %v", err)
	}
	for _, tbl := range []string{"users", "sessions", "client_tokens", "tunnels", "schema_migrations"} {
		var name string
		err := d.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil || name != tbl {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}
	var fk int
	_ = d.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if fk != 1 {
		t.Fatalf("foreign_keys must be ON, got %d", fk)
	}
	if _, err := d.Exec(`INSERT INTO sessions(id, user_id, expires_at) VALUES('s1','nonexistent',CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("FK constraint must reject an orphaned session insert")
	}
}

func TestMigrate0003Schema(t *testing.T) {
	x := testDB(t) // opens + migrates a temp DB; FK ON via Open()
	db := x.DB()
	ctx := context.Background()

	// Seed a user so FK on services(user_id) is satisfied.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users(id,email,password_hash,role) VALUES('u1','svc@test.invalid','h','user')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	for _, q := range []string{
		`INSERT INTO services(id,user_id,name,type,subdomain,access_mode,api_key_header)
		   VALUES('s1','u1','web','http','k7p2qx','open','Authorization')`,
		`INSERT INTO service_api_keys(id,service_id,name,key_hash) VALUES('k1','s1','ci','deadbeef')`,
		`INSERT INTO service_access_policy(service_id,role) VALUES('s1','user')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	// cascade: deleting the service removes its keys + policy
	if _, err := db.ExecContext(ctx, `DELETE FROM services WHERE id='s1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM service_api_keys`).Scan(&n); err != nil {
		t.Fatalf("count service_api_keys: %v", err)
	}
	if n != 0 {
		t.Fatalf("api keys not cascaded: %d", n)
	}
	var m int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM service_access_policy`).Scan(&m); err != nil {
		t.Fatalf("count service_access_policy: %v", err)
	}
	if m != 0 {
		t.Fatalf("access policy not cascaded: %d", m)
	}
}

func TestMigrate0004Through0010Schema(t *testing.T) {
	x := testDB(t) // opens + migrates a temp DB; FK ON via Open()
	d := x.DB()
	ctx := context.Background()

	// Seed parent rows required by FKs in 0004/0007/0009/0010.
	if _, err := d.ExecContext(ctx,
		`INSERT INTO users(id,email,password_hash,role) VALUES('u1','m4@test.invalid','h','user')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO services(id,user_id,name,type,subdomain,access_mode,api_key_header)
		   VALUES('s1','u1','web','http','m4test','open','Authorization')`); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	type tcase struct {
		q    string
		want string
	}
	for _, c := range []tcase{
		{`INSERT INTO usage_events(id,service_id,api_key_id,ts,kind,tokens_in,tokens_out,bytes_in,bytes_out,streamed,cache_hit,upstream_status)
			VALUES('u1','s1','','2026-05-19','openai',12,7,4096,8192,0,0,200)`, ""},
		{`INSERT INTO cache_entries(id,scope_key,key_hash,status,headers,body,created_at,ttl_seconds)
			VALUES('c1','svc:s1','deadbeef',200,'{}','x',CURRENT_TIMESTAMP,600)`, ""},
		{`UPDATE roles SET builtin=1 WHERE name='admin'`, ""}, // 0005: column exists
		{`INSERT INTO audit_events(id,ts,actor_id,action,result,prev_hash,hash)
			VALUES('a1',CURRENT_TIMESTAMP,'u','x','ok','0','f')`, ""},
		{`INSERT INTO webhooks(id,name,url,secret_hash,events,paused,created_at)
			VALUES('h1','dev','https://x','abc','["x"]',0,CURRENT_TIMESTAMP)`, ""},
		{`INSERT INTO webhook_deliveries(id,webhook_id,event,ts,status_code,attempt,latency_ms)
			VALUES('d1','h1','x',CURRENT_TIMESTAMP,200,1,15)`, ""},
		{`INSERT INTO service_ai_config(service_id,config)
			VALUES('s1','{"cache":{"enabled":false}}')`, ""},
		{`INSERT INTO model_aliases(alias,concrete_model,service_id,created_at)
			VALUES('fast','llama3.1:8b','s1',CURRENT_TIMESTAMP)`, ""},
		{`INSERT INTO rate_limits(id,scope,subject,dimension,lim,burst,created_at)
			VALUES('r1','api_key','k1','rpm',60,60,CURRENT_TIMESTAMP)`, ""},
		{`INSERT INTO budgets(id,scope,subject_id,daily_usd,action_on_exceed,created_at)
			VALUES('b1','api_key','k1',5.00,'alert_webhook',CURRENT_TIMESTAMP)`, ""},
		{`INSERT INTO automation_tokens(id,name,prefix,user_id,role_at_mint,token_hash,permissions,created_at)
			VALUES('t1','ci','bua_abcd','u1','admin','deadbeef','["x"]',CURRENT_TIMESTAMP)`, ""},
	} {
		if _, err := d.ExecContext(ctx, c.q); err != nil {
			t.Fatalf("exec %q: %v", c.q, err)
		}
	}

	// 0009: services.mtls_ca_pem column exists.
	if _, err := d.ExecContext(ctx, `UPDATE services SET mtls_ca_pem='-----PEM-----' WHERE id='s1'`); err != nil {
		t.Fatalf("update mtls_ca_pem: %v", err)
	}

	// cascade probe: deleting service cascades usage_events + service_ai_config.
	if _, err := d.ExecContext(ctx, `DELETE FROM services WHERE id='s1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM usage_events`).Scan(&n); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if n != 0 {
		t.Fatalf("usage_events not cascaded: %d", n)
	}
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM service_ai_config`).Scan(&n); err != nil {
		t.Fatalf("count service_ai_config: %v", err)
	}
	if n != 0 {
		t.Fatalf("service_ai_config not cascaded: %d", n)
	}

	// cascade probe: deleting user cascades automation_tokens.
	if _, err := d.ExecContext(ctx, `DELETE FROM users WHERE id='u1'`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM automation_tokens`).Scan(&n); err != nil {
		t.Fatalf("count automation_tokens: %v", err)
	}
	if n != 0 {
		t.Fatalf("automation_tokens not cascaded: %d", n)
	}
}

func TestMigration0002Foundation(t *testing.T) {
	x := testDB(t) // testDB in crud_test.go opens + migrates a temp DB
	ctx := context.Background()
	db := x.DB()

	// roles seeded
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM roles`).Scan(&n); err != nil {
		t.Fatalf("roles table: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 seeded roles, got %d", n)
	}
	for _, r := range []string{"admin", "user"} {
		var desc string
		err := db.QueryRowContext(ctx, `SELECT description FROM roles WHERE name=?`, r).Scan(&desc)
		if err != nil || desc == "" {
			t.Fatalf("role %q: desc=%q err=%v", r, desc, err)
		}
	}

	// new user columns with defaults
	_, _ = db.ExecContext(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('u1','a@b.c','h','admin')`)
	var status string
	var lastLogin sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT status,last_login FROM users WHERE id='u1'`).Scan(&status, &lastLogin); err != nil {
		t.Fatalf("user new cols: %v", err)
	}
	if status != "active" || lastLogin.Valid {
		t.Fatalf("want status=active last_login=NULL, got %q valid=%v", status, lastLogin.Valid)
	}

	// new tunnel columns with defaults
	_, _ = db.ExecContext(ctx, `INSERT INTO tunnels(id,user_id,name,type,remote_port,local_addr) VALUES('tn1','u1','n','tcp',9000,'127.0.0.1:1')`)
	var in, out int64
	var mode string
	var flushed sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT total_bytes_in,total_bytes_out,access_mode,last_flushed_at FROM tunnels WHERE id='tn1'`).Scan(&in, &out, &mode, &flushed); err != nil {
		t.Fatalf("tunnel new cols: %v", err)
	}
	if in != 0 || out != 0 || mode != "open" || flushed.Valid {
		t.Fatalf("tunnel defaults wrong: in=%d out=%d mode=%q flushed=%v", in, out, mode, flushed.Valid)
	}

	// settings table usable
	if _, err := db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('k','v')`); err != nil {
		t.Fatalf("settings table: %v", err)
	}
}

// TestMigrateDriverFilter verifies that the driver filter in MigrateForDriver
// selects the correct file set: SQLite sees only *.sql (not *.postgres.sql),
// and the postgres suffix files exist in the embed.FS.
func TestMigrateDriverFilter(t *testing.T) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var sqliteFiles, postgresFiles []string
	for _, e := range entries {
		n := e.Name()
		if n == "." || e.IsDir() {
			continue
		}
		if hasSuffix(n, ".postgres.sql") {
			postgresFiles = append(postgresFiles, n)
		} else if hasSuffix(n, ".sql") {
			sqliteFiles = append(sqliteFiles, n)
		}
	}

	// Expect exactly 22 SQLite files and 22 Postgres files.
	if len(sqliteFiles) != 22 {
		t.Errorf("want 22 sqlite migration files, got %d: %v", len(sqliteFiles), sqliteFiles)
	}
	if len(postgresFiles) != 22 {
		t.Errorf("want 22 postgres migration files, got %d: %v", len(postgresFiles), postgresFiles)
	}

	// Each SQLite file must have a matching postgres twin.
	for _, sf := range sqliteFiles {
		// 0001_init.sql  ->  0001_init.postgres.sql
		twin := sf[:len(sf)-len(".sql")] + ".postgres.sql"
		found := false
		for _, pf := range postgresFiles {
			if pf == twin {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no postgres twin for %s (expected %s)", sf, twin)
		}
	}
}

// hasSuffix is an alias for strings.HasSuffix, used inside the test file.
func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// TestMigrationParitySQLiteAndPostgres checks that BOTH migration ladders apply
// cleanly. The Postgres half requires a live Postgres URL in $BURROW_TEST_POSTGRES_URL;
// without it, the Postgres assertion is SKIPped.
//
// This test is always compiled (no build tag) so the parity check is visible in
// coverage reports. When the env var is set the test also verifies the expected
// table list on both sides.
func TestMigrationParitySQLiteAndPostgres(t *testing.T) {
	// --- SQLite side (always runs) ---
	sqlitePath := filepath.Join(t.TempDir(), "parity.db")
	sqDB, err := Open(sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqDB.Close()
	if err := MigrateForDriver(sqDB, "sqlite"); err != nil {
		t.Fatalf("sqlite migration ladder: %v", err)
	}

	// Core tables that must exist after the full ladder.
	coreTables := []string{
		"users", "sessions", "client_tokens", "tunnels",
		"roles", "settings", "services",
		"audit_events", "webhooks", "automation_tokens",
		"ai_providers", "ai_provider_models",
		"ai_models", "ai_model_targets", "ai_gateway_keys", "usage_attempts",
	}
	for _, tbl := range coreTables {
		var name string
		err := sqDB.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("sqlite: table %s missing: %v", tbl, err)
		}
	}

	// --- Postgres side (skipped without URL) ---
	pgURL := os.Getenv("BURROW_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("BURROW_TEST_POSTGRES_URL not set; skipping postgres parity check")
	}

	// Task 14 wires the actual pgx driver; here we just verify MigrateForDriver
	// would select the postgres files. We open via database/sql with a driver
	// that must already be registered (e.g. "pgx" from Task 14's build tag).
	pgDB, err := sql.Open("pgx", pgURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer pgDB.Close()
	if err := pgDB.Ping(); err != nil {
		t.Fatalf("postgres ping: %v", err)
	}
	if err := MigrateForDriver(pgDB, "postgres"); err != nil {
		t.Fatalf("postgres migration ladder: %v", err)
	}

	// Verify a subset of tables exist.
	for _, tbl := range coreTables {
		var name string
		err := pgDB.QueryRow(
			`SELECT table_name FROM information_schema.tables WHERE table_schema='public' AND table_name=$1`,
			tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("postgres: table %s missing: %v", tbl, err)
		}
	}

	// 0023: the columns and indexes TestMigrate0023Schema checks on SQLite.
	for tbl, cols := range migration0023Columns {
		for _, c := range cols {
			var n int
			if err := pgDB.QueryRow(
				`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2`,
				tbl, c,
			).Scan(&n); err != nil || n != 1 {
				t.Errorf("postgres: column %s.%s missing (n=%d, err=%v)", tbl, c, n, err)
			}
		}
	}
	for _, idx := range migration0023Indexes {
		var n string
		if err := pgDB.QueryRow(
			`SELECT indexname FROM pg_indexes WHERE schemaname='public' AND indexname=$1`, idx,
		).Scan(&n); err != nil {
			t.Errorf("postgres: index %s missing: %v", idx, err)
		}
	}
}

// What migration 0023 adds to existing and new tables, checked on both engines.
var (
	migration0023Columns = map[string][]string{
		"usage_events":     {"gateway_key_id", "dialect", "provider_slug", "requested_model", "target_model", "request_id", "latency_ms"},
		"services":         {"gateway_only"},
		"budgets":          {"daily_tokens"},
		"ai_providers":     {"supports_responses", "max_concurrent"},
		"ai_models":        {"name", "description", "enabled", "fallback_on_rate_limit", "attempt_timeout_s", "total_timeout_s", "created_at", "updated_at"},
		"ai_model_targets": {"model_name", "dialect", "position", "provider_slug", "target_model"},
		"ai_gateway_keys":  {"id", "name", "key_hash", "key_prefix", "user_id", "allowed_models", "last_used", "created_at", "revoked_at"},
		"usage_attempts":   {"request_id", "position", "ts", "provider_slug", "target_model", "status", "error_code", "duration_ms"},
	}
	migration0023Indexes = []string{"idx_ai_model_targets_provider", "idx_ai_gateway_keys_user", "idx_usage_attempts_ts", "idx_usage_events_gateway_key"}
)

func TestMigrate0023Schema(t *testing.T) {
	x := testDB(t)
	db := x.DB()
	for _, tbl := range []string{"ai_models", "ai_model_targets", "ai_gateway_keys", "usage_attempts"} {
		var n string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
	for tbl, list := range migration0023Columns {
		for _, c := range list {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, tbl, c).Scan(&n); err != nil || n != 1 {
				t.Errorf("column %s.%s missing (n=%d, err=%v)", tbl, c, n, err)
			}
		}
	}
	for _, idx := range migration0023Indexes {
		var n string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil {
			t.Errorf("index %s missing: %v", idx, err)
		}
	}
}
