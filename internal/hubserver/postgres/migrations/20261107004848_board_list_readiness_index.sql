-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_list_pending_idx;
CREATE INDEX CONCURRENTLY native_board_list_pending_idx ON native_board_items (organization_id,project_id,work_item_id) WHERE NOT list_ready;
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS native_board_list_pending_idx;
