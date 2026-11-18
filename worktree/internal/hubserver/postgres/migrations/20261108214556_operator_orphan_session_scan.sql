-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY runner_sessions_orphan_scan
ON runner_sessions(organization_id,project_id,started_at DESC,id DESC)
WHERE completed_at IS NULL AND final_state='running'
AND (provider_thread_id<>'' OR provider_session_id<>'');

-- +goose Down
DROP INDEX CONCURRENTLY runner_sessions_orphan_scan;
