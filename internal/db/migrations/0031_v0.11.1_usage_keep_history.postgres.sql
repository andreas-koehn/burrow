-- migration 0031 (postgres)
-- Usage rows are history: they outlive the service they were metered for, as
-- the connection rollups do. The foreign key on service_id (migration 0004)
-- deleted them, and with them the spend that global budgets add up, when a
-- service was deleted. A row keeps the id of its service; readers show an id
-- that names no service as unknown.
-- +goose Up
DO $$
DECLARE c text;
BEGIN
  FOR c IN SELECT conname FROM pg_constraint
           WHERE conrelid = 'usage_events'::regclass AND contype = 'f' AND confrelid = 'services'::regclass
  LOOP
    EXECUTE format('ALTER TABLE usage_events DROP CONSTRAINT %I', c);
  END LOOP;
END $$;

-- +goose Down
-- Rows of services that are gone cannot keep a foreign key: they are dropped.
DELETE FROM usage_events WHERE service_id NOT IN (SELECT id FROM services);
ALTER TABLE usage_events ADD CONSTRAINT usage_events_service_id_fkey
    FOREIGN KEY (service_id) REFERENCES services(id) ON DELETE CASCADE;
