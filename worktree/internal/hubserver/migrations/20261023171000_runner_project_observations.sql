-- +goose Up
CREATE TABLE runner_project_observations (
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    observed_at TEXT NOT NULL,
    diagnostics_observed_at TEXT,
    runtime_observed_at TEXT,
    snapshot_digest TEXT NOT NULL,
    PRIMARY KEY (organization_id, runner_id, project_id)
);

-- +goose Down
DROP TABLE runner_project_observations;
