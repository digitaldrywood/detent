-- +goose Up
ALTER TABLE attempt_diffs ADD COLUMN manifest_expired boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE attempt_diffs DROP COLUMN manifest_expired;
