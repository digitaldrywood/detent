-- +goose Up
CREATE TABLE github_cutovers (
 organization_id text NOT NULL, project_id text NOT NULL, checkpoint text NOT NULL,
 source_profile text NOT NULL, expected_revision bigint NOT NULL CHECK(expected_revision>0),
 receipt_object_key text NOT NULL, receipt_sha256 text NOT NULL, receipt_bytes bigint NOT NULL CHECK(receipt_bytes>0),receipt_content_bytes bigint NOT NULL CHECK(receipt_content_bytes>0),
 actor_id text NOT NULL, created_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id,checkpoint),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE TABLE local_board_imports (
 organization_id text NOT NULL,project_id text NOT NULL,source_key text NOT NULL,source_kind text NOT NULL,
 source_digest text NOT NULL,work_item_id text NOT NULL,imported_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id,source_key),
 FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES issues(organization_id,project_id,native_id)
);
CREATE TABLE neutral_import_sources (
 organization_id text NOT NULL REFERENCES organizations(organization_id),source_id text NOT NULL,
 schema_version bigint NOT NULL CHECK(schema_version=1),checksum text NOT NULL CHECK(length(checksum)=64),
 schema_digest text NOT NULL CHECK(length(schema_digest)=64),verified_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,source_id)
);
CREATE TABLE neutral_import_records (
 organization_id text NOT NULL,source_id text NOT NULL,family text NOT NULL,source_key text NOT NULL,
 record_digest text NOT NULL CHECK(length(record_digest)=64),destination_id text NOT NULL,source_revision bigint NOT NULL CHECK(source_revision>=0),
 payload_object_key text NOT NULL,payload_sha256 text NOT NULL,payload_bytes bigint NOT NULL CHECK(payload_bytes>0),payload_content_bytes bigint NOT NULL CHECK(payload_content_bytes>0),
 imported_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,source_id,family,source_key),
 FOREIGN KEY(organization_id,source_id) REFERENCES neutral_import_sources(organization_id,source_id)
);

ALTER TABLE github_cutovers ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_cutovers FORCE ROW LEVEL SECURITY;
REVOKE ALL ON github_cutovers FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON github_cutovers TO detent_runtime;
CREATE POLICY tenant_isolation ON github_cutovers TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE local_board_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE local_board_imports FORCE ROW LEVEL SECURITY;
REVOKE ALL ON local_board_imports FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON local_board_imports TO detent_runtime;
CREATE POLICY tenant_isolation ON local_board_imports TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE neutral_import_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE neutral_import_sources FORCE ROW LEVEL SECURITY;
REVOKE ALL ON neutral_import_sources FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON neutral_import_sources TO detent_runtime;
CREATE POLICY tenant_isolation ON neutral_import_sources TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE neutral_import_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE neutral_import_records FORCE ROW LEVEL SECURITY;
REVOKE ALL ON neutral_import_records FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON neutral_import_records TO detent_runtime;
CREATE POLICY tenant_isolation ON neutral_import_records TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE neutral_import_records;
DROP TABLE neutral_import_sources;
DROP TABLE local_board_imports;
DROP TABLE github_cutovers;
