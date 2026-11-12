-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_intake_worker_ready;
CREATE INDEX CONCURRENTLY github_intake_worker_ready ON onboarding_issue_intake(organization_id,next_attempt_at,project_id) WHERE batch_status IN ('discovering','importing');
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_intake_worker_ready;
RESET lock_timeout;
