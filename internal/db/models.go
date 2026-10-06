package db

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound is returned by Get* when no row matches.
var ErrNotFound = errors.New("db: not found")

// User is a row of the users table.
type User struct {
	ID, Email, PasswordHash, Role string
	Status                        string
	LastLogin                     *time.Time
	CreatedAt, UpdatedAt          time.Time
}

// Session is a row of the sessions table.
type Session struct {
	ID, UserID           string
	ExpiresAt, CreatedAt time.Time
	UserAgent, IP        string
}

// ClientToken is a row of the client_tokens table (token_hash only).
type ClientToken struct {
	ID, UserID, Name, TokenHash string
	LastUsed                    *time.Time
	CreatedAt                   time.Time
}

// Tunnel is a persisted tunnel row (live state stays in-memory).
type Tunnel struct {
	ID, UserID, Name, Type, LocalAddr string
	RemotePort                        int
	CreatedAt                         time.Time
	LastSeen                          *time.Time
	TotalBytesIn, TotalBytesOut       int64
	LastFlushedAt                     *time.Time
	AccessMode                        string
}

// Role is a row of the roles table. v0.4.0 extends with custom-role fields.
type Role struct {
	Name, Description  string
	CreatedAt          time.Time
	Builtin            bool
	Permissions        []string // JSON-encoded in roles.permissions
	DefaultForNewUsers bool
}

// Setting is a row of the settings key/value table.
type Setting struct {
	Key, Value string
	UpdatedAt  time.Time
}

// Service is a row of the services table (HTTP-tunnel access configuration).
type Service struct {
	ID           string
	UserID       string
	Name         string
	Type         string // 'http' or 'tcp'
	Subdomain    string // "" for tcp
	AccessMode   string // 'open'|'api_key'|'burrow_login'|'mtls'
	APIKeyHeader string // default "Authorization"
	CreatedAt    time.Time
	// MTLSCAPEM is the operator-supplied PEM-encoded trust anchor used to
	// verify visitor client certs when AccessMode == "mtls". Empty for any
	// other mode (or even mtls services that haven't been configured yet —
	// the API rejects switching to mtls without one). Burrow does NOT sign
	// client certs in v0.4.0.
	MTLSCAPEM string
	// GatewayOnly: reachable only through the AI gateway, not directly.
	GatewayOnly bool
}

// ServiceAPIKey is a row of the service_api_keys table.
type ServiceAPIKey struct {
	ID        string
	ServiceID string
	Name      string
	KeyHash   string
	LastUsed  *time.Time
	CreatedAt time.Time
}

// --- v0.4.0 model structs (migrations 0004-0010) -----------------------------

// UsageEvent is a row of the usage_events table (AI traffic meter).
type UsageEvent struct {
	ID             string
	ServiceID      string
	APIKeyID       string
	Ts             time.Time
	Kind           string // openai|anthropic|mcp|unknown
	TokensIn       int64
	TokensOut      int64
	BytesIn        int64
	BytesOut       int64
	Streamed       bool
	CacheHit       bool
	UpstreamStatus int
	// CostUSD is the cost the upstream reported for the request; nil (NULL)
	// when it reported none and the price table applies.
	CostUSD *float64
	// How the gateway routed the request; all "" for traffic that did not
	// come through a gateway key.
	GatewayKeyID   string
	Dialect        string // openai|anthropic
	ProviderSlug   string
	RequestedModel string
	TargetModel    string
	RequestID      string
	LatencyMs      int64
}

// CacheEntry is a row of the cache_entries table (AI response cache).
type CacheEntry struct {
	ID         string
	ScopeKey   string
	KeyHash    string
	Status     int
	Headers    string // JSON
	Body       []byte
	CreatedAt  time.Time
	TTLSeconds int
	LastHitAt  *time.Time
}

// AuditEvent is a row of the audit_events table (hash-chained audit log).
type AuditEvent struct {
	ID           string // ulid
	Ts           time.Time
	ActorID      string
	ActorEmail   string
	Action       string
	SubjectID    string
	SubjectLabel string
	Result       string // ok|denied|error
	SourceIP     string
	UserAgent    string
	RequestID    string
	Payload      string // JSON
	PrevHash     string
	Hash         string
}

// Webhook is a row of the webhooks table.
type Webhook struct {
	ID                  string
	Name                string
	URL                 string
	SecretHash          string
	Events              string // JSON array
	Paused              bool
	ConsecutiveFailures int
	FirstFailureAt      *time.Time
	CreatedAt           time.Time
	PayloadTemplate     string // v0.5.0: optional Go template for the payload body
}

// WebhookDelivery is a row of the webhook_deliveries table.
type WebhookDelivery struct {
	ID              string
	WebhookID       string
	Event           string
	Ts              time.Time
	StatusCode      int
	Attempt         int
	LatencyMs       int
	RequestPreview  *string
	ResponsePreview *string
}

// ServiceAIConfig is a row of the service_ai_config table.
type ServiceAIConfig struct {
	ServiceID string
	Config    string // JSON (ServiceAIConfig per spec Part B.7)
	UpdatedAt time.Time
}

// ModelAlias is a row of the model_aliases table.
type ModelAlias struct {
	Alias         string
	ConcreteModel string
	ServiceID     string
	CreatedAt     time.Time
	Provider      string // v0.5.0: upstream provider name (e.g. "openai", "ollama")
	Priority      int    // v0.5.0: routing priority (lower = higher priority)
}

