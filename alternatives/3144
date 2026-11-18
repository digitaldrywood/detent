-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_tenant_recorded_idx;
CREATE INDEX CONCURRENTLY collaboration_events_tenant_recorded_idx ON collaboration_events(organization_id,recorded_at DESC,id DESC);
DROP INDEX CONCURRENTLY IF EXISTS hosted_sessions_tenant_created_idx;
CREATE INDEX CONCURRENTLY hosted_sessions_tenant_created_idx ON hosted_sessions(organization_id,created_at DESC,token_hash DESC);
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS hosted_sessions_tenant_created_idx;
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_tenant_recorded_idx;
