-- +goose Up
CREATE TABLE ai_models (
  name                   TEXT PRIMARY KEY,
  description            TEXT NOT NULL DEFAULT '',
  enabled                INTEGER NOT NULL DEFAULT 1,
  fallback_on_rate_limit INTEGER NOT NULL DEFAULT 0,
  attempt_timeout_s      INTEGER NOT NULL DEFAULT 60,
  total_timeout_s        INTEGER NOT NULL DEFAULT 120,
  created_at             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE ai_model_targets (
  model_name    TEXT NOT NULL REFERENCES ai_models(name) ON DELETE CASCADE ON UPDATE CASCADE,
  dialect       TEXT NOT NULL,
  position      INTEGER NOT NULL,
  provider_slug TEXT NOT NULL REFERENCES ai_providers(slug) ON UPDATE CASCADE,
  target_model  TEXT NOT NULL,
  PRIMARY KEY (model_name, dialect, position)
);
CREATE INDEX idx_ai_model_targets_provider ON ai_model_targets(provider_slug);

CREATE TABLE ai_gateway_keys (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,
  key_hash       TEXT NOT NULL UNIQUE,
  key_prefix     TEXT NOT NULL,
  user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  allowed_models TEXT NOT NULL DEFAULT '[]',
  last_used      DATETIME,
  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  revoked_at     DATETIME
);
CREATE INDEX idx_ai_gateway_keys_user ON ai_gateway_keys(user_id);

CREATE TABLE usage_attempts (
  request_id    TEXT NOT NULL,
  position      INTEGER NOT NULL,
  ts            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  provider_slug TEXT NOT NULL,
  target_model  TEXT NOT NULL,
  status        INTEGER NOT NULL DEFAULT 0,
  error_code    TEXT NOT NULL DEFAULT '',
  duration_ms   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (request_id, position)
);
CREATE INDEX idx_usage_attempts_ts ON usage_attempts(ts);

ALTER TABLE usage_events ADD COLUMN gateway_key_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN dialect         TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN provider_slug   TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN requested_model TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN target_model    TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN request_id      TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN latency_ms      INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_usage_events_gateway_key ON usage_events(gateway_key_id, ts);

ALTER TABLE services ADD COLUMN gateway_only INTEGER NOT NULL DEFAULT 0;
ALTER TABLE budgets  ADD COLUMN daily_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ai_providers ADD COLUMN supports_responses INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ai_providers ADD COLUMN max_concurrent     INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE ai_providers DROP COLUMN max_concurrent;
ALTER TABLE ai_providers DROP COLUMN supports_responses;
ALTER TABLE budgets  DROP COLUMN daily_tokens;
ALTER TABLE services DROP COLUMN gateway_only;
DROP INDEX idx_usage_events_gateway_key;
ALTER TABLE usage_events DROP COLUMN latency_ms;
ALTER TABLE usage_events DROP COLUMN request_id;
ALTER TABLE usage_events DROP COLUMN target_model;
ALTER TABLE usage_events DROP COLUMN requested_model;
ALTER TABLE usage_events DROP COLUMN provider_slug;
ALTER TABLE usage_events DROP COLUMN dialect;
ALTER TABLE usage_events DROP COLUMN gateway_key_id;
DROP TABLE usage_attempts;
DROP TABLE ai_gateway_keys;
DROP TABLE ai_model_targets;
DROP TABLE ai_models;
