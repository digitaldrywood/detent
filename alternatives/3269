-- +goose Up
CREATE POLICY search_fingerprint_migration ON search_documents TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_fingerprint_migration ON search_scope_refresh TO detent_migration USING(true) WITH CHECK(true);
CREATE POLICY search_fingerprint_migration ON projects TO detent_migration USING(true) WITH CHECK(true);
INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id)
SELECT DISTINCT d.organization_id,d.project_id,'',false,'' FROM search_documents d WHERE d.project_id IS NOT NULL AND d.kind='issue'
 AND EXISTS(SELECT 1 FROM projects p WHERE p.organization_id=d.organization_id AND p.id=d.project_id AND p.deleted_at IS NULL)
ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET after_id='',refresh_order=nextval('search_refresh_order');
DROP POLICY search_fingerprint_migration ON search_documents;
DROP POLICY search_fingerprint_migration ON search_scope_refresh;
DROP POLICY search_fingerprint_migration ON projects;

-- +goose Down
SELECT 1;
