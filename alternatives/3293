-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS native_work_item_search (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 number bigint NOT NULL,
 terms tsvector NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id)
) WITH (toast_tuple_target = 8160);
ALTER TABLE native_work_item_search ALTER COLUMN terms SET STORAGE MAIN;
CREATE INDEX IF NOT EXISTS native_work_item_search_window ON native_work_item_search (organization_id,project_id,number DESC,work_item_id);
ALTER TABLE native_work_item_search ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_work_item_search FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_work_item_search FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON native_work_item_search TO detent_runtime_v2;
DROP POLICY IF EXISTS tenant_isolation ON native_work_item_search;
CREATE POLICY tenant_isolation ON native_work_item_search TO detent_runtime_v2,detent_migration_v2 USING (organization_id=current_setting('detent.organization_id',true)) WITH CHECK (organization_id=current_setting('detent.organization_id',true));
DROP POLICY IF EXISTS native_work_item_search_backfill ON native_work_item_search;
CREATE POLICY native_work_item_search_backfill ON native_work_item_search TO detent_migration USING (true) WITH CHECK (true);
DROP POLICY IF EXISTS native_work_item_search_backfill ON native_work_item_pages;
CREATE POLICY native_work_item_search_backfill ON native_work_item_pages FOR SELECT TO detent_migration USING (true);
INSERT INTO native_work_item_search (organization_id,project_id,work_item_id,number,terms)
SELECT organization_id,project_id,work_item_id,number,
 setweight(to_tsvector('pg_catalog.english'::regconfig,lower(left(coalesce(title,''),1024) COLLATE pg_catalog.pg_c_utf8)),'A')
 || strip(to_tsvector('pg_catalog.english'::regconfig,lower(left(coalesce(body,''),32768) COLLATE pg_catalog.pg_c_utf8)))
FROM native_work_item_pages ORDER BY organization_id,project_id,number DESC,work_item_id
ON CONFLICT (organization_id,project_id,work_item_id) DO NOTHING;
DROP POLICY native_work_item_search_backfill ON native_work_item_pages;
DROP POLICY native_work_item_search_backfill ON native_work_item_search;
ANALYZE native_work_item_search;
DROP INDEX CONCURRENTLY IF EXISTS native_work_item_pages_search_window;
ALTER TABLE native_work_item_pages DROP COLUMN IF EXISTS search_vector;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
ALTER TABLE native_work_item_pages ADD COLUMN IF NOT EXISTS search_vector tsvector GENERATED ALWAYS AS (
 setweight(to_tsvector('pg_catalog.english'::regconfig,lower(left(coalesce(title,''),1024) COLLATE pg_catalog.pg_c_utf8)),'A')
 || setweight(to_tsvector('pg_catalog.english'::regconfig,lower(left(coalesce(body,''),32768) COLLATE pg_catalog.pg_c_utf8)),'D')
) STORED;
CREATE INDEX CONCURRENTLY IF NOT EXISTS native_work_item_pages_search_window ON native_work_item_pages(organization_id,project_id,number DESC,work_item_id);
DROP TABLE IF EXISTS native_work_item_search;
RESET lock_timeout;
