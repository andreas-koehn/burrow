-- +goose Up
-- tcp complete (v0.13.0, round 2): the per-service tcp settings and their
-- state. NULL or 0 is "off" for each of them.
--
-- tcp_hostname: the host name a TLS client reaches the service by through
--   the relay's TLS passthrough listener (SNI); unique, NULL for none.
-- proxy_protocol: '' or 'v2', the PROXY protocol header the home client
--   writes to its target.
-- idle_timeout_s: close a connection with no byte for that long; 0 never,
--   else 60 to 86400.
-- rate_limit_kbps: the bandwidth of all the service's connections, both
--   ways; 0 none.
-- daily_bytes: the bytes in and out a UTC day may carry; 0 none.
-- tcp_autoban: the automatic ban settings as JSON; NULL is off.
ALTER TABLE services ADD COLUMN tcp_hostname    TEXT;
ALTER TABLE services ADD COLUMN proxy_protocol  TEXT NOT NULL DEFAULT '';
ALTER TABLE services ADD COLUMN idle_timeout_s  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE services ADD COLUMN rate_limit_kbps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE services ADD COLUMN daily_bytes     BIGINT NOT NULL DEFAULT 0;
ALTER TABLE services ADD COLUMN tcp_autoban     TEXT;
CREATE UNIQUE INDEX idx_services_tcp_hostname ON services(tcp_hostname) WHERE tcp_hostname IS NOT NULL;

-- A source (an address or an IPv6 /64) banned from a tcp service until
-- "until"; kept so that bans survive a restart.
CREATE TABLE tcp_bans (
    service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    source     TEXT NOT NULL,
    until      TIMESTAMPTZ NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (service_id, source)
);
CREATE INDEX idx_tcp_bans_until ON tcp_bans(until);

-- The bytes a tcp service carried per UTC day (YYYY-MM-DD).
CREATE TABLE tcp_daily_usage (
    service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    day        TEXT NOT NULL,
    bytes_in   BIGINT NOT NULL DEFAULT 0,
    bytes_out  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (service_id, day)
);

-- What the first bytes of a tcp connection were (tls, ssh, rdp, http,
-- unknown) and the server name of a TLS ClientHello. NULL for rows of other
-- kinds and for connections whose bytes were never looked at.
ALTER TABLE connection_logs ADD COLUMN sni      TEXT;
ALTER TABLE connection_logs ADD COLUMN protocol TEXT;

-- +goose Down
ALTER TABLE connection_logs DROP COLUMN IF EXISTS protocol;
ALTER TABLE connection_logs DROP COLUMN IF EXISTS sni;
DROP TABLE IF EXISTS tcp_daily_usage;
DROP INDEX IF EXISTS idx_tcp_bans_until;
DROP TABLE IF EXISTS tcp_bans;
DROP INDEX IF EXISTS idx_services_tcp_hostname;
ALTER TABLE services DROP COLUMN IF EXISTS tcp_autoban;
ALTER TABLE services DROP COLUMN IF EXISTS daily_bytes;
ALTER TABLE services DROP COLUMN IF EXISTS rate_limit_kbps;
ALTER TABLE services DROP COLUMN IF EXISTS idle_timeout_s;
ALTER TABLE services DROP COLUMN IF EXISTS proxy_protocol;
ALTER TABLE services DROP COLUMN IF EXISTS tcp_hostname;
