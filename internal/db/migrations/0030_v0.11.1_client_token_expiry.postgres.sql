-- +goose Up
-- When a client token stops working. NULL means it never does, which is what
-- every token made before this had.
ALTER TABLE client_tokens ADD COLUMN expires_at TIMESTAMP WITH TIME ZONE;

-- +goose Down
ALTER TABLE client_tokens DROP COLUMN IF EXISTS expires_at;
