-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS report_usage_current_analytics_seek;
CREATE INDEX CONCURRENTLY report_usage_current_analytics_seek ON report_usage_current (organization_id,project_id,period,provider,model,attempt_id,source_key);
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_analytics_seek;
CREATE INDEX CONCURRENTLY attempt_usage_analytics_seek ON attempt_usage (organization_id,project_id,period,provider,model,attempt_id);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY report_usage_current_analytics_seek;
DROP INDEX CONCURRENTLY attempt_usage_analytics_seek;
RESET lock_timeout;
