-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_reports_started_seek;
CREATE INDEX CONCURRENTLY native_attempts_reports_started_seek ON native_attempts (organization_id,issue_id,started_at,started_at_submicro_ns,id);
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_reports_started_idx;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_reports_started_idx;
CREATE INDEX CONCURRENTLY native_attempts_reports_started_idx ON native_attempts (organization_id,issue_id,started_at,id);
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_reports_started_seek;
RESET lock_timeout;
