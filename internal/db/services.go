package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrDuplicateService is returned by CreateService when the (id) or
// (user_id, name) UNIQUE constraint is violated. Mirrors the sentinel pattern
// used by ErrDuplicateHostname in custom_domains.go.
var ErrDuplicateService = fmt.Errorf("service already exists")

// isDuplicateServiceErr reports whether err is a UNIQUE constraint violation
// produced by an INSERT into services. Mirrors isDuplicateError in
// custom_domains.go; kept package-private and topic-local so the services file
// stays self-contained.
func isDuplicateServiceErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "unique constraint")
}

// CreateService inserts a new services row exactly as supplied by the
// admin-facing POST /api/v1/services handler. Unlike GetOrCreateService
// (which silently no-ops on conflict) this surface returns ErrDuplicateService
// on UNIQUE-constraint violations so the API can map them to HTTP 409.
//
// The caller is responsible for setting s.ID, s.UserID, s.Name, s.Type, and
// s.AccessMode; CreatedAt defaults to time.Now().UTC() when zero. Empty
// optional fields (Subdomain, APIKeyHeader) are stored as their column
// defaults (NULL / "Authorization") via COALESCE-friendly NULL handling.
func (x *DB) CreateService(ctx context.Context, s Service) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	// subdomain column is nullable (UNIQUE); insert NULL when empty so the
	// uniqueness constraint does not collide across multiple subdomain-less
	// rows. Use sql.NullString to surface NULL distinctly from empty.
	var sub sql.NullString
	if s.Subdomain != "" {
		sub = sql.NullString{String: s.Subdomain, Valid: true}
	}
	header := s.APIKeyHeader
	if header == "" {
		header = "Authorization"
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO services(id, user_id, name, type, subdomain, access_mode, api_key_header, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		s.ID, s.UserID, s.Name, s.Type, sub, s.AccessMode, header, s.CreatedAt,
	)
	if err != nil {
		if isDuplicateServiceErr(err) {
			return ErrDuplicateService
		}
		return fmt.Errorf("create service: %w", err)
	}
	return nil
}

// selectServiceRow is the shared SELECT column list for service rows.
// mtls_ca_pem (migration 0009) is included so the proxy layer can populate
// proxy.Resolved.MTLSCAPEM without a second round-trip; non-mtls services
// store it as NULL, which COALESCE rewrites to ”.
const selectServiceCols = `id, user_id, name, type, COALESCE(subdomain,''), access_mode, api_key_header, created_at, COALESCE(mtls_ca_pem,''), gateway_only`

// ErrServiceNameReserved is returned by GetOrCreateService when the name
// belongs to the backing row of a direct AI provider.
var ErrServiceNameReserved = errors.New("db: service name is used by a direct AI provider")

// GetOrCreateService returns the service row for (userID, name), creating it
// with the given type and default access_mode/api_key_header if it does not
// exist. The operation is idempotent (INSERT ... ON CONFLICT DO NOTHING then
// SELECT), safe for concurrent callers.
//
// A row of type "direct" backs an AI provider and is never handed out: a
// tunnel registering under that name would otherwise bind to the provider's
// keys and policy. Such a call fails with ErrServiceNameReserved.
func (x *DB) GetOrCreateService(ctx context.Context, userID, name, typ string) (Service, error) {
	id := uuid.NewString()
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO services(id, user_id, name, type)
		 VALUES(?,?,?,?)
		 ON CONFLICT(user_id, name) DO NOTHING`,
		id, userID, name, typ,
	)
	if err != nil {
		return Service{}, fmt.Errorf("get or create service: %w", err)
	}
	var s Service
	err = x.sqlDB.QueryRowContext(ctx,
		`SELECT `+selectServiceCols+` FROM services WHERE user_id=? AND name=?`,
		userID, name,
	).Scan(&s.ID, &s.UserID, &s.Name, &s.Type, &s.Subdomain, &s.AccessMode, &s.APIKeyHeader, &s.CreatedAt, &s.MTLSCAPEM, (*intBool)(&s.GatewayOnly))
	if err == sql.ErrNoRows {
		return Service{}, ErrNotFound
	}
	if err != nil {
		return Service{}, fmt.Errorf("get or create service select: %w", err)
	}
	if s.Type == "direct" {
		return Service{}, ErrServiceNameReserved
	}
	return s, nil
}

// GetServiceByID returns the service with the given id, or ErrNotFound.
func (x *DB) GetServiceByID(ctx context.Context, id string) (Service, error) {
	var s Service
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT `+selectServiceCols+` FROM services WHERE id=?`, id,
	).Scan(&s.ID, &s.UserID, &s.Name, &s.Type, &s.Subdomain, &s.AccessMode, &s.APIKeyHeader, &s.CreatedAt, &s.MTLSCAPEM, (*intBool)(&s.GatewayOnly))
	if err == sql.ErrNoRows {
		return Service{}, ErrNotFound
	}
	if err != nil {
		return Service{}, fmt.Errorf("get service by id: %w", err)
	}
	return s, nil
}

