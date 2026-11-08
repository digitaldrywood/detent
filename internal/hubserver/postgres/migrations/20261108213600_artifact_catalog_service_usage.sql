-- +goose Up
CREATE TABLE artifact_catalog_service_usage (
 organization_id text NOT NULL REFERENCES organizations(organization_id),service_id text NOT NULL,
 reserved_bytes bigint NOT NULL DEFAULT 0 CHECK(reserved_bytes>=0),retained_bytes bigint NOT NULL DEFAULT 0 CHECK(retained_bytes>=0),
 PRIMARY KEY(organization_id,service_id)
);
INSERT INTO artifact_catalog_service_usage SELECT organization_id,service_id,sum(reserved_bytes),sum(retained_bytes) FROM artifact_catalog_uploads GROUP BY organization_id,service_id;
INSERT INTO artifact_catalog_service_usage SELECT organization_id,'',sum(reserved_bytes),sum(retained_bytes) FROM artifact_catalog_uploads WHERE binding_mode='hosted' GROUP BY organization_id;
CREATE INDEX artifact_catalog_active_service_usage ON artifact_catalog_uploads(organization_id,service_id) WHERE reserved_bytes>0 OR retained_bytes>0;
CREATE INDEX artifact_catalog_active_hosted_usage ON artifact_catalog_uploads(organization_id,binding_mode) WHERE reserved_bytes>0 OR retained_bytes>0;
ALTER TABLE artifact_catalog_service_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_service_usage FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_service_usage FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_service_usage TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_service_usage TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP INDEX artifact_catalog_active_hosted_usage;
DROP INDEX artifact_catalog_active_service_usage;
DROP TABLE artifact_catalog_service_usage;
