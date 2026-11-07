-- +goose Up
ALTER TABLE native_pr_publications ALTER COLUMN source_attempt_id DROP NOT NULL;

-- +goose Down
ALTER TABLE native_pr_publications ALTER COLUMN source_attempt_id SET NOT NULL;
