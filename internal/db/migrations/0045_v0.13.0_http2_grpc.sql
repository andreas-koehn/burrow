-- +goose Up
-- http/2, gRPC and certificate source (v0.13.0, round 3).
--
-- services:
--   upstream_protocol: how the relay speaks to the local service over the
--     tunnel: http1 (default), https, h2c or h2 (checked in Go).
--   upstream_tls_verify: 1 = verify the local service's certificate (https
--     and h2 only); 0 = do not.
--   upstream_tls_server_name: the TLS server name to verify and send; NULL
--     takes it from the local host header.
--   upstream_tls_ca_pem: the CA that verification is against; NULL for the
--     system roots.
--   grpc_max_duration_s: caps every gRPC call, streams included; 0 is off.
-- service_custom_domains.cert_source: 'uploaded' (certificate and key are
--   stored) or 'auto' (the relay obtains the certificate; the certificate
--   columns stay empty).
-- connection_logs: the method and the grpc-status of a gRPC call; NULL for
--   any other row.
ALTER TABLE services ADD COLUMN upstream_protocol TEXT NOT NULL DEFAULT 'http1';
ALTER TABLE services ADD COLUMN upstream_tls_verify INTEGER NOT NULL DEFAULT 0;
ALTER TABLE services ADD COLUMN upstream_tls_server_name TEXT;
ALTER TABLE services ADD COLUMN upstream_tls_ca_pem TEXT;
ALTER TABLE services ADD COLUMN grpc_max_duration_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE service_custom_domains ADD COLUMN cert_source TEXT NOT NULL DEFAULT 'uploaded';
ALTER TABLE connection_logs ADD COLUMN grpc_method TEXT;
ALTER TABLE connection_logs ADD COLUMN grpc_status INTEGER;

-- +goose Down
ALTER TABLE connection_logs DROP COLUMN grpc_status;
ALTER TABLE connection_logs DROP COLUMN grpc_method;
ALTER TABLE service_custom_domains DROP COLUMN cert_source;
ALTER TABLE services DROP COLUMN grpc_max_duration_s;
ALTER TABLE services DROP COLUMN upstream_tls_ca_pem;
ALTER TABLE services DROP COLUMN upstream_tls_server_name;
ALTER TABLE services DROP COLUMN upstream_tls_verify;
ALTER TABLE services DROP COLUMN upstream_protocol;
