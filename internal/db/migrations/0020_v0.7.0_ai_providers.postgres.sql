-- internal/db/migrations/0020_v0.7.0_ai_providers.postgres.sql
-- +goose Up
CREATE TABLE ai_providers (
  slug        TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL DEFAULT 'tunnel',
  service_id  TEXT NOT NULL UNIQUE REFERENCES services(id) ON DELETE CASCADE,
  api_format  TEXT NOT NULL DEFAULT 'openai',
  created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE ai_providers;
