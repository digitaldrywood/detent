-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title_unicode;
CREATE INDEX CONCURRENTLY native_board_search_title_unicode ON native_board_items USING gin (page_title COLLATE pg_catalog.pg_c_utf8 gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body_unicode;
CREATE INDEX CONCURRENTLY native_work_item_search_body_unicode ON native_work_item_pages USING gin (body COLLATE pg_catalog.pg_c_utf8 gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title;
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title;
CREATE INDEX CONCURRENTLY native_board_search_title ON native_board_items USING gin (page_title gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body;
CREATE INDEX CONCURRENTLY native_work_item_search_body ON native_work_item_pages USING gin (body gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body_unicode;
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title_unicode;
RESET lock_timeout;
