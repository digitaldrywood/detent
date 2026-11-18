-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS health_questions_unresolved_seek;
CREATE INDEX CONCURRENTLY health_questions_unresolved_seek ON conversation_questions (organization_id,conversation_id,id) WHERE status IN ('pending','sending','sent','unknown');
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS health_questions_unresolved_seek;
RESET lock_timeout;
