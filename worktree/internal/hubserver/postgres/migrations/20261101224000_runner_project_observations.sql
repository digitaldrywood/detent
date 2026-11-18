-- +goose Up
CREATE TABLE runner_project_observations (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    observed_at timestamptz NOT NULL,
    diagnostics_at timestamptz,
    runtime_observed_at timestamptz,
    snapshot_digest text NOT NULL CHECK (snapshot_digest ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (organization_id, runner_id, project_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE runner_project_diagnostic_attempts (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    position integer NOT NULL CHECK (position BETWEEN 0 AND 255),
    record_key text NOT NULL,
    issue_id text NOT NULL,
    local_attempt_id bigint NOT NULL CHECK (local_attempt_id >= 0),
    native_attempt_id text NOT NULL DEFAULT '',
    generation bigint NOT NULL CHECK (generation >= 0),
    fence bigint NOT NULL CHECK (fence >= 0),
    stage text NOT NULL DEFAULT '',
    running boolean NOT NULL,
    native_claim boolean NOT NULL,
    provider_completed_at timestamptz,
    host_operation text NOT NULL DEFAULT '',
    operation_pending boolean NOT NULL,
    lease_renewed_at timestamptz,
    PRIMARY KEY (organization_id, runner_id, project_id, position),
    FOREIGN KEY (organization_id, runner_id, project_id) REFERENCES runner_project_observations (organization_id, runner_id, project_id) ON DELETE CASCADE
);

CREATE INDEX runner_project_diagnostic_issue_idx ON runner_project_diagnostic_attempts (organization_id, runner_id, project_id, issue_id, position);

ALTER TABLE runner_project_observations OWNER TO detent_migration;
ALTER TABLE runner_project_diagnostic_attempts OWNER TO detent_migration;
REVOKE ALL ON runner_project_observations, runner_project_diagnostic_attempts FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_project_observations, runner_project_diagnostic_attempts TO detent_runtime;
ALTER TABLE runner_project_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_project_observations FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_project_diagnostic_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_project_diagnostic_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_project_observations TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON runner_project_diagnostic_attempts TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE runner_project_diagnostic_attempts;
DROP TABLE runner_project_observations;
