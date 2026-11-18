-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE artifact_grants (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 token_hash text NOT NULL CHECK (token_hash ~ '^[0-9a-f]{64}$'),
 principal_id text NOT NULL,
 service_id text NOT NULL,
 artifact_id text NOT NULL,
 revision bigint NOT NULL CHECK (revision > 0),
 expires_at timestamptz NOT NULL,
 hosted_session_hash text NOT NULL,
 PRIMARY KEY (organization_id, token_hash),
 FOREIGN KEY (organization_id, project_id, service_id) REFERENCES artifact_services(organization_id, project_id, id),
 FOREIGN KEY (organization_id, project_id, artifact_id, revision) REFERENCES artifact_references(organization_id, project_id, artifact_id, revision),
 FOREIGN KEY (organization_id, principal_id) REFERENCES api_tokens(organization_id, id)
);
ALTER TABLE artifact_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_grants FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_grants FROM PUBLIC;
GRANT SELECT, INSERT, DELETE ON artifact_grants TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON artifact_grants TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON artifact_grants TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE artifact_grants;
