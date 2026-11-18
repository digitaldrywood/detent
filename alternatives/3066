-- +goose Up
ALTER TABLE native_board_dirty ADD COLUMN queued_at timestamptz NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE native_board_dirty DROP COLUMN queued_at;
