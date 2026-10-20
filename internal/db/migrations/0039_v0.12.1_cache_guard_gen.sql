-- +goose Up
-- The answer inspection an exact-cache entry was stored under: a short hash
-- of the guardrail settings that read the answer (answer patterns, custom
-- answer patterns, a judge that checks answers), or '' when nothing read it.
-- An entry is served only under the same generation, or where answers are
-- not inspected at all: an answer cached while inspection was off, or under
-- other patterns, would otherwise be replayed without the check. Existing
-- rows get '' (stored uninspected, as far as anyone can tell).
ALTER TABLE cache_entries ADD COLUMN guard_gen TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE cache_entries DROP COLUMN guard_gen;
