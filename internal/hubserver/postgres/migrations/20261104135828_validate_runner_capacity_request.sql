-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_identities VALIDATE CONSTRAINT runner_capacity_request_complete;

-- +goose Down
SELECT 1;
