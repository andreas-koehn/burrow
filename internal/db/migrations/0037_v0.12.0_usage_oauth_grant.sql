-- +goose Up
-- A tool call made with an OAuth grant's access token (MCP gateway, D23) is
-- metered under the grant, not a gateway key: gateway_key_id stays empty and
-- oauth_grant_id names the grant. Empty for every other row.
ALTER TABLE usage_events ADD COLUMN oauth_grant_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE usage_events DROP COLUMN oauth_grant_id;
