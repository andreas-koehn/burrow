-- +goose Up
-- tcp complete (v0.13.0): a tcp service may be reached by its host name
-- only (TLS passthrough by SNI), without a public port of its own.
-- tcp_hostname_only: 1 = the relay opens no public port for the service
-- while it is reachable by a host name on the passthrough listener; 0 (every
-- row so far) = by port, and by host name when it has one.
ALTER TABLE services ADD COLUMN tcp_hostname_only INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE services DROP COLUMN IF EXISTS tcp_hostname_only;
