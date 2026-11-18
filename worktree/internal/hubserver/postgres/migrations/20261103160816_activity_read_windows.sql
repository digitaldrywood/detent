-- +goose Up
CREATE INDEX native_attempts_activity_recent_idx ON native_attempts (organization_id,updated_at DESC,id DESC) WHERE status<>'running';
CREATE INDEX native_attempts_activity_running_idx ON native_attempts (organization_id,id) WHERE status='running';
CREATE INDEX issues_activity_recent_idx ON issues (organization_id,native_updated_at DESC,id DESC);

-- +goose Down
DROP INDEX issues_activity_recent_idx;
DROP INDEX native_attempts_activity_running_idx;
DROP INDEX native_attempts_activity_recent_idx;
