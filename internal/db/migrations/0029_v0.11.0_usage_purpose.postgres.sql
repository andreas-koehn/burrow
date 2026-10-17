-- internal/db/migrations/0029_v0.11.0_usage_purpose.postgres.sql
-- +goose Up
-- Why the relay itself made a request: 'guardrail-judge' for a call to the
-- judge model, '' for an app's request. The providers' request counts and
-- the p95 latency that fastest routing reads leave the judge's rows out.
ALTER TABLE usage_events ADD COLUMN purpose TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE usage_events DROP COLUMN IF EXISTS purpose;
