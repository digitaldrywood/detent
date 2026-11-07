-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_page_pending_idx;
CREATE INDEX CONCURRENTLY native_board_page_pending_idx ON native_board_items (organization_id,project_id,work_item_id) WHERE NOT page_ready;
DROP INDEX CONCURRENTLY IF EXISTS native_board_page_refresh_due;
CREATE INDEX CONCURRENTLY native_board_page_refresh_due ON native_board_items (organization_id,page_refresh_after,work_item_id) WHERE page_refresh_after IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS native_board_dirty_oldest;
CREATE INDEX CONCURRENTLY native_board_dirty_oldest ON native_board_dirty (organization_id,project_id,queued_at);
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS native_board_dirty_oldest;
DROP INDEX CONCURRENTLY IF EXISTS native_board_page_refresh_due;
DROP INDEX CONCURRENTLY IF EXISTS native_board_page_pending_idx;
