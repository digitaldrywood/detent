-- +goose Up
CREATE TABLE native_pr_publications (
    organization_id text NOT NULL,
    version_id text NOT NULL,
    change_id text NOT NULL,
    repository text NOT NULL,
    base_ref text NOT NULL,
    branch text NOT NULL,
    head_sha text NOT NULL,
    policy_id text NOT NULL,
    source_attempt_id text NOT NULL,
    recorder_attempt_id text NOT NULL,
    provider text NOT NULL,
    external_id text NOT NULL,
    external_url text NOT NULL,
    PRIMARY KEY (organization_id, version_id),
    FOREIGN KEY (organization_id, change_id, version_id) REFERENCES change_versions (organization_id, change_id, id),
    FOREIGN KEY (organization_id, source_attempt_id) REFERENCES native_attempts (organization_id, id),
    FOREIGN KEY (organization_id, recorder_attempt_id) REFERENCES native_attempts (organization_id, id)
);

CREATE TABLE release_deliveries (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    source_issue_id text NOT NULL,
    source_change_id text NOT NULL,
    source_merge_sha text NOT NULL,
    repository_id bigint,
    repository text NOT NULL,
    pr_number bigint NOT NULL CHECK (pr_number > 0),
    head_ref text NOT NULL,
    head_sha text NOT NULL,
    base_ref text NOT NULL,
    base_sha text NOT NULL,
    frozen_sha text NOT NULL,
    change_id text NOT NULL,
    version_id text,
    principal_id text NOT NULL,
    actor_kind text NOT NULL,
    via_key text NOT NULL,
    associated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, work_item_id),
    UNIQUE (organization_id, repository_id, pr_number),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id),
    FOREIGN KEY (organization_id, source_issue_id) REFERENCES issues (organization_id, native_id),
    FOREIGN KEY (organization_id, source_change_id) REFERENCES change_requests (organization_id, id),
    FOREIGN KEY (organization_id, change_id) REFERENCES change_requests (organization_id, id),
    FOREIGN KEY (organization_id, change_id, version_id) REFERENCES change_versions (organization_id, change_id, id)
);

CREATE UNIQUE INDEX release_deliveries_repository_pr_idx ON release_deliveries (organization_id, lower(repository), pr_number);

ALTER TABLE native_pr_publications ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_pr_publications FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_pr_publications FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_pr_publications TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON native_pr_publications TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_pr_publications TO detent_platform USING (true) WITH CHECK (true);

ALTER TABLE release_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE release_deliveries FORCE ROW LEVEL SECURITY;
REVOKE ALL ON release_deliveries FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON release_deliveries TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON release_deliveries TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON release_deliveries TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE release_deliveries;
DROP TABLE native_pr_publications;
