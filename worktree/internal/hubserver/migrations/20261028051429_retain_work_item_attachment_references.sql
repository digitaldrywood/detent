-- +goose Up
ALTER TABLE attachment_references ADD COLUMN retain_with_work_item INTEGER NOT NULL DEFAULT 0 CHECK (retain_with_work_item IN (0,1));

-- +goose Down
ALTER TABLE attachment_references DROP COLUMN retain_with_work_item;
