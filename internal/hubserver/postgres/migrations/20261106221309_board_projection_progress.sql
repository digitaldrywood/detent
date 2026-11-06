-- +goose Up
ALTER TABLE native_board_dirty ADD COLUMN queued_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX native_board_dirty_oldest ON native_board_dirty (organization_id,project_id,queued_at);

-- +goose Down
DROP INDEX native_board_dirty_oldest;
ALTER TABLE native_board_dirty DROP COLUMN queued_at;
