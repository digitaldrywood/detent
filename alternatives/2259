-- +goose Up
ALTER TABLE projects OWNER TO detent_migration;
ALTER TABLE issues OWNER TO detent_migration;
ALTER TABLE native_comments OWNER TO detent_migration;

REVOKE ALL ON projects, issues, native_comments FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON projects, issues, native_comments TO detent_runtime, detent_platform;
GRANT USAGE ON SEQUENCE issues_id_seq TO detent_runtime, detent_platform;

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
ALTER TABLE issues ENABLE ROW LEVEL SECURITY;
ALTER TABLE issues FORCE ROW LEVEL SECURITY;
ALTER TABLE native_comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_comments FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON projects TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON issues TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON native_comments TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

CREATE POLICY platform_access ON projects TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON issues TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON native_comments TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP POLICY platform_access ON native_comments;
DROP POLICY platform_access ON issues;
DROP POLICY platform_access ON projects;
DROP POLICY tenant_isolation ON native_comments;
DROP POLICY tenant_isolation ON issues;
DROP POLICY tenant_isolation ON projects;

ALTER TABLE native_comments NO FORCE ROW LEVEL SECURITY;
ALTER TABLE native_comments DISABLE ROW LEVEL SECURITY;
ALTER TABLE issues NO FORCE ROW LEVEL SECURITY;
ALTER TABLE issues DISABLE ROW LEVEL SECURITY;
ALTER TABLE projects NO FORCE ROW LEVEL SECURITY;
ALTER TABLE projects DISABLE ROW LEVEL SECURITY;

REVOKE ALL ON projects, issues, native_comments FROM detent_runtime, detent_platform;
REVOKE USAGE ON SEQUENCE issues_id_seq FROM detent_runtime, detent_platform;
