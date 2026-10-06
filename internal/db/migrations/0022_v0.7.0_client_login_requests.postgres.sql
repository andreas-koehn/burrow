-- +goose Up
-- Browser-approved client sign-in (the shape of RFC 8628). Only the hash of
-- the device code is stored; the client token is minted when the approved
-- request is collected and is never written here.
CREATE TABLE client_login_requests (
  device_code_hash TEXT PRIMARY KEY,
  user_code        TEXT NOT NULL UNIQUE,
  hostname         TEXT NOT NULL DEFAULT '',
  os               TEXT NOT NULL DEFAULT '',
  arch             TEXT NOT NULL DEFAULT '',
  client_version   TEXT NOT NULL DEFAULT '',
  source_ip        TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL DEFAULT 'pending',
  approved_by      TEXT REFERENCES users(id) ON DELETE CASCADE,
  token_name       TEXT NOT NULL DEFAULT '',
  last_poll_at     TIMESTAMP WITH TIME ZONE,
  created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
  expires_at       TIMESTAMP WITH TIME ZONE NOT NULL
);
CREATE INDEX idx_client_login_expires ON client_login_requests(expires_at);

-- +goose Down
DROP TABLE client_login_requests;
