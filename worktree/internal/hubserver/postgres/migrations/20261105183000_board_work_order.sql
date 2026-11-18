-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_board_items ADD COLUMN issue_number bigint;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE native_board_items DROP COLUMN issue_number;
