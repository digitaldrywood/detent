-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY runner_audit_dispositions_session_idx
ON runner_audit_dispositions(organization_id,project_id,audit_session_id,id);

-- +goose Down
DROP INDEX CONCURRENTLY runner_audit_dispositions_session_idx;
