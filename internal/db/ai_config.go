package db

// ai_config.go — raw read of service_ai_config rows. Decoding into the
// typed aigw.ServiceAIConfig lives in cmd/server (the chainConfigLoader
// adapter) because internal/cache/exact (the cache.Settings sub-type)
// already imports internal/db; pulling aigw types in here would close an
// import cycle. The cmd/server layer already imports both packages so
// the decode is wired there without violating dependency direction.
//
// Fail-open contract: callers treat any non-nil error as ok=false +
// fall through to v0.3.0 pass-through, so a malformed config row can
// never break the proxy.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetServiceAIConfigRaw returns the raw JSON blob persisted under
// service_ai_config.config for the given service id. ok=false (with a
// nil error) when no row exists — preserves the v0.3.0 pass-through
// invariant for services that have not opted into AI features.
//
// The caller (cmd/server's chainConfigLoader) decodes the blob into
// aigw.ServiceAIConfig. See the package doc comment for the reason the
// decode is not done here.
func (x *DB) GetServiceAIConfigRaw(ctx context.Context, serviceID string) (raw []byte, ok bool, err error) {
	var s sql.NullString
	err = x.sqlDB.QueryRowContext(ctx,
		`SELECT config FROM service_ai_config WHERE service_id=?`, serviceID,
	).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get service ai config: %w", err)
	}
	if !s.Valid {
		return nil, true, nil
	}
	return []byte(s.String), true, nil
}

// ListServiceAIConfigsRaw returns the raw JSON blob of every
// service_ai_config row, in no particular order (a row without a config is
// left out).
func (x *DB) ListServiceAIConfigsRaw(ctx context.Context) ([][]byte, error) {
	rows, err := x.sqlDB.QueryContext(ctx, `SELECT config FROM service_ai_config`)
	if err != nil {
		return nil, fmt.Errorf("list service ai configs: %w", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("list service ai configs: scan: %w", err)
		}
		if s.Valid {
			out = append(out, []byte(s.String))
		}
	}
	return out, rows.Err()
}

// UpsertServiceAIConfig inserts or replaces the JSON config blob for the
// given service. The service must already exist in the services table (the
// foreign key constraint enforces this). The config blob is expected to be
// valid JSON but is not decoded here — validation lives at the API layer.
//
// v0.5.0 Task 4: used by PUT /api/v1/services/{id}/ai-config.
func (x *DB) UpsertServiceAIConfig(ctx context.Context, serviceID string, config []byte) error {
	if len(config) == 0 {
		config = []byte("{}")
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO service_ai_config(service_id, config, updated_at)
		 VALUES(?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(service_id) DO UPDATE SET
		   config     = excluded.config,
		   updated_at = CURRENT_TIMESTAMP`,
		serviceID, string(config),
	)
	if err != nil {
		return fmt.Errorf("upsert service ai config: %w", err)
	}
	return nil
}
