-- +goose Up
SET LOCAL lock_timeout = '5s';
DROP TRIGGER IF EXISTS projects_search_scope_refresh ON projects;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_parent_scope_refresh() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $refresh$
BEGIN
 UPDATE public.projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
 RETURN NEW;
END
$refresh$;
-- +goose StatementEnd
DROP TABLE IF EXISTS search_postings;
DROP TABLE IF EXISTS search_document_index;
DROP TABLE IF EXISTS search_scope_documents;
DROP TABLE IF EXISTS search_scope_refresh;
DROP TABLE IF EXISTS search_documents;
DROP FUNCTION IF EXISTS search_document_postings();
DROP FUNCTION IF EXISTS search_source_revision();
DROP FUNCTION IF EXISTS search_work_source_changed();
DROP FUNCTION IF EXISTS search_deleted_scope_cleanup();
DROP FUNCTION IF EXISTS search_document_metadata();
DROP FUNCTION IF EXISTS search_metadata_vector(text,text,text[]);
DROP FUNCTION IF EXISTS search_segments_valid(text,integer[]);
DROP SEQUENCE IF EXISTS search_refresh_order;
DROP INDEX IF EXISTS native_board_search_title_unicode;
DROP POLICY IF EXISTS board_search_reader ON native_board_items;
DROP POLICY IF EXISTS board_search_reader ON native_work_item_pages;
DROP POLICY IF EXISTS board_search_reader ON issues;
REVOKE SELECT(organization_id,project_id,work_item_id,page_title) ON native_board_items FROM detent_search_reader_v2;
REVOKE SELECT(organization_id,project_id,work_item_id,body) ON native_work_item_pages FROM detent_search_reader_v2;
REVOKE SELECT(organization_id,project_id,native_id,actor_principal_id) ON issues FROM detent_search_reader_v2;

-- +goose Down
-- +goose StatementBegin
DO $rollback$
BEGIN
 RAISE EXCEPTION 'removing the search posting tables cannot be rolled back; restore from a verified backup';
END
$rollback$;
-- +goose StatementEnd
