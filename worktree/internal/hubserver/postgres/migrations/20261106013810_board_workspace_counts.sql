-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_board_counts ADD COLUMN workspace_total bigint NOT NULL DEFAULT 0;
ALTER TABLE native_board_counts ADD CONSTRAINT native_board_workspace_total_nonnegative CHECK (workspace_total >= 0) NOT VALID;
ALTER TABLE native_board_items ADD COLUMN workspace_counted boolean NOT NULL DEFAULT false;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_board_items DROP COLUMN workspace_counted;
ALTER TABLE native_board_counts DROP COLUMN workspace_total;
