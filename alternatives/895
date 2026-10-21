-- +goose Up
ALTER TABLE audit ADD COLUMN query_length INTEGER;

-- +goose Down
ALTER TABLE audit DROP COLUMN query_length;
