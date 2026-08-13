-- +goose Up
CREATE INDEX work_attempts_project_identifier_idx
ON work_attempts(project_id, identifier);

CREATE INDEX work_attempts_project_issue_url_idx
ON work_attempts(project_id, issue_url);

CREATE INDEX workflow_phase_events_project_identifier_idx
ON workflow_phase_events(project_id, identifier);

CREATE INDEX workflow_phase_events_project_issue_url_idx
ON workflow_phase_events(project_id, issue_url);

CREATE INDEX issue_park_acknowledgements_project_identifier_idx
ON issue_park_acknowledgements(project_id, identifier);

CREATE INDEX issue_park_acknowledgements_project_issue_url_idx
ON issue_park_acknowledgements(project_id, issue_url);

-- +goose Down
DROP INDEX IF EXISTS issue_park_acknowledgements_project_issue_url_idx;
DROP INDEX IF EXISTS issue_park_acknowledgements_project_identifier_idx;
DROP INDEX IF EXISTS workflow_phase_events_project_issue_url_idx;
DROP INDEX IF EXISTS workflow_phase_events_project_identifier_idx;
DROP INDEX IF EXISTS work_attempts_project_issue_url_idx;
DROP INDEX IF EXISTS work_attempts_project_identifier_idx;
