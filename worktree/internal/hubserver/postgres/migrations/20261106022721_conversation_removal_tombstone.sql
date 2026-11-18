-- +goose Up
ALTER TABLE conversations ADD COLUMN deleted_at timestamptz;

-- +goose Down
ALTER TABLE conversations DROP COLUMN deleted_at;
