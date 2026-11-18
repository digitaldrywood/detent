-- +goose Up
CREATE TABLE github_import_records (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 import_id text NOT NULL,
 sequence bigint GENERATED ALWAYS AS IDENTITY,
 source_key text NOT NULL,
 kind text NOT NULL,
 current_dependency boolean NOT NULL DEFAULT true,
 dependency_id text NOT NULL DEFAULT '',
 provenance_external_id text NOT NULL DEFAULT '',
 payload_object_key text NOT NULL,
 payload_sha256 text NOT NULL,
 payload_bytes bigint NOT NULL,
 payload_content_bytes bigint NOT NULL,
 PRIMARY KEY (organization_id,project_id,import_id,sequence),
 UNIQUE (organization_id,project_id,import_id,source_key),
 FOREIGN KEY (organization_id,project_id,import_id) REFERENCES github_imports (organization_id,project_id,id)
);
ALTER TABLE github_import_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_import_records FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON github_import_records TO detent_runtime, detent_platform;
GRANT USAGE ON SEQUENCE github_import_records_sequence_seq TO detent_runtime,detent_platform,detent_migration;
CREATE POLICY tenant_isolation ON github_import_records TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON github_import_records TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE github_import_records;
