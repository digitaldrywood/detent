-- +goose Up
-- Legacy ages remain unknown until the original comment is reconciled.
ALTER TABLE human_questions ADD COLUMN asked_at TEXT;

-- +goose Down
ALTER TABLE human_questions DROP COLUMN asked_at;
