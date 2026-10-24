package db

// ipgeo.go — typed CRUD over the service_ip_geo table (migration 0009).
//
// The service_ip_geo table is defined by spec Part J (Network access). One row
// per service carries the per-service IP allow/block CIDR lists and the
// allow/block country-code lists. The country lists are accepted in v0.4.0 for
// forward-compat with the upcoming `geo` build tag (Task 17); in the default
// build the proxy ignores them.
//
// JSON arrays are stored as TEXT columns so SQLite remains a single-file
// dependency; the typed view (ServiceIPGeoConfig) exposes []string slices
// while the underlying ServiceIPGeo carries the raw JSON strings.
//
// The PRIMARY KEY is service_id with ON DELETE CASCADE on services(id), so
// deleting a service auto-clears its ip-geo row.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ServiceIPGeoConfig is the decoded, slice-typed view of a service_ip_geo row.
// Empty slices are returned as non-nil [] (never nil); callers can json-encode
// them directly without nil-guards.
type ServiceIPGeoConfig struct {
	ServiceID      string
	Enabled        bool
	AllowCIDRs     []string
	BlockCIDRs     []string
	AllowCountries []string
	BlockCountries []string
}

// GetServiceIPGeo returns the typed config for the given service. When no row
// exists, returns a zero-value config with Enabled=false and empty (non-nil)
// slices, nil error — same shape as a row with everything default.
func (x *DB) GetServiceIPGeo(ctx context.Context, serviceID string) (ServiceIPGeoConfig, error) {
	var row ServiceIPGeo
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT service_id, enabled, allow_cidrs, block_cidrs, allow_countries, block_countries
		   FROM service_ip_geo WHERE service_id=?`, serviceID,
	).Scan(&row.ServiceID, &row.Enabled, &row.AllowCIDRs, &row.BlockCIDRs,
		&row.AllowCountries, &row.BlockCountries)
	if errors.Is(err, sql.ErrNoRows) {
		// No row → treat as the empty default; callers see (enabled=false, [],[],[],[]).
		return ServiceIPGeoConfig{
			ServiceID:      serviceID,
			AllowCIDRs:     []string{},
			BlockCIDRs:     []string{},
			AllowCountries: []string{},
			BlockCountries: []string{},
		}, nil
	}
	if err != nil {
		return ServiceIPGeoConfig{}, fmt.Errorf("get service ip-geo: %w", err)
	}
	cfg := ServiceIPGeoConfig{
		ServiceID: row.ServiceID,
		Enabled:   row.Enabled,
	}
	if err := decodeJSONArray(row.AllowCIDRs, &cfg.AllowCIDRs); err != nil {
		return ServiceIPGeoConfig{}, fmt.Errorf("decode allow_cidrs: %w", err)
	}
	if err := decodeJSONArray(row.BlockCIDRs, &cfg.BlockCIDRs); err != nil {
		return ServiceIPGeoConfig{}, fmt.Errorf("decode block_cidrs: %w", err)
	}
	if err := decodeJSONArray(row.AllowCountries, &cfg.AllowCountries); err != nil {
		return ServiceIPGeoConfig{}, fmt.Errorf("decode allow_countries: %w", err)
	}
	if err := decodeJSONArray(row.BlockCountries, &cfg.BlockCountries); err != nil {
		return ServiceIPGeoConfig{}, fmt.Errorf("decode block_countries: %w", err)
	}
	return cfg, nil
}

// SetServiceIPGeo upserts the row for the given service. JSON encoding is
// deterministic (always non-nil arrays) so the round-trip via GetServiceIPGeo
// returns the same shape regardless of nil-vs-empty inputs.
func (x *DB) SetServiceIPGeo(ctx context.Context, cfg ServiceIPGeoConfig) error {
	allowCIDRs, err := encodeJSONArray(cfg.AllowCIDRs)
	if err != nil {
		return fmt.Errorf("encode allow_cidrs: %w", err)
	}
	blockCIDRs, err := encodeJSONArray(cfg.BlockCIDRs)
	if err != nil {
		return fmt.Errorf("encode block_cidrs: %w", err)
	}
	allowCountries, err := encodeJSONArray(cfg.AllowCountries)
	if err != nil {
		return fmt.Errorf("encode allow_countries: %w", err)
	}
	blockCountries, err := encodeJSONArray(cfg.BlockCountries)
	if err != nil {
		return fmt.Errorf("encode block_countries: %w", err)
	}
	// UPSERT on PRIMARY KEY (service_id).
	_, err = x.sqlDB.ExecContext(ctx,
		`INSERT INTO service_ip_geo(service_id, enabled, allow_cidrs, block_cidrs, allow_countries, block_countries)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(service_id) DO UPDATE SET
		    enabled         = excluded.enabled,
		    allow_cidrs     = excluded.allow_cidrs,
		    block_cidrs     = excluded.block_cidrs,
		    allow_countries = excluded.allow_countries,
		    block_countries = excluded.block_countries`,
		cfg.ServiceID, cfg.Enabled, allowCIDRs, blockCIDRs, allowCountries, blockCountries,
	)
	if err != nil {
		return fmt.Errorf("set service ip-geo: %w", err)
	}
	return nil
}

// CountCountryRules counts the services that have a country allow or block
// list. Lists are stored as JSON arrays, so an empty one is "[]".
func (x *DB) CountCountryRules(ctx context.Context) (int, error) {
	var n int
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM service_ip_geo
		  WHERE (allow_countries <> '[]' AND allow_countries <> '')
		     OR (block_countries <> '[]' AND block_countries <> '')`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count country rules: %w", err)
	}
	return n, nil
}

// SetServiceMTLSCAPEM sets the operator-supplied CA PEM blob on the services
// row. An empty pem string clears the column (NULL). Returns ErrNotFound when
// no service row matches.
func (x *DB) SetServiceMTLSCAPEM(ctx context.Context, serviceID, pem string) error {
	var val any
	if pem != "" {
		val = pem
	}
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE services SET mtls_ca_pem=? WHERE id=?`, val, serviceID,
	)
	if err != nil {
		return fmt.Errorf("set service mtls ca pem: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set service mtls ca pem rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetServiceMTLSCAPEM returns the operator-supplied CA PEM blob for the
// service, or "" when no row exists / the column is NULL. Returns ErrNotFound
// when the service row itself does not exist.
func (x *DB) GetServiceMTLSCAPEM(ctx context.Context, serviceID string) (string, error) {
	var pem sql.NullString
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT mtls_ca_pem FROM services WHERE id=?`, serviceID,
	).Scan(&pem)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get service mtls ca pem: %w", err)
	}
	if !pem.Valid {
		return "", nil
	}
	return pem.String, nil
}

// decodeJSONArray decodes the JSON text-column value into out. A blank input
// is treated as an empty array (matches the migration's DEFAULT '[]').
func decodeJSONArray(raw string, out *[]string) error {
	if raw == "" {
		*out = []string{}
		return nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return err
	}
	if *out == nil {
		*out = []string{}
	}
	return nil
}

// encodeJSONArray serialises a string slice into a JSON text-column value.
// nil slices are serialised as "[]" (never "null"), keeping the column
// invariant the rest of the codebase expects.
func encodeJSONArray(in []string) (string, error) {
	if in == nil {
		in = []string{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
