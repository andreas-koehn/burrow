package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerDefaultListen(t *testing.T) {
	// No env set: assert the compiled-in default is :7000.
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.Listen != ":7000" {
		t.Fatalf("default listen = %q", c.Listen)
	}
}

func TestServerEnvOverrideListen(t *testing.T) {
	// Prove env-override works on a still-existing field.
	t.Setenv("BURROW_LISTEN", ":7777")
	c2, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer (env override): %v", err)
	}
	if c2.Listen != ":7777" {
		t.Fatalf("env override failed: got %q, want :7777", c2.Listen)
	}
}

func TestServerNoTokenRequired(t *testing.T) {
	// Server no longer requires an auth token — auth is DB-backed (Phase 4).
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("expected no error when BURROW_AUTH_TOKEN unset, got: %v", err)
	}
	if c.Listen != ":7000" {
		t.Fatalf("default listen = %q", c.Listen)
	}
	if c.DatabasePath != "./burrow.db" {
		t.Fatalf("default database_path = %q", c.DatabasePath)
	}
	if c.HTTPListen != ":8080" {
		t.Fatalf("default http_listen = %q", c.HTTPListen)
	}
}

func TestClientValidation(t *testing.T) {
	_, err := LoadClient(map[string]any{"server": "", "token": "x"})
	if err == nil {
		t.Fatal("expected error for empty server")
	}
	c, err := LoadClient(map[string]any{"server": "localhost:7000", "token": "x"})
	if err != nil || c.Server != "localhost:7000" {
		t.Fatalf("LoadClient: %+v err=%v", c, err)
	}
}

func TestServerPublicDefaults(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.PublicBind != "0.0.0.0" || c.PortMin != 9000 || c.PortMax != 9100 {
		t.Fatalf("got PublicBind=%q PortMin=%d PortMax=%d", c.PublicBind, c.PortMin, c.PortMax)
	}
}

func TestServerConfigPhase4(t *testing.T) {
	t.Setenv("BURROW_ADMIN_EMAIL", "a@x")
	t.Setenv("BURROW_ADMIN_PASSWORD", "pw")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.DatabasePath != "./burrow.db" || c.AdminEmail != "a@x" || c.AdminPassword != "pw" || c.HTTPListen != ":8080" {
		t.Fatalf("phase4 fields: %+v", c)
	}
}

func TestServerTLSRequired(t *testing.T) {
	if _, err := LoadServer(map[string]any{"tls_cert": ""}); err == nil {
		t.Fatal("empty tls_cert must fail validation")
	}
	if _, err := LoadServer(map[string]any{"tls_key": ""}); err == nil {
		t.Fatal("empty tls_key must fail validation")
	}
}

func TestServerHTTPSecureCookiesDefaultAndOverride(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.HTTPSecureCookies {
		t.Fatalf("default http_secure_cookies must be false, got true")
	}
	t.Setenv("BURROW_HTTP_SECURE_COOKIES", "true")
	c2, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if !c2.HTTPSecureCookies {
		t.Fatalf("env override BURROW_HTTP_SECURE_COOKIES=true not applied")
	}
}

func TestServerAIAllowPrivateUpstreamsDefaultAndOverride(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.AIAllowPrivateUpstreams {
		t.Fatalf("default ai_allow_private_upstreams must be false, got true")
	}
	t.Setenv("BURROW_AI_ALLOW_PRIVATE_UPSTREAMS", "true")
	c2, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if !c2.AIAllowPrivateUpstreams {
		t.Fatalf("env override BURROW_AI_ALLOW_PRIVATE_UPSTREAMS=true not applied")
	}
}

// The guard must not be switched off, or silently left on, by a value the
// operator mistyped: anything that is not a boolean stops the server.
func TestServerAIAllowPrivateUpstreamsValues(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "TRUE": true, "true": true, "": false, "0": false, "false": false} {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv("BURROW_AI_ALLOW_PRIVATE_UPSTREAMS", value)
			c, err := LoadServer(nil)
			if err != nil {
				t.Fatalf("LoadServer: %v", err)
			}
			if c.AIAllowPrivateUpstreams != want {
				t.Fatalf("ai_allow_private_upstreams = %v, want %v", c.AIAllowPrivateUpstreams, want)
			}
		})
	}
	for _, value := range []string{"yes", "on", "2", "enabled"} {
		t.Run("invalid="+value, func(t *testing.T) {
			t.Setenv("BURROW_AI_ALLOW_PRIVATE_UPSTREAMS", value)
			if c, err := LoadServer(nil); err == nil {
				t.Fatalf("LoadServer accepted %q (parsed as %v)", value, c.AIAllowPrivateUpstreams)
			}
		})
	}
}

