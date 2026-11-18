-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_diagnostics_decision_seek;
CREATE INDEX CONCURRENTLY collaboration_events_diagnostics_decision_seek ON collaboration_events (organization_id,project_id,recorded_at DESC,id DESC) WHERE type='scheduler.decision';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_activity_recent_cover;
CREATE INDEX CONCURRENTLY native_attempts_activity_recent_cover ON native_attempts (organization_id,updated_at DESC,id DESC) INCLUDE (issue_id,status,started_at) WHERE status<>'running';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_activity_recent_idx;
ALTER INDEX native_attempts_activity_recent_cover RENAME TO native_attempts_activity_recent_idx;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_activity_recent_narrow;
CREATE INDEX CONCURRENTLY native_attempts_activity_recent_narrow ON native_attempts (organization_id,updated_at DESC,id DESC) WHERE status<>'running';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_activity_recent_idx;
ALTER INDEX native_attempts_activity_recent_narrow RENAME TO native_attempts_activity_recent_idx;
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_diagnostics_decision_seek;
RESET lock_timeout;
