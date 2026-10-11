-- internal/db/migrations/0026_v0.10.0_client_last_session.postgres.sql
-- +goose Up
-- What the relay remembers of the last session of a client token. The values
-- come from the client and are display text only. last_served holds the names
-- of the services the session served, joined by a newline: RecordClientSession
-- strips control characters from every name before it joins them.
ALTER TABLE client_tokens ADD COLUMN last_seen           TIMESTAMPTZ;
ALTER TABLE client_tokens ADD COLUMN last_os             TEXT NOT NULL DEFAULT '';
ALTER TABLE client_tokens ADD COLUMN last_arch           TEXT NOT NULL DEFAULT '';
ALTER TABLE client_tokens ADD COLUMN last_client_version TEXT NOT NULL DEFAULT '';
ALTER TABLE client_tokens ADD COLUMN last_remote_addr    TEXT NOT NULL DEFAULT '';
ALTER TABLE client_tokens ADD COLUMN last_served         TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE client_tokens DROP COLUMN last_served;
ALTER TABLE client_tokens DROP COLUMN last_remote_addr;
ALTER TABLE client_tokens DROP COLUMN last_client_version;
ALTER TABLE client_tokens DROP COLUMN last_arch;
ALTER TABLE client_tokens DROP COLUMN last_os;
ALTER TABLE client_tokens DROP COLUMN last_seen;
