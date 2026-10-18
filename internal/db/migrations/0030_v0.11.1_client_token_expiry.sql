-- +goose Up
-- When a client token stops working. NULL means it never does, which is what
-- every token made before this had.
ALTER TABLE client_tokens ADD COLUMN expires_at DATETIME;

-- +goose Down
ALTER TABLE client_tokens DROP COLUMN expires_at;
