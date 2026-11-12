-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS hosted_audit_retention_seek;
CREATE INDEX CONCURRENTLY hosted_audit_retention_seek ON hosted_audit (recorded_at_ns,organization_id,id);
DROP INDEX CONCURRENTLY IF EXISTS hosted_audit_legacy_read_cleanup_seek;
CREATE INDEX CONCURRENTLY hosted_audit_legacy_read_cleanup_seek ON hosted_audit (organization_id,id)
WHERE event='action' AND status<400 AND actual_actor=effective_user AND reason='' AND route LIKE 'GET %' AND mutation_object_key IS NULL;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS hosted_audit_legacy_read_cleanup_seek;
DROP INDEX CONCURRENTLY IF EXISTS hosted_audit_retention_seek;
RESET lock_timeout;
