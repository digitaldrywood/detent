-- +goose Up
CREATE TABLE change_viewed_files (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    change_id text NOT NULL,
    version_id text NOT NULL,
    principal_id text NOT NULL CHECK (principal_id <> ''),
    manifest_sha256 text NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    file_sha256 text NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    viewed boolean NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, version_id, principal_id, manifest_sha256, file_sha256),
    FOREIGN KEY (organization_id, project_id, change_id) REFERENCES change_requests (organization_id, project_id, id),
    FOREIGN KEY (organization_id, change_id, version_id) REFERENCES change_versions (organization_id, change_id, id)
);

ALTER TABLE change_viewed_files ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_viewed_files FORCE ROW LEVEL SECURITY;
REVOKE ALL ON change_viewed_files FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON change_viewed_files TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON change_viewed_files TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON change_viewed_files TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE change_viewed_files;
