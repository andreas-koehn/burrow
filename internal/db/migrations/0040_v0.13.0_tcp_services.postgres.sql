-- +goose Up
-- A tcp tunnel is a service like an http one (type 'tcp'). tcp_port is the
-- public port the relay gave it last, so that a reconnect gets the same one;
-- NULL until it was given one, and for http services. A port belongs to one
-- service at a time. tcp_port_pinned: the client asked for that port (a
-- fixed --remote); an automatic port (0) may be handed to another service
-- while this one is offline, a pinned one only goes with the service or a
-- new request of its own. max_connections caps the concurrent visitors of the
-- service's port; NULL is the relay's default.
ALTER TABLE services ADD COLUMN tcp_port        INTEGER;
ALTER TABLE services ADD COLUMN tcp_port_pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE services ADD COLUMN max_connections INTEGER;
CREATE UNIQUE INDEX idx_services_tcp_port ON services(tcp_port) WHERE tcp_port IS NOT NULL;

-- Just-in-time grants of a tcp service in mode ip: a CIDR (or a bare IP)
-- that may connect until expires_at, next to the permanent lists in
-- service_ip_geo.
CREATE TABLE service_ip_grants (
    id         TEXT PRIMARY KEY,
    service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    cidr       TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_service_ip_grants_service ON service_ip_grants(service_id, expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_service_ip_grants_service;
DROP TABLE IF EXISTS service_ip_grants;
DROP INDEX IF EXISTS idx_services_tcp_port;
ALTER TABLE services DROP COLUMN IF EXISTS max_connections;
ALTER TABLE services DROP COLUMN IF EXISTS tcp_port_pinned;
ALTER TABLE services DROP COLUMN IF EXISTS tcp_port;
