-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS hosted_ci_classification_receipts_retention_idx;
CREATE INDEX CONCURRENTLY hosted_ci_classification_receipts_retention_idx ON hosted_usage_receipts (organization_id,window_start,receipt_id)
 WHERE starts_with(metric,'scheduled-ci/') OR (metric='api_mutations' AND starts_with(receipt_id,'scheduled_ci_') AND strpos(receipt_id,'/')=0);
DROP INDEX CONCURRENTLY IF EXISTS hosted_ci_usage_receipts_retention_idx;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS hosted_ci_usage_receipts_retention_idx;
CREATE INDEX CONCURRENTLY hosted_ci_usage_receipts_retention_idx ON hosted_usage_receipts (organization_id,window_start,receipt_id)
 WHERE starts_with(metric,'scheduled-ci/');
DROP INDEX CONCURRENTLY IF EXISTS hosted_ci_classification_receipts_retention_idx;
RESET lock_timeout;
