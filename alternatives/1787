-- +goose Up
ALTER TABLE conversations ADD COLUMN read_audience TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE conversations DROP COLUMN read_audience;
