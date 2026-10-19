-- internal/db/migrations/0032_v0.12.0_gateway_key_tools.sql
-- +goose Up
-- Which MCP tools a gateway key may call. allowed_tools is a JSON array of
-- "<source>__<tool>" and "<source>__*" entries; unlike allowed_models an empty
-- list means no tools, so existing keys gain nothing. tools_read_only limits
-- the key to the tools the operator classed as read-only.
ALTER TABLE ai_gateway_keys ADD COLUMN allowed_tools TEXT NOT NULL DEFAULT '[]';
ALTER TABLE ai_gateway_keys ADD COLUMN tools_read_only INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE ai_gateway_keys DROP COLUMN tools_read_only;
ALTER TABLE ai_gateway_keys DROP COLUMN allowed_tools;
