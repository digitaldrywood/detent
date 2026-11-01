-- +goose Up
ALTER TABLE work_attempts ADD COLUMN metadata_pr_number INTEGER;
ALTER TABLE work_attempts ADD COLUMN metadata_pr_head_sha TEXT;
ALTER TABLE work_attempts ADD COLUMN metadata_pr_base_sha TEXT;
ALTER TABLE work_attempts ADD COLUMN metadata_run_mode TEXT;
ALTER TABLE work_attempts ADD COLUMN outcome_excluded INTEGER;
ALTER TABLE work_attempts ADD COLUMN forge_wait_present INTEGER;
ALTER TABLE work_attempts ADD COLUMN forge_wait_host TEXT;
ALTER TABLE work_attempts ADD COLUMN forge_wait_error_class TEXT;
ALTER TABLE work_attempts ADD COLUMN forge_write_completed_host TEXT;
ALTER TABLE work_attempts ADD COLUMN metrics_turns INTEGER;
ALTER TABLE work_attempts ADD COLUMN projection_ready INTEGER;
ALTER TABLE workflow_phase_events ADD COLUMN retry_identity TEXT;
ALTER TABLE workflow_phase_events ADD COLUMN input_hash TEXT;
ALTER TABLE workflow_phase_events ADD COLUMN correlation_id TEXT;
ALTER TABLE workflow_phase_events ADD COLUMN terminal_outcome TEXT;
ALTER TABLE workflow_phase_events ADD COLUMN projection_ready INTEGER;
ALTER TABLE scheduler_decisions ADD COLUMN global_available INTEGER;
ALTER TABLE scheduler_decisions ADD COLUMN projection_ready INTEGER;
ALTER TABLE backlog_admission_runs ADD COLUMN projection_ready INTEGER;
CREATE TABLE backlog_admission_run_issues (
  run_id INTEGER NOT NULL REFERENCES backlog_admission_runs(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  issue_id TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  PRIMARY KEY (run_id, position)
);
CREATE INDEX backlog_admission_run_issues_history_idx ON backlog_admission_run_issues(issue_id, run_id DESC) WHERE fingerprint != '';
DROP INDEX workflow_operator_retry_identity_idx;
CREATE UNIQUE INDEX workflow_operator_retry_identity_idx ON workflow_phase_events(retry_identity) WHERE phase_type = 'operator_action' AND retry_identity IS NOT NULL;
CREATE INDEX work_attempts_projection_pending_idx ON work_attempts(id) WHERE projection_ready IS NULL;
CREATE INDEX workflow_phase_events_projection_pending_idx ON workflow_phase_events(id) WHERE projection_ready IS NULL;
CREATE INDEX scheduler_decisions_projection_pending_idx ON scheduler_decisions(id) WHERE projection_ready IS NULL;
CREATE INDEX backlog_admission_runs_projection_pending_idx ON backlog_admission_runs(id) WHERE projection_ready IS NULL;

CREATE INDEX work_attempts_project_projection_pending_idx ON work_attempts(project_id,id) WHERE projection_ready IS NULL;
CREATE INDEX workflow_phase_events_project_projection_pending_idx ON workflow_phase_events(project_id,id) WHERE projection_ready IS NULL;
CREATE INDEX workflow_operator_projection_pending_idx ON workflow_phase_events(id) WHERE phase_type = 'operator_action' AND projection_ready IS NULL;

-- +goose Down
DROP INDEX workflow_operator_projection_pending_idx;
DROP INDEX workflow_phase_events_project_projection_pending_idx;
DROP INDEX work_attempts_project_projection_pending_idx;
DROP TABLE backlog_admission_run_issues;
DROP INDEX workflow_operator_retry_identity_idx;
DROP INDEX work_attempts_projection_pending_idx;
DROP INDEX workflow_phase_events_projection_pending_idx;
DROP INDEX scheduler_decisions_projection_pending_idx;
DROP INDEX backlog_admission_runs_projection_pending_idx;
ALTER TABLE work_attempts DROP COLUMN projection_ready;
ALTER TABLE work_attempts DROP COLUMN metrics_turns;
ALTER TABLE work_attempts DROP COLUMN forge_write_completed_host;
ALTER TABLE work_attempts DROP COLUMN forge_wait_error_class;
ALTER TABLE work_attempts DROP COLUMN forge_wait_host;
ALTER TABLE work_attempts DROP COLUMN forge_wait_present;
ALTER TABLE work_attempts DROP COLUMN outcome_excluded;
ALTER TABLE work_attempts DROP COLUMN metadata_run_mode;
ALTER TABLE work_attempts DROP COLUMN metadata_pr_base_sha;
ALTER TABLE work_attempts DROP COLUMN metadata_pr_head_sha;
ALTER TABLE work_attempts DROP COLUMN metadata_pr_number;
ALTER TABLE workflow_phase_events DROP COLUMN projection_ready;
ALTER TABLE workflow_phase_events DROP COLUMN terminal_outcome;
ALTER TABLE workflow_phase_events DROP COLUMN correlation_id;
ALTER TABLE workflow_phase_events DROP COLUMN input_hash;
ALTER TABLE workflow_phase_events DROP COLUMN retry_identity;
ALTER TABLE scheduler_decisions DROP COLUMN projection_ready;
ALTER TABLE scheduler_decisions DROP COLUMN global_available;
ALTER TABLE backlog_admission_runs DROP COLUMN projection_ready;
