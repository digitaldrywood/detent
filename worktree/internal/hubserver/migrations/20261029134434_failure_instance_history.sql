-- +goose Up
CREATE INDEX native_attempts_instance_history ON native_attempts(organization_id, project_id, runner_id, fencing_token DESC, id DESC);

-- +goose Down
DROP INDEX native_attempts_instance_history;
