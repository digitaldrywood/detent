-- +goose Up
CREATE INDEX artifact_catalog_service_upload ON artifact_catalog_uploads(organization_id,service_id,id,project_id);
CREATE INDEX artifact_catalog_service_upload_page ON artifact_catalog_uploads(organization_id,service_id,project_id,id);
CREATE INDEX artifact_catalog_service_expiry ON artifact_catalog_uploads(organization_id,service_id,expires_at,project_id,id) WHERE state<>'deleted';
CREATE INDEX artifact_catalog_service_abandoned ON artifact_catalog_uploads(organization_id,service_id,upload_deadline,project_id,id) WHERE state='uploading';
CREATE INDEX artifact_catalog_service_deleting ON artifact_catalog_uploads(organization_id,service_id,expires_at,project_id,id) WHERE state='deletion_pending';
CREATE INDEX artifact_catalog_service_deleted ON artifact_catalog_uploads(organization_id,service_id,deleted_at,project_id,id) WHERE state='deleted';
CREATE INDEX artifact_catalog_service_manifest_page ON artifact_catalog_manifests(organization_id,service_id,project_id,upload_id,revision);
CREATE INDEX artifact_catalog_service_publication_page ON artifact_catalog_publications(organization_id,service_id,project_id,manifest_id) WHERE outcome<>'committed';
CREATE INDEX artifact_catalog_service_object_page ON artifact_catalog_objects(organization_id,service_id,project_id,upload_id,sequence);
CREATE INDEX artifact_catalog_service_intent_page ON artifact_catalog_manifest_intents(organization_id,service_id,project_id,upload_id,id);
CREATE INDEX artifact_catalog_service_binding ON artifact_services(organization_id,id,publisher_token_id,project_id);
CREATE TABLE artifact_catalog_service_traffic (
 organization_id text NOT NULL REFERENCES organizations(organization_id),service_id text NOT NULL,minute timestamptz NOT NULL,
 upload_bytes bigint NOT NULL CHECK(upload_bytes>=0),download_bytes bigint NOT NULL CHECK(download_bytes>=0),requests bigint NOT NULL CHECK(requests>=0),
 PRIMARY KEY(organization_id,service_id,minute)
);
ALTER TABLE artifact_catalog_traffic NO FORCE ROW LEVEL SECURITY;
ALTER TABLE artifact_services NO FORCE ROW LEVEL SECURITY;
INSERT INTO artifact_catalog_service_traffic SELECT organization_id,service_id,minute,sum(upload_bytes),sum(download_bytes),sum(requests) FROM artifact_catalog_traffic GROUP BY organization_id,service_id,minute;
INSERT INTO artifact_catalog_service_traffic SELECT t.organization_id,'',t.minute,sum(t.upload_bytes),sum(t.download_bytes),sum(t.requests) FROM artifact_catalog_traffic t JOIN artifact_services b ON (b.organization_id,b.project_id,b.id)=(t.organization_id,t.project_id,t.service_id) WHERE b.binding_mode='hosted' AND b.hosted_opt_in GROUP BY t.organization_id,t.minute;
ALTER TABLE artifact_catalog_traffic FORCE ROW LEVEL SECURITY;
ALTER TABLE artifact_services FORCE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_service_traffic ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_service_traffic FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_service_traffic FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_service_traffic TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_service_traffic TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE artifact_catalog_service_traffic;
DROP INDEX artifact_catalog_service_binding;
DROP INDEX artifact_catalog_service_intent_page;
DROP INDEX artifact_catalog_service_object_page;
DROP INDEX artifact_catalog_service_publication_page;
DROP INDEX artifact_catalog_service_manifest_page;
DROP INDEX artifact_catalog_service_deleted;
DROP INDEX artifact_catalog_service_deleting;
DROP INDEX artifact_catalog_service_abandoned;
DROP INDEX artifact_catalog_service_expiry;
DROP INDEX artifact_catalog_service_upload_page;
DROP INDEX artifact_catalog_service_upload;
