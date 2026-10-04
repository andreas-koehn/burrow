// Package config loads server/client configuration: defaults < env < _FILE env < overrides.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"
)

var validate = validator.New()

// TunnelSpec is one tunnel the client asks the server to register.
type TunnelSpec struct {
	Name       string `koanf:"name"`
	LocalAddr  string `koanf:"local_addr" validate:"required,hostname_port"`
	RemotePort int    `koanf:"remote_port" validate:"gte=0,lte=65535"`
}

// ServerConfig configures burrowd.
type ServerConfig struct {
	Listen            string `koanf:"listen" validate:"required"`
	TLSCert           string `koanf:"tls_cert" validate:"required"`
	TLSKey            string `koanf:"tls_key" validate:"required"`
	LogLevel          string `koanf:"log_level"`
	LogFormat         string `koanf:"log_format"`
	PublicBind        string `koanf:"public_bind"`
	PortMin           int    `koanf:"port_min" validate:"gte=1,lte=65535"`
	PortMax           int    `koanf:"port_max" validate:"gte=1,lte=65535,gtefield=PortMin"`
	DatabasePath      string `koanf:"database_path"`
	AdminEmail        string `koanf:"admin_email"`
	AdminPassword     string `koanf:"admin_password"`
	HTTPListen        string `koanf:"http_listen"`
	HTTPSecureCookies bool   `koanf:"http_secure_cookies"`
	// HTTPTLSCert and HTTPTLSKey control native TLS for the HTTP API/dashboard.
	// Both must be set together (both-or-neither); if exactly one is set,
	// LoadServer returns an error. When both are set the HTTP server is served
	// over HTTPS using these certificates (distinct from the control-plane
	// TLSCert/TLSKey); when both are empty (the default) the server serves
	// plain HTTP and the operator is expected to terminate TLS at a proxy.
	// Env: BURROW_HTTP_TLS_CERT / BURROW_HTTP_TLS_KEY (also _FILE variants).
	HTTPTLSCert string `koanf:"http_tls_cert"`
	HTTPTLSKey  string `koanf:"http_tls_key"`
	// TrustedProxies is the list of CIDRs or IP addresses of reverse proxies
	// whose X-Forwarded-For / X-Real-IP headers the server will honor.
	//
	// Empty (default) means NO forwarded headers are trusted: the raw TCP peer
	// address is always used as the client IP. This is the safe default for a
	// direct-internet deployment and prevents XFF spoofing from bypassing the
	// per-IP login rate-limiter or poisoning session IP records.
	//
	// Non-empty: the server honors forwarded headers ONLY when the immediate
	// TCP peer IP is within one of the listed CIDRs. Set to your load-balancer
	// or proxy IP/CIDR (e.g. "10.0.0.0/8") when deploying behind a proxy.
	// Env: BURROW_TRUSTED_PROXIES (comma-separated CIDRs/IPs).
	TrustedProxies []string `koanf:"trusted_proxies"`
	// SMTPPassword is the SMTP auth password for the dashboard mailer. It is a
	// SECRET: sourced from BURROW_SMTP_PASSWORD or BURROW_SMTP_PASSWORD_FILE
	// (the _FILE form wins, handled by applyFileSecrets) and is NEVER written
	// to the settings table. The non-secret SMTP fields (host/port/username/
	// from/tls) live in the settings table instead. Empty = SMTP unconfigured.
	// Env: BURROW_SMTP_PASSWORD (also _FILE variant).
	SMTPPassword string `koanf:"smtp_password"`
	// HTTPProxyListen is the TCP address of the host-routed ingress
	// (<slug>.<auth_domain>). Empty (the default) leaves it off: services are
	// reached by path at https://<auth_domain>/svc/<slug>/. Setting it needs
	// wildcard DNS and a wildcard certificate.
	// Env: BURROW_HTTP_PROXY_LISTEN.
	HTTPProxyListen string `koanf:"http_proxy_listen"`
	// HTTPProxyTLSCert and HTTPProxyTLSKey are the TLS certificate and key files
	// for the HTTP reverse-proxy listener. Both must be set together
	// (both-or-neither); if exactly one is set, LoadServer returns an error.
	// When both are empty (the default) the proxy listener runs without TLS and
	// the operator is expected to terminate TLS upstream.
	// Env: BURROW_HTTP_PROXY_TLS_CERT / BURROW_HTTP_PROXY_TLS_KEY (also _FILE variants).
	HTTPProxyTLSCert string `koanf:"http_proxy_tls_cert"`
	HTTPProxyTLSKey  string `koanf:"http_proxy_tls_key"`
	// AuthDomain is the domain used as the authentication boundary for the HTTP
	// reverse proxy (e.g. "tunnels.example.com"). Empty by default; Task 12 will
	// use this to construct per-tunnel subdomains.
	// Env: BURROW_AUTH_DOMAIN.
	AuthDomain string `koanf:"auth_domain"`

	// ----- v0.4.0 additions (Task 24) ---------------------------------------

	// MCPListen is the TCP address the optional `burrowd mcp` JSON-RPC listener
	// binds to (spec Part P). Defaults to "" — listener disabled. ":7800" is
	// the conventional bind-all form. Env: BURROW_MCP_LISTEN.
	MCPListen string `koanf:"mcp_listen"`
	// BurrowMCPToken is the plaintext bua_-prefixed bearer token the MCP
	// listener authenticates with. Empty (default) means the listener falls
	// back to bearer-token lookup against the automation_tokens table. SECRET
	// — sourced from BURROW_MCP_TOKEN or BURROW_MCP_TOKEN_FILE (the _FILE form
	// wins, handled by applyFileSecrets) and never written to the settings
	// table. Env: BURROW_MCP_TOKEN (also _FILE variant).
	//
	// koanf key is "mcp_token" — derived from BURROW_MCP_TOKEN via the
	// prefix-strip + lowercase rule. The Go field name keeps the "Burrow"
	// stem to match the plan's Type contract.
	BurrowMCPToken string `koanf:"mcp_token"`
	// GeoDBPath is the on-disk path to a MaxMind GeoLite2 database file used
	// by the geo-restriction access mode (spec Q11). Empty (default) selects
	// the NoopGeoLookup — geo features are inert. Env: BURROW_GEO_DB_PATH.
	GeoDBPath string `koanf:"geo_db_path"`
	// PricingPath, when non-empty, overrides the embedded pricing.yaml the
	// cost engine ships with (spec Part F). Empty (default) uses the embedded
	// file. Env: BURROW_PRICING_PATH (also _FILE variant — the _FILE form
	// reads a file CONTAINING the path, kept for consistency with the
	// generic _FILE pattern).
	PricingPath string `koanf:"pricing_path"`
	// BackupDir is the on-disk directory the backup API and `burrowd backup`
	// CLI write/read archives in (Task 20). Defaults to "<DatabasePath>.backups"
	// so a stock deployment gets a working JSON API out of the box. When set
	// to a relative path it is resolved against the process working directory
	// at use time. Env: BURROW_BACKUP_DIR.
	BackupDir string `koanf:"backup_dir"`

	// ----- v0.5.0 additions (Task 15) ----------------------------------------

	// DatabaseURL is the Postgres connection URL (e.g.
	// "postgres://user:pass@host/dbname?sslmode=verify-full"). When non-empty
	// the server uses PostgreSQL instead of SQLite. Requires
	// ExperimentalPostgres=true AND a binary built with -tags=postgres.
	// Both DatabasePath and DatabaseURL being non-empty is a fatal error.
	// Env: BURROW_DATABASE_URL (also _FILE variant).
	DatabaseURL string `koanf:"database_url"`
	// ExperimentalPostgres enables the PostgreSQL backend. Must be true when
	// DatabaseURL is set; ignored when DatabaseURL is empty (no-op).
	// Guards against operators accidentally enabling an alpha feature.
	// Env: BURROW_EXPERIMENTAL_POSTGRES_BACKEND.
	// koanf key: experimental_postgres_backend (flat underscore — koanf nested
	// dot-path tags do not unmarshal correctly to flat struct fields in this
	// codebase; the flat key matches the BURROW_EXPERIMENTAL_POSTGRES_BACKEND
	// env-var after the standard prefix-strip + lowercase transform).
	ExperimentalPostgres bool `koanf:"experimental_postgres_backend"`

	// LoginRateLimitPerIP overrides api.LoginRateLimitPerIP (default 10) for
	// the per-IP login rate limiter. Zero (the default) means use the
	// compiled-in constant. Set to a small value in test environments to
	// trigger rate-limit responses with fewer attempts.
	// Env: BURROW_LOGIN_RATE_LIMIT_PER_IP.
	LoginRateLimitPerIP int `koanf:"login_rate_limit_per_ip"`

	// CertValidationRootsFile, when non-empty, is the path to a PEM file
	// containing one or more CA certificates to use as the trust roots when
	// validating custom-domain TLS certificates submitted via the API
	// (POST /admin/custom-domains/*/cert). Empty (the default) means the
	// system root pool is used. This field is intended for e2e / staging
	// environments that use a private CA not trusted by the OS store.
	// Env: BURROW_CERT_VALIDATION_ROOTS_FILE (also _FILE variant — the _FILE
	// form reads a file CONTAINING the path, consistent with the generic
	// _FILE pattern; however in the common case the env var IS the path).
	CertValidationRootsFile string `koanf:"cert_validation_roots_file"`

	// ACME (v0.6.0): built-in Let's Encrypt. Setting ACMEDomain enables ACME
	// and is mutually exclusive with the *_TLS_CERT/KEY file-cert fields.
	ACMEDomain  string `koanf:"acme_domain"`
	ACMEEmail   string `koanf:"acme_email"`
	ACMECA      string `koanf:"acme_ca"`
	ACMEStorage string `koanf:"acme_storage"`
}

