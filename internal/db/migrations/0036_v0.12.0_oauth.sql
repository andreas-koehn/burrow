-- internal/db/migrations/0036_v0.12.0_oauth.sql
-- +goose Up
-- Burrow as the OAuth 2.1 authorization server of its MCP resources (spec
-- D19-D25). Public clients only: a client is pre-registered here by an admin
-- or identified by the https URL of its metadata document (never stored).
CREATE TABLE oauth_clients (
  client_id     TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  redirect_uris TEXT NOT NULL DEFAULT '[]',
  created_by    TEXT NOT NULL DEFAULT '',
  created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- A grant is one user's consent for one client at one resource (audience).
-- It is created when the client exchanges its authorization code, carries
-- the scopes and, at a gateway resource, the tools the user chose, and ends
-- with revoked_at (by the user, an admin, the client, or refresh reuse).
CREATE TABLE oauth_grants (
  id              TEXT PRIMARY KEY,
  client_id       TEXT NOT NULL,
  client_name     TEXT NOT NULL,
  user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  resource        TEXT NOT NULL,
  scopes          TEXT NOT NULL DEFAULT '[]',
  allowed_tools   TEXT NOT NULL DEFAULT '[]',
  tools_read_only INTEGER NOT NULL DEFAULT 0,
  created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_used       DATETIME,
  revoked_at      DATETIME
);
CREATE INDEX idx_oauth_grants_user ON oauth_grants(user_id);
CREATE INDEX idx_oauth_grants_client ON oauth_grants(client_id);

-- Authorization codes: sha256 of the code only, 60 s, single use. The grant
-- fields wait here until the exchange; grant_id names the grant the code was
-- exchanged for, so that a second use can revoke it.
CREATE TABLE oauth_codes (
  code_hash       TEXT PRIMARY KEY,
  client_id       TEXT NOT NULL,
  client_name     TEXT NOT NULL,
  user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  redirect_uri    TEXT NOT NULL,
  code_challenge  TEXT NOT NULL,
  resource        TEXT NOT NULL,
  scopes          TEXT NOT NULL DEFAULT '[]',
  allowed_tools   TEXT NOT NULL DEFAULT '[]',
  tools_read_only INTEGER NOT NULL DEFAULT 0,
  expires_at      DATETIME NOT NULL,
  used            INTEGER NOT NULL DEFAULT 0,
  grant_id        TEXT NOT NULL DEFAULT '',
  created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Access (boa_, 1 h) and refresh (bor_, 30 d) tokens: sha256 only. A refresh
-- token is used once (used_at); its family is the grant's.
CREATE TABLE oauth_tokens (
  token_hash TEXT PRIMARY KEY,
  kind       TEXT NOT NULL,
  grant_id   TEXT NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
  family_id  TEXT NOT NULL,
  expires_at DATETIME NOT NULL,
  used_at    DATETIME,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_oauth_tokens_grant ON oauth_tokens(grant_id);

-- +goose Down
DROP TABLE oauth_tokens;
DROP TABLE oauth_codes;
DROP TABLE oauth_grants;
DROP TABLE oauth_clients;
