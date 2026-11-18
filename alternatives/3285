-- +goose Up
SET LOCAL lock_timeout = '5s';
DROP INDEX IF EXISTS native_work_item_search_body_unicode;
ALTER TABLE native_work_item_pages ADD COLUMN search_vector tsvector GENERATED ALWAYS AS (
 setweight(to_tsvector('pg_catalog.english'::regconfig,lower(left(coalesce(title,''),1024) COLLATE pg_catalog.pg_c_utf8)),'A')
 || setweight(to_tsvector('pg_catalog.english'::regconfig,lower(left(coalesce(body,''),32768) COLLATE pg_catalog.pg_c_utf8)),'D')
) STORED;
CREATE INDEX native_work_item_pages_search_window ON native_work_item_pages(organization_id,project_id,number DESC,work_item_id);
CREATE INDEX native_work_item_page_dirty_items ON native_work_item_page_dirty(organization_id,project_id) WHERE kind='item';

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP INDEX IF EXISTS native_work_item_page_dirty_items;
DROP INDEX IF EXISTS native_work_item_pages_search_window;
ALTER TABLE native_work_item_pages DROP COLUMN IF EXISTS search_vector;
CREATE INDEX native_work_item_search_body_unicode ON native_work_item_pages USING gin (organization_id,project_id,body COLLATE pg_catalog.pg_c_utf8 gin_trgm_ops);
