-- +goose Up
ALTER TABLE native_comments ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1));

-- +goose Down
ALTER TABLE native_comments DROP COLUMN deleted;
