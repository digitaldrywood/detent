-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY runner_audit_dispositions_review_lookup
ON runner_audit_dispositions(organization_id,project_id,change_id,review_sequence,id) INCLUDE (audit_session_id);

-- +goose Down
DROP INDEX CONCURRENTLY runner_audit_dispositions_review_lookup;
