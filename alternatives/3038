-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE hosted_artifact_usage (
    organization_id text PRIMARY KEY REFERENCES organizations(organization_id) ON DELETE CASCADE,
    service_id text NOT NULL,
    observed_at timestamptz NOT NULL,
    retained_bytes bigint NOT NULL CHECK (retained_bytes >= 0),
    reserved_bytes bigint NOT NULL CHECK (reserved_bytes >= 0)
);
ALTER TABLE hosted_artifact_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_artifact_usage FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_artifact_usage FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_artifact_usage TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON hosted_artifact_usage TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON hosted_artifact_usage TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE hosted_artifact_usage;
