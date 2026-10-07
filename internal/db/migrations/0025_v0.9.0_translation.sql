-- internal/db/migrations/0025_v0.9.0_translation.sql
-- +goose Up
ALTER TABLE ai_models    ADD COLUMN translate  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_events ADD COLUMN translated TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN dropped    TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE usage_events DROP COLUMN dropped;
ALTER TABLE usage_events DROP COLUMN translated;
ALTER TABLE ai_models    DROP COLUMN translate;
