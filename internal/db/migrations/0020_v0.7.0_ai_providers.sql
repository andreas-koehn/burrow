-- internal/db/migrations/0020_v0.7.0_ai_providers.sql
-- +goose Up
CREATE TABLE ai_providers (
  slug        TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL DEFAULT 'tunnel',
  service_id  TEXT NOT NULL UNIQUE REFERENCES services(id) ON DELETE CASCADE,
  api_format  TEXT NOT NULL DEFAULT 'openai',
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE ai_providers;
