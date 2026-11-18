-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE machines ADD COLUMN routing_revision bigint NOT NULL DEFAULT 1;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE machines DROP COLUMN routing_revision;
