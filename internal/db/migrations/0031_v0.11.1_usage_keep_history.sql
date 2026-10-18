-- migration 0031 (sqlite)
-- Usage rows are history: they outlive the service they were metered for, as
-- the connection rollups do. The foreign key on service_id (migration 0004)
-- deleted them, and with them the spend that global budgets add up, when a
-- service was deleted. A row keeps the id of its service; readers show an id
-- that names no service as unknown.
-- +goose Up
-- SQLite cannot drop a constraint: the table is rebuilt with every column and index.
CREATE TABLE usage_events_new (
    id              TEXT PRIMARY KEY,
    service_id      TEXT NOT NULL,
    api_key_id      TEXT NOT NULL DEFAULT '',
    ts              DATETIME NOT NULL,
    kind            TEXT NOT NULL,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    bytes_in        INTEGER NOT NULL DEFAULT 0,
    bytes_out       INTEGER NOT NULL DEFAULT 0,
    streamed        INTEGER NOT NULL DEFAULT 0,
    cache_hit       INTEGER NOT NULL DEFAULT 0,
    upstream_status INTEGER NOT NULL DEFAULT 0,
    cost_usd        REAL,
    gateway_key_id  TEXT NOT NULL DEFAULT '',
    dialect         TEXT NOT NULL DEFAULT '',
    provider_slug   TEXT NOT NULL DEFAULT '',
    requested_model TEXT NOT NULL DEFAULT '',
    target_model    TEXT NOT NULL DEFAULT '',
    request_id      TEXT NOT NULL DEFAULT '',
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    translated      TEXT NOT NULL DEFAULT '',
    dropped         TEXT NOT NULL DEFAULT '',
    purpose         TEXT NOT NULL DEFAULT ''
);
INSERT INTO usage_events_new (id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, bytes_in, bytes_out, streamed, cache_hit, upstream_status, cost_usd, gateway_key_id, dialect, provider_slug, requested_model, target_model, request_id, latency_ms, translated, dropped, purpose)
    SELECT id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, bytes_in, bytes_out, streamed, cache_hit, upstream_status, cost_usd, gateway_key_id, dialect, provider_slug, requested_model, target_model, request_id, latency_ms, translated, dropped, purpose FROM usage_events;
DROP TABLE usage_events;
ALTER TABLE usage_events_new RENAME TO usage_events;
CREATE INDEX usage_events_service_ts ON usage_events(service_id, ts DESC);
CREATE INDEX usage_events_api_key_ts ON usage_events(api_key_id, ts DESC);
CREATE INDEX idx_usage_events_requested_model ON usage_events(requested_model, ts);
CREATE INDEX idx_usage_events_ts ON usage_events(ts);
CREATE INDEX idx_usage_events_gateway_key ON usage_events(gateway_key_id, ts);

-- +goose Down
-- Rows of services that are gone cannot keep a foreign key: they are dropped.
CREATE TABLE usage_events_new (
    id              TEXT PRIMARY KEY,
    service_id      TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    api_key_id      TEXT NOT NULL DEFAULT '',
    ts              DATETIME NOT NULL,
    kind            TEXT NOT NULL,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    bytes_in        INTEGER NOT NULL DEFAULT 0,
    bytes_out       INTEGER NOT NULL DEFAULT 0,
    streamed        INTEGER NOT NULL DEFAULT 0,
    cache_hit       INTEGER NOT NULL DEFAULT 0,
    upstream_status INTEGER NOT NULL DEFAULT 0,
    cost_usd        REAL,
    gateway_key_id  TEXT NOT NULL DEFAULT '',
    dialect         TEXT NOT NULL DEFAULT '',
    provider_slug   TEXT NOT NULL DEFAULT '',
    requested_model TEXT NOT NULL DEFAULT '',
    target_model    TEXT NOT NULL DEFAULT '',
    request_id      TEXT NOT NULL DEFAULT '',
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    translated      TEXT NOT NULL DEFAULT '',
    dropped         TEXT NOT NULL DEFAULT '',
    purpose         TEXT NOT NULL DEFAULT ''
);
INSERT INTO usage_events_new (id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, bytes_in, bytes_out, streamed, cache_hit, upstream_status, cost_usd, gateway_key_id, dialect, provider_slug, requested_model, target_model, request_id, latency_ms, translated, dropped, purpose)
    SELECT id, service_id, api_key_id, ts, kind, tokens_in, tokens_out, bytes_in, bytes_out, streamed, cache_hit, upstream_status, cost_usd, gateway_key_id, dialect, provider_slug, requested_model, target_model, request_id, latency_ms, translated, dropped, purpose FROM usage_events WHERE service_id IN (SELECT id FROM services);
DROP TABLE usage_events;
ALTER TABLE usage_events_new RENAME TO usage_events;
CREATE INDEX usage_events_service_ts ON usage_events(service_id, ts DESC);
CREATE INDEX usage_events_api_key_ts ON usage_events(api_key_id, ts DESC);
CREATE INDEX idx_usage_events_requested_model ON usage_events(requested_model, ts);
CREATE INDEX idx_usage_events_ts ON usage_events(ts);
CREATE INDEX idx_usage_events_gateway_key ON usage_events(gateway_key_id, ts);
