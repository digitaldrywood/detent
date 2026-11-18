-- +goose Up
CREATE TABLE lease_policy_bindings (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    lease_id text NOT NULL,
    policy_id text NOT NULL,
    bound_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, lease_id),
    FOREIGN KEY (organization_id, lease_id) REFERENCES leases (organization_id, lease_id),
    FOREIGN KEY (organization_id, project_id, policy_id)
        REFERENCES policy_revisions (organization_id, project_id, policy_id)
);

ALTER TABLE lease_policy_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE lease_policy_bindings FORCE ROW LEVEL SECURITY;
REVOKE ALL ON lease_policy_bindings FROM PUBLIC;
GRANT SELECT, INSERT ON lease_policy_bindings TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON lease_policy_bindings TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON lease_policy_bindings TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE lease_policy_bindings;
