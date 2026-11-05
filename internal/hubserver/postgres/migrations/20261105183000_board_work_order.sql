-- +goose Up
ALTER TABLE native_board_items ADD COLUMN issue_number bigint;
CREATE INDEX native_board_items_work_order ON native_board_items
 (organization_id, project_id, running DESC, dispatchable DESC, issue_number DESC, identifier);
CREATE INDEX native_board_items_number_pending ON native_board_items
 (organization_id, project_id) WHERE issue_number IS NULL;

-- +goose Down
DROP INDEX native_board_items_number_pending;
DROP INDEX native_board_items_work_order;
ALTER TABLE native_board_items DROP COLUMN issue_number;
