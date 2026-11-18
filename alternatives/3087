-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE EXTENSION IF NOT EXISTS btree_gin;
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title_unicode_tenant;
CREATE INDEX CONCURRENTLY native_board_search_title_unicode_tenant ON native_board_items USING gin (organization_id,project_id,page_title COLLATE pg_catalog.pg_c_utf8 gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title_unicode;
ALTER INDEX native_board_search_title_unicode_tenant RENAME TO native_board_search_title_unicode;
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body_unicode_tenant;
CREATE INDEX CONCURRENTLY native_work_item_search_body_unicode_tenant ON native_work_item_pages USING gin (organization_id,project_id,body COLLATE pg_catalog.pg_c_utf8 gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body_unicode;
ALTER INDEX native_work_item_search_body_unicode_tenant RENAME TO native_work_item_search_body_unicode;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title_unicode_tenant;
CREATE INDEX CONCURRENTLY native_board_search_title_unicode_tenant ON native_board_items USING gin (page_title COLLATE pg_catalog.pg_c_utf8 gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_board_search_title_unicode;
ALTER INDEX native_board_search_title_unicode_tenant RENAME TO native_board_search_title_unicode;
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body_unicode_tenant;
CREATE INDEX CONCURRENTLY native_work_item_search_body_unicode_tenant ON native_work_item_pages USING gin (body COLLATE pg_catalog.pg_c_utf8 gin_trgm_ops);
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_search_body_unicode;
ALTER INDEX native_work_item_search_body_unicode_tenant RENAME TO native_work_item_search_body_unicode;
RESET lock_timeout;
