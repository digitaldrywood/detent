-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_identities
    ADD COLUMN capacity_request_revision text NOT NULL DEFAULT '',
    ADD COLUMN capacity_request_limit integer NOT NULL DEFAULT 0,
    ADD COLUMN capacity_request_backend text NOT NULL DEFAULT '',
    ADD CONSTRAINT runner_capacity_request_complete CHECK (
        (capacity_request_revision = '' AND capacity_request_limit = 0 AND capacity_request_backend = '')
        OR (capacity_request_revision ~ '^[0-9a-f]{64}$' AND capacity_request_limit BETWEEN 1 AND 10000)
    ) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_identities
    DROP CONSTRAINT runner_capacity_request_complete,
    DROP COLUMN capacity_request_backend,
    DROP COLUMN capacity_request_limit,
    DROP COLUMN capacity_request_revision;