// TestTrustedProxiesDefaultEmpty asserts that the default TrustedProxies is an
// empty slice (safe: no forwarded headers trusted).
func TestTrustedProxiesDefaultEmpty(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if len(c.TrustedProxies) != 0 {
		t.Fatalf("default trusted_proxies must be empty, got %v", c.TrustedProxies)
	}
}

// TestTrustedProxiesEnvParseCIDRList asserts that BURROW_TRUSTED_PROXIES
// (comma-separated) is correctly split and stored as a []string.
func TestTrustedProxiesEnvParseCIDRList(t *testing.T) {
	t.Setenv("BURROW_TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.1")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if len(c.TrustedProxies) != 2 {
		t.Fatalf("expected 2 entries, got %v", c.TrustedProxies)
	}
	if c.TrustedProxies[0] != "10.0.0.0/8" {
		t.Fatalf("first entry = %q, want 10.0.0.0/8", c.TrustedProxies[0])
	}
	if c.TrustedProxies[1] != "192.168.1.1" {
		t.Fatalf("second entry = %q, want 192.168.1.1", c.TrustedProxies[1])
	}
}

// TestTrustedProxiesOverrideParseCIDR asserts that an override map entry works.
func TestTrustedProxiesOverrideParseCIDR(t *testing.T) {
	c, err := LoadServer(map[string]any{"trusted_proxies": []string{"172.16.0.0/12"}})
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if len(c.TrustedProxies) != 1 || c.TrustedProxies[0] != "172.16.0.0/12" {
		t.Fatalf("expected [172.16.0.0/12], got %v", c.TrustedProxies)
	}
}

// TestTrustedProxiesInvalidCIDRError asserts that an unparseable entry causes
// LoadServer to return an error (fail-fast validation).
func TestTrustedProxiesInvalidCIDRError(t *testing.T) {
	_, err := LoadServer(map[string]any{"trusted_proxies": []string{"not-a-cidr"}})
	if err == nil {
		t.Fatal("expected error for invalid CIDR in trusted_proxies, got nil")
	}
	if !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("error should mention trusted_proxies, got: %v", err)
	}
}

// writeSecret writes content to a temp file and returns its path. The file is
// cleaned up automatically when the test ends.
func writeSecret(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "secret-*")
	if err != nil {
		t.Fatalf("create temp secret: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp secret: %v", err)
	}
	f.Close()
	return f.Name()
}

// TestServerAdminPasswordFile asserts that BURROW_ADMIN_PASSWORD_FILE is read
// and its trailing newline is stripped, leaving internal content intact.
func TestServerAdminPasswordFile(t *testing.T) {
	path := writeSecret(t, "s3cret\n")
	t.Setenv("BURROW_ADMIN_PASSWORD_FILE", path)

	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.AdminPassword != "s3cret" {
		t.Fatalf("admin_password = %q, want %q", c.AdminPassword, "s3cret")
	}
}

// TestServerAdminPasswordFileInternalSpacesPreserved proves that only the
// trailing newline is stripped; internal content (including spaces) is kept.
func TestServerAdminPasswordFileInternalSpacesPreserved(t *testing.T) {
	path := writeSecret(t, "pass word with spaces\n")
	t.Setenv("BURROW_ADMIN_PASSWORD_FILE", path)

	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.AdminPassword != "pass word with spaces" {
		t.Fatalf("admin_password = %q, want %q", c.AdminPassword, "pass word with spaces")
	}
}

// TestServerFilePrecedenceOverLiteralEnv asserts that when both BURROW_<KEY>
// and BURROW_<KEY>_FILE are set, the _FILE value wins (Docker convention).
func TestServerFilePrecedenceOverLiteralEnv(t *testing.T) {
	path := writeSecret(t, "from-file\n")
	t.Setenv("BURROW_ADMIN_PASSWORD", "from-literal")
	t.Setenv("BURROW_ADMIN_PASSWORD_FILE", path)

	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.AdminPassword != "from-file" {
		t.Fatalf("_FILE must win over literal env: got %q, want %q", c.AdminPassword, "from-file")
	}
}

// TestServerMissingFileReturnsError asserts that a BURROW_*_FILE pointing to a
// non-existent path causes LoadServer to return a hard error (fail-fast).
func TestServerMissingFileReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.txt")
	t.Setenv("BURROW_ADMIN_PASSWORD_FILE", missing)

	_, err := LoadServer(nil)
	if err == nil {
		t.Fatal("expected error for missing _FILE path, got nil")
	}
	if !strings.Contains(err.Error(), "cannot read secret file") {
		t.Fatalf("error should mention 'cannot read secret file', got: %v", err)
	}
}

