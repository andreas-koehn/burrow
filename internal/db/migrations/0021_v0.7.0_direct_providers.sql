-- internal/db/migrations/0021_v0.7.0_direct_providers.sql
-- +goose Up
ALTER TABLE ai_providers ADD COLUMN base_url        TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_providers ADD COLUMN credential_slot TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_providers ADD COLUMN auth_header     TEXT NOT NULL DEFAULT 'Authorization';
ALTER TABLE ai_providers ADD COLUMN auth_format     TEXT NOT NULL DEFAULT 'Bearer {key}';
ALTER TABLE ai_providers ADD COLUMN extra_headers   TEXT NOT NULL DEFAULT '{}';
ALTER TABLE ai_providers ADD COLUMN billing         TEXT NOT NULL DEFAULT 'metered';

CREATE TABLE ai_provider_models (
  provider_slug  TEXT NOT NULL REFERENCES ai_providers(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  model_id       TEXT NOT NULL,
  display_name   TEXT NOT NULL DEFAULT '',
  context_length INTEGER NOT NULL DEFAULT 0,
  synced_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (provider_slug, model_id)
);

ALTER TABLE usage_events ADD COLUMN cost_usd REAL;

-- +goose Down
ALTER TABLE usage_events DROP COLUMN cost_usd;
DROP TABLE ai_provider_models;
ALTER TABLE ai_providers DROP COLUMN billing;
ALTER TABLE ai_providers DROP COLUMN extra_headers;
ALTER TABLE ai_providers DROP COLUMN auth_format;
ALTER TABLE ai_providers DROP COLUMN auth_header;
ALTER TABLE ai_providers DROP COLUMN credential_slot;
ALTER TABLE ai_providers DROP COLUMN base_url;
