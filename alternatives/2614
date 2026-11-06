-- +goose Up

CREATE TABLE policy_revision_approvals (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    policy_id text NOT NULL,
    approved_by text NOT NULL CHECK (approved_by <> ''),
    approved_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, project_id, policy_id),
    FOREIGN KEY (organization_id, project_id, policy_id) REFERENCES policy_revisions (organization_id, project_id, policy_id)
);

INSERT INTO policy_revision_approvals (organization_id, project_id, policy_id, approved_by, approved_at)
SELECT r.organization_id, r.project_id, r.policy_id, r.approved_by, r.approved_at
FROM policy_revisions r
WHERE r.approved_by <> 'runner-report'
ON CONFLICT DO NOTHING;

INSERT INTO policy_revision_approvals (organization_id, project_id, policy_id, approved_by, approved_at)
SELECT r.organization_id, r.project_id, r.policy_id, r.approved_by, r.approved_at
FROM project_policies p
JOIN policy_revisions r USING (organization_id, project_id, policy_id)
ON CONFLICT DO NOTHING;

CREATE TABLE project_policy_observations (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    runner_id text NOT NULL CHECK (runner_id <> ''),
    policy_id text NOT NULL,
    observed_at timestamptz NOT NULL,
    source_present boolean NOT NULL,
    source_repository text NOT NULL DEFAULT '' CHECK (length(source_repository) <= 1024),
    source_commit text NOT NULL DEFAULT '' CHECK (length(source_commit) <= 256),
    source_default_branch text NOT NULL DEFAULT '' CHECK (length(source_default_branch) <= 255),
    source_default_branch_head text NOT NULL DEFAULT '' CHECK (length(source_default_branch_head) <= 256),
    source_default_branch_reachable boolean NOT NULL DEFAULT false,
    PRIMARY KEY (organization_id, project_id, runner_id),
    FOREIGN KEY (organization_id, project_id, policy_id) REFERENCES policy_revisions (organization_id, project_id, policy_id)
);

CREATE INDEX project_policy_observations_recent_idx
    ON project_policy_observations (organization_id, project_id, observed_at DESC, runner_id);
CREATE INDEX project_policy_observations_policy_idx
    ON project_policy_observations (organization_id, project_id, policy_id, runner_id);

ALTER TABLE policy_revision_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy_revision_approvals FORCE ROW LEVEL SECURITY;
REVOKE ALL ON policy_revision_approvals FROM PUBLIC;
GRANT SELECT, INSERT ON policy_revision_approvals TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON policy_revision_approvals TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON policy_revision_approvals TO detent_platform USING (true) WITH CHECK (true);
CREATE TRIGGER policy_revision_approvals_immutable BEFORE UPDATE OR DELETE ON policy_revision_approvals FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();

ALTER TABLE project_policy_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_policy_observations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON project_policy_observations FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON project_policy_observations TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON project_policy_observations TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON project_policy_observations TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE project_policy_observations;
DROP TABLE policy_revision_approvals;