// TestServerFileGenericSecondKey proves genericity: a second BURROW_*_FILE
// variable (BURROW_ADMIN_EMAIL_FILE) is also resolved correctly.
func TestServerFileGenericSecondKey(t *testing.T) {
	emailPath := writeSecret(t, "file@example.com\n")
	pwPath := writeSecret(t, "filepw\n")
	t.Setenv("BURROW_ADMIN_EMAIL_FILE", emailPath)
	t.Setenv("BURROW_ADMIN_PASSWORD_FILE", pwPath)

	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.AdminEmail != "file@example.com" {
		t.Fatalf("admin_email = %q, want %q", c.AdminEmail, "file@example.com")
	}
	if c.AdminPassword != "filepw" {
		t.Fatalf("admin_password = %q, want %q", c.AdminPassword, "filepw")
	}
}

// TestServerOverridesStillBeatFile asserts that explicit programmatic overrides
// win over _FILE values (final precedence: defaults<litenv<_FILE<overrides).
func TestServerOverridesStillBeatFile(t *testing.T) {
	path := writeSecret(t, "from-file\n")
	t.Setenv("BURROW_ADMIN_PASSWORD_FILE", path)

	c, err := LoadServer(map[string]any{"admin_password": "from-override"})
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.AdminPassword != "from-override" {
		t.Fatalf("override must beat _FILE: got %q, want %q", c.AdminPassword, "from-override")
	}
}

// TestClientTokenFile asserts that BURROW_TOKEN_FILE is resolved for the
// client config path (proving _FILE works in LoadClient too).
func TestClientTokenFile(t *testing.T) {
	path := writeSecret(t, "bur_tok123\n")
	t.Setenv("BURROW_TOKEN_FILE", path)

	c, err := LoadClient(map[string]any{"server": "localhost:7000"})
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if c.Token != "bur_tok123" {
		t.Fatalf("token = %q, want %q", c.Token, "bur_tok123")
	}
}

// TestHTTPTLSBothSetIsValid asserts that providing both http_tls_cert and
// http_tls_key via overrides is accepted by LoadServer.
func TestHTTPTLSBothSetIsValid(t *testing.T) {
	c, err := LoadServer(map[string]any{
		"http_tls_cert": "/tmp/cert.pem",
		"http_tls_key":  "/tmp/key.pem",
	})
	if err != nil {
		t.Fatalf("both http_tls_cert+key set: expected no error, got %v", err)
	}
	if c.HTTPTLSCert != "/tmp/cert.pem" {
		t.Fatalf("http_tls_cert = %q, want /tmp/cert.pem", c.HTTPTLSCert)
	}
	if c.HTTPTLSKey != "/tmp/key.pem" {
		t.Fatalf("http_tls_key = %q, want /tmp/key.pem", c.HTTPTLSKey)
	}
}

// TestHTTPTLSBothEmptyIsValid asserts that the default (both empty) is accepted.
func TestHTTPTLSBothEmptyIsValid(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("both empty: expected no error, got %v", err)
	}
	if c.HTTPTLSCert != "" || c.HTTPTLSKey != "" {
		t.Fatalf("default http_tls_cert/key must be empty, got %q / %q", c.HTTPTLSCert, c.HTTPTLSKey)
	}
}

