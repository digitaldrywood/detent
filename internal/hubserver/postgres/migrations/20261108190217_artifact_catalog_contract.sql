-- +goose Up
CREATE TABLE artifact_catalog_usage (
 organization_id text PRIMARY KEY REFERENCES organizations(organization_id),
 reserved_bytes bigint NOT NULL DEFAULT 0 CHECK(reserved_bytes>=0),retained_bytes bigint NOT NULL DEFAULT 0 CHECK(retained_bytes>=0)
);
CREATE TABLE artifact_catalog_uploads (
 organization_id text NOT NULL,project_id text NOT NULL,service_id text NOT NULL,id text NOT NULL,idempotency_key text NOT NULL,
 request_hash text NOT NULL CHECK(length(request_hash)=64),work_item_id text NOT NULL,run_id text NOT NULL,attempt_id text NOT NULL,version_id text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('log','diff','screenshot','video')),lease_id text NOT NULL,fencing_token bigint NOT NULL CHECK(fencing_token>=0),
 requested_bytes bigint NOT NULL CHECK(requested_bytes>0 AND requested_bytes<=268435456),part_limit bigint NOT NULL CHECK(part_limit>0),
 state text NOT NULL DEFAULT 'uploading' CHECK(state IN ('uploading','complete','interrupted','deletion_pending','deleted')),
 reserved_bytes bigint NOT NULL CHECK(reserved_bytes>=0),retained_bytes bigint NOT NULL DEFAULT 0 CHECK(retained_bytes>=0),
 manifest_revision bigint NOT NULL DEFAULT 0 CHECK(manifest_revision>=0),created_at timestamptz NOT NULL,upload_deadline timestamptz NOT NULL,expires_at timestamptz NOT NULL,deleted_at timestamptz,deletion_receipt_object_key text,
 PRIMARY KEY(organization_id,project_id,service_id,id),UNIQUE(organization_id,project_id,service_id,idempotency_key),
 FOREIGN KEY(organization_id,project_id,service_id) REFERENCES artifact_services(organization_id,project_id,id),
 FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES issues(organization_id,project_id,native_id),
 CHECK(reserved_bytes+retained_bytes<=requested_bytes),CHECK(upload_deadline>created_at AND expires_at>created_at)
);
CREATE INDEX artifact_catalog_expiry ON artifact_catalog_uploads(organization_id,expires_at,project_id,service_id,id) WHERE state<>'deleted';
CREATE INDEX artifact_catalog_abandoned ON artifact_catalog_uploads(organization_id,upload_deadline,project_id,service_id,id) WHERE state='uploading';
CREATE INDEX artifact_catalog_active_project ON artifact_catalog_uploads(organization_id,project_id) WHERE state IN ('uploading','deletion_pending');
CREATE INDEX artifact_catalog_deleting ON artifact_catalog_uploads(organization_id,expires_at,project_id,service_id,id) WHERE state='deletion_pending';
CREATE TABLE artifact_catalog_objects (
 organization_id text NOT NULL,project_id text NOT NULL,service_id text NOT NULL,upload_id text NOT NULL,sequence integer NOT NULL CHECK(sequence BETWEEN 0 AND 1023),
 id text NOT NULL,media_type text NOT NULL,size bigint NOT NULL CHECK(size>=0 AND size<=268435456),sha256 text NOT NULL CHECK(length(sha256)=64),path text NOT NULL,side text NOT NULL,
 byte_offset bigint NOT NULL CHECK(byte_offset>=0),storage_version text NOT NULL DEFAULT '',verified_at timestamptz,discarded_at timestamptz,discard_receipt_object_key text,
 CHECK(verified_at IS NULL OR discarded_at IS NULL),
 PRIMARY KEY(organization_id,project_id,service_id,upload_id,sequence),UNIQUE(organization_id,project_id,service_id,id),
 FOREIGN KEY(organization_id,project_id,service_id,upload_id) REFERENCES artifact_catalog_uploads(organization_id,project_id,service_id,id) ON DELETE CASCADE
);
CREATE TABLE artifact_catalog_manifests (
 organization_id text NOT NULL,project_id text NOT NULL,service_id text NOT NULL,upload_id text NOT NULL,revision bigint NOT NULL CHECK(revision>0),id text NOT NULL,
 state text NOT NULL CHECK(state IN ('partial','complete','interrupted')),sha256 text NOT NULL CHECK(length(sha256)=64),body_object_key text NOT NULL CHECK(btrim(body_object_key)<>''),body_bytes bigint NOT NULL CHECK(body_bytes>0 AND body_bytes<=1048576),
 total_bytes bigint NOT NULL CHECK(total_bytes>=0),objects integer NOT NULL CHECK(objects BETWEEN 0 AND 1024),retention_policy_id text NOT NULL,
 capture_base text NOT NULL,capture_head text NOT NULL,capture_merge_base text NOT NULL,capture_context_lines integer NOT NULL,capture_file_context text NOT NULL,capture_working_tree boolean NOT NULL,
 created_at timestamptz NOT NULL,expires_at timestamptz NOT NULL,observed_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id,service_id,upload_id,revision),UNIQUE(organization_id,project_id,service_id,id),
 FOREIGN KEY(organization_id,project_id,service_id,upload_id) REFERENCES artifact_catalog_uploads(organization_id,project_id,service_id,id) ON DELETE CASCADE
);
CREATE TRIGGER artifact_catalog_manifest_immutable BEFORE UPDATE ON artifact_catalog_manifests FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
CREATE TABLE artifact_catalog_publications (
 organization_id text NOT NULL,project_id text NOT NULL,service_id text NOT NULL,manifest_id text NOT NULL,
 outcome text NOT NULL DEFAULT 'pending' CHECK(outcome IN ('pending','uncertain','committed')),external_receipt text NOT NULL DEFAULT '',updated_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id,service_id,manifest_id),
 FOREIGN KEY(organization_id,project_id,service_id,manifest_id) REFERENCES artifact_catalog_manifests(organization_id,project_id,service_id,id) ON DELETE CASCADE
);
CREATE INDEX artifact_catalog_publication_pending ON artifact_catalog_publications(organization_id,updated_at,project_id,service_id,manifest_id) WHERE outcome<>'committed';
CREATE TABLE artifact_catalog_traffic_settings (
 organization_id text NOT NULL,project_id text NOT NULL,service_id text NOT NULL,window_seconds bigint NOT NULL CHECK(window_seconds>0 AND window_seconds<=31536000),retention_seconds bigint NOT NULL CHECK(retention_seconds>=window_seconds AND retention_seconds<=31536000),
 PRIMARY KEY(organization_id,project_id,service_id),FOREIGN KEY(organization_id,project_id,service_id) REFERENCES artifact_services(organization_id,project_id,id)
);
CREATE TABLE artifact_catalog_traffic (
 organization_id text NOT NULL,project_id text NOT NULL,service_id text NOT NULL,minute timestamptz NOT NULL,
 upload_bytes bigint NOT NULL CHECK(upload_bytes>=0),download_bytes bigint NOT NULL CHECK(download_bytes>=0),requests bigint NOT NULL CHECK(requests>=0),
 PRIMARY KEY(organization_id,project_id,service_id,minute),FOREIGN KEY(organization_id,project_id,service_id) REFERENCES artifact_services(organization_id,project_id,id)
);
CREATE TABLE artifact_catalog_traffic_receipts (
 organization_id text NOT NULL,project_id text NOT NULL,service_id text NOT NULL,id text NOT NULL,minute timestamptz NOT NULL,
 upload_bytes bigint NOT NULL CHECK(upload_bytes>=0),download_bytes bigint NOT NULL CHECK(download_bytes>=0),requests bigint NOT NULL CHECK(requests>=0),
 PRIMARY KEY(organization_id,project_id,service_id,id),FOREIGN KEY(organization_id,project_id,service_id) REFERENCES artifact_services(organization_id,project_id,id)
);
CREATE INDEX artifact_catalog_traffic_receipt_expiry ON artifact_catalog_traffic_receipts(organization_id,minute,project_id,service_id,id);

