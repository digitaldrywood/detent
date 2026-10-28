-- +goose Up
ALTER TABLE runner_identities ADD COLUMN project_configuration_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_project_observations ADD COLUMN board_projection_version INTEGER NOT NULL DEFAULT 0;
CREATE INDEX runner_project_board_conversion_idx ON runner_project_observations(organization_id,runner_id,project_id) WHERE board_projection_version=0;
CREATE TABLE runner_project_diagnostic_attempts (
 organization_id TEXT NOT NULL,
 runner_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 position INTEGER NOT NULL,
 record_key TEXT NOT NULL,
 issue_id TEXT NOT NULL,
 local_attempt_id INTEGER NOT NULL,
 native_attempt_id TEXT NOT NULL,
 generation INTEGER NOT NULL,
 fence INTEGER NOT NULL,
 stage TEXT NOT NULL,
 running INTEGER NOT NULL,
 native_claim INTEGER NOT NULL,
 provider_completed_at TEXT NOT NULL,
 host_operation TEXT NOT NULL,
 operation_pending INTEGER NOT NULL,
 lease_renewed_at TEXT NOT NULL,
 PRIMARY KEY(organization_id,runner_id,project_id,position),
 FOREIGN KEY(organization_id,runner_id,project_id) REFERENCES runner_project_observations(organization_id,runner_id,project_id) ON DELETE CASCADE
);
CREATE INDEX runner_project_diagnostic_issue_idx ON runner_project_diagnostic_attempts(organization_id,runner_id,project_id,issue_id);

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN project_configuration_revision;
DROP TABLE runner_project_diagnostic_attempts;
DROP INDEX runner_project_board_conversion_idx;
ALTER TABLE runner_project_observations DROP COLUMN board_projection_version;
