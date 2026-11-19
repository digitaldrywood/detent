-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_items_closed_at_seek;
CREATE INDEX CONCURRENTLY native_board_items_closed_at_seek ON native_board_items (organization_id,project_id,closed_at);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_items_closed_at_seek;
RESET lock_timeout;
