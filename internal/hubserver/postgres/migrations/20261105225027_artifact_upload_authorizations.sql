-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE artifact_upload_authorizations (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 service_id text NOT NULL,
 idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
 work_item_id text NOT NULL,
 run_id text NOT NULL,
 attempt_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('log','diff','screenshot','video')),
 bytes bigint NOT NULL CHECK (bytes > 0 AND bytes <= 268435456),
 artifact_id text,
 created_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id, project_id, service_id, idempotency_key),
 UNIQUE (organization_id, project_id, artifact_id),
 FOREIGN KEY (organization_id, project_id, service_id) REFERENCES artifact_services(organization_id, project_id, id),
 FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
CREATE INDEX artifact_upload_authorizations_unbound ON artifact_upload_authorizations
 (organization_id, project_id, service_id, attempt_id, kind, created_at) WHERE artifact_id IS NULL;
ALTER TABLE artifact_upload_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_upload_authorizations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_upload_authorizations FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON artifact_upload_authorizations TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON artifact_upload_authorizations TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON artifact_upload_authorizations TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE artifact_upload_authorizations;
