-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts ADD COLUMN worker_cleanup_required boolean;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts DROP COLUMN worker_cleanup_required;
