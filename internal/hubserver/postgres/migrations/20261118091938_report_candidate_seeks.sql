-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issues_report_updated;
CREATE INDEX CONCURRENTLY issues_report_updated ON issues (organization_id,project_id,native_updated_at,id) INCLUDE (archived);
DROP INDEX CONCURRENTLY IF EXISTS issues_report_terminal;
CREATE INDEX CONCURRENTLY issues_report_terminal ON issues (organization_id,project_id,terminal_entered_at,id) WHERE NOT archived AND terminal_entered_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS issues_report_state_scan;
CREATE INDEX CONCURRENTLY issues_report_state_scan ON issues (organization_id,project_id,workflow_state_name,id);
DROP INDEX CONCURRENTLY IF EXISTS events_report_window;
CREATE INDEX CONCURRENTLY events_report_window ON collaboration_events (organization_id,project_id,recorded_at,id) INCLUDE (work_item_id);
DROP INDEX CONCURRENTLY IF EXISTS events_report_history_cover;
CREATE INDEX CONCURRENTLY events_report_history_cover ON collaboration_events (organization_id,project_id,work_item_id,recorded_at,sequence) INCLUDE (id,version_id,type,from_state,to_state) WHERE type IN ('workflow.transitioned','scheduler.decision');
DROP INDEX CONCURRENTLY IF EXISTS events_report_created_window;
CREATE INDEX CONCURRENTLY events_report_created_window ON collaboration_events (organization_id,project_id,work_item_id,recorded_at) WHERE type='issue.created';
DROP INDEX CONCURRENTLY IF EXISTS events_report_attempt_window;
CREATE INDEX CONCURRENTLY events_report_attempt_window ON collaboration_events (organization_id,project_id,recorded_at,id) INCLUDE (attempt_id) WHERE attempt_id<>'';
DROP INDEX CONCURRENTLY IF EXISTS attempts_report_window_items;
CREATE INDEX CONCURRENTLY attempts_report_window_items ON native_attempts (organization_id,project_id,started_at,issue_id);
DROP INDEX CONCURRENTLY IF EXISTS leases_report_project_expiry;
CREATE INDEX CONCURRENTLY leases_report_project_expiry ON leases (organization_id,project_id,expires_at,lease_id);
DROP INDEX CONCURRENTLY IF EXISTS changes_report_item_landings;
CREATE INDEX CONCURRENTLY changes_report_item_landings ON change_requests (organization_id,project_id,work_item_id,landed_at,id) WHERE landed_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS changes_report_verified_window;
CREATE INDEX CONCURRENTLY changes_report_verified_window ON change_requests (organization_id,project_id,landed_at,id) INCLUDE (work_item_id,landed_version_id,landed_head_sha) WHERE landed_at IS NOT NULL AND landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS changes_report_verified_window;
DROP INDEX CONCURRENTLY IF EXISTS changes_report_item_landings;
DROP INDEX CONCURRENTLY IF EXISTS leases_report_project_expiry;
DROP INDEX CONCURRENTLY IF EXISTS attempts_report_window_items;
DROP INDEX CONCURRENTLY IF EXISTS events_report_attempt_window;
DROP INDEX CONCURRENTLY IF EXISTS events_report_created_window;
DROP INDEX CONCURRENTLY IF EXISTS events_report_history_cover;
DROP INDEX CONCURRENTLY IF EXISTS events_report_window;
DROP INDEX CONCURRENTLY IF EXISTS issues_report_state_scan;
DROP INDEX CONCURRENTLY IF EXISTS issues_report_terminal;
DROP INDEX CONCURRENTLY IF EXISTS issues_report_updated;
RESET lock_timeout;
