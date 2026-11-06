-- +goose Up
ALTER TABLE native_board_items ADD COLUMN execution_fence INTEGER NOT NULL DEFAULT 0;
CREATE INDEX native_attempts_running_repair ON native_attempts(id,lease_id) WHERE status='running';

CREATE INDEX native_board_running_scope ON native_board_items(organization_id,project_id,archived,state,work_item_id) WHERE running=1;

-- +goose Down
DROP INDEX native_board_running_scope;
DROP INDEX native_attempts_running_repair;
ALTER TABLE native_board_items DROP COLUMN execution_fence;
