-- +goose Up
-- A session is either the dashboard's (burrow_session on the dashboard's
-- host) or the services host's (burrow_svc_session, made by the sign-in
-- handoff). Each host accepts only its own kind. A services session names
-- the dashboard session that handed it off, so that signing out of the
-- dashboard, or revoking that session, ends it too. Existing rows become
-- dashboard sessions: a services session made before this version no longer
-- opens anything on the services host, whose visitor goes through the
-- handoff once more (its cookie is host-only there and never reaches the
-- dashboard).
ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'dashboard';
ALTER TABLE sessions ADD COLUMN parent_id TEXT;
CREATE INDEX idx_sessions_parent ON sessions(parent_id);

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_parent;
ALTER TABLE sessions DROP COLUMN IF EXISTS parent_id;
ALTER TABLE sessions DROP COLUMN IF EXISTS kind;
