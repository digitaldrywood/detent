-- +goose Up
CREATE INDEX native_attempts_progress_terminal ON native_attempts(organization_id, project_id, work_item_id, fencing_token DESC) WHERE status <> 'running';

-- +goose Down
DROP INDEX native_attempts_progress_terminal;