// ClientConfig configures burrow.
type ClientConfig struct {
	Server     string       `koanf:"server" validate:"required,hostname_port"`
	Token      string       `koanf:"token" validate:"required"`
	Insecure   bool         `koanf:"insecure"`
	CACert     string       `koanf:"cacert"`
	ServerName string       `koanf:"server_name"`
	Tunnels    []TunnelSpec `koanf:"tunnels"`
	LogLevel   string       `koanf:"log_level"`
	LogFormat  string       `koanf:"log_format"`
}

func base() *koanf.Koanf { return koanf.New(".") }

func envProvider(prefix string) *env.Env {
	return env.Provider(prefix, ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, prefix)), "__", ".")
	})
}

// burrowEnvProvider is like envProvider but additionally splits
// BURROW_TRUSTED_PROXIES (a comma-separated list) into a []string so that
// koanf can unmarshal it into ServerConfig.TrustedProxies correctly.
func burrowEnvProvider() *env.Env {
	return env.ProviderWithValue("BURROW_", ".", func(rawKey, rawVal string) (string, interface{}) {
		key := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(rawKey, "BURROW_")), "__", ".")
		if key == "trusted_proxies" {
			if rawVal == "" {
				return key, []string{}
			}
			parts := strings.Split(rawVal, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					out = append(out, p)
				}
			}
			return key, out
		}
		return key, rawVal
	})
}

