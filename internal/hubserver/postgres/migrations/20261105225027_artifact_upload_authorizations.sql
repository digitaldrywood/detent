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
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
 PRIMARY KEY (organization_id, project_id, service_id, idempotency_key),
 FOREIGN KEY (organization_id, project_id, service_id) REFERENCES artifact_services(organization_id, project_id, id),
 FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
CREATE INDEX artifact_upload_authorizations_attempt ON artifact_upload_authorizations
 (organization_id, project_id, attempt_id, expires_at);
CREATE INDEX artifact_upload_authorizations_match ON artifact_upload_authorizations
 (organization_id, project_id, service_id, attempt_id, kind, expires_at);
CREATE INDEX artifact_upload_authorizations_expiry ON artifact_upload_authorizations
 (organization_id, expires_at);
ALTER TABLE artifact_upload_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_upload_authorizations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_upload_authorizations FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON artifact_upload_authorizations TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON artifact_upload_authorizations TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON artifact_upload_authorizations TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE artifact_upload_authorizations;