// GetServiceBySubdomain returns the service with the given subdomain, or
// ErrNotFound. Empty subdomain always returns ErrNotFound.
func (x *DB) GetServiceBySubdomain(ctx context.Context, sub string) (Service, error) {
	if sub == "" {
		return Service{}, ErrNotFound
	}
	var s Service
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT `+selectServiceCols+` FROM services WHERE subdomain=?`, sub,
	).Scan(&s.ID, &s.UserID, &s.Name, &s.Type, &s.Subdomain, &s.AccessMode, &s.APIKeyHeader, &s.CreatedAt, &s.MTLSCAPEM, (*intBool)(&s.GatewayOnly))
	if err == sql.ErrNoRows {
		return Service{}, ErrNotFound
	}
	if err != nil {
		return Service{}, fmt.Errorf("get service by subdomain: %w", err)
	}
	return s, nil
}

// ListServicesByUser returns all service rows belonging to the given user,
// ordered by created_at.
func (x *DB) ListServicesByUser(ctx context.Context, userID string) ([]Service, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT `+selectServiceCols+` FROM services WHERE user_id=? ORDER BY created_at`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list services by user: %w", err)
	}
	defer rows.Close()
	var out []Service
	for rows.Next() {
		var s Service
		if err := rows.Scan(&s.ID, &s.UserID, &s.Name, &s.Type, &s.Subdomain, &s.AccessMode, &s.APIKeyHeader, &s.CreatedAt, &s.MTLSCAPEM, (*intBool)(&s.GatewayOnly)); err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list services by user rows: %w", err)
	}
	return out, nil
}

// ListAllServices returns all service rows ordered by created_at.
func (x *DB) ListAllServices(ctx context.Context) ([]Service, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT `+selectServiceCols+` FROM services ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all services: %w", err)
	}
	defer rows.Close()
	var out []Service
	for rows.Next() {
		var s Service
		if err := rows.Scan(&s.ID, &s.UserID, &s.Name, &s.Type, &s.Subdomain, &s.AccessMode, &s.APIKeyHeader, &s.CreatedAt, &s.MTLSCAPEM, (*intBool)(&s.GatewayOnly)); err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all services rows: %w", err)
	}
	return out, nil
}

// SetServiceAccessMode updates the access_mode and api_key_header for a
// service. Returns ErrNotFound if no row matched.
// Enum validation is the caller's responsibility (store layer).
func (x *DB) SetServiceAccessMode(ctx context.Context, id, mode, header string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE services SET access_mode=?, api_key_header=? WHERE id=?`, mode, header, id,
	)
	if err != nil {
		return fmt.Errorf("set service access mode: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set service access mode rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetServiceSubdomain sets the subdomain for a service. Returns ErrNotFound if
// no row matched (service id does not exist). On a UNIQUE(subdomain) violation
// the raw driver error is wrapped so the caller (store layer) can detect a
// collision and retry with a different subdomain value.
func (x *DB) SetServiceSubdomain(ctx context.Context, id, sub string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE services SET subdomain=? WHERE id=?`, sub, id,
	)
	if err != nil {
		// Wrap and surface; the UNIQUE constraint error from the sqlite driver is
		// preserved inside the wrapping so callers can inspect it via errors.As or
		// string-matching the driver error message.
		return fmt.Errorf("set service subdomain: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set service subdomain rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteService removes the service with the given ID.
// ON DELETE CASCADE removes associated api_keys and access_policy rows.
// Returns ErrNotFound if no row matched. A service behind a provider that a
// synthetic model targets stays: the error wraps ErrProviderInUse and names
// the models.
func (x *DB) DeleteService(ctx context.Context, id string) error {
	res, err := x.sqlDB.ExecContext(ctx, `DELETE FROM services WHERE id=?`, id)
	if err != nil {
		return x.providerInUseOr(ctx, fmt.Errorf("delete service: %w", err), `WHERE s.id=?`, id)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete service rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete service: %w", ErrNotFound)
	}
	return nil
}

// CreateServiceAPIKey inserts a new service API key row.
func (x *DB) CreateServiceAPIKey(ctx context.Context, k ServiceAPIKey) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO service_api_keys(id, service_id, name, key_hash) VALUES(?,?,?,?)`,
		k.ID, k.ServiceID, k.Name, k.KeyHash,
	)
	if err != nil {
		return fmt.Errorf("create service api key: %w", err)
	}
	return nil
}

// GetServiceAPIKeyByHash returns the API key whose key_hash matches, scoped to
// the given serviceID. Returns ErrNotFound if no row matched.
func (x *DB) GetServiceAPIKeyByHash(ctx context.Context, serviceID, hash string) (ServiceAPIKey, error) {
	var k ServiceAPIKey
	var lastUsed sql.NullTime
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT id, service_id, name, key_hash, last_used, created_at
		   FROM service_api_keys WHERE service_id=? AND key_hash=?`,
		serviceID, hash,
	).Scan(&k.ID, &k.ServiceID, &k.Name, &k.KeyHash, &lastUsed, &k.CreatedAt)
	if err == sql.ErrNoRows {
		return ServiceAPIKey{}, ErrNotFound
	}
	if err != nil {
		return ServiceAPIKey{}, fmt.Errorf("get service api key by hash: %w", err)
	}
	if lastUsed.Valid {
		k.LastUsed = &lastUsed.Time
	}
	return k, nil
}

