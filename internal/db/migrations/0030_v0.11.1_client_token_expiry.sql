-- +goose Up
-- When a client token stops working. NULL means it never does, which is what
-- every token made before this had.
ALTER TABLE client_tokens ADD COLUMN expires_at DATETIME;
CREATE INDEX idx_client_tokens_expires_at ON client_tokens (expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_client_tokens_expires_at;
ALTER TABLE client_tokens DROP COLUMN expires_at;
