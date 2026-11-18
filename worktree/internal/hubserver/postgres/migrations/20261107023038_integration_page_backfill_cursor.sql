-- +goose Up
CREATE TABLE project_integration_page_progress (
 organization_id text PRIMARY KEY,
 after_project_id text NOT NULL
);
ALTER TABLE project_integration_page_progress ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_integration_page_progress FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_integration_page_progress TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON project_integration_page_progress TO detent_runtime_v2,detent_migration_v2
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE project_integration_page_progress;