// normalizeEnvKey maps a raw env-var name (without the BURROW_ prefix) to the
// koanf key the env providers use: lowercase + double-underscore to dot.
// This MUST match the transform in both envProvider and burrowEnvProvider so
// that _FILE keys resolve to exactly the same koanf keys as the literal vars.
func normalizeEnvKey(rawSuffix string) string {
	return strings.ReplaceAll(strings.ToLower(rawSuffix), "__", ".")
}

// applyFileSecrets scans the process environment for variables of the form
// BURROW_<KEY>_FILE. For each one it reads the file at the referenced path,
// trims a single trailing newline (Docker/Swarm secrets conventionally append
// one), and sets the corresponding koanf key in k.
//
// Precedence: this layer sits AFTER the literal env provider, so a _FILE value
// WINS over a literal BURROW_<KEY> set in the environment. A missing or
// unreadable file returns a hard error (fail-fast; silent fallback would leave
// the server unseeded).
func applyFileSecrets(k *koanf.Koanf) error {
	const prefix = "BURROW_"
	const fileSuffix = "_FILE"

	overrides := map[string]any{}
	for _, kv := range os.Environ() {
		eqIdx := strings.IndexByte(kv, '=')
		if eqIdx < 0 {
			continue
		}
		name, path := kv[:eqIdx], kv[eqIdx+1:]

		// Must start with BURROW_ and end with _FILE.
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}

		// Strip the BURROW_ prefix and the _FILE suffix to get the raw key suffix.
		rawSuffix := strings.TrimSuffix(strings.TrimPrefix(name, prefix), fileSuffix)
		if rawSuffix == "" {
			// Edge-case: "BURROW__FILE" — no meaningful key; skip.
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("config: %s=%q: cannot read secret file: %w", name, path, err)
		}

		// Trim only a single trailing \r\n or \n (Docker file-secrets convention).
		// Internal content (spaces, special chars) is intentionally left intact.
		value := strings.TrimRight(string(data), "\r\n")

		koanfKey := normalizeEnvKey(rawSuffix)
		overrides[koanfKey] = value
	}
	if len(overrides) == 0 {
		return nil
	}
	return k.Load(confmap.Provider(overrides, "."), nil)
}

