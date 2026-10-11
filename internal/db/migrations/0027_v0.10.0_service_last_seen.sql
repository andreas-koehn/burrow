-- internal/db/migrations/0027_v0.10.0_service_last_seen.sql
-- +goose Up
-- When a client last served a service, and the name of the client token it
-- logged in with. last_seen stays NULL for a service no client has served.
ALTER TABLE services ADD COLUMN last_seen   TIMESTAMP;
ALTER TABLE services ADD COLUMN last_client TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE services DROP COLUMN last_client;
ALTER TABLE services DROP COLUMN last_seen;