// TestHTTPTLSOnlyCertSetIsError asserts that setting only http_tls_cert (xor) fails.
func TestHTTPTLSOnlyCertSetIsError(t *testing.T) {
	_, err := LoadServer(map[string]any{
		"http_tls_cert": "/tmp/cert.pem",
	})
	if err == nil {
		t.Fatal("only http_tls_cert set: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "http_tls_cert") {
		t.Fatalf("error should mention http_tls_cert, got: %v", err)
	}
}

// TestHTTPTLSOnlyKeySetIsError asserts that setting only http_tls_key (xor) fails.
func TestHTTPTLSOnlyKeySetIsError(t *testing.T) {
	_, err := LoadServer(map[string]any{
		"http_tls_key": "/tmp/key.pem",
	})
	if err == nil {
		t.Fatal("only http_tls_key set: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "http_tls_cert") {
		t.Fatalf("error should mention http_tls_cert, got: %v", err)
	}
}

// TestHTTPTLSEnvVars asserts that BURROW_HTTP_TLS_CERT/KEY env vars are loaded.
func TestHTTPTLSEnvVars(t *testing.T) {
	t.Setenv("BURROW_HTTP_TLS_CERT", "/env/cert.pem")
	t.Setenv("BURROW_HTTP_TLS_KEY", "/env/key.pem")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("env vars: expected no error, got %v", err)
	}
	if c.HTTPTLSCert != "/env/cert.pem" {
		t.Fatalf("http_tls_cert = %q, want /env/cert.pem", c.HTTPTLSCert)
	}
	if c.HTTPTLSKey != "/env/key.pem" {
		t.Fatalf("http_tls_key = %q, want /env/key.pem", c.HTTPTLSKey)
	}
}

// TestClientMissingFileReturnsError asserts fail-fast behavior in LoadClient.
func TestClientMissingFileReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-secret.txt")
	t.Setenv("BURROW_TOKEN_FILE", missing)

	_, err := LoadClient(map[string]any{"server": "localhost:7000"})
	if err == nil {
		t.Fatal("expected error for missing _FILE in LoadClient, got nil")
	}
	if !strings.Contains(err.Error(), "cannot read secret file") {
		t.Fatalf("error should mention 'cannot read secret file', got: %v", err)
	}
}

func TestServerConfigSMTPPassword(t *testing.T) {
	t.Setenv("BURROW_SMTP_PASSWORD", "s3cr3t")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.SMTPPassword != "s3cr3t" {
		t.Fatalf("SMTPPassword = %q, want s3cr3t", c.SMTPPassword)
	}
}

// ---------------------------------------------------------------------------
// Task 11: proxy listener, proxy TLS pair, auth domain
// ---------------------------------------------------------------------------

// TestHTTPProxyListenDefault asserts that the default http_proxy_listen is
// empty: the host-routed ingress is opt-in.
func TestHTTPProxyListenDefault(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.HTTPProxyListen != "" {
		t.Fatalf("HTTPProxyListen default = %q, want empty (host routing is opt-in)", c.HTTPProxyListen)
	}
}

// TestHTTPProxyListenEnvOverride asserts that BURROW_HTTP_PROXY_LISTEN overrides the default.
func TestHTTPProxyListenEnvOverride(t *testing.T) {
	t.Setenv("BURROW_HTTP_PROXY_LISTEN", ":9443")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.HTTPProxyListen != ":9443" {
		t.Fatalf("env override http_proxy_listen = %q, want :9443", c.HTTPProxyListen)
	}
}

// TestHTTPProxyTLSDefaultsEmpty asserts that http_proxy_tls_cert and
// http_proxy_tls_key default to empty strings.
func TestHTTPProxyTLSDefaultsEmpty(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.HTTPProxyTLSCert != "" || c.HTTPProxyTLSKey != "" {
		t.Fatalf("default proxy tls cert/key must be empty, got %q / %q",
			c.HTTPProxyTLSCert, c.HTTPProxyTLSKey)
	}
}

// TestAuthDomainDefault asserts that auth_domain defaults to "".
func TestAuthDomainDefault(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.AuthDomain != "" {
		t.Fatalf("default auth_domain = %q, want empty", c.AuthDomain)
	}
}

// TestAuthDomainEnvOverride asserts that BURROW_AUTH_DOMAIN overrides the default.
func TestAuthDomainEnvOverride(t *testing.T) {
	t.Setenv("BURROW_AUTH_DOMAIN", "tunnels.example.com")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.AuthDomain != "tunnels.example.com" {
		t.Fatalf("env override auth_domain = %q, want tunnels.example.com", c.AuthDomain)
	}
}

// TestHTTPProxyTLSBothSetIsValid asserts that providing both http_proxy_tls_cert and
// http_proxy_tls_key via overrides is accepted by LoadServer.
func TestHTTPProxyTLSBothSetIsValid(t *testing.T) {
	c, err := LoadServer(map[string]any{
		"http_proxy_tls_cert": "/tmp/proxy-cert.pem",
		"http_proxy_tls_key":  "/tmp/proxy-key.pem",
	})
	if err != nil {
		t.Fatalf("both proxy tls cert+key set: expected no error, got %v", err)
	}
	if c.HTTPProxyTLSCert != "/tmp/proxy-cert.pem" {
		t.Fatalf("http_proxy_tls_cert = %q, want /tmp/proxy-cert.pem", c.HTTPProxyTLSCert)
	}
	if c.HTTPProxyTLSKey != "/tmp/proxy-key.pem" {
		t.Fatalf("http_proxy_tls_key = %q, want /tmp/proxy-key.pem", c.HTTPProxyTLSKey)
	}
}

