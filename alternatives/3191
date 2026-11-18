-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS action_run_recent_order;
CREATE INDEX CONCURRENTLY action_run_recent_order ON project_action_runs (organization_id,project_id,action_id,created_at DESC,id DESC);
DROP INDEX CONCURRENTLY IF EXISTS action_run_dispatch_seek;
CREATE INDEX CONCURRENTLY action_run_dispatch_seek ON project_action_runs (organization_id,created_at,id) WHERE status='queued' AND claimed_by='';
DROP INDEX CONCURRENTLY IF EXISTS action_run_active_seek;
CREATE INDEX CONCURRENTLY action_run_active_seek ON project_action_runs (organization_id,project_id,id) WHERE status IN ('queued','running');
DROP INDEX CONCURRENTLY IF EXISTS terminal_recording_relay_order;
CREATE INDEX CONCURRENTLY terminal_recording_relay_order ON workspace_terminal_recordings (organization_id,project_id,workspace_id,relay_session_id,started_at DESC,id DESC);
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS terminal_recording_relay_order;
DROP INDEX CONCURRENTLY IF EXISTS action_run_active_seek;
DROP INDEX CONCURRENTLY IF EXISTS action_run_dispatch_seek;
DROP INDEX CONCURRENTLY IF EXISTS action_run_recent_order;
