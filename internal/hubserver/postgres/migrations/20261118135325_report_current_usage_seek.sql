-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS report_usage_attempt_retired;
DROP INDEX CONCURRENTLY IF EXISTS report_usage_attempt_build;
CREATE INDEX CONCURRENTLY report_usage_attempt_build ON report_usage_current(organization_id,attempt_id,period,source_key);
-- +goose StatementBegin
DO $$ BEGIN
 ALTER INDEX report_usage_current_attempt_idx RENAME TO report_usage_attempt_retired;
 ALTER INDEX report_usage_attempt_build RENAME TO report_usage_current_attempt_idx;
END $$;
-- +goose StatementEnd
DROP INDEX CONCURRENTLY IF EXISTS report_usage_attempt_retired;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS report_usage_attempt_retired;
DROP INDEX CONCURRENTLY IF EXISTS report_usage_attempt_build;
CREATE INDEX CONCURRENTLY report_usage_attempt_build ON report_usage_current(organization_id,attempt_id,period);
-- +goose StatementBegin
DO $$ BEGIN
 ALTER INDEX report_usage_current_attempt_idx RENAME TO report_usage_attempt_retired;
 ALTER INDEX report_usage_attempt_build RENAME TO report_usage_current_attempt_idx;
END $$;
-- +goose StatementEnd
DROP INDEX CONCURRENTLY IF EXISTS report_usage_attempt_retired;
RESET lock_timeout;