// TestHTTPProxyTLSBothEmptyIsValid asserts that the default (both empty) is accepted.
func TestHTTPProxyTLSBothEmptyIsValid(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("both empty: expected no error, got %v", err)
	}
	if c.HTTPProxyTLSCert != "" || c.HTTPProxyTLSKey != "" {
		t.Fatalf("default proxy tls cert/key must be empty, got %q / %q",
			c.HTTPProxyTLSCert, c.HTTPProxyTLSKey)
	}
}

// TestHTTPProxyTLSOnlyCertSetIsError asserts that setting only http_proxy_tls_cert (xor) fails.
func TestHTTPProxyTLSOnlyCertSetIsError(t *testing.T) {
	_, err := LoadServer(map[string]any{
		"http_proxy_tls_cert": "/tmp/proxy-cert.pem",
	})
	if err == nil {
		t.Fatal("only http_proxy_tls_cert set: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "http_proxy_tls_cert") {
		t.Fatalf("error should mention http_proxy_tls_cert, got: %v", err)
	}
}

// TestHTTPProxyTLSOnlyKeySetIsError asserts that setting only http_proxy_tls_key (xor) fails.
func TestHTTPProxyTLSOnlyKeySetIsError(t *testing.T) {
	_, err := LoadServer(map[string]any{
		"http_proxy_tls_key": "/tmp/proxy-key.pem",
	})
	if err == nil {
		t.Fatal("only http_proxy_tls_key set: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "http_proxy_tls_cert") {
		t.Fatalf("error should mention http_proxy_tls_cert, got: %v", err)
	}
}

// TestHTTPProxyTLSEnvVars asserts that BURROW_HTTP_PROXY_TLS_CERT/KEY env vars are loaded.
func TestHTTPProxyTLSEnvVars(t *testing.T) {
	t.Setenv("BURROW_HTTP_PROXY_TLS_CERT", "/env/proxy-cert.pem")
	t.Setenv("BURROW_HTTP_PROXY_TLS_KEY", "/env/proxy-key.pem")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("env vars: expected no error, got %v", err)
	}
	if c.HTTPProxyTLSCert != "/env/proxy-cert.pem" {
		t.Fatalf("http_proxy_tls_cert = %q, want /env/proxy-cert.pem", c.HTTPProxyTLSCert)
	}
	if c.HTTPProxyTLSKey != "/env/proxy-key.pem" {
		t.Fatalf("http_proxy_tls_key = %q, want /env/proxy-key.pem", c.HTTPProxyTLSKey)
	}
}

// TestHTTPProxyTLSCertFile asserts that BURROW_HTTP_PROXY_TLS_CERT_FILE is resolved
// via the generic applyFileSecrets mechanism.
func TestHTTPProxyTLSCertKeyFile(t *testing.T) {
	certPath := writeSecret(t, "/path/to/proxy-cert.pem\n")
	keyPath := writeSecret(t, "/path/to/proxy-key.pem\n")
	t.Setenv("BURROW_HTTP_PROXY_TLS_CERT_FILE", certPath)
	t.Setenv("BURROW_HTTP_PROXY_TLS_KEY_FILE", keyPath)

	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer with _FILE secrets: %v", err)
	}
	if c.HTTPProxyTLSCert != "/path/to/proxy-cert.pem" {
		t.Fatalf("http_proxy_tls_cert from _FILE = %q, want /path/to/proxy-cert.pem", c.HTTPProxyTLSCert)
	}
	if c.HTTPProxyTLSKey != "/path/to/proxy-key.pem" {
		t.Fatalf("http_proxy_tls_key from _FILE = %q, want /path/to/proxy-key.pem", c.HTTPProxyTLSKey)
	}
}

// ---------------------------------------------------------------------------
// Task 24 (v0.4.0): MCP listener, geo db, pricing override, backup dir, MCP
// token — defaults + env + _FILE + validation.
// ---------------------------------------------------------------------------

