-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY oauth_grants_identity ON platform.oauth_grants(id);

-- +goose Down
DROP INDEX CONCURRENTLY platform.oauth_grants_identity;
