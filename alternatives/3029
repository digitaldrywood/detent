-- +goose Up
-- Identity-first indexes serve both scoped refresh reads and legacy unscoped
-- activity reads. Replace the equivalent project-first indexes rather than
-- maintaining two copies on every historical insert.
DROP INDEX work_attempts_project_identifier_idx;
DROP INDEX work_attempts_project_issue_url_idx;
DROP INDEX workflow_phase_events_project_identifier_idx;
DROP INDEX workflow_phase_events_project_issue_url_idx;

CREATE INDEX work_attempts_project_identifier_idx ON work_attempts(identifier, project_id);
CREATE INDEX work_attempts_project_issue_url_idx ON work_attempts(issue_url, project_id);
CREATE INDEX workflow_phase_events_project_identifier_idx ON workflow_phase_events(identifier, project_id);
CREATE INDEX workflow_phase_events_project_issue_url_idx ON workflow_phase_events(issue_url, project_id);
CREATE INDEX codex_sessions_issue_idx ON codex_sessions(issue_id, project_id);
CREATE INDEX codex_sessions_issue_url_idx ON codex_sessions(issue_url, project_id);
CREATE INDEX scheduler_decisions_identifier_idx ON scheduler_decisions(identifier, project_id);
CREATE INDEX scheduler_decisions_issue_url_idx ON scheduler_decisions(issue_url, project_id);
CREATE INDEX scheduler_decisions_at_idx ON scheduler_decisions(decision_at DESC, id DESC);
DROP INDEX usage_events_project_issue_day_idx;
DROP INDEX usage_events_project_identifier_day_idx;
CREATE INDEX usage_events_project_issue_day_idx ON usage_events(issue_id, project_id, event_day);
CREATE INDEX usage_events_project_identifier_day_idx ON usage_events(identifier, project_id, event_day);

-- +goose Down
DROP INDEX usage_events_project_issue_day_idx;
DROP INDEX usage_events_project_identifier_day_idx;
CREATE INDEX usage_events_project_issue_day_idx ON usage_events(project_id, issue_id, event_day);
CREATE INDEX usage_events_project_identifier_day_idx ON usage_events(project_id, identifier, event_day);
DROP INDEX scheduler_decisions_at_idx;
DROP INDEX scheduler_decisions_issue_url_idx;
DROP INDEX scheduler_decisions_identifier_idx;
DROP INDEX codex_sessions_issue_url_idx;
DROP INDEX codex_sessions_issue_idx;
DROP INDEX workflow_phase_events_project_issue_url_idx;
DROP INDEX workflow_phase_events_project_identifier_idx;
DROP INDEX work_attempts_project_issue_url_idx;
DROP INDEX work_attempts_project_identifier_idx;
CREATE INDEX work_attempts_project_identifier_idx ON work_attempts(project_id, identifier);
CREATE INDEX work_attempts_project_issue_url_idx ON work_attempts(project_id, issue_url);
CREATE INDEX workflow_phase_events_project_identifier_idx ON workflow_phase_events(project_id, identifier);
CREATE INDEX workflow_phase_events_project_issue_url_idx ON workflow_phase_events(project_id, issue_url);