// TestV04DefaultsAllNewFields asserts every new v0.4.0 ServerConfig field
// has the documented default when no env / overrides are set.
//
// Defaults (Task 24):
//
//	MCPListen        ""     (MCP listener disabled)
//	GeoDBPath        ""     (NoopGeoLookup)
//	PricingPath      ""     (embedded pricing.yaml)
//	BackupDir        "./burrow.db.backups"  (derived from default DatabasePath)
//	BurrowMCPToken   ""     (empty; falls back to automation_tokens lookup)
func TestV04DefaultsAllNewFields(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.MCPListen != "" {
		t.Errorf("default mcp_listen = %q, want \"\"", c.MCPListen)
	}
	if c.GeoDBPath != "" {
		t.Errorf("default geo_db_path = %q, want \"\"", c.GeoDBPath)
	}
	if c.PricingPath != "" {
		t.Errorf("default pricing_path = %q, want \"\"", c.PricingPath)
	}
	if c.BackupDir != "./burrow.db.backups" {
		t.Errorf("default backup_dir = %q, want \"./burrow.db.backups\"", c.BackupDir)
	}
	if c.BurrowMCPToken != "" {
		t.Errorf("default burrow_mcp_token = %q, want \"\"", c.BurrowMCPToken)
	}
}

// TestMCPListenEnvOverride asserts BURROW_MCP_LISTEN takes effect.
func TestMCPListenEnvOverride(t *testing.T) {
	t.Setenv("BURROW_MCP_LISTEN", ":7800")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.MCPListen != ":7800" {
		t.Fatalf("mcp_listen = %q, want :7800", c.MCPListen)
	}
}

// TestGeoDBPathEnvOverride asserts BURROW_GEO_DB_PATH takes effect.
func TestGeoDBPathEnvOverride(t *testing.T) {
	t.Setenv("BURROW_GEO_DB_PATH", "/var/lib/burrow/GeoLite2.mmdb")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.GeoDBPath != "/var/lib/burrow/GeoLite2.mmdb" {
		t.Fatalf("geo_db_path = %q, want /var/lib/burrow/GeoLite2.mmdb", c.GeoDBPath)
	}
}

// TestPricingPathEnvOverride asserts BURROW_PRICING_PATH takes effect.
func TestPricingPathEnvOverride(t *testing.T) {
	t.Setenv("BURROW_PRICING_PATH", "/etc/burrow/pricing.yaml")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.PricingPath != "/etc/burrow/pricing.yaml" {
		t.Fatalf("pricing_path = %q, want /etc/burrow/pricing.yaml", c.PricingPath)
	}
}

// TestMCPListenInvalidPort asserts that mcp_listen=":abc" fails validation
// (port must parse).
func TestMCPListenInvalidPort(t *testing.T) {
	_, err := LoadServer(map[string]any{"mcp_listen": ":abc"})
	if err == nil {
		t.Fatal("expected error for mcp_listen with non-numeric port, got nil")
	}
	if !strings.Contains(err.Error(), "mcp_listen") {
		t.Fatalf("error should mention mcp_listen, got: %v", err)
	}
}

// TestMCPListenEmptyHostValid asserts that mcp_listen=":7800" (empty host)
// is accepted — common bind-all form.
func TestMCPListenEmptyHostValid(t *testing.T) {
	c, err := LoadServer(map[string]any{"mcp_listen": ":7800"})
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.MCPListen != ":7800" {
		t.Fatalf("mcp_listen = %q, want :7800", c.MCPListen)
	}
}

// TestMCPTokenFile asserts that BURROW_MCP_TOKEN_FILE is read via the
// generic applyFileSecrets mechanism into BurrowMCPToken.
func TestMCPTokenFile(t *testing.T) {
	path := writeSecret(t, "bua_some_token_value\n")
	t.Setenv("BURROW_MCP_TOKEN_FILE", path)
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.BurrowMCPToken != "bua_some_token_value" {
		t.Fatalf("burrow_mcp_token from _FILE = %q, want bua_some_token_value", c.BurrowMCPToken)
	}
}

// TestPricingPathFile asserts that BURROW_PRICING_PATH_FILE is resolved.
// This is the unusual "_FILE points to a path containing a path" variant —
// included for consistency with the generic _FILE pattern.
func TestPricingPathFile(t *testing.T) {
	path := writeSecret(t, "/etc/burrow/pricing.yaml\n")
	t.Setenv("BURROW_PRICING_PATH_FILE", path)
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.PricingPath != "/etc/burrow/pricing.yaml" {
		t.Fatalf("pricing_path from _FILE = %q, want /etc/burrow/pricing.yaml", c.PricingPath)
	}
}

// TestBackupDirDerivedFromDatabasePath asserts that BackupDir defaults to
// "<DatabasePath>.backups" when no explicit override is set.
func TestBackupDirDerivedFromDatabasePath(t *testing.T) {
	t.Setenv("BURROW_DATABASE_PATH", "/var/lib/burrow/main.db")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.BackupDir != "/var/lib/burrow/main.db.backups" {
		t.Fatalf("backup_dir derived = %q, want /var/lib/burrow/main.db.backups", c.BackupDir)
	}
}

