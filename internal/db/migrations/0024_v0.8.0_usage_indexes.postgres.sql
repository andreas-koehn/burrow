-- internal/db/migrations/0024_v0.8.0_usage_indexes.postgres.sql
-- Indexes for the usage reads that run per request or per dashboard load:
-- the day sum of a requested model (model rate limits) and the window
-- aggregation of the cost engine (budgets, the budget guard, cost summary).
-- +goose Up
CREATE INDEX idx_usage_events_requested_model ON usage_events(requested_model, ts);
CREATE INDEX idx_usage_events_ts ON usage_events(ts);

-- +goose Down
DROP INDEX idx_usage_events_ts;
DROP INDEX idx_usage_events_requested_model;
