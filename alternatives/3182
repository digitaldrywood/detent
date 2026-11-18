-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_project_started_seek;
CREATE INDEX CONCURRENTLY native_attempts_project_started_seek ON native_attempts (organization_id,project_id,started_at,started_at_submicro_ns,id);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY native_attempts_project_started_seek;
RESET lock_timeout;
