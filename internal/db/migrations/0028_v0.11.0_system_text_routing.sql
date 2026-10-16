-- internal/db/migrations/0028_v0.11.0_system_text_routing.sql
-- +goose Up
-- A fixed system text per synthetic model and per gateway key, and the order
-- a model's targets are tried in: ordered, fastest or cheapest.
ALTER TABLE ai_models ADD COLUMN system_text TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_models ADD COLUMN routing TEXT NOT NULL DEFAULT 'ordered';
ALTER TABLE ai_gateway_keys ADD COLUMN system_text TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE ai_gateway_keys DROP COLUMN system_text;
ALTER TABLE ai_models DROP COLUMN routing;
ALTER TABLE ai_models DROP COLUMN system_text;
