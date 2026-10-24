-- +goose Up
ALTER TABLE projects ADD COLUMN deleted_at TEXT;

-- +goose Down
ALTER TABLE projects DROP COLUMN deleted_at;
