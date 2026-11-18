-- +goose Up
ALTER TABLE artifact_catalog_uploads ADD COLUMN manifest_reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (manifest_reserved_bytes>=0 AND manifest_reserved_bytes<=reserved_bytes);
CREATE TABLE artifact_catalog_manifest_intents (
 organization_id text NOT NULL, project_id text NOT NULL, service_id text NOT NULL, upload_id text NOT NULL, id text NOT NULL,
 revision bigint NOT NULL CHECK(revision>0), body_object_key text NOT NULL, sha256 text NOT NULL CHECK(length(sha256)=64), body_bytes bigint NOT NULL CHECK(body_bytes>0 AND body_bytes<=1048576),
 manifest_state text NOT NULL CHECK(manifest_state IN ('partial','complete','interrupted')),state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','discarding','discarded','committed')), storage_version text NOT NULL DEFAULT '', receipt_object_key text NOT NULL DEFAULT '', created_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id,service_id,upload_id,id),
 UNIQUE(organization_id,project_id,service_id,upload_id,body_object_key),
 FOREIGN KEY(organization_id,project_id,service_id,upload_id) REFERENCES artifact_catalog_uploads(organization_id,project_id,service_id,id) ON DELETE CASCADE
);
CREATE INDEX artifact_catalog_manifest_intents_pending ON artifact_catalog_manifest_intents(organization_id,project_id,service_id,upload_id,id) WHERE state IN ('pending','discarding');
CREATE UNIQUE INDEX artifact_catalog_manifest_intents_active_content ON artifact_catalog_manifest_intents(organization_id,project_id,service_id,upload_id,revision,sha256) WHERE state IN ('pending','discarding');
ALTER TABLE artifact_catalog_manifest_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_manifest_intents FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_manifest_intents FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_manifest_intents TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_manifest_intents TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE artifact_catalog_manifest_intents;
ALTER TABLE artifact_catalog_uploads DROP COLUMN manifest_reserved_bytes;