ALTER TABLE artifact_catalog_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_usage FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_usage FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_usage TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_usage TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE artifact_catalog_uploads ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_uploads FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_uploads FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_uploads TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_uploads TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE artifact_catalog_objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_objects FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_objects FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_objects TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_objects TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE artifact_catalog_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_manifests FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_manifests FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_manifests TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_manifests TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE artifact_catalog_publications ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_publications FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_publications FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_publications TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_publications TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE artifact_catalog_traffic_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_traffic_settings FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_traffic_settings FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_traffic_settings TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_traffic_settings TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE artifact_catalog_traffic ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_traffic FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_traffic FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_traffic TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_traffic TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE artifact_catalog_traffic_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_catalog_traffic_receipts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_catalog_traffic_receipts FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON artifact_catalog_traffic_receipts TO detent_runtime;
CREATE POLICY tenant_isolation ON artifact_catalog_traffic_receipts TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE artifact_catalog_traffic_receipts;
DROP TABLE artifact_catalog_traffic;
DROP TABLE artifact_catalog_traffic_settings;
DROP TABLE artifact_catalog_publications;
DROP TABLE artifact_catalog_manifests;
DROP TABLE artifact_catalog_objects;
DROP TABLE artifact_catalog_uploads;
DROP TABLE artifact_catalog_usage;
