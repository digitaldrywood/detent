-- +goose Up
CREATE TABLE organization_slugs (
 organization_id text NOT NULL REFERENCES organizations (organization_id) ON DELETE CASCADE,
 slug text NOT NULL CHECK (slug <> ''),
 PRIMARY KEY (organization_id, slug)
);
ALTER TABLE organization_slugs ENABLE ROW LEVEL SECURITY;
ALTER TABLE organization_slugs FORCE ROW LEVEL SECURITY;
REVOKE ALL ON organization_slugs FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON organization_slugs TO detent_runtime;
CREATE POLICY tenant_isolation ON organization_slugs TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE organization_slugs;
