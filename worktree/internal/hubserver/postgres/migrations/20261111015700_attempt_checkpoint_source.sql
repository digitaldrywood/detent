-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts ADD COLUMN checkpoint_preserves_source boolean;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_attempts DROP COLUMN checkpoint_preserves_source;
