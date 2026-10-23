-- +goose Up
ALTER TABLE project_dispatch_status
ADD COLUMN wait_reason_code TEXT;

-- +goose Down
ALTER TABLE project_dispatch_status
DROP COLUMN wait_reason_code;
