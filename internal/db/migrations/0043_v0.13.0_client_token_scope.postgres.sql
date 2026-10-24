-- +goose Up
-- What a client token may do: all (register tunnels and open forwards),
-- forward (open forwards only) or publish (register tunnels only). Every
-- token made before this keeps doing both. A sign-in request carries the scope
-- the client wished for and the approver chose.
ALTER TABLE client_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'all' CHECK (scope IN ('all','forward','publish'));
ALTER TABLE client_login_requests ADD COLUMN scope TEXT NOT NULL DEFAULT 'all' CHECK (scope IN ('all','forward','publish'));

-- +goose Down
ALTER TABLE client_login_requests DROP COLUMN scope;
ALTER TABLE client_tokens DROP COLUMN scope;