// TestBackupDirExplicitOverride asserts that an explicit backup_dir override
// wins over the derived default.
func TestBackupDirExplicitOverride(t *testing.T) {
	c, err := LoadServer(map[string]any{"backup_dir": "/srv/backups"})
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.BackupDir != "/srv/backups" {
		t.Fatalf("backup_dir override = %q, want /srv/backups", c.BackupDir)
	}
}

// ---------------------------------------------------------------------------
// v0.5.0 Task 15: database_url + experimental.postgres_backend validation
// ---------------------------------------------------------------------------

// TestDatabaseURLDefaultEmpty asserts that database_url defaults to "".
func TestDatabaseURLDefaultEmpty(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.DatabaseURL != "" {
		t.Fatalf("default database_url = %q, want empty", c.DatabaseURL)
	}
}

// TestExperimentalPostgresDefaultFalse asserts that experimental.postgres_backend
// defaults to false.
func TestExperimentalPostgresDefaultFalse(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.ExperimentalPostgres {
		t.Fatal("default experimental.postgres_backend must be false, got true")
	}
}

// TestConfigRejectsBothDatabasePathAndURL asserts that setting both
// database_path and database_url returns a fatal error.
func TestConfigRejectsBothDatabasePathAndURL(t *testing.T) {
	_, err := LoadServer(map[string]any{
		"database_path":                 "/x/burrow.db",
		"database_url":                  "postgres://user:pass@host/db",
		"experimental.postgres_backend": true,
	})
	if err == nil {
		t.Fatal("expected error when both database_path and database_url are set, got nil")
	}
	if !strings.Contains(err.Error(), "database_url") || !strings.Contains(err.Error(), "database_path") {
		t.Fatalf("error should mention both database_url and database_path, got: %v", err)
	}
}

// TestConfigRequiresExperimentalFlagForPostgres asserts that setting database_url
// without experimental_postgres_backend=true returns a fatal error.
func TestConfigRequiresExperimentalFlagForPostgres(t *testing.T) {
	_, err := LoadServer(map[string]any{
		"database_path": "",
		"database_url":  "postgres://user:pass@host/db",
		// experimental_postgres_backend intentionally NOT set (defaults to false).
	})
	if err == nil {
		t.Fatal("expected error when database_url set without experimental flag, got nil")
	}
	if !strings.Contains(err.Error(), "experimental_postgres_backend") {
		t.Fatalf("error should mention experimental_postgres_backend, got: %v", err)
	}
}

// TestConfigDatabaseURLEnvVar asserts that BURROW_DATABASE_URL is loaded.
func TestConfigDatabaseURLEnvVar(t *testing.T) {
	t.Setenv("BURROW_DATABASE_URL", "postgres://u:p@host/db")
	t.Setenv("BURROW_DATABASE_PATH", "")
	t.Setenv("BURROW_EXPERIMENTAL_POSTGRES_BACKEND", "true")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer with database_url env: %v", err)
	}
	if c.DatabaseURL != "postgres://u:p@host/db" {
		t.Fatalf("database_url = %q, want postgres://u:p@host/db", c.DatabaseURL)
	}
	if !c.ExperimentalPostgres {
		t.Fatal("experimental_postgres_backend must be true after BURROW_EXPERIMENTAL_POSTGRES_BACKEND=true")
	}
}

// ---------------------------------------------------------------------------
// v0.6.0: ACME built-in Let's Encrypt config keys + validation
// ---------------------------------------------------------------------------

func TestLoadServer_ACMEEnablesAndValidates(t *testing.T) {
	t.Setenv("BURROW_ACME_DOMAIN", "burrow.example.com")
	t.Setenv("BURROW_ACME_EMAIL", "admin@example.com")
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.ACMEDomain != "burrow.example.com" || c.ACMEEmail != "admin@example.com" {
		t.Fatalf("acme fields not parsed: %+v", c)
	}
	if c.ACMECA == "" {
		t.Fatalf("ACMECA should default to a non-empty CA directory")
	}
	if c.ACMEStorage == "" {
		t.Fatalf("ACMEStorage should derive a default path")
	}
}

func TestLoadServer_ACMEDomainRequiresEmail(t *testing.T) {
	t.Setenv("BURROW_ACME_DOMAIN", "burrow.example.com")
	if _, err := LoadServer(nil); err == nil {
		t.Fatal("expected error when ACME_DOMAIN set without ACME_EMAIL")
	}
}