// ListServiceAPIKeys returns all API keys for a service, ordered by created_at.
func (x *DB) ListServiceAPIKeys(ctx context.Context, serviceID string) ([]ServiceAPIKey, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, service_id, name, key_hash, last_used, created_at
		   FROM service_api_keys WHERE service_id=? ORDER BY created_at`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list service api keys: %w", err)
	}
	defer rows.Close()
	var out []ServiceAPIKey
	for rows.Next() {
		var k ServiceAPIKey
		var lastUsed sql.NullTime
		if err := rows.Scan(&k.ID, &k.ServiceID, &k.Name, &k.KeyHash, &lastUsed, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan service api key: %w", err)
		}
		if lastUsed.Valid {
			k.LastUsed = &lastUsed.Time
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list service api keys rows: %w", err)
	}
	return out, nil
}

// TouchServiceAPIKey sets last_used to the current timestamp for the given key
// ID. Best-effort: exec errors are returned, but 0-rows-affected is a no-op.
func (x *DB) TouchServiceAPIKey(ctx context.Context, id string) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`UPDATE service_api_keys SET last_used=CURRENT_TIMESTAMP WHERE id=?`, id,
	)
	if err != nil {
		return fmt.Errorf("touch service api key: %w", err)
	}
	return nil
}

// DeleteServiceAPIKey removes the API key with the given id, scoped to
// serviceID. Returns ErrNotFound if no row matched.
func (x *DB) DeleteServiceAPIKey(ctx context.Context, id, serviceID string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM service_api_keys WHERE id=? AND service_id=?`, id, serviceID,
	)
	if err != nil {
		return fmt.Errorf("delete service api key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete service api key rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete service api key: %w", ErrNotFound)
	}
	return nil
}

// GetAccessPolicy returns the roles for a service's access policy, ordered
// alphabetically. Always returns a non-nil slice (empty when no roles exist).
func (x *DB) GetAccessPolicy(ctx context.Context, serviceID string) ([]string, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT role FROM service_access_policy WHERE service_id=? ORDER BY role`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("get access policy: %w", err)
	}
	defer rows.Close()
	out := []string{} // never nil — spec requires always-an-array
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("scan access policy role: %w", err)
		}
		out = append(out, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get access policy rows: %w", err)
	}
	return out, nil
}

// SetAccessPolicy replaces the full access policy for a service in a single
// transaction: DELETE existing roles then INSERT the new set. An empty roles
// slice means deny-all (just the delete, no inserts). Mirrors the tx pattern
// in settings.go (SetSettings).
func (x *DB) SetAccessPolicy(ctx context.Context, serviceID string, roles []string) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin access policy tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM service_access_policy WHERE service_id=?`, serviceID,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete access policy: %w", err)
	}
	for _, role := range roles {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO service_access_policy(service_id, role) VALUES(?,?)`, serviceID, role,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert access policy role %q: %w", role, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit access policy tx: %w", err)
	}
	return nil
}

// SetServiceGatewayOnly marks a service as reachable only through the AI
// gateway. ErrNotFound when no service has the id.
func (x *DB) SetServiceGatewayOnly(ctx context.Context, id string, on bool) error {
	res, err := x.sqlDB.ExecContext(ctx, `UPDATE services SET gateway_only=? WHERE id=?`, boolToInt(on), id)
	if err != nil {
		return fmt.Errorf("set service gateway only: %w", err)
	}
	return notFoundIfNoRows(res, "set service gateway only")
}
