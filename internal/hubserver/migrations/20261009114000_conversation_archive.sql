-- +goose Up
ALTER TABLE conversations ADD COLUMN archived INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1));

-- +goose Down
ALTER TABLE conversations DROP COLUMN archived;