// AIProvider is a row of the ai_providers table: a named upstream speaking an
// LLM API, backed by a service.
type AIProvider struct {
	Slug      string
	Name      string
	Kind      string // "tunnel" (Phase 1) or "direct" (Phase 2)
	ServiceID string
	APIFormat string // "openai" or "anthropic"

	BaseURL           string            // direct only, e.g. "https://openrouter.ai/api/v1"
	CredentialSlot    string            // direct only, vault slot name (never the secret)
	AuthHeader        string            // default "Authorization"
	AuthFormat        string            // default "Bearer {key}"
	ExtraHeaders      map[string]string // static headers sent upstream; never nil after a read
	Billing           string            // "metered" or "flat"
	SupportsResponses bool              // offers POST /responses
	MaxConcurrent     int               // 0 = unlimited
	CreatedAt         time.Time
}

// AIProviderModel is a row of the ai_provider_models catalog.
type AIProviderModel struct {
	ProviderSlug  string
	ModelID       string
	DisplayName   string
	ContextLength int64
	SyncedAt      time.Time
}

// RateLimit is a row of the rate_limits table.
type RateLimit struct {
	ID        string
	Scope     string // api_key|role|service|global
	Subject   string
	Dimension string // rpm|bpm
	Lim       int64
	Burst     int64
	Window    string // minute|day
	CreatedAt time.Time
}

// Budget is a row of the budgets table.
type Budget struct {
	ID             string
	Scope          string // api_key|service|user|global
	SubjectID      string
	DailyUSD       float64
	DailyTokens    int64  // 0 = no token limit
	ActionOnExceed string // alert_webhook|throttle_zero|disable_key
	AlertWebhookID *string
	CreatedAt      time.Time
}

// ServiceIPGeo is a row of the service_ip_geo table.
type ServiceIPGeo struct {
	ServiceID      string
	Enabled        bool
	AllowCIDRs     string // JSON array
	BlockCIDRs     string // JSON array
	AllowCountries string // JSON array
	BlockCountries string // JSON array
}

// AutomationToken is a row of the automation_tokens table.
type AutomationToken struct {
	ID          string
	Name        string
	Prefix      string
	UserID      string
	RoleAtMint  string
	TokenHash   string
	Permissions string // JSON array
	ExpiresAt   *time.Time
	LastUsed    *time.Time
	CreatedAt   time.Time
}

// --- v0.5.0 model structs (migrations 0011-0017) -----------------------------

// SemanticIndexRow is a row of the semantic_index table.
type SemanticIndexRow struct {
	ServiceID         string
	ExactKeyHash      string
	PromptFingerprint string
	EmbeddingDim      int
	EmbeddingBlob     []byte
	CreatedAt         time.Time
}

// ServiceUpstreamCredential is a row of the service_upstream_credentials table.
type ServiceUpstreamCredential struct {
	ServiceID    string
	Slot         string
	HeaderName   string
	HeaderFormat string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ServiceCustomDomain is a row of the service_custom_domains table.
type ServiceCustomDomain struct {
	ID         string
	ServiceID  string
	Hostname   string
	CertPEM    string
	KeyPEM     string
	CertSHA256 string
	NotBefore  time.Time
	NotAfter   time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// v0.5.2 Task 10: status state machine.
	// Status is one of customdomain.StatusPending / StatusActive /
	// StatusCertExpiring / StatusCertExpired. The daily tick walks
	// cert-bearing rows and persists transitions via UpdateCustomDomainStatus.
	Status          string
	StatusUpdatedAt time.Time
}

// ConnectionLog is a row of the connection_logs table.
type ConnectionLog struct {
	ID              string
	Kind            string
	ServiceID       string
	TunnelID        string
	UserID          string
	ClientSessionID string
	SourceIP        string
	UserAgent       string
	StartedAt       time.Time
	EndedAt         time.Time
	DurationMs      int
	BytesIn         int64
	BytesOut        int64
	Status          string
	Reason          string
	CreatedAt       time.Time
}

// ConnectionLogRollup is a row of the connection_log_rollups table.
type ConnectionLogRollup struct {
	Day           string // DATE stored as TEXT in SQLite (YYYY-MM-DD)
	ServiceID     string
	Kind          string
	Sessions      int64
	BytesIn       int64
	BytesOut      int64
	AvgDurationMs int64
	P95DurationMs int64
	CreatedAt     time.Time
}

// DB wraps *sql.DB and exposes typed CRUD methods for Burrow's tables.
type DB struct {
	sqlDB *sql.DB
}

// Wrap wraps an existing *sql.DB (already opened and migrated) in the typed DB.
func Wrap(d *sql.DB) *DB {
	return &DB{sqlDB: d}
}

// DB returns the underlying *sql.DB.
func (x *DB) DB() *sql.DB {
	return x.sqlDB
}

// Close closes the underlying database connection.
func (x *DB) Close() error {
	return x.sqlDB.Close()
}

// Driver implements Backend. *DB always wraps SQLite.
func (x *DB) Driver() string { return "sqlite" }

// Now implements Backend. Returns the SQLite CURRENT_TIMESTAMP fragment.
func (x *DB) Now() string { return "CURRENT_TIMESTAMP" }

// Placeholder implements Backend. SQLite uses "?" for all positional args.
func (x *DB) Placeholder(_ int) string { return "?" }

// compile-time assertion: *DB satisfies Backend.
var _ Backend = (*DB)(nil)
