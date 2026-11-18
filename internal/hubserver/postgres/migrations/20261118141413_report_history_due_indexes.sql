-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS report_issue_checkpoints_history_due;
CREATE INDEX CONCURRENTLY report_issue_checkpoints_history_due ON report_issue_checkpoints (organization_id,project_id,work_item_id) WHERE history_sequence=-1 OR history_sequence-history_through>=256;
DROP INDEX CONCURRENTLY IF EXISTS report_issue_checkpoints_history_policy;
CREATE INDEX CONCURRENTLY report_issue_checkpoints_history_policy ON report_issue_checkpoints (organization_id,project_id,history_policy_revision,work_item_id) WHERE history_through>0;
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_history_sequence;
CREATE INDEX CONCURRENTLY collaboration_events_history_sequence ON collaboration_events (organization_id,project_id,work_item_id,sequence) WHERE type IN ('workflow.transitioned','scheduler.decision');
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_history_sequence;
DROP INDEX CONCURRENTLY IF EXISTS report_issue_checkpoints_history_policy;
DROP INDEX CONCURRENTLY IF EXISTS report_issue_checkpoints_history_due;
RESET lock_timeout;