func TestLoadServer_ACMEConflictsWithFileCerts(t *testing.T) {
	t.Setenv("BURROW_ACME_DOMAIN", "burrow.example.com")
	t.Setenv("BURROW_ACME_EMAIL", "admin@example.com")
	t.Setenv("BURROW_TLS_CERT", "certs/x.pem")
	t.Setenv("BURROW_TLS_KEY", "certs/x-key.pem")
	if _, err := LoadServer(nil); err == nil {
		t.Fatal("expected error when ACME and file certs are both configured")
	}
}

func TestLoadServer_ACMEOffPreservesDefaults(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.ACMEDomain != "" {
		t.Fatalf("ACME should be off by default, got %q", c.ACMEDomain)
	}
}

// TestMinClientVersion: empty by default, set from BURROW_MIN_CLIENT_VERSION,
// and refused at start when it is not a version a client can compare with.
func TestMinClientVersion(t *testing.T) {
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.MinClientVersion != "" {
		t.Fatalf("min_client_version default = %q, want empty", c.MinClientVersion)
	}
	for _, v := range []string{"0.6.0", "v0.6.0", " 1.10.2 "} {
		t.Setenv("BURROW_MIN_CLIENT_VERSION", v)
		c, err := LoadServer(nil)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if want := strings.TrimSpace(v); c.MinClientVersion != want {
			t.Fatalf("min_client_version = %q, want %q", c.MinClientVersion, want)
		}
	}
	for _, v := range []string{"0.6", "latest", "v1.2.3-rc1", "1.2.3.4", "1..3"} {
		t.Setenv("BURROW_MIN_CLIENT_VERSION", v)
		if _, err := LoadServer(nil); err == nil || !strings.Contains(err.Error(), "min_client_version") {
			t.Fatalf("%q: err = %v, want one naming min_client_version", v, err)
		}
	}
}

// TestClientDownloadSettings: the base defaults to this repository's
// releases, is trimmed, and is refused at start when it is not a plain
// http(s) URL; the directory is empty by default and must exist when set.
func TestClientDownloadSettings(t *testing.T) {
	const def = "https://github.com/andreas-koehn/burrow/releases/download"
	c, err := LoadServer(nil)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if c.ClientDownloadBase != def {
		t.Fatalf("client_download_base default = %q, want %q", c.ClientDownloadBase, def)
	}
	if c.ClientDownloadDir != "" {
		t.Fatalf("client_download_dir default = %q, want empty", c.ClientDownloadDir)
	}
	for in, want := range map[string]string{
		" https://mirror.example.com/burrow/ ": "https://mirror.example.com/burrow",
		"http://assets:8080/dl":                "http://assets:8080/dl",
		"":                                     def,
	} {
		t.Setenv("BURROW_CLIENT_DOWNLOAD_BASE", in)
		c, err := LoadServer(nil)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if c.ClientDownloadBase != want {
			t.Fatalf("client_download_base %q = %q, want %q", in, c.ClientDownloadBase, want)
		}
	}
	for _, in := range []string{"ftp://x/y", "mirror.example.com", "https://u:p@x/y", "https://x/y?z", "https://x/$(id)", "https://x/a b", "https://x/\"", "javascript:alert(1)"} {
		t.Setenv("BURROW_CLIENT_DOWNLOAD_BASE", in)
		_, err := LoadServer(nil)
		if err == nil || !strings.Contains(err.Error(), "client_download_base") {
			t.Fatalf("%q: err = %v, want one naming client_download_base", in, err)
		}
		// The message must not carry a value that may hold credentials.
		if strings.Contains(err.Error(), "u:p@") {
			t.Fatalf("the error repeats the value: %v", err)
		}
	}
	t.Setenv("BURROW_CLIENT_DOWNLOAD_BASE", "")

	dir := t.TempDir()
	t.Setenv("BURROW_CLIENT_DOWNLOAD_DIR", dir)
	c, err = LoadServer(nil)
	if err != nil {
		t.Fatalf("existing directory: %v", err)
	}
	if c.ClientDownloadDir != dir {
		t.Fatalf("client_download_dir = %q, want %q", c.ClientDownloadDir, dir)
	}
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{filepath.Join(dir, "missing"), file} {
		t.Setenv("BURROW_CLIENT_DOWNLOAD_DIR", in)
		if _, err := LoadServer(nil); err == nil || !strings.Contains(err.Error(), "client_download_dir") {
			t.Fatalf("%q: err = %v, want one naming client_download_dir", in, err)
		}
	}
}
