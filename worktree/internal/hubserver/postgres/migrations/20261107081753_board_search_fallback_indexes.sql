-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title;
CREATE INDEX CONCURRENTLY native_board_search_title ON native_board_items USING gin (page_title gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body;
CREATE INDEX CONCURRENTLY native_work_item_search_body ON native_work_item_pages USING gin (body gin_trgm_ops);
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body;
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title;
