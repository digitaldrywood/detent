-- +goose Up
ALTER TABLE merge_required_check_streaks
ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE merge_required_check_streaks
DROP COLUMN head_sha;
