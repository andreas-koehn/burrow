-- internal/db/migrations/0033_v0.12.0_mcp_sources.sql
-- +goose Up
-- Tool sources of the MCP gateway. A remote source is an https MCP server the
-- relay calls; a tunnel source is an MCP server behind a client. Either has a
-- backing service row (a hidden type 'direct' row for a remote source, an
-- existing http service for a tunnel source), so inspector, usage and quota
-- keep working on service ids. credential_slot names a vault slot, never a
-- value.
CREATE TABLE mcp_sources (
  slug             TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  kind             TEXT NOT NULL,
  service_id       TEXT NOT NULL UNIQUE REFERENCES services(id) ON DELETE CASCADE,
  url              TEXT NOT NULL DEFAULT '',
  credential_slot  TEXT NOT NULL DEFAULT '',
  auth_header      TEXT NOT NULL DEFAULT 'Authorization',
  auth_format      TEXT NOT NULL DEFAULT 'Bearer {key}',
  protocol_version TEXT NOT NULL DEFAULT '',
  rpm              INTEGER NOT NULL DEFAULT 0,
  max_concurrent   INTEGER NOT NULL DEFAULT 0,
  timeout_s        INTEGER NOT NULL DEFAULT 60,
  enabled          INTEGER NOT NULL DEFAULT 1,
  last_sync_at     DATETIME,
  last_sync_error  TEXT NOT NULL DEFAULT '',
  created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- The tools a source offers, filled by the sync. definition is the JSON of the
-- current upstream tool, approved_definition the one an admin approved; a
-- tool serves only while status is 'approved'. rename, description_override
-- and read_only (NULL = the upstream hint) are the operator's overrides.
CREATE TABLE mcp_tools (
  source_slug          TEXT NOT NULL REFERENCES mcp_sources(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  name                 TEXT NOT NULL,
  approved_hash        TEXT NOT NULL DEFAULT '',
  current_hash         TEXT NOT NULL DEFAULT '',
  definition           TEXT NOT NULL DEFAULT '{}',
  approved_definition  TEXT NOT NULL DEFAULT '',
  status               TEXT NOT NULL DEFAULT 'pending',
  hidden               INTEGER NOT NULL DEFAULT 0,
  rename               TEXT NOT NULL DEFAULT '',
  description_override TEXT NOT NULL DEFAULT '',
  read_only            INTEGER,
  guardrail_hits       TEXT NOT NULL DEFAULT '[]',
  updated_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (source_slug, name)
);

-- +goose Down
DROP TABLE mcp_tools;
DROP TABLE mcp_sources;
