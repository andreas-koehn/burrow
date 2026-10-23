-- +goose Up
-- A forward stream (a signed-in client reaching a tcp service through the
-- relay) is a connection-log row of kind tcp_forward whose user is the one
-- who forwards; token_name is the name of their client token. NULL for
-- every other row.
ALTER TABLE connection_logs ADD COLUMN token_name TEXT;

-- Every tcp service has a slug: the global name `burrow forward <slug>`
-- reaches it by. The proxy never routes one over HTTP. Rows from before get
-- a generated one ('t' and ten hex digits; new rows get the usual six
-- characters).
UPDATE services SET subdomain = 't' || substr(md5(random()::text || id), 1, 10)
 WHERE type = 'tcp' AND (subdomain IS NULL OR subdomain = '');

-- +goose Down
UPDATE services SET subdomain = NULL WHERE type = 'tcp';
ALTER TABLE connection_logs DROP COLUMN token_name;