// deriveV04Fields fills in the post-merge derived defaults for v0.4.0:
// BackupDir. Called only after koanf has merged defaults < env < _FILE <
// overrides so the derivations see the final values for DatabasePath.
func deriveV04Fields(c *ServerConfig) {
	if c.BackupDir == "" {
		c.BackupDir = c.DatabasePath + ".backups"
	}
}

// validateV04Fields checks the spec-mandated shape of the v0.4.0 fields:
//
//   - MCPListen: empty (disabled) or parseable as [host]:port.
//
// Returns the first violation as an error.
func validateV04Fields(c *ServerConfig) error {
	if c.MCPListen != "" {
		_, port, err := net.SplitHostPort(c.MCPListen)
		if err != nil {
			return fmt.Errorf("mcp_listen %q is not a valid [host]:port: %w", c.MCPListen, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("mcp_listen %q has invalid numeric port", c.MCPListen)
		}
	}
	return nil
}

// validateDatabaseConfig enforces the v0.5.0 Task 15 backend-selection rules:
//
//   - Both database_path AND database_url set → fatal (ambiguous).
//   - database_url set but experimental.postgres_backend != true → fatal
//     (operator must explicitly opt in to the alpha Postgres feature).
//
// Note: database_path has a non-empty default ("./burrow.db"), so "both set"
// means database_url is non-empty AND database_path is its default or
// an explicit override. The validation rejects any non-empty database_url
// when database_path is also non-empty, covering both cases.
func validateDatabaseConfig(c *ServerConfig) error {
	if c.DatabaseURL == "" {
		return nil // SQLite-only; nothing to validate.
	}
	// database_url is set. Reject if database_path is also set (the default
	// "./burrow.db" counts as "set" — operators must clear it explicitly via
	// BURROW_DATABASE_PATH="" when switching to Postgres).
	if c.DatabasePath != "" {
		return fmt.Errorf("database_url and database_path must not both be set; " +
			"clear database_path (BURROW_DATABASE_PATH=\"\") when using database_url")
	}
	// Require the explicit opt-in flag.
	if !c.ExperimentalPostgres {
		return fmt.Errorf("database_url is set but experimental_postgres_backend is false; " +
			"set BURROW_EXPERIMENTAL_POSTGRES_BACKEND=true to enable the alpha Postgres backend")
	}
	return nil
}

// parseTrustedProxies validates that each entry in the list is a valid CIDR or
// IP address. Returns an error naming the first invalid entry.
func parseTrustedProxies(entries []string) error {
	for _, e := range entries {
		if e == "" {
			continue
		}
		// Accept both bare IPs and CIDR notation.
		if _, _, err := net.ParseCIDR(e); err != nil {
			if net.ParseIP(e) == nil {
				return fmt.Errorf("trusted_proxies: %q is not a valid CIDR or IP address", e)
			}
		}
	}
	return nil
}

// LoadServer loads the server config, merging defaults < BURROW_ env < _FILE env < overrides.
//
// If both BURROW_<KEY> and BURROW_<KEY>_FILE are set the _FILE value wins
// (Docker convention); the literal env is loaded first, then _FILE secrets
// overwrite it. Explicit programmatic overrides still win over everything.
// A non-existent or unreadable _FILE path is a hard error.
func LoadServer(overrides map[string]any) (*ServerConfig, error) {
	k := base()
	_ = k.Load(confmap.Provider(map[string]any{
		"listen": ":7000", "tls_cert": "certs/dev-server.pem",
		"tls_key": "certs/dev-server-key.pem", "log_level": "info", "log_format": "text",
		"public_bind": "0.0.0.0", "port_min": 9000, "port_max": 9100,
		// database_path is resolved relative to the process working directory;
		// supply an absolute path via BURROW_DATABASE_PATH in production.
		"database_path": "./burrow.db", "http_listen": ":8080", "http_secure_cookies": false,
		// trusted_proxies defaults to empty: no forwarded headers trusted.
		"trusted_proxies": []string{},
		// http_proxy_listen defaults to empty: the host-routed ingress is off
		// and services are reached by path at https://<auth_domain>/svc/<slug>/.
		// http_proxy_tls_cert/key default to empty (no TLS; operator terminates upstream).
		// auth_domain defaults to empty (no subdomain-routing configured).
		"http_proxy_listen": "", "http_proxy_tls_cert": "", "http_proxy_tls_key": "", "auth_domain": "",
		// v0.4.0 (Task 24) defaults. The empty-string defaults disable the
		// associated feature (MCP listener, geo lookup, pricing override).
		// BackupDir defaults to empty here and is derived after unmarshalling —
		// once DatabasePath has been resolved through env+_FILE+overrides — so
		// the derivation rule sees the final post-merge value.
		"mcp_listen": "", "burrow_mcp_token": "", "geo_db_path": "", "pricing_path": "",
		"backup_dir": "",
		// v0.5.0 (Task 15): Postgres backend defaults — both off by default.
		"database_url": "", "experimental_postgres_backend": false,
		// login_rate_limit_per_ip: 0 means use api.LoginRateLimitPerIP constant.
		"login_rate_limit_per_ip": 0,
		// cert_validation_roots_file: empty = use system root pool.
		"cert_validation_roots_file": "",
		// ACME: empty domain = disabled. CA defaults to Let's Encrypt production.
		// acme_storage is derived after merge (like backup_dir) when left empty.
		"acme_domain": "", "acme_email": "",
		"acme_ca": "https://acme-v02.api.letsencrypt.org/directory", "acme_storage": "",
	}, "."), nil)
	_ = k.Load(burrowEnvProvider(), nil)
	if err := applyFileSecrets(k); err != nil {
		return nil, err
	}
	if overrides != nil {
		_ = k.Load(confmap.Provider(overrides, "."), nil)
	}
	var c ServerConfig
	if err := k.Unmarshal("", &c); err != nil {
		return nil, fmt.Errorf("unmarshal server config: %w", err)
	}
	if err := validate.Struct(&c); err != nil {
		return nil, fmt.Errorf("invalid server config: %w", err)
	}
	if err := parseTrustedProxies(c.TrustedProxies); err != nil {
		return nil, fmt.Errorf("invalid server config: %w", err)
	}
	// Both-or-neither validation for HTTP TLS cert/key pair (xor is invalid).
	if (c.HTTPTLSCert == "") != (c.HTTPTLSKey == "") {
		return nil, fmt.Errorf("invalid server config: http_tls_cert and http_tls_key must both be set or both be empty")
	}
	// Both-or-neither validation for HTTP proxy TLS cert/key pair (xor is invalid).
	if (c.HTTPProxyTLSCert == "") != (c.HTTPProxyTLSKey == "") {
		return nil, fmt.Errorf("invalid server config: http_proxy_tls_cert and http_proxy_tls_key must both be set or both be empty")
	}
	// v0.4.0 (Task 24): derive BackupDir from the already-merged DatabasePath,
	// then validate the v0.4.0 field shapes.
	deriveV04Fields(&c)
	if err := validateV04Fields(&c); err != nil {
		return nil, fmt.Errorf("invalid server config: %w", err)
	}
	// v0.5.0 (Task 15): validate Postgres backend config. Both database_path
	// and database_url being non-empty is ambiguous and therefore fatal.
	// database_url without the experimental flag is also rejected (operators
	// must opt in explicitly).
	if err := validateDatabaseConfig(&c); err != nil {
		return nil, fmt.Errorf("invalid server config: %w", err)
	}
	// ACME validation (v0.6.0).
	if c.ACMEDomain != "" {
		if c.ACMEEmail == "" {
			return nil, fmt.Errorf("invalid server config: acme_email is required when acme_domain is set")
		}
		if c.TLSCert != "" && c.TLSCert != "certs/dev-server.pem" ||
			c.HTTPTLSCert != "" || c.HTTPProxyTLSCert != "" {
			return nil, fmt.Errorf("invalid server config: acme_domain is mutually exclusive with *_tls_cert/key (ACME manages all certificates)")
		}
		if c.ACMEStorage == "" {
			c.ACMEStorage = filepath.Join(filepath.Dir(c.DatabasePath), "acme")
		}
	}
	return &c, nil
}

// LoadClient loads the client config, merging defaults < BURROW_ env < _FILE env < overrides.
//
// If both BURROW_<KEY> and BURROW_<KEY>_FILE are set the _FILE value wins.
// A non-existent or unreadable _FILE path is a hard error.
func LoadClient(overrides map[string]any) (*ClientConfig, error) {
	k := base()
	_ = k.Load(confmap.Provider(map[string]any{
		"log_level": "info", "log_format": "text",
	}, "."), nil)
	_ = k.Load(envProvider("BURROW_"), nil)
	if err := applyFileSecrets(k); err != nil {
		return nil, err
	}
	if overrides != nil {
		_ = k.Load(confmap.Provider(overrides, "."), nil)
	}
	var c ClientConfig
	if err := k.Unmarshal("", &c); err != nil {
		return nil, fmt.Errorf("unmarshal client config: %w", err)
	}
	if err := validate.Struct(&c); err != nil {
		return nil, fmt.Errorf("invalid client config: %w", err)
	}
	return &c, nil
}
