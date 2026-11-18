-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE artifact_services (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    id text NOT NULL,
    origin text NOT NULL,
    binding_mode text NOT NULL CHECK (binding_mode IN ('customer', 'hosted')),
    hosted_opt_in boolean NOT NULL,
    publisher_token_id text NOT NULL,
    PRIMARY KEY (organization_id, project_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    FOREIGN KEY (organization_id, publisher_token_id) REFERENCES api_tokens (organization_id, id),
    CHECK (binding_mode <> 'hosted' OR hosted_opt_in)
);
ALTER TABLE artifact_services ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_services FORCE ROW LEVEL SECURITY;
REVOKE ALL ON artifact_services FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON artifact_services TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON artifact_services TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON artifact_services TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE artifact_services;
